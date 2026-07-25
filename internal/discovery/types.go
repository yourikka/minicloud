// Package discovery builds complete, checksummed serving views for Gateways.
// It contains no transport, persistence, or wall-clock reads.
package discovery

import (
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
)

const (
	ServingSchemaVersion   = "minicloud-serving-v1"
	ServingChecksumDomain  = "serving-snapshot"
	ServingChecksumVersion = "v1"

	DefaultMaxEndpoints = 1_000
	HardMaxEndpoints    = 1_000
)

// AuthPolicy controls whether the default HTTP Trigger requires a token.
type AuthPolicy string

const (
	AuthPublic AuthPolicy = "public"
	AuthToken  AuthPolicy = "token"
)

// AssignmentDesiredState is the committed Assignment intent considered by
// the serving projection.
type AssignmentDesiredState string

const (
	AssignmentAssigned  AssignmentDesiredState = "Assigned"
	AssignmentCancelled AssignmentDesiredState = "Cancelled"
)

// EndpointState is the only actual Replica state publishable to a Gateway.
type EndpointState string

const EndpointReady EndpointState = "Ready"

// ExclusionReason is a stable explanation for omitting one candidate.
type ExclusionReason string

const (
	ReasonFunctionNotActive            ExclusionReason = "function_not_active"
	ReasonTriggerDisabled              ExclusionReason = "trigger_disabled"
	ReasonRouteDisabled                ExclusionReason = "route_disabled"
	ReasonInvalidAssignment            ExclusionReason = "invalid_assignment"
	ReasonAssignmentNotAssigned        ExclusionReason = "assignment_not_assigned"
	ReasonAuthorizationNotInstalled    ExclusionReason = "authorization_not_installed"
	ReasonAuthorizationMismatch        ExclusionReason = "authorization_mismatch"
	ReasonAssignmentModeNotNormal      ExclusionReason = "assignment_mode_not_normal"
	ReasonInvalidWorkerObservation     ExclusionReason = "invalid_worker_observation"
	ReasonWorkerSessionMismatch        ExclusionReason = "worker_session_mismatch"
	ReasonWorkerNotSchedulable         ExclusionReason = "worker_not_schedulable"
	ReasonWorkerNotReady               ExclusionReason = "worker_not_ready"
	ReasonWorkerDraining               ExclusionReason = "worker_draining"
	ReasonReplicaNotReady              ExclusionReason = "replica_not_ready"
	ReasonRouteTargetMissing           ExclusionReason = "route_target_missing"
	ReasonAdmissionEpochMismatch       ExclusionReason = "admission_epoch_mismatch"
	ReasonDeploymentGenerationMismatch ExclusionReason = "deployment_generation_mismatch"
	ReasonPolicyDigestMismatch         ExclusionReason = "policy_digest_mismatch"
	ReasonInvalidAddress               ExclusionReason = "invalid_address"
)

// Function is the serving-safe Function projection. Description, labels, and
// persistence timestamps are deliberately excluded.
type Function struct {
	ID               string                  `json:"id"`
	Name             string                  `json:"name"`
	ResourceRevision uint64                  `json:"resource_revision"`
	Lifecycle        model.FunctionLifecycle `json:"lifecycle"`
}

// HTTPTrigger is the complete default HTTP Trigger authentication view.
// TokenVerifierDigest is sensitive in-memory data and must not be logged or
// placed in a public/diagnostic projection.
type HTTPTrigger struct {
	ID                  string         `json:"id"`
	FunctionID          string         `json:"function_id"`
	ResourceRevision    uint64         `json:"resource_revision"`
	Enabled             bool           `json:"enabled"`
	AuthPolicy          AuthPolicy     `json:"auth_policy"`
	TokenVerifierDigest *digest.SHA256 `json:"token_verifier_digest,omitempty"`
}

// Route is the complete internal routing projection. Present=false represents
// a Function for which no Route has been published yet.
type Route struct {
	Present          bool                 `json:"present"`
	FunctionID       string               `json:"function_id"`
	ResourceRevision uint64               `json:"resource_revision"`
	RouteRevision    uint64               `json:"route_revision"`
	Enabled          bool                 `json:"enabled"`
	Targets          []model.RouteTarget  `json:"targets"`
	Affinity         model.AffinitySource `json:"affinity_source"`
	AffinityHeader   string               `json:"affinity_header,omitempty"`
	HashVersion      string               `json:"hash_version"`
	SaltID           string               `json:"salt_id"`
	Salt             []byte               `json:"salt"`
}

// Endpoint carries the exact Worker/Assignment fence installed by the Worker.
type Endpoint struct {
	Assignment servingauth.AssignmentIdentity `json:"assignment"`
	Address    string                         `json:"address"`
	State      EndpointState                  `json:"state"`
}

// Validate checks the immutable Endpoint fence and internal address.
func (e Endpoint) Validate() error {
	if err := e.Assignment.Validate(); err != nil {
		return problem.Invalid("endpoint.assignment", "contains an invalid Assignment fence")
	}
	if e.Assignment.Mode != servingauth.ModeNormal || e.State != EndpointReady {
		return problem.Invalid("endpoint", "is not a normal ready Endpoint")
	}
	if !validEndpointAddress(e.Address) {
		return problem.Invalid("endpoint.address", "is not a valid internal address")
	}
	return nil
}

// EndpointCandidate joins committed Assignment intent, current Worker state,
// Replica readiness, authorization installation, and its internal RPC address.
type EndpointCandidate struct {
	Assignment    servingauth.AssignmentIdentity
	DesiredState  AssignmentDesiredState
	Worker        scheduler.WorkerSnapshot
	ReplicaReady  bool
	Authorization *servingauth.Authorization
	Address       string
}

// CandidateDecision records whether a candidate entered the snapshot and why.
type CandidateDecision struct {
	AssignmentID string
	Included     bool
	Reasons      []ExclusionReason
}

// Snapshot is one atomic Function-scoped serving view. Checksum covers every
// other field, including the verifier digest and internal Route salt.
type Snapshot struct {
	SchemaVersion   string        `json:"schema_version"`
	FunctionID      string        `json:"function_id"`
	DiscoveryEpoch  uint64        `json:"discovery_epoch"`
	ServingSequence uint64        `json:"serving_sequence"`
	GeneratedAt     time.Time     `json:"generated_at"`
	Function        Function      `json:"function"`
	Trigger         HTTPTrigger   `json:"http_trigger"`
	Route           Route         `json:"route"`
	Endpoints       []Endpoint    `json:"endpoints"`
	Checksum        digest.SHA256 `json:"checksum"`
}

// Input contains the complete trusted inputs for one Build operation.
type Input struct {
	DiscoveryEpoch  uint64
	ServingSequence uint64
	GeneratedAt     time.Time
	Function        Function
	Trigger         HTTPTrigger
	Route           Route
	Candidates      []EndpointCandidate
}

// Result contains the immutable snapshot and explainable candidate decisions.
type Result struct {
	Snapshot   Snapshot
	Candidates []CandidateDecision
}

// Config defines the bounded cardinality of one Builder.
type Config struct {
	MaxEndpoints int
}
