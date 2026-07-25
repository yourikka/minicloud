package controlplane

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	validatorprotocol "github.com/yourikka/minicloud/internal/validator/protocol"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

const (
	DefaultMaxVersionsPerFunction = 100
	defaultMaxLogBytes            = 256 << 10
)

// CreateVersionCommand records immutable Uploaded Version metadata after the
// Artifact CAS has atomically published the referenced bytes. Local Core uses
// a server-generated VersionID that is globally unique, which is stronger than
// the v1 minimum of per-Function uniqueness and keeps Route identity unambiguous.
type CreateVersionCommand struct {
	IfNoneMatch  bool          `json:"if_none_match"`
	AppliedIndex uint64        `json:"applied_index"`
	Version      model.Version `json:"version"`
}

// StartValidationCommand fences a Validator attempt. ValidationID is generated
// by the command creator, so a late child result cannot overwrite a newer
// Version state.
type StartValidationCommand struct {
	VersionID                string    `json:"version_id"`
	ExpectedResourceRevision uint64    `json:"expected_resource_revision"`
	ValidationID             string    `json:"validation_id"`
	UpdatedAt                time.Time `json:"updated_at"`
	AppliedIndex             uint64    `json:"applied_index"`
}

// CompleteValidationCommand consumes exactly one fenced Validator report. A
// valid report must carry the Deployment Generation 1 that becomes immutable
// in the same state transition as Version Ready.
type CompleteValidationCommand struct {
	VersionID                string                   `json:"version_id"`
	ExpectedResourceRevision uint64                   `json:"expected_resource_revision"`
	Report                   validatorprotocol.Report `json:"report"`
	Deployment               *model.Deployment        `json:"deployment,omitempty"`
	UpdatedAt                time.Time                `json:"updated_at"`
	AppliedIndex             uint64                   `json:"applied_index"`
}

// ReleaseSnapshot is an ordered defensive view of immutable Versions and their
// initial Deployment state.
type ReleaseSnapshot struct {
	Versions           []model.Version           `json:"versions"`
	Deployments        []model.Deployment        `json:"deployments"`
	PendingValidations []VersionAdmissionAttempt `json:"pending_validations"`
}

// VersionAdmissionAttempt is the persisted fence for one validator process.
// It is retained only while the Version is Validating.
type VersionAdmissionAttempt struct {
	VersionID    string `json:"version_id"`
	ValidationID string `json:"validation_id"`
}

// ReleaseStore owns the Version validation lifecycle. A future Raft FSM must
// call it in the same transaction as the Function catalog and Operation Ledger.
type ReleaseStore struct {
	mu sync.Mutex

	functions             *Catalog
	versions              map[string]model.Version
	versionCountByFunc    map[string]int
	validationIDByVersion map[string]string
	deployments           map[string]model.Deployment
}

// NewReleaseStore returns an empty store bound to the Function catalog that
// owns Version parent identities.
func NewReleaseStore(functions *Catalog) *ReleaseStore {
	return &ReleaseStore{
		functions:             functions,
		versions:              make(map[string]model.Version),
		versionCountByFunc:    make(map[string]int),
		validationIDByVersion: make(map[string]string),
		deployments:           make(map[string]model.Deployment),
	}
}

