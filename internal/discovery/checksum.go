package discovery

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/servingauth"
)

type canonicalSnapshotV1 struct {
	SchemaVersion   string                `json:"schema_version"`
	FunctionID      string                `json:"function_id"`
	DiscoveryEpoch  uint64                `json:"discovery_epoch"`
	ServingSequence uint64                `json:"serving_sequence"`
	GeneratedAt     string                `json:"generated_at"`
	Function        Function              `json:"function"`
	Trigger         HTTPTrigger           `json:"http_trigger"`
	Route           Route                 `json:"route"`
	Endpoints       []canonicalEndpointV1 `json:"endpoints"`
}

type canonicalEndpointV1 struct {
	AssignmentID         string           `json:"assignment_id"`
	VersionID            string           `json:"version_id"`
	AdmissionEpoch       uint64           `json:"admission_epoch"`
	DeploymentGeneration uint64           `json:"deployment_generation"`
	PolicyDigest         digest.SHA256    `json:"effective_policy_digest"`
	Mode                 servingauth.Mode `json:"mode"`
	WorkerID             string           `json:"worker_id"`
	BootID               string           `json:"boot_id"`
	SessionEpoch         uint64           `json:"session_epoch"`
	Address              string           `json:"address"`
	State                EndpointState    `json:"state"`
}

// Validate verifies all fields, canonical ordering, and the complete checksum.
func (s Snapshot) Validate() error {
	if err := validateSnapshotHeader(s); err != nil {
		return err
	}
	if err := validateFunction(s.Function); err != nil {
		return err
	}
	if s.FunctionID != s.Function.ID {
		return problem.Invalid("function_id", "must match the Function projection")
	}
	if err := validateHTTPTrigger(s.Trigger, s.FunctionID); err != nil {
		return err
	}
	if err := validateRoute(s.Route, s.FunctionID); err != nil {
		return err
	}
	if len(s.Endpoints) > HardMaxEndpoints {
		return problem.Invalid("endpoints", "exceeds the v1 endpoint limit")
	}
	if s.Endpoints == nil {
		return problem.Invalid("endpoints", "must be an explicit array")
	}
	if !slices.IsSortedFunc(s.Route.Targets, compareRouteTargets) {
		return problem.Invalid("route.targets", "must use canonical target order")
	}
	if !slices.IsSortedFunc(s.Endpoints, compareEndpoints) {
		return problem.Invalid("endpoints", "must use canonical endpoint order")
	}
	if len(s.Endpoints) != 0 && (s.Function.Lifecycle != model.FunctionActive || !s.Trigger.Enabled || !s.Route.Present || !s.Route.Enabled) {
		return problem.Invalid("endpoints", "must be empty when serving is disabled")
	}
	seen := make(map[string]struct{}, len(s.Endpoints))
	for _, endpoint := range s.Endpoints {
		if err := endpoint.Validate(); err != nil {
			return err
		}
		if _, exists := seen[endpoint.Assignment.AssignmentID]; exists {
			return problem.Invalid("endpoints.assignment_id", "contains a duplicate Assignment ID")
		}
		seen[endpoint.Assignment.AssignmentID] = struct{}{}
		if reasons := routeMismatchReasons(s.Route.Targets, endpoint.Assignment); len(reasons) != 0 {
			return problem.Invalid("endpoints", "contains an Endpoint outside the Route target fence")
		}
	}
	if _, err := digest.ParseSHA256(s.Checksum.String()); err != nil {
		return problem.Invalid("checksum", "must be a lowercase SHA-256 digest")
	}
	want, err := calculateChecksum(s)
	if err != nil {
		return err
	}
	if s.Checksum != want {
		return problem.Invalid("checksum", "does not match the complete serving snapshot")
	}
	return nil
}

// Clone returns a defensive copy of every mutable Snapshot field.
func (s Snapshot) Clone() Snapshot {
	s.Function = cloneFunction(s.Function)
	s.Trigger = cloneHTTPTrigger(s.Trigger)
	s.Route = cloneRoute(s.Route)
	s.Endpoints = slices.Clone(s.Endpoints)
	return s
}

