package controlplane

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
)

const defaultMaxAssignments = 100_000

// AssignmentDesiredState is the committed control intent for one immutable
// placement. Cancellation never removes the identity or permits its reuse.
type AssignmentDesiredState string

const (
	AssignmentAssigned  AssignmentDesiredState = "Assigned"
	AssignmentCancelled AssignmentDesiredState = "Cancelled"
)

// AssignmentRecord is one committed placement and its mutable desired state.
type AssignmentRecord struct {
	FunctionID       string
	Placement        scheduler.Assignment
	DesiredState     AssignmentDesiredState
	CreatedAt        time.Time
	UpdatedAt        time.Time
	CreatedRaftIndex uint64
	LastAppliedIndex uint64
	ResourceRevision uint64
}

// Identity returns the immutable normal-serving fence for this placement.
func (r AssignmentRecord) Identity() servingauth.AssignmentIdentity {
	return servingauth.AssignmentIdentity{
		Worker:               r.Placement.Worker,
		AssignmentID:         r.Placement.AssignmentID,
		VersionID:            r.Placement.VersionID,
		AdmissionEpoch:       r.Placement.AdmissionEpoch,
		DeploymentGeneration: r.Placement.DeploymentGeneration,
		PolicyDigest:         r.Placement.PolicyDigest,
		Mode:                 servingauth.ModeNormal,
	}
}

// InstallAssignmentCommand persists a scheduler result before any Worker RPC.
type InstallAssignmentCommand struct {
	FunctionID              string
	Placement               scheduler.Assignment
	IfNoneMatch             bool
	ExpectedScalingRevision uint64
	AppliedIndex            uint64
	UpdatedAt               time.Time
}

// CancelAssignmentCommand conditionally withdraws one exact Assignment intent.
type CancelAssignmentCommand struct {
	AssignmentID             string
	ExpectedResourceRevision uint64
	AppliedIndex             uint64
	UpdatedAt                time.Time
}

// AssignmentStore retains immutable Assignment identities and their desired
// state. Cross-store installs use Catalog -> Release -> Route -> Assignment.
type AssignmentStore struct {
	routes *RouteStore

	mu             sync.Mutex
	maxAssignments int
	records        map[string]AssignmentRecord
	activeCounts   map[assignmentTarget]uint32
}

type assignmentTarget struct {
	functionID           string
	versionID            string
	admissionEpoch       uint64
	deploymentGeneration uint64
}

// NewAssignmentStore binds Assignment intent to the current Route authority.
func NewAssignmentStore(routes *RouteStore) (*AssignmentStore, error) {
	if routes == nil || routes.catalog == nil || routes.releases == nil {
		return nil, errors.New("control-plane assignment store dependencies are required")
	}
	return &AssignmentStore{
		routes:         routes,
		maxAssignments: defaultMaxAssignments,
		records:        make(map[string]AssignmentRecord),
		activeCounts:   make(map[assignmentTarget]uint32),
	}, nil
}

// Install validates a complete placement against the current Ready Route and
// atomically retains it as Assigned intent. The outer command ledger owns
// replay; every reused Assignment ID is rejected here.
func (s *AssignmentStore) Install(command InstallAssignmentCommand) (AssignmentRecord, error) {
	if s == nil || s.routes == nil || s.routes.catalog == nil || s.routes.releases == nil {
		return AssignmentRecord{}, errors.New("control-plane assignment store dependencies are required")
	}
	if err := validateInstallAssignment(command); err != nil {
		return AssignmentRecord{}, err
	}
	candidate := AssignmentRecord{
		FunctionID:       command.FunctionID,
		Placement:        command.Placement,
		DesiredState:     AssignmentAssigned,
		CreatedAt:        command.UpdatedAt.Round(0),
		UpdatedAt:        command.UpdatedAt.Round(0),
		CreatedRaftIndex: command.AppliedIndex,
		LastAppliedIndex: command.AppliedIndex,
		ResourceRevision: 1,
	}

	s.routes.catalog.mu.Lock()
	s.routes.releases.mu.Lock()
	s.routes.mu.Lock()
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.routes.mu.Unlock()
	defer s.routes.releases.mu.Unlock()
	defer s.routes.catalog.mu.Unlock()

	assignmentID := command.Placement.AssignmentID
	if _, exists := s.records[assignmentID]; exists {
		return AssignmentRecord{}, classified(problem.CodeConflict, "assignment id was already used")
	}
	if len(s.records) >= s.maxAssignments {
		return AssignmentRecord{}, classified(problem.CodeOverloaded, "assignment store is full")
	}
	target, deployment, err := s.validatePlacementLocked(command)
	if err != nil {
		return AssignmentRecord{}, err
	}
	if command.ExpectedScalingRevision != deployment.ScalingRevision {
		return AssignmentRecord{}, revisionConflict(
			"scaling_revision",
			command.ExpectedScalingRevision,
			deployment.ScalingRevision,
		)
	}
	targetKey := assignmentTargetFor(command.FunctionID, target)
	if s.activeCounts[targetKey] >= deployment.DesiredReplicas {
		return AssignmentRecord{}, classified(problem.CodeConflict, "deployment desired replica count is already satisfied")
	}
	s.records[assignmentID] = candidate
	s.activeCounts[targetKey]++
	return cloneAssignmentRecord(candidate), nil
}

