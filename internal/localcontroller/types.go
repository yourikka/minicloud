package localcontroller

import (
	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/scheduler"
)

// CreateFunctionInput contains the caller-controlled Function fields. The
// controller generates persistent IDs, timestamps, revisions, and index.
type CreateFunctionInput struct {
	Name       string                  `json:"name"`
	Labels     map[string]string       `json:"labels"`
	AuthPolicy controlplane.AuthPolicy `json:"auth_policy"`
}

// CreateFunctionOperationInput binds an authenticated idempotency key to one
// create-only Function command.
type CreateFunctionOperationInput struct {
	Operation controlplane.OperationKey
	Function  CreateFunctionInput
}

// SetFunctionLifecycleOperationInput binds an idempotency key to one Function
// enable/disable command with an exact resource-revision CAS.
type SetFunctionLifecycleOperationInput struct {
	Operation controlplane.OperationKey
	Lifecycle SetFunctionLifecycleInput
}

// RotateInvocationTokenOperationInput binds an idempotency key to one verifier
// replacement. Its plaintext credential is never replayable.
type RotateInvocationTokenOperationInput struct {
	Operation controlplane.OperationKey
	Rotation  RotateInvocationTokenInput
}

// CreateVersionOperationInput binds an idempotency key to one create-only
// Version command. Admission runs after the operation record commits.
type CreateVersionOperationInput struct {
	Operation controlplane.OperationKey
	Version   CreateVersionInput
}

// PublishRouteOperationInput binds an idempotency key to one Route publication
// with an exact active-route-revision CAS.
type PublishRouteOperationInput struct {
	Operation controlplane.OperationKey
	Route     PublishRouteInput
}

// FunctionOperationResult contains the retained safe Operation record and the
// current created resource view. InvocationToken exists only on first apply.
type FunctionOperationResult struct {
	Disposition controlplane.CompletionDisposition
	Record      controlplane.Record
	View        FunctionView
}

// VersionOperationResult contains the retained safe Operation record and the
// current persisted Version. Deployment exists only for a Ready Version.
type VersionOperationResult struct {
	Disposition controlplane.CompletionDisposition
	Record      controlplane.Record
	Version     model.Version
	Deployment  *model.Deployment
}

// RouteOperationResult contains the retained safe Operation record, the
// published Route snapshot, and the Function whose active pointer advanced.
type RouteOperationResult struct {
	Disposition controlplane.CompletionDisposition
	Record      controlplane.Record
	Route       model.Route
	Function    model.Function
}

// FunctionView is one Function with its mandatory default HTTP Trigger.
// InvocationToken is populated only by create and rotate responses.
type FunctionView struct {
	Function        model.Function           `json:"function"`
	Trigger         controlplane.HTTPTrigger `json:"http_trigger"`
	InvocationToken string                   `json:"invocation_token,omitempty"`
}

// RotateInvocationTokenInput applies an exact Trigger resource-revision CAS.
type RotateInvocationTokenInput struct {
	FunctionID               string
	ExpectedResourceRevision uint64
}

// SetFunctionLifecycleInput applies an exact Function resource-revision CAS.
type SetFunctionLifecycleInput struct {
	FunctionID               string
	ExpectedResourceRevision uint64
	Lifecycle                model.FunctionLifecycle
}

// CreateVersionInput contains immutable version manifest values. The Artifact
// must already have been published through PutArtifact under ArtifactDigest.
type CreateVersionInput struct {
	FunctionID            string                    `json:"function_id"`
	ArtifactDigest        digest.SHA256             `json:"artifact_digest"`
	ManifestDigest        digest.SHA256             `json:"manifest_digest"`
	Toolchain             model.ToolchainMetadata   `json:"toolchain"`
	ResourceRequest       model.ResourceRequest     `json:"resource_request"`
	RequestedCapabilities []model.CapabilityRequest `json:"requested_capabilities"`
}

// AdmissionResult is the current persisted result of one synchronous admission
// request. A temporary validator error returns a Validating Version with error.
type AdmissionResult struct {
	Version    model.Version
	Deployment *model.Deployment
}

// PublishRouteInput atomically points a Function at one Ready Version's
// immutable Generation 1 policy. The expected revision is the Route CAS.
type PublishRouteInput struct {
	FunctionID                  string
	VersionID                   string
	ExpectedActiveRouteRevision uint64
}

// CommitAssignmentInput persists one Planner result against an exact
// Deployment scaling revision before any Worker preparation is attempted.
type CommitAssignmentInput struct {
	FunctionID              string
	Placement               scheduler.Assignment
	ExpectedScalingRevision uint64
}

// CancelAssignmentInput withdraws one Assignment with a resource-revision CAS.
type CancelAssignmentInput struct {
	AssignmentID             string
	ExpectedResourceRevision uint64
}
