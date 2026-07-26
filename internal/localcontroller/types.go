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
	Name                string
	Labels              map[string]string
	AuthPolicy          controlplane.AuthPolicy
	TokenVerifierDigest *digest.SHA256
}

// FunctionView is one Function with its mandatory default HTTP Trigger.
type FunctionView struct {
	Function model.Function
	Trigger  controlplane.HTTPTrigger
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
	FunctionID            string
	ArtifactDigest        digest.SHA256
	ManifestDigest        digest.SHA256
	Toolchain             model.ToolchainMetadata
	ResourceRequest       model.ResourceRequest
	RequestedCapabilities []model.CapabilityRequest
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