// Cancel advances Assigned intent to Cancelled with an exact revision CAS.
func (s *AssignmentStore) Cancel(command CancelAssignmentCommand) (AssignmentRecord, error) {
	if s == nil {
		return AssignmentRecord{}, errors.New("control-plane assignment store is nil")
	}
	if err := validateCancelAssignment(command); err != nil {
		return AssignmentRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[command.AssignmentID]
	if !exists {
		return AssignmentRecord{}, classified(problem.CodeNotFound, "assignment was not found")
	}
	if record.ResourceRevision != command.ExpectedResourceRevision {
		return AssignmentRecord{}, revisionConflict(
			"resource_revision",
			command.ExpectedResourceRevision,
			record.ResourceRevision,
		)
	}
	if record.DesiredState != AssignmentAssigned {
		return AssignmentRecord{}, classified(problem.CodeConflict, "assignment is already cancelled")
	}
	if command.UpdatedAt.Before(record.UpdatedAt) {
		return AssignmentRecord{}, problem.Invalid("updated_at", "must not precede the assignment update time")
	}
	if command.AppliedIndex <= record.LastAppliedIndex {
		return AssignmentRecord{}, problem.Invalid("applied_index", "must advance the assignment apply position")
	}
	if record.ResourceRevision == math.MaxUint64 {
		return AssignmentRecord{}, classified(problem.CodeConflict, "assignment revision space is exhausted")
	}
	targetKey := assignmentTargetForRecord(record)
	if s.activeCounts[targetKey] == 0 {
		return AssignmentRecord{}, errors.New("assignment store invariant: active count is missing")
	}
	record.DesiredState = AssignmentCancelled
	record.UpdatedAt = command.UpdatedAt.Round(0)
	record.LastAppliedIndex = command.AppliedIndex
	record.ResourceRevision++
	s.records[command.AssignmentID] = record
	s.activeCounts[targetKey]--
	return cloneAssignmentRecord(record), nil
}

// Get returns one defensive Assignment record by its globally unique ID.
func (s *AssignmentStore) Get(assignmentID string) (AssignmentRecord, error) {
	if s == nil {
		return AssignmentRecord{}, errors.New("control-plane assignment store is nil")
	}
	if !identifierPattern.MatchString(assignmentID) {
		return AssignmentRecord{}, problem.Invalid("assignment_id", "must be a valid identifier")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[assignmentID]
	if !exists {
		return AssignmentRecord{}, classified(problem.CodeNotFound, "assignment was not found")
	}
	return cloneAssignmentRecord(record), nil
}

// Snapshot returns every retained Assignment in stable creation order.
func (s *AssignmentStore) Snapshot() []AssignmentRecord {
	if s == nil {
		return []AssignmentRecord{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]AssignmentRecord, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, cloneAssignmentRecord(record))
	}
	slices.SortFunc(records, compareAssignmentRecord)
	return records
}

func (s *AssignmentStore) validatePlacementLocked(
	command InstallAssignmentCommand,
) (model.RouteTarget, model.Deployment, error) {
	function, exists := s.routes.catalog.functionsByID[command.FunctionID]
	if !exists {
		return model.RouteTarget{}, model.Deployment{}, classified(
			problem.CodeNotFound,
			"assignment function was not found",
		)
	}
	if function.Lifecycle != model.FunctionActive {
		return model.RouteTarget{}, model.Deployment{}, classified(
			problem.CodeConflict,
			"assignment function is not active",
		)
	}
	route, exists := s.routes.routes[command.FunctionID]
	if !exists || !route.Enabled || route.RouteRevision != function.ActiveRouteRevision {
		return model.RouteTarget{}, model.Deployment{}, classified(
			problem.CodeConflict,
			"assignment function has no current enabled route",
		)
	}
	if err := s.routes.validateReadyTargetLocked(route, function); err != nil {
		return model.RouteTarget{}, model.Deployment{}, fmt.Errorf("validating assignment route target: %w", err)
	}
	if command.UpdatedAt.Before(route.UpdatedAt) || command.AppliedIndex <= route.CreatedRaftIndex {
		return model.RouteTarget{}, model.Deployment{}, problem.Invalid(
			"assignment",
			"must be committed after the current route",
		)
	}
	target := route.Targets[0]
	placement := command.Placement
	targetMatches := placement.VersionID == target.VersionID &&
		placement.AdmissionEpoch == target.AdmissionEpoch &&
		placement.DeploymentGeneration == target.DeploymentGeneration &&
		placement.PolicyDigest == target.EffectivePolicyDigest
	if !targetMatches {
		return model.RouteTarget{}, model.Deployment{}, classified(
			problem.CodeStaleGeneration,
			"assignment does not match the current route target",
		)
	}
	version := s.routes.releases.versions[target.VersionID]
	deployment := s.routes.releases.deployments[target.VersionID]
	runtimeMatches := placement.ArtifactDigest == version.ArtifactDigest &&
		placement.ArtifactSize == version.ArtifactSize &&
		placement.ABI == version.ABI &&
		placement.HostAPI == version.HostAPIProfile &&
		placement.FeatureProfile == version.RuntimeFeatureProfile &&
		placement.MemoryMiB == deployment.ResourceLimits.MemoryMiB
	if !runtimeMatches {
		return model.RouteTarget{}, model.Deployment{}, classified(
			problem.CodeConflict,
			"assignment runtime does not match the admitted policy",
		)
	}
	return target, deployment, nil
}

func validateInstallAssignment(command InstallAssignmentCommand) error {
	if !identifierPattern.MatchString(command.FunctionID) {
		return problem.Invalid("function_id", "must be a valid identifier")
	}
	if !command.IfNoneMatch {
		return problem.Invalid("if_none_match", "must be true when creating assignment intent")
	}
	if command.ExpectedScalingRevision == 0 {
		return problem.Invalid("expected_scaling_revision", "must be greater than zero")
	}
	if err := command.Placement.Validate(); err != nil {
		return fmt.Errorf("validating assignment placement: %w", err)
	}
	if command.AppliedIndex == 0 {
		return problem.Invalid("applied_index", "must be greater than zero")
	}
	return validateUTC("updated_at", command.UpdatedAt)
}

func validateCancelAssignment(command CancelAssignmentCommand) error {
	if !identifierPattern.MatchString(command.AssignmentID) {
		return problem.Invalid("assignment_id", "must be a valid identifier")
	}
	if command.ExpectedResourceRevision == 0 {
		return problem.Invalid("expected_resource_revision", "must be greater than zero")
	}
	if command.AppliedIndex == 0 {
		return problem.Invalid("applied_index", "must be greater than zero")
	}
	return validateUTC("updated_at", command.UpdatedAt)
}

func cloneAssignmentRecord(record AssignmentRecord) AssignmentRecord {
	return record
}

func assignmentTargetFor(functionID string, target model.RouteTarget) assignmentTarget {
	return assignmentTarget{
		functionID:           functionID,
		versionID:            target.VersionID,
		admissionEpoch:       target.AdmissionEpoch,
		deploymentGeneration: target.DeploymentGeneration,
	}
}

func assignmentTargetForRecord(record AssignmentRecord) assignmentTarget {
	return assignmentTarget{
		functionID:           record.FunctionID,
		versionID:            record.Placement.VersionID,
		admissionEpoch:       record.Placement.AdmissionEpoch,
		deploymentGeneration: record.Placement.DeploymentGeneration,
	}
}

func compareAssignmentRecord(left, right AssignmentRecord) int {
	if left.CreatedRaftIndex < right.CreatedRaftIndex {
		return -1
	}
	if left.CreatedRaftIndex > right.CreatedRaftIndex {
		return 1
	}
	return strings.Compare(left.Placement.AssignmentID, right.Placement.AssignmentID)
}
