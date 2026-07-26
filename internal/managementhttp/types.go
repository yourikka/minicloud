package managementhttp

import (
	"time"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/localcontroller"
	"github.com/yourikka/minicloud/internal/model"
)

// The management response types below are deliberate projections. They never
// serialize internal state types directly, so the Trigger token verifier
// digest and the Route hashing salt cannot reach a management client.

type metadataBody struct {
	ID               string    `json:"id"`
	Namespace        string    `json:"namespace"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	CreatedRaftIndex uint64    `json:"created_raft_index"`
	ResourceRevision uint64    `json:"resource_revision"`
}

type functionBody struct {
	metadataBody
	Name                string                  `json:"name"`
	Description         string                  `json:"description,omitempty"`
	ActiveRouteRevision uint64                  `json:"active_route_revision"`
	Labels              map[string]string       `json:"labels"`
	Lifecycle           model.FunctionLifecycle `json:"lifecycle"`
}

type triggerBody struct {
	metadataBody
	FunctionID string                  `json:"function_id"`
	Enabled    bool                    `json:"enabled"`
	AuthPolicy controlplane.AuthPolicy `json:"auth_policy"`
}

type versionBody struct {
	metadataBody
	FunctionID            string                    `json:"function_id"`
	VersionID             string                    `json:"version_id"`
	ArtifactDigest        digest.SHA256             `json:"artifact_digest"`
	ManifestDigest        digest.SHA256             `json:"manifest_digest"`
	ArtifactSize          int64                     `json:"artifact_size"`
	ABI                   string                    `json:"abi"`
	HostAPIProfile        string                    `json:"host_api_profile"`
	RuntimeFeatureProfile string                    `json:"runtime_feature_profile"`
	Toolchain             model.ToolchainMetadata   `json:"toolchain_metadata"`
	AdmissionEpoch        uint64                    `json:"admission_epoch"`
	ResourceRequest       resourceRequestBody       `json:"resource_request"`
	RequestedCapabilities []model.CapabilityRequest `json:"requested_capabilities"`
	State                 model.VersionState        `json:"state"`
	ValidationError       *model.SafeError          `json:"validation_error,omitempty"`
}

type deploymentBody struct {
	metadataBody
	VersionID             string                    `json:"version_id"`
	Generation            uint64                    `json:"generation"`
	ScalingRevision       uint64                    `json:"scaling_revision"`
	EffectivePolicyDigest digest.SHA256             `json:"effective_policy_digest"`
	GrantedCapabilities   []model.CapabilityRequest `json:"granted_capabilities"`
	MinReplicas           uint32                    `json:"min_replicas"`
	MaxReplicas           uint32                    `json:"max_replicas"`
	DesiredReplicas       uint32                    `json:"desired_replicas"`
	ReadyReplicas         uint32                    `json:"ready_replicas"`
	TargetConcurrency     uint32                    `json:"target_concurrency"`
	ScalingMode           model.ScalingMode         `json:"scaling_mode"`
	DesiredPhase          model.DeploymentPhase     `json:"desired_phase"`
}

type routeBody struct {
	metadataBody
	FunctionID    string               `json:"function_id"`
	RouteRevision uint64               `json:"route_revision"`
	Targets       []model.RouteTarget  `json:"targets"`
	Affinity      model.AffinitySource `json:"affinity_source"`
	HashVersion   string               `json:"hash_version"`
	SaltID        string               `json:"salt_id"`
	Enabled       bool                 `json:"enabled"`
}

type operationBody struct {
	ID               string                             `json:"id"`
	Disposition      controlplane.CompletionDisposition `json:"disposition"`
	RaftAppliedIndex uint64                             `json:"raft_applied_index"`
	CompletedAt      time.Time                          `json:"completed_at"`
}

type operationRecordBody struct {
	OperationID       string                          `json:"operation_id"`
	Principal         string                          `json:"principal"`
	Namespace         string                          `json:"namespace"`
	RequestDigest     digest.SHA256                   `json:"request_digest"`
	Status            controlplane.OutcomeStatus      `json:"status"`
	Failure           *controlplane.Failure           `json:"failure,omitempty"`
	AffectedResources []controlplane.AffectedResource `json:"affected_resources"`
	CredentialIssued  bool                            `json:"credential_issued"`
	CompletedAt       time.Time                       `json:"completed_at"`
	RaftAppliedIndex  uint64                          `json:"raft_applied_index"`
}

type functionResponse struct {
	Function        functionBody   `json:"function"`
	HTTPTrigger     *triggerBody   `json:"http_trigger,omitempty"`
	InvocationToken string         `json:"invocation_token,omitempty"`
	Operation       *operationBody `json:"operation,omitempty"`
}

type functionListResponse struct {
	Functions []functionResponse `json:"functions"`
}

type versionResponse struct {
	Version    versionBody     `json:"version"`
	Deployment *deploymentBody `json:"deployment,omitempty"`
	Operation  *operationBody  `json:"operation,omitempty"`
}

type routeResponse struct {
	Route     routeBody      `json:"route"`
	Function  *functionBody  `json:"function,omitempty"`
	Operation *operationBody `json:"operation,omitempty"`
}

type artifactResponse struct {
	Artifact artifactBody `json:"artifact"`
}

type artifactBody struct {
	Digest digest.SHA256 `json:"digest"`
	Size   int64         `json:"size"`
}

type profileResponse struct {
	Profile    string `json:"profile"`
	Replicated bool   `json:"replicated"`
	Durable    bool   `json:"durable"`
	Message    string `json:"message"`
}

type createFunctionRequest struct {
	Name       string            `json:"name"`
	Labels     map[string]string `json:"labels"`
	AuthPolicy string            `json:"auth_policy"`
}

type setLifecycleRequest struct {
	Lifecycle string `json:"lifecycle"`
}

type createVersionRequest struct {
	ArtifactDigest        string                    `json:"artifact_digest"`
	ManifestDigest        string                    `json:"manifest_digest"`
	Toolchain             model.ToolchainMetadata   `json:"toolchain_metadata"`
	ResourceRequest       resourceRequestBody       `json:"resource_request"`
	RequestedCapabilities []model.CapabilityRequest `json:"requested_capabilities"`
}

// resourceRequestBody uses whole milliseconds because the controller rejects
// sub-millisecond timeouts and JSON duration encoding is nanoseconds.
type resourceRequestBody struct {
	TimeoutMS      int64  `json:"timeout_ms"`
	MemoryMiB      uint32 `json:"memory_mib"`
	MaxConcurrency uint32 `json:"max_concurrency"`
	MaxInputBytes  int64  `json:"max_input_bytes"`
	MaxOutputBytes int64  `json:"max_output_bytes"`
}

type putRouteRequest struct {
	VersionID string `json:"version_id"`
}

func metadataView(metadata model.Metadata) metadataBody {
	return metadataBody{
		ID:               metadata.ID,
		Namespace:        metadata.Namespace,
		CreatedAt:        metadata.CreatedAt,
		UpdatedAt:        metadata.UpdatedAt,
		CreatedRaftIndex: metadata.CreatedRaftIndex,
		ResourceRevision: metadata.ResourceRevision,
	}
}

func functionView(function model.Function) functionBody {
	return functionBody{
		metadataBody:        metadataView(function.Metadata),
		Name:                function.Name,
		Description:         function.Description,
		ActiveRouteRevision: function.ActiveRouteRevision,
		Labels:              function.Labels,
		Lifecycle:           function.Lifecycle,
	}
}

func triggerView(trigger controlplane.HTTPTrigger) *triggerBody {
	if trigger.ID == "" {
		return nil
	}
	return &triggerBody{
		metadataBody: metadataView(trigger.Metadata),
		FunctionID:   trigger.FunctionID,
		Enabled:      trigger.Enabled,
		AuthPolicy:   trigger.AuthPolicy,
	}
}

func versionView(version model.Version) versionBody {
	return versionBody{
		metadataBody:          metadataView(version.Metadata),
		FunctionID:            version.FunctionID,
		VersionID:             version.VersionID,
		ArtifactDigest:        version.ArtifactDigest,
		ManifestDigest:        version.ManifestDigest,
		ArtifactSize:          version.ArtifactSize,
		ABI:                   version.ABI,
		HostAPIProfile:        version.HostAPIProfile,
		RuntimeFeatureProfile: version.RuntimeFeatureProfile,
		Toolchain:             version.Toolchain,
		AdmissionEpoch:        version.AdmissionEpoch,
		ResourceRequest: resourceRequestBody{
			TimeoutMS:      version.ResourceRequest.Timeout.Milliseconds(),
			MemoryMiB:      version.ResourceRequest.MemoryMiB,
			MaxConcurrency: version.ResourceRequest.MaxConcurrency,
			MaxInputBytes:  version.ResourceRequest.MaxInputBytes,
			MaxOutputBytes: version.ResourceRequest.MaxOutputBytes,
		},
		RequestedCapabilities: version.RequestedCapabilities,
		State:                 version.State,
		ValidationError:       version.ValidationError,
	}
}

func deploymentView(deployment *model.Deployment) *deploymentBody {
	if deployment == nil {
		return nil
	}
	return &deploymentBody{
		metadataBody:          metadataView(deployment.Metadata),
		VersionID:             deployment.VersionID,
		Generation:            deployment.Generation,
		ScalingRevision:       deployment.ScalingRevision,
		EffectivePolicyDigest: deployment.EffectivePolicyDigest,
		GrantedCapabilities:   deployment.GrantedCapabilities,
		MinReplicas:           deployment.MinReplicas,
		MaxReplicas:           deployment.MaxReplicas,
		DesiredReplicas:       deployment.DesiredReplicas,
		ReadyReplicas:         deployment.ReadyReplicas,
		TargetConcurrency:     deployment.TargetConcurrency,
		ScalingMode:           deployment.ScalingMode,
		DesiredPhase:          deployment.DesiredPhase,
	}
}

func routeView(route model.Route) routeBody {
	return routeBody{
		metadataBody:  metadataView(route.Metadata),
		FunctionID:    route.FunctionID,
		RouteRevision: route.RouteRevision,
		Targets:       route.Targets,
		Affinity:      route.Affinity,
		HashVersion:   route.HashVersion,
		SaltID:        route.SaltID,
		Enabled:       route.Enabled,
	}
}

func operationView(
	key controlplane.OperationKey,
	disposition controlplane.CompletionDisposition,
	record controlplane.Record,
) *operationBody {
	return &operationBody{
		ID:               key.OperationID,
		Disposition:      disposition,
		RaftAppliedIndex: record.AppliedIndex,
		CompletedAt:      record.CompletedAt,
	}
}

func operationRecordView(record controlplane.Record) operationRecordBody {
	return operationRecordBody{
		OperationID:       record.Key.OperationID,
		Principal:         record.Key.Principal,
		Namespace:         record.Key.Namespace,
		RequestDigest:     record.Digest,
		Status:            record.Outcome.Status,
		Failure:           record.Outcome.Failure,
		AffectedResources: record.Outcome.AffectedResources,
		CredentialIssued:  record.Outcome.CredentialIssued,
		CompletedAt:       record.CompletedAt,
		RaftAppliedIndex:  record.AppliedIndex,
	}
}

func functionOperationResponse(
	key controlplane.OperationKey,
	result localcontroller.FunctionOperationResult,
) functionResponse {
	return functionResponse{
		Function:        functionView(result.View.Function),
		HTTPTrigger:     triggerView(result.View.Trigger),
		InvocationToken: result.View.InvocationToken,
		Operation:       operationView(key, result.Disposition, result.Record),
	}
}

func artifactView(info artifact.Info) artifactResponse {
	return artifactResponse{Artifact: artifactBody{Digest: info.Digest, Size: info.Size}}
}
