package model

import (
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/problem"
)

const MaxReplicasV1 = uint32(100)

// ScalingMode controls whether desired replicas are explicit or calculated.
type ScalingMode string

const (
	ScalingManual ScalingMode = "manual"
	ScalingAuto   ScalingMode = "auto"
)

// DeploymentPhase is the persisted desired phase of one immutable generation.
type DeploymentPhase string

const (
	DeploymentActive   DeploymentPhase = "Active"
	DeploymentDraining DeploymentPhase = "Draining"
	DeploymentStopped  DeploymentPhase = "Stopped"
	DeploymentDeleting DeploymentPhase = "Deleting"
)

// ResourceLimits is the approved execution envelope for a generation.
type ResourceLimits struct {
	Timeout        time.Duration `json:"timeout"`
	MemoryMiB      uint32        `json:"memory_mib"`
	MaxInputBytes  int64         `json:"max_input_bytes"`
	MaxOutputBytes int64         `json:"max_output_bytes"`
	MaxLogBytes    int64         `json:"max_log_bytes"`
}

// Deployment is an immutable execution policy generation plus mutable scaling
// state. ReadyReplicas and Conditions are derived and are not FSM inputs.
type Deployment struct {
	Metadata
	VersionID             string              `json:"version_id"`
	Generation            uint64              `json:"generation"`
	ScalingRevision       uint64              `json:"scaling_revision"`
	ResourceLimits        ResourceLimits      `json:"resource_limits"`
	GrantedCapabilities   []CapabilityRequest `json:"granted_capabilities"`
	EffectivePolicyDigest digest.SHA256       `json:"effective_policy_digest"`
	MinReplicas           uint32              `json:"min_replicas"`
	MaxReplicas           uint32              `json:"max_replicas"`
	DesiredReplicas       uint32              `json:"desired_replicas"`
	ReadyReplicas         uint32              `json:"ready_replicas"`
	TargetConcurrency     uint32              `json:"target_concurrency"`
	ScalingMode           ScalingMode         `json:"scaling_mode"`
	IdleTimeout           time.Duration       `json:"idle_timeout"`
	DesiredPhase          DeploymentPhase     `json:"desired_phase"`
}

// Validate checks generation identity, resource bounds, and scaling relations.
func (d Deployment) Validate() error {
	if err := d.Metadata.Validate(); err != nil {
		return err
	}
	if !idPattern.MatchString(d.VersionID) {
		return problem.Invalid("version_id", "must be a valid identifier")
	}
	if d.Generation == 0 {
		return problem.Invalid("generation", "must be greater than zero")
	}
	if d.ScalingRevision == 0 {
		return problem.Invalid("scaling_revision", "must be greater than zero")
	}
	if err := d.ResourceLimits.Validate(); err != nil {
		return err
	}
	if err := validateCapabilities(d.GrantedCapabilities); err != nil {
		return err
	}
	if _, err := digest.ParseSHA256(d.EffectivePolicyDigest.String()); err != nil {
		return problem.Invalid("effective_policy_digest", "must be a lowercase SHA-256 digest")
	}
	if d.MaxReplicas == 0 || d.MaxReplicas > MaxReplicasV1 {
		return problem.Invalid("max_replicas", "must be between 1 and 100")
	}
	if d.MinReplicas > d.MaxReplicas {
		return problem.Invalid("min_replicas", "must not exceed max_replicas")
	}
	if d.DesiredReplicas < d.MinReplicas || d.DesiredReplicas > d.MaxReplicas {
		return problem.Invalid("desired_replicas", "must be within the replica bounds")
	}
	if d.ReadyReplicas > d.MaxReplicas {
		return problem.Invalid("ready_replicas", "must not exceed max_replicas")
	}
	if d.TargetConcurrency == 0 {
		return problem.Invalid("target_concurrency", "must be greater than zero")
	}
	if !d.ScalingMode.valid() {
		return problem.Invalid("scaling_mode", "is not supported")
	}
	if d.IdleTimeout < 0 {
		return problem.Invalid("idle_timeout", "must not be negative")
	}
	if !d.DesiredPhase.valid() {
		return problem.Invalid("desired_phase", "is not supported")
	}
	return nil
}

// Validate checks the approved runtime resource envelope.
func (l ResourceLimits) Validate() error {
	request := ResourceRequest{
		Timeout:        l.Timeout,
		MemoryMiB:      l.MemoryMiB,
		MaxConcurrency: 1,
		MaxInputBytes:  l.MaxInputBytes,
		MaxOutputBytes: l.MaxOutputBytes,
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if l.MaxLogBytes <= 0 || l.MaxLogBytes > 256<<10 {
		return problem.Invalid("resource_limits.max_log_bytes", "must be within 256 KiB")
	}
	return nil
}

func (m ScalingMode) valid() bool {
	return m == ScalingManual || m == ScalingAuto
}

func (p DeploymentPhase) valid() bool {
	switch p {
	case DeploymentActive, DeploymentDraining, DeploymentStopped, DeploymentDeleting:
		return true
	default:
		return false
	}
}