// CreateVersion retains one immutable Version in Uploaded state.
func (s *ReleaseStore) CreateVersion(command CreateVersionCommand) (model.Version, error) {
	if s == nil {
		return model.Version{}, errors.New("control-plane release store is nil")
	}
	if err := validateCreateVersion(command); err != nil {
		return model.Version{}, err
	}
	if s.functions == nil {
		return model.Version{}, errors.New("control-plane release store has no function catalog")
	}
	function, _, err := s.functions.GetFunction(command.Version.FunctionID)
	if err != nil {
		return model.Version{}, fmt.Errorf("resolving version parent function: %w", err)
	}
	if function.Lifecycle == model.FunctionDeleting || function.Lifecycle == model.FunctionTombstoned {
		return model.Version{}, classified(problem.CodeConflict, "function does not accept new versions")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.versions[command.Version.VersionID]; exists {
		return model.Version{}, classified(problem.CodeConflict, "version id already exists")
	}
	if s.versionCountByFunc[command.Version.FunctionID] >= DefaultMaxVersionsPerFunction {
		return model.Version{}, classified(problem.CodeOverloaded, "function version limit reached")
	}
	s.versions[command.Version.VersionID] = cloneVersion(command.Version)
	s.versionCountByFunc[command.Version.FunctionID]++
	return cloneVersion(command.Version), nil
}

// StartValidation changes an Uploaded Version to Validating and records the
// only Validation ID whose report may complete this transition.
func (s *ReleaseStore) StartValidation(command StartValidationCommand) (model.Version, error) {
	if s == nil {
		return model.Version{}, errors.New("control-plane release store is nil")
	}
	if err := validateStartValidation(command); err != nil {
		return model.Version{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	version, exists := s.versions[command.VersionID]
	if !exists {
		return model.Version{}, classified(problem.CodeNotFound, "version was not found")
	}
	if version.ResourceRevision != command.ExpectedResourceRevision {
		return model.Version{}, revisionConflict("resource_revision", command.ExpectedResourceRevision, version.ResourceRevision)
	}
	if version.State != model.VersionUploaded {
		return model.Version{}, classified(problem.CodeConflict, "version is not awaiting validation")
	}
	version.State = model.VersionValidating
	version.UpdatedAt = command.UpdatedAt.Round(0)
	version.ResourceRevision++
	if err := version.Validate(); err != nil {
		return model.Version{}, fmt.Errorf("validating started version: %w", err)
	}
	s.versions[version.VersionID] = version
	s.validationIDByVersion[version.VersionID] = command.ValidationID
	return cloneVersion(version), nil
}

// CompleteValidation records a validator outcome once. A valid report creates
// Generation 1 atomically; an invalid report creates a safe Failed Version and
// cannot leave an unvalidated Deployment behind.
func (s *ReleaseStore) CompleteValidation(command CompleteValidationCommand) (model.Version, *model.Deployment, error) {
	if s == nil {
		return model.Version{}, nil, errors.New("control-plane release store is nil")
	}
	if err := validateCompleteValidation(command); err != nil {
		return model.Version{}, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	version, exists := s.versions[command.VersionID]
	if !exists {
		return model.Version{}, nil, classified(problem.CodeNotFound, "version was not found")
	}
	if version.ResourceRevision != command.ExpectedResourceRevision {
		return model.Version{}, nil, revisionConflict("resource_revision", command.ExpectedResourceRevision, version.ResourceRevision)
	}
	if version.State != model.VersionValidating {
		return model.Version{}, nil, classified(problem.CodeStaleGeneration, "version is no longer validating")
	}
	if s.validationIDByVersion[version.VersionID] != command.Report.ValidationID {
		return model.Version{}, nil, classified(problem.CodeStaleGeneration, "validator report does not match the active validation")
	}
	if err := validateReportForVersion(command.Report, version); err != nil {
		return model.Version{}, nil, err
	}
	if command.UpdatedAt.Before(version.UpdatedAt) {
		return model.Version{}, nil, problem.Invalid("updated_at", "must not precede the current version update time")
	}

	version.UpdatedAt = command.UpdatedAt.Round(0)
	version.ResourceRevision++
	if !command.Report.Valid {
		version.State = model.VersionFailed
		version.ValidationError = &model.SafeError{Code: problem.Code(command.Report.Code), Message: command.Report.Message}
		if err := version.Validate(); err != nil {
			return model.Version{}, nil, fmt.Errorf("validating failed version: %w", err)
		}
		s.versions[version.VersionID] = version
		delete(s.validationIDByVersion, version.VersionID)
		return cloneVersion(version), nil, nil
	}

	if err := validateInitialDeployment(*command.Deployment, version, command.AppliedIndex, command.UpdatedAt); err != nil {
		return model.Version{}, nil, err
	}
	version.State = model.VersionReady
	if err := version.Validate(); err != nil {
		return model.Version{}, nil, fmt.Errorf("validating ready version: %w", err)
	}
	deployment := cloneDeployment(*command.Deployment)
	s.versions[version.VersionID] = version
	s.deployments[version.VersionID] = deployment
	delete(s.validationIDByVersion, version.VersionID)
	return cloneVersion(version), pointerDeployment(deployment), nil
}

// Get returns the current immutable Version and its Generation 1 Deployment,
// when validation succeeded.
func (s *ReleaseStore) Get(versionID string) (model.Version, *model.Deployment, error) {
	if s == nil {
		return model.Version{}, nil, errors.New("control-plane release store is nil")
	}
	if !identifierPattern.MatchString(versionID) {
		return model.Version{}, nil, problem.Invalid("version_id", "must be a valid identifier")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	version, exists := s.versions[versionID]
	if !exists {
		return model.Version{}, nil, classified(problem.CodeNotFound, "version was not found")
	}
	deployment, exists := s.deployments[versionID]
	if !exists {
		return cloneVersion(version), nil, nil
	}
	return cloneVersion(version), pointerDeployment(deployment), nil
}

// Snapshot returns ordered defensive copies for future snapshot and state-digest
// construction.
func (s *ReleaseStore) Snapshot() ReleaseSnapshot {
	if s == nil {
		return ReleaseSnapshot{
			Versions:           []model.Version{},
			Deployments:        []model.Deployment{},
			PendingValidations: []VersionAdmissionAttempt{},
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := ReleaseSnapshot{
		Versions:           make([]model.Version, 0, len(s.versions)),
		Deployments:        make([]model.Deployment, 0, len(s.deployments)),
		PendingValidations: make([]VersionAdmissionAttempt, 0, len(s.validationIDByVersion)),
	}
	for _, version := range s.versions {
		snapshot.Versions = append(snapshot.Versions, cloneVersion(version))
	}
	slices.SortFunc(snapshot.Versions, compareVersion)
	for _, version := range snapshot.Versions {
		if deployment, exists := s.deployments[version.VersionID]; exists {
			snapshot.Deployments = append(snapshot.Deployments, cloneDeployment(deployment))
		}
		if validationID, exists := s.validationIDByVersion[version.VersionID]; exists {
			snapshot.PendingValidations = append(snapshot.PendingValidations, VersionAdmissionAttempt{
				VersionID:    version.VersionID,
				ValidationID: validationID,
			})
		}
	}
	return snapshot
}

func validateCreateVersion(command CreateVersionCommand) error {
	if !command.IfNoneMatch {
		return problem.Invalid("if_none_match", "is required when creating a version")
	}
	if command.AppliedIndex == 0 {
		return problem.Invalid("applied_index", "must be greater than zero")
	}
	if err := command.Version.Validate(); err != nil {
		return err
	}
	if command.Version.State != model.VersionUploaded || command.Version.ValidationError != nil {
		return problem.Invalid("state", "new version must be Uploaded without a validation error")
	}
	if command.Version.ID != command.Version.VersionID || command.Version.AdmissionEpoch != 1 {
		return problem.Invalid("version", "new version id must match version id with admission epoch one")
	}
	if command.Version.RuntimeFeatureProfile != wasmprofile.FeatureProfile {
		return problem.Invalid("runtime_feature_profile", "must use the fixed v1 feature profile")
	}
	if command.Version.CreatedRaftIndex != command.AppliedIndex || command.Version.ResourceRevision != 1 {
		return problem.Invalid("version", "must be created at this applied index with resource revision one")
	}
	return nil
}

func validateStartValidation(command StartValidationCommand) error {
	if !identifierPattern.MatchString(command.VersionID) || !identifierPattern.MatchString(command.ValidationID) {
		return problem.Invalid("validation", "version id and validation id must be valid identifiers")
	}
	if command.ExpectedResourceRevision == 0 || command.AppliedIndex == 0 {
		return problem.Invalid("revision", "expected resource revision and applied index must be greater than zero")
	}
	return validateUTC("updated_at", command.UpdatedAt)
}

func validateCompleteValidation(command CompleteValidationCommand) error {
	if !identifierPattern.MatchString(command.VersionID) || command.ExpectedResourceRevision == 0 || command.AppliedIndex == 0 {
		return problem.Invalid("validation", "version id, expected revision, and applied index are required")
	}
	if err := validateUTC("updated_at", command.UpdatedAt); err != nil {
		return err
	}
	if err := command.Report.Validate(); err != nil {
		return problem.Invalid("report", "must be a valid validator report")
	}
	if command.Report.Valid && command.Deployment == nil {
		return problem.Invalid("deployment", "is required for a successful validation")
	}
	if !command.Report.Valid && command.Deployment != nil {
		return problem.Invalid("deployment", "is not allowed for a failed validation")
	}
	return nil
}

func validateReportForVersion(report validatorprotocol.Report, version model.Version) error {
	if report.ArtifactDigest != version.ArtifactDigest || report.ArtifactSize != version.ArtifactSize ||
		report.RuntimeFeatureProfile != version.RuntimeFeatureProfile {
		return problem.Invalid("report", "does not match immutable version artifact or runtime profile")
	}
	if report.RuntimeEngine != validatorprotocol.EngineCompiler {
		return problem.Invalid("report.runtime_engine", "must use the fixed Local Core compiler engine")
	}
	if !report.Valid && (!utf8.ValidString(report.Message) || len(report.Message) > MaxOperationMessage) {
		return problem.Invalid("report.message", "must be safe UTF-8 within 512 bytes")
	}
	return nil
}

func validateInitialDeployment(deployment model.Deployment, version model.Version, appliedIndex uint64, updatedAt time.Time) error {
	if err := deployment.Validate(); err != nil {
		return err
	}
	exactLimits := deployment.ResourceLimits.Timeout == version.ResourceRequest.Timeout &&
		deployment.ResourceLimits.MemoryMiB == version.ResourceRequest.MemoryMiB &&
		deployment.ResourceLimits.MaxInputBytes == version.ResourceRequest.MaxInputBytes &&
		deployment.ResourceLimits.MaxOutputBytes == version.ResourceRequest.MaxOutputBytes &&
		deployment.ResourceLimits.MaxLogBytes == defaultMaxLogBytes
	if deployment.VersionID != version.VersionID || deployment.Generation != 1 || deployment.ScalingRevision != 1 ||
		deployment.CreatedRaftIndex != appliedIndex || deployment.ResourceRevision != 1 ||
		!deployment.CreatedAt.Equal(updatedAt) || !deployment.UpdatedAt.Equal(updatedAt) || !exactLimits ||
		deployment.MinReplicas != 1 || deployment.MaxReplicas != 1 || deployment.DesiredReplicas != 1 ||
		deployment.ReadyReplicas != 0 || deployment.TargetConcurrency != version.ResourceRequest.MaxConcurrency ||
		deployment.ScalingMode != model.ScalingManual || deployment.IdleTimeout != 0 || deployment.DesiredPhase != model.DeploymentActive {
		return problem.Invalid("deployment", "must be the fixed Local Core generation one policy")
	}
	if !capabilitiesSubset(deployment.GrantedCapabilities, version.RequestedCapabilities) {
		return problem.Invalid("deployment.granted_capabilities", "must be requested by the version")
	}
	policy := model.EffectivePolicy{
		VersionID: version.VersionID, AdmissionEpoch: version.AdmissionEpoch, DeploymentGeneration: deployment.Generation,
		ArtifactDigest: version.ArtifactDigest, ArtifactSize: version.ArtifactSize, ABI: version.ABI,
		HostAPIProfile: version.HostAPIProfile, RuntimeFeatureProfile: version.RuntimeFeatureProfile,
		ResourceLimits: deployment.ResourceLimits, MaxConcurrency: version.ResourceRequest.MaxConcurrency,
		GrantedCapabilities: deployment.GrantedCapabilities,
	}
	policyDigest, err := policy.Digest()
	if err != nil {
		return fmt.Errorf("calculating initial effective policy: %w", err)
	}
	if deployment.EffectivePolicyDigest != policyDigest {
		return problem.Invalid("deployment.effective_policy_digest", "does not match generation one effective policy")
	}
	return nil
}

func capabilitiesSubset(granted, requested []model.CapabilityRequest) bool {
	requestedSet := make(map[string]struct{}, len(requested))
	for _, capability := range requested {
		requestedSet[capability.Name+"\x00"+capability.Version] = struct{}{}
	}
	for _, capability := range granted {
		if _, exists := requestedSet[capability.Name+"\x00"+capability.Version]; !exists {
			return false
		}
	}
	return true
}

func cloneVersion(version model.Version) model.Version {
	version.RequestedCapabilities = slices.Clone(version.RequestedCapabilities)
	if version.ValidationError != nil {
		value := *version.ValidationError
		version.ValidationError = &value
	}
	return version
}

func cloneDeployment(deployment model.Deployment) model.Deployment {
	deployment.GrantedCapabilities = slices.Clone(deployment.GrantedCapabilities)
	return deployment
}

func pointerDeployment(deployment model.Deployment) *model.Deployment {
	copy := cloneDeployment(deployment)
	return &copy
}

func compareVersion(left, right model.Version) int {
	if left.FunctionID != right.FunctionID {
		return strings.Compare(left.FunctionID, right.FunctionID)
	}
	return strings.Compare(left.VersionID, right.VersionID)
}
