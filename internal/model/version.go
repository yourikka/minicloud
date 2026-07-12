package model

import (
	"slices"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/problem"
)

const (
	ABIWASICommandV1   = "wasi-command-v1"
	HostAPIProfileNone = "none"
	MaxArtifactBytes   = 256 << 20
)

var supportedMemoryMiB = []uint32{64, 128, 256, 512}

// VersionState is the immutable version admission lifecycle.
type VersionState string

const (
	VersionUploaded   VersionState = "Uploaded"
	VersionValidating VersionState = "Validating"
	VersionReady      VersionState = "Ready"
	VersionFailed     VersionState = "Failed"
	VersionDeprecated VersionState = "Deprecated"
	VersionDeleting   VersionState = "Deleting"
	VersionTombstoned VersionState = "Tombstoned"
)

// ToolchainMetadata is uploader-supplied diagnostic data. It is never trusted
// for admission or compatibility decisions.
type ToolchainMetadata struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Provenance string `json:"provenance"`
}

// ResourceRequest is the immutable resource request in a Version manifest.
type ResourceRequest struct {
	Timeout        time.Duration `json:"timeout"`
	MemoryMiB      uint32        `json:"memory_mib"`
	MaxConcurrency uint32        `json:"max_concurrency"`
	MaxInputBytes  int64         `json:"max_input_bytes"`
	MaxOutputBytes int64         `json:"max_output_bytes"`
}

// CapabilityRequest names a versioned optional host capability request.
type CapabilityRequest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// SafeError is a stable code and non-sensitive diagnostic summary.
type SafeError struct {
	Code    problem.Code `json:"code"`
	Message string       `json:"message"`
}

// Version binds immutable execution policy inputs to one artifact.
type Version struct {
	Metadata
	FunctionID            string              `json:"function_id"`
	VersionID             string              `json:"version_id"`
	ArtifactDigest        digest.SHA256       `json:"artifact_digest"`
	ManifestDigest        digest.SHA256       `json:"manifest_digest"`
	ArtifactSize          int64               `json:"artifact_size"`
	ABI                   string              `json:"abi"`
	HostAPIProfile        string              `json:"host_api_profile"`
	RuntimeFeatureProfile string              `json:"runtime_feature_profile"`
	Toolchain             ToolchainMetadata   `json:"toolchain_metadata"`
	AdmissionEpoch        uint64              `json:"admission_epoch"`
	ResourceRequest       ResourceRequest     `json:"resource_request"`
	RequestedCapabilities []CapabilityRequest `json:"requested_capabilities"`
	State                 VersionState        `json:"state"`
	ValidationError       *SafeError          `json:"validation_error,omitempty"`
}

// Validate checks immutable Version metadata and state-dependent fields.
func (v Version) Validate() error {
	if err := v.Metadata.Validate(); err != nil {
		return err
	}
	if !idPattern.MatchString(v.FunctionID) {
		return problem.Invalid("function_id", "must be a valid identifier")
	}
	if !idPattern.MatchString(v.VersionID) {
		return problem.Invalid("version_id", "must be a valid identifier")
	}
	if _, err := digest.ParseSHA256(v.ArtifactDigest.String()); err != nil {
		return problem.Invalid("artifact_digest", "must be a lowercase SHA-256 digest")
	}
	if _, err := digest.ParseSHA256(v.ManifestDigest.String()); err != nil {
		return problem.Invalid("manifest_digest", "must be a lowercase SHA-256 digest")
	}
	if v.ArtifactSize <= 0 || v.ArtifactSize > MaxArtifactBytes {
		return problem.Invalid("artifact_size", "must be within the v1 artifact size limit")
	}
	if v.ABI != ABIWASICommandV1 {
		return problem.Invalid("abi", "only wasi-command-v1 is supported in v1")
	}
	if v.HostAPIProfile != HostAPIProfileNone {
		return problem.Invalid("host_api_profile", "only none is supported in v1")
	}
	if v.RuntimeFeatureProfile == "" {
		return problem.Invalid("runtime_feature_profile", "is required")
	}
	if v.Toolchain.Name == "" || v.Toolchain.Version == "" {
		return problem.Invalid("toolchain_metadata", "name and version are required")
	}
	if v.Toolchain.Provenance == "" {
		return problem.Invalid("toolchain_metadata", "provenance is required")
	}
	if v.AdmissionEpoch == 0 {
		return problem.Invalid("admission_epoch", "must be greater than zero")
	}
	if err := v.ResourceRequest.Validate(); err != nil {
		return err
	}
	if err := validateCapabilities(v.RequestedCapabilities); err != nil {
		return err
	}
	if !v.State.valid() {
		return problem.Invalid("state", "is not a supported version state")
	}
	if v.State == VersionFailed && v.ValidationError == nil {
		return problem.Invalid("validation_error", "is required for a failed version")
	}
	if v.ValidationError != nil && (v.ValidationError.Code == "" || v.ValidationError.Message == "") {
		return problem.Invalid("validation_error", "code and message are required")
	}
	if v.ValidationError != nil && !problem.Known(v.ValidationError.Code) {
		return problem.Invalid("validation_error", "code is not a stable v1 error")
	}
	if v.State != VersionFailed && v.ValidationError != nil {
		return problem.Invalid("validation_error", "is only allowed for a failed version")
	}
	return nil
}

// Validate checks the resource tiers and absolute v1 limits.
func (r ResourceRequest) Validate() error {
	if r.Timeout <= 0 || r.Timeout > 30*time.Second {
		return problem.Invalid("resource_request.timeout", "must be between zero and 30 seconds")
	}
	if !slices.Contains(supportedMemoryMiB, r.MemoryMiB) {
		return problem.Invalid("resource_request.memory_mib", "must be one of 64, 128, 256, or 512")
	}
	if r.MaxConcurrency == 0 {
		return problem.Invalid("resource_request.max_concurrency", "must be greater than zero")
	}
	if r.MaxInputBytes <= 0 || r.MaxInputBytes > 1<<20 {
		return problem.Invalid("resource_request.max_input_bytes", "must be within 1 MiB")
	}
	if r.MaxOutputBytes <= 0 || r.MaxOutputBytes > 1<<20 {
		return problem.Invalid("resource_request.max_output_bytes", "must be within 1 MiB")
	}
	return nil
}

func validateCapabilities(capabilities []CapabilityRequest) error {
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if !labelKeyPattern.MatchString(capability.Name) || capability.Version == "" {
			return problem.Invalid("requested_capabilities", "contains an invalid capability")
		}
		key := capability.Name + "\x00" + capability.Version
		if _, exists := seen[key]; exists {
			return problem.Invalid("requested_capabilities", "contains a duplicate capability")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func (s VersionState) valid() bool {
	switch s {
	case VersionUploaded, VersionValidating, VersionReady, VersionFailed,
		VersionDeprecated, VersionDeleting, VersionTombstoned:
		return true
	default:
		return false
	}
}
