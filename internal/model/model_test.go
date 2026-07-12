package model_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
)

func TestMetadataValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*model.Metadata)
		field  string
	}{
		{name: "valid"},
		{name: "invalid id", mutate: func(m *model.Metadata) { m.ID = "bad id" }, field: "id"},
		{name: "unsupported namespace", mutate: func(m *model.Metadata) { m.Namespace = "tenant" }, field: "namespace"},
		{name: "non UTC creation", mutate: func(m *model.Metadata) {
			m.CreatedAt = time.Date(2026, 7, 12, 8, 0, 0, 0, time.FixedZone("CST", 8*60*60))
		}, field: "created_at"},
		{name: "updated before created", mutate: func(m *model.Metadata) {
			m.UpdatedAt = m.CreatedAt.Add(-time.Second)
		}, field: "updated_at"},
		{name: "zero raft index", mutate: func(m *model.Metadata) { m.CreatedRaftIndex = 0 }, field: "created_raft_index"},
		{name: "zero resource revision", mutate: func(m *model.Metadata) { m.ResourceRevision = 0 }, field: "resource_revision"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			metadata := validMetadata()
			if tt.mutate != nil {
				tt.mutate(&metadata)
			}
			err := metadata.Validate()
			assertValidationField(t, err, tt.field)
		})
	}
}

func TestFunctionValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*model.Function)
		field  string
	}{
		{name: "valid"},
		{name: "uppercase name", mutate: func(f *model.Function) { f.Name = "Bad" }, field: "name"},
		{name: "description byte limit", mutate: func(f *model.Function) {
			f.Description = strings.Repeat("界", 171)
		}, field: "description"},
		{name: "too many labels", mutate: func(f *model.Function) {
			for i := range 33 {
				f.Labels[string(rune('a'+i%26))+strings.Repeat("x", i/26)] = "value"
			}
		}, field: "labels"},
		{name: "invalid label key", mutate: func(f *model.Function) { f.Labels["Bad"] = "value" }, field: "labels"},
		{name: "invalid lifecycle", mutate: func(f *model.Function) { f.Lifecycle = "Unknown" }, field: "lifecycle"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			function := validFunction()
			if tt.mutate != nil {
				tt.mutate(&function)
			}
			err := function.Validate()
			assertValidationField(t, err, tt.field)
		})
	}
}

func TestVersionValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*model.Version)
		field  string
	}{
		{name: "valid"},
		{name: "unsupported ABI", mutate: func(v *model.Version) { v.ABI = "component-v1" }, field: "abi"},
		{name: "unsupported host profile", mutate: func(v *model.Version) { v.HostAPIProfile = "http-v1" }, field: "host_api_profile"},
		{name: "artifact too large", mutate: func(v *model.Version) { v.ArtifactSize = model.MaxArtifactBytes + 1 }, field: "artifact_size"},
		{name: "unsupported memory tier", mutate: func(v *model.Version) { v.ResourceRequest.MemoryMiB = 96 }, field: "resource_request.memory_mib"},
		{name: "timeout above hard limit", mutate: func(v *model.Version) { v.ResourceRequest.Timeout = 31 * time.Second }, field: "resource_request.timeout"},
		{name: "duplicate capability", mutate: func(v *model.Version) {
			capability := model.CapabilityRequest{Name: "env", Version: "v1"}
			v.RequestedCapabilities = []model.CapabilityRequest{capability, capability}
		}, field: "requested_capabilities"},
		{name: "failed without safe error", mutate: func(v *model.Version) { v.State = model.VersionFailed }, field: "validation_error"},
		{name: "safe error on ready version", mutate: func(v *model.Version) {
			v.ValidationError = &model.SafeError{Code: problem.CodeInvalidArgument, Message: "module rejected"}
		}, field: "validation_error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			version := validVersion()
			if tt.mutate != nil {
				tt.mutate(&version)
			}
			err := version.Validate()
			assertValidationField(t, err, tt.field)
		})
	}
}

func TestRouteValidateCore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*model.Route)
		field  string
	}{
		{name: "valid enabled route"},
		{name: "valid disabled route", mutate: func(r *model.Route) { r.Enabled = false; r.Targets = []model.RouteTarget{} }},
		{name: "enabled without target", mutate: func(r *model.Route) { r.Targets = []model.RouteTarget{} }, field: "targets"},
		{name: "disabled with target", mutate: func(r *model.Route) { r.Enabled = false }, field: "targets"},
		{name: "incorrect weight", mutate: func(r *model.Route) { r.Targets[0].WeightBasisPoints = 9_999 }, field: "targets"},
		{name: "invalid salt size", mutate: func(r *model.Route) { r.Salt = []byte("short") }, field: "salt"},
		{name: "header source needs field", mutate: func(r *model.Route) { r.Affinity = model.AffinityHeader }, field: "affinity_header"},
		{name: "multiple targets blocked in core", mutate: func(r *model.Route) {
			r.Targets[0].WeightBasisPoints = 5_000
			second := r.Targets[0]
			second.VersionID = "ver_2"
			second.WeightBasisPoints = 5_000
			r.Targets = append(r.Targets, second)
		}, field: "targets"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			route := validRoute()
			if tt.mutate != nil {
				tt.mutate(&route)
			}
			err := route.ValidateCore()
			assertValidationField(t, err, tt.field)
		})
	}
}

func TestDeploymentValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*model.Deployment)
		field  string
	}{
		{name: "valid"},
		{name: "max replicas hard limit", mutate: func(d *model.Deployment) { d.MaxReplicas = 101 }, field: "max_replicas"},
		{name: "minimum exceeds maximum", mutate: func(d *model.Deployment) { d.MinReplicas = 11 }, field: "min_replicas"},
		{name: "desired outside bounds", mutate: func(d *model.Deployment) { d.DesiredReplicas = 0 }, field: "desired_replicas"},
		{name: "ready outside bounds", mutate: func(d *model.Deployment) { d.ReadyReplicas = 11 }, field: "ready_replicas"},
		{name: "invalid scaling mode", mutate: func(d *model.Deployment) { d.ScalingMode = "dynamic" }, field: "scaling_mode"},
		{name: "invalid phase", mutate: func(d *model.Deployment) { d.DesiredPhase = "Ready" }, field: "desired_phase"},
		{name: "log limit above maximum", mutate: func(d *model.Deployment) { d.ResourceLimits.MaxLogBytes = 257 << 10 }, field: "resource_limits.max_log_bytes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			deployment := validDeployment()
			if tt.mutate != nil {
				tt.mutate(&deployment)
			}
			err := deployment.Validate()
			assertValidationField(t, err, tt.field)
		})
	}
}

func validMetadata() model.Metadata {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	return model.Metadata{
		ID:               "obj_01",
		Namespace:        model.DefaultNamespace,
		CreatedAt:        now,
		UpdatedAt:        now,
		CreatedRaftIndex: 1,
		ResourceRevision: 1,
	}
}

func validFunction() model.Function {
	return model.Function{
		Metadata:    validMetadata(),
		Name:        "echo",
		Description: "Echo a request",
		Labels:      map[string]string{"team": "platform"},
		Lifecycle:   model.FunctionActive,
	}
}

func validVersion() model.Version {
	return model.Version{
		Metadata:              validMetadata(),
		FunctionID:            "fn_01",
		VersionID:             "ver_01",
		ArtifactDigest:        digest.Sum([]byte("wasm")),
		ManifestDigest:        digest.Sum([]byte("manifest")),
		ArtifactSize:          4,
		ABI:                   model.ABIWASICommandV1,
		HostAPIProfile:        model.HostAPIProfileNone,
		RuntimeFeatureProfile: "wasi-p1-v1",
		Toolchain: model.ToolchainMetadata{
			Name:       "go",
			Version:    "go1.26.2",
			Provenance: "unverified",
		},
		AdmissionEpoch: 1,
		ResourceRequest: model.ResourceRequest{
			Timeout:        5 * time.Second,
			MemoryMiB:      128,
			MaxConcurrency: 1,
			MaxInputBytes:  1 << 20,
			MaxOutputBytes: 1 << 20,
		},
		RequestedCapabilities: []model.CapabilityRequest{},
		State:                 model.VersionReady,
	}
}

func validRoute() model.Route {
	return model.Route{
		Metadata:      validMetadata(),
		FunctionID:    "fn_01",
		RouteRevision: 1,
		Targets: []model.RouteTarget{
			{
				VersionID:             "ver_01",
				AdmissionEpoch:        1,
				DeploymentGeneration:  1,
				EffectivePolicyDigest: digest.Sum([]byte("policy")),
				WeightBasisPoints:     model.TotalRouteWeightBasisPoints,
			},
		},
		Affinity:    model.AffinityRequestID,
		HashVersion: model.HashVersionSHA256V1,
		SaltID:      "salt_01",
		Salt:        []byte("0123456789abcdef"),
		Enabled:     true,
	}
}

func validDeployment() model.Deployment {
	return model.Deployment{
		Metadata:        validMetadata(),
		VersionID:       "ver_01",
		Generation:      1,
		ScalingRevision: 1,
		ResourceLimits: model.ResourceLimits{
			Timeout:        5 * time.Second,
			MemoryMiB:      128,
			MaxInputBytes:  1 << 20,
			MaxOutputBytes: 1 << 20,
			MaxLogBytes:    256 << 10,
		},
		GrantedCapabilities:   []model.CapabilityRequest{},
		EffectivePolicyDigest: digest.Sum([]byte("policy")),
		MinReplicas:           1,
		MaxReplicas:           10,
		DesiredReplicas:       1,
		ReadyReplicas:         0,
		TargetConcurrency:     1,
		ScalingMode:           model.ScalingManual,
		IdleTimeout:           5 * time.Minute,
		DesiredPhase:          model.DeploymentActive,
	}
}

func assertValidationField(t *testing.T, err error, wantField string) {
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
