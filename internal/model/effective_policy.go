package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/problem"
)

const (
	// EffectivePolicyDomain separates policy digests from other protocol digests.
	EffectivePolicyDomain = "effective-policy"
	// EffectivePolicySchemaVersion identifies the canonical policy schema.
	EffectivePolicySchemaVersion = "v1"
)

// EffectivePolicy is the immutable v1 execution policy installed for a
// Deployment generation.
type EffectivePolicy struct {
	VersionID             string              `json:"version_id"`
	AdmissionEpoch        uint64              `json:"admission_epoch"`
	DeploymentGeneration  uint64              `json:"deployment_generation"`
	ArtifactDigest        digest.SHA256       `json:"artifact_digest"`
	ArtifactSize          int64               `json:"artifact_size"`
	ABI                   string              `json:"abi"`
	HostAPIProfile        string              `json:"host_api_profile"`
	RuntimeFeatureProfile string              `json:"runtime_feature_profile"`
	ResourceLimits        ResourceLimits      `json:"resource_limits"`
	MaxConcurrency        uint32              `json:"max_concurrency"`
	GrantedCapabilities   []CapabilityRequest `json:"granted_capabilities"`
}

// Validate checks the locked runtime profile and approved execution envelope.
func (p EffectivePolicy) Validate() error {
	if !idPattern.MatchString(p.VersionID) {
		return problem.Invalid("version_id", "must be a valid identifier")
	}
	if p.AdmissionEpoch == 0 {
		return problem.Invalid("admission_epoch", "must be greater than zero")
	}
	if p.DeploymentGeneration == 0 {
		return problem.Invalid("deployment_generation", "must be greater than zero")
	}
	if _, err := digest.ParseSHA256(p.ArtifactDigest.String()); err != nil {
		return problem.Invalid("artifact_digest", "must be a lowercase sha-256 digest")
	}
	if p.ArtifactSize < 1 || p.ArtifactSize > MaxArtifactBytes {
		return problem.Invalid("artifact_size", "must be within the v1 artifact size limit")
	}
	if p.ABI != ABIWASICommandV1 {
		return problem.Invalid("abi", "only wasi-command-v1 is supported in v1")
	}
	if p.HostAPIProfile != HostAPIProfileNone {
		return problem.Invalid("host_api_profile", "only none is supported in v1")
	}
	if p.RuntimeFeatureProfile == "" {
		return problem.Invalid("runtime_feature_profile", "is required")
	}
	if err := p.ResourceLimits.Validate(); err != nil {
		return err
	}
	if p.ResourceLimits.Timeout%time.Millisecond != 0 {
		return problem.Invalid("resource_limits.timeout", "must use whole milliseconds")
	}
	if p.MaxConcurrency == 0 {
		return problem.Invalid("max_concurrency", "must be greater than zero")
	}
	if err := validateCapabilities(p.GrantedCapabilities); err != nil {
		var validationError *problem.Error
		if errors.As(err, &validationError) {
			return problem.Invalid("granted_capabilities", validationError.Message)
		}
		return err
	}
	return nil
}

// Digest returns the domain-separated digest of the normalized v1 policy.
func (p EffectivePolicy) Digest() (digest.SHA256, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}

	capabilities := make([]CapabilityRequest, len(p.GrantedCapabilities))
	copy(capabilities, p.GrantedCapabilities)
	slices.SortStableFunc(capabilities, compareCapabilities)

	canonical := canonicalEffectivePolicyV1{
		VersionID:             p.VersionID,
		AdmissionEpoch:        p.AdmissionEpoch,
		DeploymentGeneration:  p.DeploymentGeneration,
		ArtifactDigest:        p.ArtifactDigest,
		ArtifactSize:          p.ArtifactSize,
		ABI:                   p.ABI,
		HostAPIProfile:        p.HostAPIProfile,
		RuntimeFeatureProfile: p.RuntimeFeatureProfile,
		ResourceLimits: canonicalResourceLimitsV1{
			TimeoutMS:      int64(p.ResourceLimits.Timeout / time.Millisecond),
			MemoryMiB:      p.ResourceLimits.MemoryMiB,
			MaxInputBytes:  p.ResourceLimits.MaxInputBytes,
			MaxOutputBytes: p.ResourceLimits.MaxOutputBytes,
			MaxLogBytes:    p.ResourceLimits.MaxLogBytes,
		},
		MaxConcurrency:      p.MaxConcurrency,
		GrantedCapabilities: capabilities,
	}
	source, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshaling effective policy: %w", err)
	}
	return digest.CanonicalJSON(
		EffectivePolicyDomain,
		EffectivePolicySchemaVersion,
		source,
	)
}

type canonicalEffectivePolicyV1 struct {
	VersionID             string                    `json:"version_id"`
	AdmissionEpoch        uint64                    `json:"admission_epoch"`
	DeploymentGeneration  uint64                    `json:"deployment_generation"`
	ArtifactDigest        digest.SHA256             `json:"artifact_digest"`
	ArtifactSize          int64                     `json:"artifact_size"`
	ABI                   string                    `json:"abi"`
	HostAPIProfile        string                    `json:"host_api_profile"`
	RuntimeFeatureProfile string                    `json:"runtime_feature_profile"`
	ResourceLimits        canonicalResourceLimitsV1 `json:"resource_limits"`
	MaxConcurrency        uint32                    `json:"max_concurrency"`
	GrantedCapabilities   []CapabilityRequest       `json:"granted_capabilities"`
}

type canonicalResourceLimitsV1 struct {
	TimeoutMS      int64  `json:"timeout_ms"`
	MemoryMiB      uint32 `json:"memory_mib"`
	MaxInputBytes  int64  `json:"max_input_bytes"`
	MaxOutputBytes int64  `json:"max_output_bytes"`
	MaxLogBytes    int64  `json:"max_log_bytes"`
}

func compareCapabilities(left, right CapabilityRequest) int {
	if left.Name < right.Name {
		return -1
	}
	if left.Name > right.Name {
		return 1
	}
	if left.Version < right.Version {
		return -1
	}
	if left.Version > right.Version {
		return 1
	}
	return 0
}