func calculateChecksum(snapshot Snapshot) (digest.SHA256, error) {
	canonical := canonicalSnapshotV1{
		SchemaVersion:   snapshot.SchemaVersion,
		FunctionID:      snapshot.FunctionID,
		DiscoveryEpoch:  snapshot.DiscoveryEpoch,
		ServingSequence: snapshot.ServingSequence,
		GeneratedAt:     snapshot.GeneratedAt.Format(time.RFC3339Nano),
		Function:        cloneFunction(snapshot.Function),
		Trigger:         cloneHTTPTrigger(snapshot.Trigger),
		Route:           cloneRoute(snapshot.Route),
		Endpoints:       make([]canonicalEndpointV1, 0, len(snapshot.Endpoints)),
	}
	for _, endpoint := range snapshot.Endpoints {
		identity := endpoint.Assignment
		canonical.Endpoints = append(canonical.Endpoints, canonicalEndpointV1{
			AssignmentID:         identity.AssignmentID,
			VersionID:            identity.VersionID,
			AdmissionEpoch:       identity.AdmissionEpoch,
			DeploymentGeneration: identity.DeploymentGeneration,
			PolicyDigest:         identity.PolicyDigest,
			Mode:                 identity.Mode,
			WorkerID:             identity.Worker.WorkerID,
			BootID:               identity.Worker.BootID,
			SessionEpoch:         identity.Worker.SessionEpoch,
			Address:              endpoint.Address,
			State:                endpoint.State,
		})
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshaling canonical serving snapshot: %w", err)
	}
	return digest.CanonicalJSON(ServingChecksumDomain, ServingChecksumVersion, encoded)
}

func validateSnapshotHeader(snapshot Snapshot) error {
	if snapshot.SchemaVersion != ServingSchemaVersion {
		return problem.Invalid("schema_version", "is not supported")
	}
	if !identifierPattern.MatchString(snapshot.FunctionID) {
		return problem.Invalid("function_id", "must be a valid identifier")
	}
	if snapshot.DiscoveryEpoch == 0 {
		return problem.Invalid("discovery_epoch", "must be greater than zero")
	}
	if snapshot.ServingSequence == 0 {
		return problem.Invalid("serving_sequence", "must be greater than zero")
	}
	if snapshot.GeneratedAt.IsZero() || !isUTC(snapshot.GeneratedAt) {
		return problem.Invalid("generated_at", "must be a non-zero UTC time")
	}
	return nil
}

func validateFunction(function Function) error {
	if !identifierPattern.MatchString(function.ID) || !functionNamePattern.MatchString(function.Name) {
		return problem.Invalid("function", "contains an invalid ID or name")
	}
	if function.ResourceRevision == 0 {
		return problem.Invalid("function.resource_revision", "must be greater than zero")
	}
	switch function.Lifecycle {
	case model.FunctionActive, model.FunctionDisabled, model.FunctionDeleting, model.FunctionTombstoned:
		return nil
	default:
		return problem.Invalid("function.lifecycle", "is not supported")
	}
}

func validateHTTPTrigger(trigger HTTPTrigger, functionID string) error {
	if !identifierPattern.MatchString(trigger.ID) || trigger.FunctionID != functionID {
		return problem.Invalid("http_trigger", "contains an invalid ID or Function reference")
	}
	if trigger.ResourceRevision == 0 {
		return problem.Invalid("http_trigger.resource_revision", "must be greater than zero")
	}
	switch trigger.AuthPolicy {
	case AuthPublic:
		if trigger.TokenVerifierDigest != nil {
			return problem.Invalid("http_trigger.token_verifier_digest", "must be absent for public auth")
		}
	case AuthToken:
		if trigger.TokenVerifierDigest == nil {
			return problem.Invalid("http_trigger.token_verifier_digest", "is required for token auth")
		}
		if _, err := digest.ParseSHA256(trigger.TokenVerifierDigest.String()); err != nil {
			return problem.Invalid("http_trigger.token_verifier_digest", "must be a lowercase SHA-256 digest")
		}
	default:
		return problem.Invalid("http_trigger.auth_policy", "is not supported")
	}
	return nil
}

