package model_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
)

func TestEffectivePolicyValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*model.EffectivePolicy)
		field  string
	}{
		{name: "valid"},
		{name: "invalid version id", mutate: func(p *model.EffectivePolicy) { p.VersionID = "bad id" }, field: "version_id"},
		{name: "zero admission epoch", mutate: func(p *model.EffectivePolicy) { p.AdmissionEpoch = 0 }, field: "admission_epoch"},
		{name: "zero deployment generation", mutate: func(p *model.EffectivePolicy) { p.DeploymentGeneration = 0 }, field: "deployment_generation"},
		{name: "invalid artifact digest", mutate: func(p *model.EffectivePolicy) { p.ArtifactDigest = "bad" }, field: "artifact_digest"},
		{name: "invalid artifact size", mutate: func(p *model.EffectivePolicy) { p.ArtifactSize = 0 }, field: "artifact_size"},
		{name: "unsupported abi", mutate: func(p *model.EffectivePolicy) { p.ABI = "component-v1" }, field: "abi"},
		{name: "unsupported host profile", mutate: func(p *model.EffectivePolicy) { p.HostAPIProfile = "http-v1" }, field: "host_api_profile"},
		{name: "missing runtime feature profile", mutate: func(p *model.EffectivePolicy) { p.RuntimeFeatureProfile = "" }, field: "runtime_feature_profile"},
		{name: "invalid resource limits", mutate: func(p *model.EffectivePolicy) { p.ResourceLimits.MaxLogBytes = 0 }, field: "resource_limits.max_log_bytes"},
		{name: "sub-millisecond timeout", mutate: func(p *model.EffectivePolicy) { p.ResourceLimits.Timeout += time.Nanosecond }, field: "resource_limits.timeout"},
		{name: "zero max concurrency", mutate: func(p *model.EffectivePolicy) { p.MaxConcurrency = 0 }, field: "max_concurrency"},
		{name: "duplicate capability", mutate: func(p *model.EffectivePolicy) {
			capability := model.CapabilityRequest{Name: "env", Version: "v1"}
			p.GrantedCapabilities = []model.CapabilityRequest{capability, capability}
		}, field: "granted_capabilities"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			policy := validEffectivePolicy()
			if tt.mutate != nil {
				tt.mutate(&policy)
			}
			assertEffectivePolicyValidationField(t, policy.Validate(), tt.field)
		})
	}
}

func TestEffectivePolicyDigestUsesCanonicalV1Schema(t *testing.T) {
	t.Parallel()

	policy := validEffectivePolicy()
	policy.ResourceLimits.Timeout = 1500 * time.Millisecond
	policy.MaxConcurrency = 7
	policy.GrantedCapabilities = []model.CapabilityRequest{
		{Name: "network", Version: "v2"},
		{Name: "env", Version: "v1"},
		{Name: "network", Version: "v1"},
	}

	got, err := policy.Digest()
	if err != nil {
		t.Fatalf("Digest() error = %v", err)
	}
	want, err := digest.CanonicalJSON(
		"effective-policy",
		"v1",
		[]byte(`{
			"version_id":"version-1",
			"admission_epoch":3,
			"deployment_generation":4,
			"artifact_digest":"`+policy.ArtifactDigest.String()+`",
			"artifact_size":4,
			"abi":"wasi-command-v1",
			"host_api_profile":"none",
			"runtime_feature_profile":"wasi-p1-v1",
			"resource_limits":{
				"timeout_ms":1500,
				"memory_mib":128,
				"max_input_bytes":1048576,
				"max_output_bytes":1048576,
				"max_log_bytes":262144
			},
			"max_concurrency":7,
			"granted_capabilities":[
				{"name":"env","version":"v1"},
				{"name":"network","version":"v1"},
				{"name":"network","version":"v2"}
			]
		}`),
	)
	if err != nil {
		t.Fatalf("CanonicalJSON() error = %v", err)
	}
	if got != want {
		t.Fatalf("Digest() = %q, want %q", got, want)
	}
	if model.EffectivePolicyDomain != "effective-policy" {
		t.Fatalf("EffectivePolicyDomain = %q", model.EffectivePolicyDomain)
	}
	if model.EffectivePolicySchemaVersion != "v1" {
		t.Fatalf("EffectivePolicySchemaVersion = %q", model.EffectivePolicySchemaVersion)
	}
}

