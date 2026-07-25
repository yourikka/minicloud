package controlplane

import (
	"errors"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	validatorprotocol "github.com/yourikka/minicloud/internal/validator/protocol"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

func TestReleaseStoreTransitionsValidatedVersionToReadyWithGenerationOne(t *testing.T) {
	t.Parallel()
	store := newReleaseStore(t)
	version := validUploadedVersion(1, "version-01")
	if _, err := store.CreateVersion(CreateVersionCommand{IfNoneMatch: true, AppliedIndex: 1, Version: version}); err != nil {
		t.Fatalf("CreateVersion() error = %v", err)
	}
	validating, err := store.StartValidation(StartValidationCommand{
		VersionID: version.VersionID, ExpectedResourceRevision: 1, ValidationID: "validation-01",
		UpdatedAt: version.CreatedAt.Add(time.Minute), AppliedIndex: 2,
	})
	if err != nil || validating.State != model.VersionValidating || validating.ResourceRevision != 2 {
		t.Fatalf("StartValidation() = %+v, %v", validating, err)
	}
	completedAt := validating.UpdatedAt.Add(time.Minute)
	deployment := validInitialDeployment(t, version, 3, completedAt)
	ready, installed, err := store.CompleteValidation(CompleteValidationCommand{
		VersionID: version.VersionID, ExpectedResourceRevision: 2,
		Report: validValidatorReport(version, "validation-01"), Deployment: &deployment,
		UpdatedAt: completedAt, AppliedIndex: 3,
	})
	if err != nil {
		t.Fatalf("CompleteValidation() error = %v", err)
	}
	if ready.State != model.VersionReady || ready.ResourceRevision != 3 || installed == nil ||
		installed.Generation != 1 || installed.EffectivePolicyDigest != deployment.EffectivePolicyDigest {
		t.Fatalf("CompleteValidation() = %+v %+v", ready, installed)
	}
	snapshot := store.Snapshot()
	if len(snapshot.Versions) != 1 || len(snapshot.Deployments) != 1 || len(snapshot.PendingValidations) != 0 {
		t.Fatalf("Snapshot() = %+v", snapshot)
	}
}

func TestReleaseStoreRecordsFailedValidationWithoutDeployment(t *testing.T) {
	t.Parallel()
	store := newReleaseStore(t)
	version := validUploadedVersion(1, "version-01")
	if _, err := store.CreateVersion(CreateVersionCommand{IfNoneMatch: true, AppliedIndex: 1, Version: version}); err != nil {
		t.Fatalf("CreateVersion() error = %v", err)
	}
	if _, err := store.StartValidation(StartValidationCommand{
		VersionID: version.VersionID, ExpectedResourceRevision: 1, ValidationID: "validation-01",
		UpdatedAt: version.CreatedAt.Add(time.Minute), AppliedIndex: 2,
	}); err != nil {
		t.Fatalf("StartValidation() error = %v", err)
	}
	report := validValidatorReport(version, "validation-01")
	report.Valid = false
	report.Code = string(problem.CodeInvalidModule)
	report.Reason = "missing_start"
	report.Message = "module does not export _start"
	failed, deployment, err := store.CompleteValidation(CompleteValidationCommand{
		VersionID: version.VersionID, ExpectedResourceRevision: 2, Report: report,
		UpdatedAt: version.CreatedAt.Add(2 * time.Minute), AppliedIndex: 3,
	})
	if err != nil || failed.State != model.VersionFailed || failed.ValidationError == nil || deployment != nil {
		t.Fatalf("CompleteValidation(failed) = %+v %+v, %v", failed, deployment, err)
	}
}

func TestReleaseStoreFencesLateValidationAndPreservesState(t *testing.T) {
	t.Parallel()
	store := newReleaseStore(t)
	version := validUploadedVersion(1, "version-01")
	if _, err := store.CreateVersion(CreateVersionCommand{IfNoneMatch: true, AppliedIndex: 1, Version: version}); err != nil {
		t.Fatalf("CreateVersion() error = %v", err)
	}
	if _, err := store.StartValidation(StartValidationCommand{
		VersionID: version.VersionID, ExpectedResourceRevision: 1, ValidationID: "validation-01",
		UpdatedAt: version.CreatedAt.Add(time.Minute), AppliedIndex: 2,
	}); err != nil {
		t.Fatalf("StartValidation() error = %v", err)
	}
	deployment := validInitialDeployment(t, version, 3, version.CreatedAt.Add(2*time.Minute))
	_, _, err := store.CompleteValidation(CompleteValidationCommand{
		VersionID: version.VersionID, ExpectedResourceRevision: 2,
		Report: validValidatorReport(version, "late-validation"), Deployment: &deployment,
		UpdatedAt: version.CreatedAt.Add(2 * time.Minute), AppliedIndex: 3,
	})
	assertReleaseProblemCode(t, err, problem.CodeStaleGeneration, "")
	got, installed, err := store.Get(version.VersionID)
	if err != nil || got.State != model.VersionValidating || installed != nil {
		t.Fatalf("Get() after late result = %+v %+v, %v", got, installed, err)
	}
	if pending := store.Snapshot().PendingValidations; len(pending) != 1 || pending[0].ValidationID != "validation-01" {
		t.Fatalf("Pending validations = %+v", pending)
	}
}

func TestReleaseStoreRejectsInvalidInitialDeploymentAtomically(t *testing.T) {
	t.Parallel()
	store := newReleaseStore(t)
	version := validUploadedVersion(1, "version-01")
	if _, err := store.CreateVersion(CreateVersionCommand{IfNoneMatch: true, AppliedIndex: 1, Version: version}); err != nil {
		t.Fatalf("CreateVersion() error = %v", err)
	}
	if _, err := store.StartValidation(StartValidationCommand{
		VersionID: version.VersionID, ExpectedResourceRevision: 1, ValidationID: "validation-01",
		UpdatedAt: version.CreatedAt.Add(time.Minute), AppliedIndex: 2,
	}); err != nil {
		t.Fatalf("StartValidation() error = %v", err)
	}
	deployment := validInitialDeployment(t, version, 3, version.CreatedAt.Add(2*time.Minute))
	deployment.EffectivePolicyDigest = digest.Sum([]byte("wrong-policy"))
	_, _, err := store.CompleteValidation(CompleteValidationCommand{
		VersionID: version.VersionID, ExpectedResourceRevision: 2,
		Report: validValidatorReport(version, "validation-01"), Deployment: &deployment,
		UpdatedAt: version.CreatedAt.Add(2 * time.Minute), AppliedIndex: 3,
	})
	assertReleaseProblemCode(t, err, problem.CodeInvalidArgument, "deployment.effective_policy_digest")
	got, installed, err := store.Get(version.VersionID)
	if err != nil || got.State != model.VersionValidating || got.ResourceRevision != 2 || installed != nil {
		t.Fatalf("invalid deployment changed state: %+v %+v, %v", got, installed, err)
	}
	if pending := store.Snapshot().PendingValidations; len(pending) != 1 || pending[0].ValidationID != "validation-01" {
		t.Fatalf("invalid deployment removed validation fence: %+v", pending)
	}
}

func TestReleaseStoreRejectsValidationCompletionWithRegressedTime(t *testing.T) {
	t.Parallel()
	for _, reportValid := range []bool{true, false} {
		t.Run(map[bool]string{true: "ready", false: "failed"}[reportValid], func(t *testing.T) {
			t.Parallel()
			store := newReleaseStore(t)
			version := validUploadedVersion(1, "version-01")
			if _, err := store.CreateVersion(CreateVersionCommand{IfNoneMatch: true, AppliedIndex: 1, Version: version}); err != nil {
				t.Fatalf("CreateVersion() error = %v", err)
			}
			startedAt := version.CreatedAt.Add(2 * time.Minute)
			if _, err := store.StartValidation(StartValidationCommand{
				VersionID: version.VersionID, ExpectedResourceRevision: 1, ValidationID: "validation-01", UpdatedAt: startedAt, AppliedIndex: 2,
			}); err != nil {
				t.Fatalf("StartValidation() error = %v", err)
			}
			report := validValidatorReport(version, "validation-01")
			report.Valid = reportValid
			if !reportValid {
				report.Code = string(problem.CodeInvalidModule)
				report.Reason = "invalid_module"
			}
			command := CompleteValidationCommand{
				VersionID: version.VersionID, ExpectedResourceRevision: 2, Report: report,
				UpdatedAt: version.CreatedAt.Add(time.Minute), AppliedIndex: 3,
			}
			if reportValid {
				deployment := validInitialDeployment(t, version, 3, command.UpdatedAt)
				command.Deployment = &deployment
			}
			_, _, err := store.CompleteValidation(command)
			assertReleaseProblemCode(t, err, problem.CodeInvalidArgument, "updated_at")
			got, _, err := store.Get(version.VersionID)
			if err != nil || !got.UpdatedAt.Equal(startedAt) || got.State != model.VersionValidating {
				t.Fatalf("regressed completion changed Version: %+v, %v", got, err)
			}
		})
	}
}

func TestReleaseStoreRejectsInterpreterValidatorReport(t *testing.T) {
	t.Parallel()
	store := newReleaseStore(t)
	version := validUploadedVersion(1, "version-01")
	if _, err := store.CreateVersion(CreateVersionCommand{IfNoneMatch: true, AppliedIndex: 1, Version: version}); err != nil {
		t.Fatalf("CreateVersion() error = %v", err)
	}
	if _, err := store.StartValidation(StartValidationCommand{
		VersionID: version.VersionID, ExpectedResourceRevision: 1, ValidationID: "validation-01", UpdatedAt: version.CreatedAt.Add(time.Minute), AppliedIndex: 2,
	}); err != nil {
		t.Fatalf("StartValidation() error = %v", err)
	}
	deployment := validInitialDeployment(t, version, 3, version.CreatedAt.Add(2*time.Minute))
	report := validValidatorReport(version, "validation-01")
	report.RuntimeEngine = validatorprotocol.EngineInterpreter
	_, _, err := store.CompleteValidation(CompleteValidationCommand{
		VersionID: version.VersionID, ExpectedResourceRevision: 2, Report: report, Deployment: &deployment,
		UpdatedAt: version.CreatedAt.Add(2 * time.Minute), AppliedIndex: 3,
	})
	assertReleaseProblemCode(t, err, problem.CodeInvalidArgument, "report.runtime_engine")
}

func TestReleaseStoreRejectsVersionLimit(t *testing.T) {
	store := newReleaseStore(t)
	for index := uint64(1); index <= DefaultMaxVersionsPerFunction; index++ {
		version := validUploadedVersion(index, "version-"+formatIndex(index))
		if _, err := store.CreateVersion(CreateVersionCommand{IfNoneMatch: true, AppliedIndex: index, Version: version}); err != nil {
			t.Fatalf("CreateVersion(%d) error = %v", index, err)
		}
	}
	overflow := validUploadedVersion(DefaultMaxVersionsPerFunction+1, "version-overflow")
	_, err := store.CreateVersion(CreateVersionCommand{IfNoneMatch: true, AppliedIndex: DefaultMaxVersionsPerFunction + 1, Version: overflow})
	assertReleaseProblemCode(t, err, problem.CodeOverloaded, "")
}

func validUploadedVersion(index uint64, versionID string) model.Version {
	createdAt := time.Date(2026, 7, 25, 1, 0, int(index), 0, time.UTC)
	return model.Version{
		Metadata: model.Metadata{
			ID: versionID, Namespace: model.DefaultNamespace, CreatedAt: createdAt, UpdatedAt: createdAt,
			CreatedRaftIndex: index, ResourceRevision: 1,
		},
		FunctionID: "function-01", VersionID: versionID,
		ArtifactDigest: digest.Sum([]byte("artifact-" + versionID)), ManifestDigest: digest.Sum([]byte("manifest-" + versionID)),
		ArtifactSize: 1024, ABI: model.ABIWASICommandV1, HostAPIProfile: model.HostAPIProfileNone,
		RuntimeFeatureProfile: wasmprofile.FeatureProfile,
		Toolchain:             model.ToolchainMetadata{Name: "go", Version: "1.26", Provenance: "unverified"},
		AdmissionEpoch:        1,
		ResourceRequest:       model.ResourceRequest{Timeout: time.Second, MemoryMiB: 64, MaxConcurrency: 1, MaxInputBytes: 1024, MaxOutputBytes: 1024},
		RequestedCapabilities: []model.CapabilityRequest{}, State: model.VersionUploaded,
	}
}

func newReleaseStore(t *testing.T) *ReleaseStore {
	t.Helper()
	catalog := NewCatalog()
	if _, err := catalog.CreateFunction(validCreateFunction(1, "function-01", "echo")); err != nil {
		t.Fatalf("CreateFunction() error = %v", err)
	}
	return NewReleaseStore(catalog)
}

func validInitialDeployment(t *testing.T, version model.Version, index uint64, createdAt time.Time) model.Deployment {
	t.Helper()
	limits := model.ResourceLimits{Timeout: version.ResourceRequest.Timeout, MemoryMiB: version.ResourceRequest.MemoryMiB, MaxInputBytes: version.ResourceRequest.MaxInputBytes, MaxOutputBytes: version.ResourceRequest.MaxOutputBytes, MaxLogBytes: defaultMaxLogBytes}
	policy := model.EffectivePolicy{
		VersionID: version.VersionID, AdmissionEpoch: version.AdmissionEpoch, DeploymentGeneration: 1,
		ArtifactDigest: version.ArtifactDigest, ArtifactSize: version.ArtifactSize, ABI: version.ABI, HostAPIProfile: version.HostAPIProfile,
		RuntimeFeatureProfile: version.RuntimeFeatureProfile, ResourceLimits: limits, MaxConcurrency: version.ResourceRequest.MaxConcurrency,
		GrantedCapabilities: []model.CapabilityRequest{},
	}
	policyDigest, err := policy.Digest()
	if err != nil {
		t.Fatalf("EffectivePolicy.Digest() error = %v", err)
	}
	return model.Deployment{
		Metadata:  model.Metadata{ID: "deployment-" + version.VersionID, Namespace: model.DefaultNamespace, CreatedAt: createdAt, UpdatedAt: createdAt, CreatedRaftIndex: index, ResourceRevision: 1},
		VersionID: version.VersionID, Generation: 1, ScalingRevision: 1, ResourceLimits: limits,
		GrantedCapabilities: []model.CapabilityRequest{}, EffectivePolicyDigest: policyDigest,
		MinReplicas: 1, MaxReplicas: 1, DesiredReplicas: 1, ReadyReplicas: 0, TargetConcurrency: version.ResourceRequest.MaxConcurrency,
		ScalingMode: model.ScalingManual, IdleTimeout: 0, DesiredPhase: model.DeploymentActive,
	}
}

func validValidatorReport(version model.Version, validationID string) validatorprotocol.Report {
	return validatorprotocol.Report{
		SchemaVersion: validatorprotocol.SchemaVersion, ValidationID: validationID, Valid: true, Code: validatorprotocol.CodeOK,
		Reason: "accepted", Message: "module accepted", ArtifactDigest: version.ArtifactDigest, ArtifactSize: version.ArtifactSize,
		RuntimeName: validatorprotocol.RuntimeName, RuntimeVersion: validatorprotocol.RuntimeVersion,
		RuntimeFeatureProfile: version.RuntimeFeatureProfile, RuntimeEngine: validatorprotocol.EngineCompiler,
		Imports: []validatorprotocol.Import{}, Exports: []string{"_start"}, Memory: validatorprotocol.Memory{}, Timing: validatorprotocol.Timing{}, Isolation: validatorprotocol.Isolation{ProcessBoundary: true},
	}
}

func assertReleaseProblemCode(t *testing.T, err error, wantCode problem.Code, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want code %q", wantCode)
	}
	var classified *problem.Error
	if !errors.As(err, &classified) {
		t.Fatalf("error type = %T, want *problem.Error: %v", err, err)
	}
	if classified.Code != wantCode || classified.Field != wantField {
		t.Fatalf("error = %+v, want code=%q field=%q", classified, wantCode, wantField)
	}
}