func validateRoute(route Route, functionID string) error {
	if route.FunctionID != functionID {
		return problem.Invalid("route.function_id", "must match the snapshot Function")
	}
	if !route.Present {
		if route.ResourceRevision != 0 || route.RouteRevision != 0 || route.Enabled || len(route.Targets) != 0 ||
			route.Affinity != "" || route.AffinityHeader != "" || route.HashVersion != "" || route.SaltID != "" || len(route.Salt) != 0 {
			return problem.Invalid("route", "an absent Route must contain only its Function ID")
		}
		return nil
	}
	if route.ResourceRevision == 0 || route.RouteRevision == 0 {
		return problem.Invalid("route.revision", "resource and Route revisions must be greater than zero")
	}
	if len(route.Targets) > model.MaxRouteTargets {
		return problem.Invalid("route.targets", "must not contain more than 32 entries")
	}
	if route.Enabled == (len(route.Targets) == 0) {
		return problem.Invalid("route.targets", "must be non-empty exactly when the Route is enabled")
	}
	if route.HashVersion != model.HashVersionSHA256BPSV1 || !identifierPattern.MatchString(route.SaltID) || len(route.Salt) != 16 {
		return problem.Invalid("route.hash", "contains an unsupported hash profile or Salt")
	}
	switch route.Affinity {
	case model.AffinityRequestID, model.AffinityIdempotencyKey:
		if route.AffinityHeader != "" {
			return problem.Invalid("route.affinity_header", "is only valid for header affinity")
		}
	case model.AffinityHeader:
		if !utf8.ValidString(route.AffinityHeader) || !httpTokenPattern.MatchString(route.AffinityHeader) {
			return problem.Invalid("route.affinity_header", "is required for header affinity")
		}
	default:
		return problem.Invalid("route.affinity", "is not supported")
	}
	var total uint32
	seen := make(map[string]struct{}, len(route.Targets))
	for _, target := range route.Targets {
		if !identifierPattern.MatchString(target.VersionID) || target.AdmissionEpoch == 0 || target.DeploymentGeneration == 0 ||
			target.WeightBasisPoints == 0 || target.WeightBasisPoints > model.TotalRouteWeightBasisPoints {
			return problem.Invalid("route.targets", "contains an invalid target")
		}
		if _, err := digest.ParseSHA256(target.EffectivePolicyDigest.String()); err != nil {
			return problem.Invalid("route.targets", "contains an invalid policy digest")
		}
		key := target.VersionID + "\x00" + strconv.FormatUint(target.DeploymentGeneration, 10)
		if _, exists := seen[key]; exists {
			return problem.Invalid("route.targets", "contains a duplicate target")
		}
		seen[key] = struct{}{}
		total += target.WeightBasisPoints
	}
	if len(route.Targets) > 0 && total != model.TotalRouteWeightBasisPoints {
		return problem.Invalid("route.targets", "weights must total 10000 basis points")
	}
	return nil
}

func compareRouteTargets(left, right model.RouteTarget) int {
	if value := compareString(left.VersionID, right.VersionID); value != 0 {
		return value
	}
	return compareUint64(left.DeploymentGeneration, right.DeploymentGeneration)
}

func compareEndpoints(left, right Endpoint) int {
	return compareEndpointIdentity(left.Assignment, right.Assignment)
}

func compareEndpointIdentity(left, right servingauth.AssignmentIdentity) int {
	if value := compareString(left.VersionID, right.VersionID); value != 0 {
		return value
	}
	if value := compareUint64(left.DeploymentGeneration, right.DeploymentGeneration); value != 0 {
		return value
	}
	if value := compareString(left.AssignmentID, right.AssignmentID); value != 0 {
		return value
	}
	if value := compareString(left.Worker.WorkerID, right.Worker.WorkerID); value != 0 {
		return value
	}
	if value := compareString(left.Worker.BootID, right.Worker.BootID); value != 0 {
		return value
	}
	return compareUint64(left.Worker.SessionEpoch, right.Worker.SessionEpoch)
}

func compareString(left, right string) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func compareUint64(left, right uint64) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func cloneFunction(function Function) Function {
	return function
}

func cloneHTTPTrigger(trigger HTTPTrigger) HTTPTrigger {
	if trigger.TokenVerifierDigest != nil {
		value := *trigger.TokenVerifierDigest
		trigger.TokenVerifierDigest = &value
	}
	return trigger
}

func cloneRoute(route Route) Route {
	route.Targets = slices.Clone(route.Targets)
	route.Salt = slices.Clone(route.Salt)
	if route.Targets == nil {
		route.Targets = make([]model.RouteTarget, 0)
	}
	if route.Salt == nil {
		route.Salt = make([]byte, 0)
	}
	return route
}

func cloneDecisions(decisions []CandidateDecision) []CandidateDecision {
	clone := slices.Clone(decisions)
	for index := range clone {
		clone[index].Reasons = slices.Clone(clone[index].Reasons)
	}
	return clone
}

func isUTC(value time.Time) bool {
	_, offset := value.Zone()
	return offset == 0
}