func TestEffectivePolicyDigestNormalizesCapabilitySet(t *testing.T) {
	t.Parallel()

	first := validEffectivePolicy()
	first.GrantedCapabilities = []model.CapabilityRequest{
		{Name: "network", Version: "v2"},
		{Name: "env", Version: "v1"},
	}
	original := slices.Clone(first.GrantedCapabilities)
	second := first
	second.GrantedCapabilities = slices.Clone(first.GrantedCapabilities)
	slices.Reverse(second.GrantedCapabilities)

	firstDigest, err := first.Digest()
	if err != nil {
		t.Fatalf("Digest(first) error = %v", err)
	}
	secondDigest, err := second.Digest()
	if err != nil {
		t.Fatalf("Digest(second) error = %v", err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("capability order changed digest: %q != %q", firstDigest, secondDigest)
	}
	if !slices.Equal(first.GrantedCapabilities, original) {
		t.Fatalf("Digest() mutated capabilities: %+v", first.GrantedCapabilities)
	}
}

func TestEffectivePolicyDigestBindsExecutionIdentity(t *testing.T) {
	t.Parallel()
	base := validEffectivePolicy()
	baseDigest, err := base.Digest()
	if err != nil {
		t.Fatalf("Digest(base) error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*model.EffectivePolicy)
	}{
		{name: "version", mutate: func(p *model.EffectivePolicy) { p.VersionID = "version-2" }},
		{name: "admission epoch", mutate: func(p *model.EffectivePolicy) { p.AdmissionEpoch++ }},
		{name: "deployment generation", mutate: func(p *model.EffectivePolicy) { p.DeploymentGeneration++ }},
		{name: "artifact digest", mutate: func(p *model.EffectivePolicy) { p.ArtifactDigest = digest.Sum([]byte("other")) }},
		{name: "artifact size", mutate: func(p *model.EffectivePolicy) { p.ArtifactSize++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			changed := base
			test.mutate(&changed)
			changedDigest, err := changed.Digest()
			if err != nil {
				t.Fatalf("Digest(changed) error = %v", err)
			}
			if changedDigest == baseDigest {
				t.Fatalf("%s did not change policy digest %q", test.name, baseDigest)
			}
		})
	}
}

func TestEffectivePolicyDigestNormalizesEmptyCapabilitySet(t *testing.T) {
	t.Parallel()

	nilCapabilities := validEffectivePolicy()
	nilCapabilities.GrantedCapabilities = nil
	emptyCapabilities := nilCapabilities
	emptyCapabilities.GrantedCapabilities = []model.CapabilityRequest{}

	nilDigest, err := nilCapabilities.Digest()
	if err != nil {
		t.Fatalf("Digest(nil capabilities) error = %v", err)
	}
	emptyDigest, err := emptyCapabilities.Digest()
	if err != nil {
		t.Fatalf("Digest(empty capabilities) error = %v", err)
	}
	if nilDigest != emptyDigest {
		t.Fatalf("nil and empty capability sets differ: %q != %q", nilDigest, emptyDigest)
	}
}

func TestEffectivePolicyDigestValidatesPolicy(t *testing.T) {
	t.Parallel()

	policy := validEffectivePolicy()
	policy.ResourceLimits.Timeout += time.Nanosecond
	if _, err := policy.Digest(); err == nil {
		t.Fatal("Digest() accepted a sub-millisecond timeout")
	}
}

func validEffectivePolicy() model.EffectivePolicy {
	return model.EffectivePolicy{
		VersionID:             "version-1",
		AdmissionEpoch:        3,
		DeploymentGeneration:  4,
		ArtifactDigest:        digest.Sum([]byte("wasm")),
		ArtifactSize:          4,
		ABI:                   model.ABIWASICommandV1,
		HostAPIProfile:        model.HostAPIProfileNone,
		RuntimeFeatureProfile: "wasi-p1-v1",
		ResourceLimits: model.ResourceLimits{
			Timeout:        5 * time.Second,
			MemoryMiB:      128,
			MaxInputBytes:  1 << 20,
			MaxOutputBytes: 1 << 20,
			MaxLogBytes:    256 << 10,
		},
		MaxConcurrency:      1,
		GrantedCapabilities: []model.CapabilityRequest{},
	}
}

func assertEffectivePolicyValidationField(t *testing.T, err error, wantField string) {
	t.Helper()

	if wantField == "" {
		if err != nil {
			t.Fatalf("Validate() unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("Validate() returned nil, want field %q", wantField)
	}
	var validationError *problem.Error
	if !errors.As(err, &validationError) {
		t.Fatalf("Validate() error type = %T, want *problem.Error", err)
	}
	if validationError.Field != wantField {
		t.Fatalf("Validate() field = %q, want %q", validationError.Field, wantField)
	}
}
