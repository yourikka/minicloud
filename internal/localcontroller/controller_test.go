package localcontroller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/validator"
	validatorprotocol "github.com/yourikka/minicloud/internal/validator/protocol"
)

func TestControllerCreatesFunctionUploadsArtifactAndAdmitsVersion(t *testing.T) {
	t.Parallel()
	controller, validator := newControllerHarness(t, []validatorStep{{report: acceptedReport}})
	artifactBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	artifactDigest := digest.Sum(artifactBytes)
	info, err := controller.PutArtifact(context.Background(), artifactDigest, bytes.NewReader(artifactBytes))
	if err != nil {
		t.Fatalf("PutArtifact() error = %v", err)
	}
	if info.Digest != artifactDigest || info.Size != int64(len(artifactBytes)) {
		t.Fatalf("PutArtifact() info = %+v, want verified source metadata", info)
	}

	function, err := controller.CreateFunction(context.Background(), CreateFunctionInput{
		Name:       "echo",
		AuthPolicy: controlplane.AuthPolicyPublic,
	})
	if err != nil {
		t.Fatalf("CreateFunction() error = %v", err)
	}
	if function.Function.Lifecycle != model.FunctionActive || !function.Trigger.Enabled {
		t.Fatalf("CreateFunction() = %+v, want active function and enabled trigger", function)
	}
	if function.Trigger.FunctionID != function.Function.ID || function.Trigger.AuthPolicy != controlplane.AuthPolicyPublic {
		t.Fatalf("CreateFunction() trigger = %+v, want public trigger for created function", function.Trigger)
	}

	result, err := controller.CreateVersion(context.Background(), testVersionInput(function.Function.ID, artifactDigest))
	if err != nil {
		t.Fatalf("CreateVersion() error = %v", err)
	}
	if result.Version.State != model.VersionReady || result.Deployment == nil {
		t.Fatalf("CreateVersion() = %+v, want ready version with deployment", result)
	}
	if result.Deployment.Generation != 1 || result.Deployment.DesiredReplicas != 1 || result.Deployment.ReadyReplicas != 0 {
		t.Fatalf("CreateVersion() deployment = %+v, want Local Core generation one", result.Deployment)
	}
	if !result.Version.CreatedAt.Before(result.Version.UpdatedAt) || result.Version.CreatedRaftIndex >= result.Deployment.CreatedRaftIndex {
		t.Fatalf("CreateVersion() command metadata did not advance: version=%+v deployment=%+v", result.Version.Metadata, result.Deployment.Metadata)
	}

	requests := validator.Requests()
	if len(requests) != 1 {
		t.Fatalf("validator request count = %d, want 1", len(requests))
	}
	request := requests[0]
	if request.ArtifactDigest != artifactDigest || request.ArtifactSize != int64(len(artifactBytes)) ||
		request.RuntimeEngine != validatorprotocol.EngineCompiler || request.RequestedCapabilities == nil {
		t.Fatalf("validator request = %+v, want verified artifact and fixed runtime inputs", request)
	}

	versions, deployment, err := controller.GetVersion(context.Background(), result.Version.VersionID)
	if err != nil {
		t.Fatalf("GetVersion() error = %v", err)
	}
	if versions.VersionID != result.Version.VersionID || versions.State != result.Version.State ||
		deployment == nil || deployment.EffectivePolicyDigest != result.Deployment.EffectivePolicyDigest {
		t.Fatalf("GetVersion() = (%+v, %+v), want committed admission result", versions, deployment)
	}
	listed, err := controller.ListFunctions(context.Background())
	if err != nil {
		t.Fatalf("ListFunctions() error = %v", err)
	}
	if len(listed) != 1 || listed[0].Function.ID != function.Function.ID {
		t.Fatalf("ListFunctions() = %+v, want created function", listed)
	}
}

func TestControllerPublishesReadyVersionAsSingleTargetRoute(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, []validatorStep{{report: acceptedReport}})
	artifactBytes := []byte{0x10, 0x11, 0x12}
	artifactDigest := digest.Sum(artifactBytes)
	if _, err := controller.PutArtifact(context.Background(), artifactDigest, bytes.NewReader(artifactBytes)); err != nil {
		t.Fatalf("PutArtifact() error = %v", err)
	}
	function := createTestFunction(t, controller, "publish")
	admission, err := controller.CreateVersion(context.Background(), testVersionInput(function.ID, artifactDigest))
	if err != nil {
		t.Fatalf("CreateVersion() error = %v", err)
	}

	route, updatedFunction, err := controller.PublishRoute(context.Background(), PublishRouteInput{
		FunctionID:                  function.ID,
		VersionID:                   admission.Version.VersionID,
		ExpectedActiveRouteRevision: 0,
	})
	if err != nil {
		t.Fatalf("PublishRoute() error = %v", err)
	}
	if route.RouteRevision != 1 || !route.Enabled || len(route.Targets) != 1 ||
		route.Targets[0].WeightBasisPoints != model.TotalRouteWeightBasisPoints {
		t.Fatalf("PublishRoute() route = %+v, want enabled single target", route)
	}
	if route.Targets[0].VersionID != admission.Version.VersionID ||
		route.Targets[0].AdmissionEpoch != admission.Version.AdmissionEpoch ||
		route.Targets[0].DeploymentGeneration != admission.Deployment.Generation ||
		route.Targets[0].EffectivePolicyDigest != admission.Deployment.EffectivePolicyDigest {
		t.Fatalf("PublishRoute() target = %+v, want immutable ready identity", route.Targets[0])
	}
	if len(route.Salt) != routeSaltBytes || updatedFunction.ActiveRouteRevision != 1 ||
		updatedFunction.ResourceRevision != function.ResourceRevision+1 {
		t.Fatalf("PublishRoute() = (%+v, %+v), want route pointer advance", route, updatedFunction)
	}
	stored, err := controller.GetRoute(context.Background(), function.ID)
	if err != nil {
		t.Fatalf("GetRoute() error = %v", err)
	}
	route.Targets[0].WeightBasisPoints = 1
	route.Salt[0] ^= 0xff
	if stored.Targets[0].WeightBasisPoints != model.TotalRouteWeightBasisPoints || stored.Salt[0] == route.Salt[0] {
		t.Fatalf("GetRoute() exposed route storage: %+v", stored)
	}
	states, err := controller.ListServingStates(context.Background())
	if err != nil {
		t.Fatalf("ListServingStates() error = %v", err)
	}
	if len(states) != 1 || states[0].Route == nil || states[0].Version == nil || states[0].Deployment == nil ||
		states[0].Route.Targets[0].VersionID != admission.Version.VersionID {
		t.Fatalf("ListServingStates() = %+v, want ready serving state", states)
	}
}

func TestControllerRejectsNonReadyAndCrossFunctionRouteTarget(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, []validatorStep{
		{err: validator.ErrProcessFailed},
		{report: acceptedReport},
	})
	artifactBytes := []byte{0x13, 0x14, 0x15}
	artifactDigest := digest.Sum(artifactBytes)
	if _, err := controller.PutArtifact(context.Background(), artifactDigest, bytes.NewReader(artifactBytes)); err != nil {
		t.Fatalf("PutArtifact() error = %v", err)
	}
	first := createTestFunction(t, controller, "first-route")
	second := createTestFunction(t, controller, "second-route")
	pending, err := controller.CreateVersion(context.Background(), testVersionInput(first.ID, artifactDigest))
	if !errors.Is(err, validator.ErrProcessFailed) || pending.Version.State != model.VersionValidating {
		t.Fatalf("CreateVersion() = (%+v, %v), want retryable version", pending, err)
	}
	_, _, err = controller.PublishRoute(context.Background(), PublishRouteInput{
		FunctionID:                  first.ID,
		VersionID:                   pending.Version.VersionID,
		ExpectedActiveRouteRevision: 0,
	})
	assertControllerProblemCode(t, err, problem.CodeConflict)

	if err := controller.ResumePendingValidation(context.Background(), 1); err != nil {
		t.Fatalf("ResumePendingValidation() error = %v", err)
	}
	_, _, err = controller.PublishRoute(context.Background(), PublishRouteInput{
		FunctionID:                  second.ID,
		VersionID:                   pending.Version.VersionID,
		ExpectedActiveRouteRevision: 0,
	})
	assertControllerProblemCode(t, err, problem.CodeConflict)
}

func TestControllerConcurrentRoutePublishUsesExactCAS(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, []validatorStep{{report: acceptedReport}})
	artifactBytes := []byte{0x16, 0x17, 0x18}
	artifactDigest := digest.Sum(artifactBytes)
	if _, err := controller.PutArtifact(context.Background(), artifactDigest, bytes.NewReader(artifactBytes)); err != nil {
		t.Fatalf("PutArtifact() error = %v", err)
	}
	function := createTestFunction(t, controller, "concurrent-route")
	admission, err := controller.CreateVersion(context.Background(), testVersionInput(function.ID, artifactDigest))
	if err != nil {
		t.Fatalf("CreateVersion() error = %v", err)
	}

	type outcome struct {
		route model.Route
		err   error
	}
	results := make(chan outcome, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			route, _, err := controller.PublishRoute(context.Background(), PublishRouteInput{
				FunctionID:                  function.ID,
				VersionID:                   admission.Version.VersionID,
				ExpectedActiveRouteRevision: 0,
			})
			results <- outcome{route: route, err: err}
		}()
	}
	group.Wait()
	close(results)

	successes := 0
	for result := range results {
		if result.err == nil {
			successes++
			if result.route.RouteRevision != 1 {
				t.Fatalf("successful route = %+v, want revision one", result.route)
			}
			continue
		}
		assertControllerProblemCode(t, result.err, problem.CodeRevisionConflict)
	}
	if successes != 1 {
		t.Fatalf("successful route publishes = %d, want 1", successes)
	}
	stored, err := controller.GetRoute(context.Background(), function.ID)
	if err != nil || stored.RouteRevision != 1 {
		t.Fatalf("GetRoute() = (%+v, %v), want one committed route", stored, err)
	}
}

func TestControllerResumesInfrastructureFailedValidationWithSameFence(t *testing.T) {
	t.Parallel()
	controller, driver := newControllerHarness(t, []validatorStep{
		{err: validator.ErrProcessFailed},
		{report: acceptedReport},
	})
	artifactBytes := []byte{0x01, 0x02, 0x03}
	artifactDigest := digest.Sum(artifactBytes)
	if _, err := controller.PutArtifact(context.Background(), artifactDigest, bytes.NewReader(artifactBytes)); err != nil {
		t.Fatalf("PutArtifact() error = %v", err)
	}
	function := createTestFunction(t, controller, "resume")

	result, err := controller.CreateVersion(context.Background(), testVersionInput(function.ID, artifactDigest))
	if !errors.Is(err, validator.ErrProcessFailed) {
		t.Fatalf("CreateVersion() error = %v, want validator process failure", err)
	}
	if result.Version.State != model.VersionValidating || result.Deployment != nil {
		t.Fatalf("CreateVersion() = %+v, want retryable validating version", result)
	}
	initialValidationID := driver.Requests()[0].ValidationID

	if err := controller.ResumePendingValidation(context.Background(), 1); err != nil {
		t.Fatalf("ResumePendingValidation() error = %v", err)
	}
	version, deployment, err := controller.GetVersion(context.Background(), result.Version.VersionID)
	if err != nil {
		t.Fatalf("GetVersion() error = %v", err)
	}
	if version.State != model.VersionReady || deployment == nil {
		t.Fatalf("GetVersion() = (%+v, %+v), want resumed ready admission", version, deployment)
	}
	requests := driver.Requests()
	if len(requests) != 2 || requests[1].ValidationID != initialValidationID {
		t.Fatalf("validator requests = %+v, want one persisted validation fence", requests)
	}
}

func TestControllerMarksValidatorLimitAsFailedVersion(t *testing.T) {
	t.Parallel()
	controller, driver := newControllerHarness(t, []validatorStep{{err: validator.ErrTimedOut}})
	artifactBytes := []byte{0x04, 0x05, 0x06}
	artifactDigest := digest.Sum(artifactBytes)
	if _, err := controller.PutArtifact(context.Background(), artifactDigest, bytes.NewReader(artifactBytes)); err != nil {
		t.Fatalf("PutArtifact() error = %v", err)
	}
	function := createTestFunction(t, controller, "timeout")

	result, err := controller.CreateVersion(context.Background(), testVersionInput(function.ID, artifactDigest))
	if err != nil {
		t.Fatalf("CreateVersion() error = %v", err)
	}
	if result.Version.State != model.VersionFailed || result.Deployment != nil || result.Version.ValidationError == nil {
		t.Fatalf("CreateVersion() = %+v, want failed version without deployment", result)
	}
	if result.Version.ValidationError.Code != problem.CodeInvalidModule || result.Version.ValidationError.Message != "module validation exceeded the admission limit" {
		t.Fatalf("validation error = %+v, want safe admission-limit classification", result.Version.ValidationError)
	}
	if err := controller.ResumePendingValidation(context.Background(), 1); err != nil {
		t.Fatalf("ResumePendingValidation() error = %v, want no pending validation", err)
	}
	if len(driver.Requests()) != 1 {
		t.Fatalf("validator request count = %d, want no retry after failed limit", len(driver.Requests()))
	}
}

func TestControllerRejectsSubMillisecondTimeoutBeforeVersionCreation(t *testing.T) {
	t.Parallel()
	controller, driver := newControllerHarness(t, []validatorStep{{report: acceptedReport}})
	artifactBytes := []byte{0x07, 0x08, 0x09}
	artifactDigest := digest.Sum(artifactBytes)
	if _, err := controller.PutArtifact(context.Background(), artifactDigest, bytes.NewReader(artifactBytes)); err != nil {
		t.Fatalf("PutArtifact() error = %v", err)
	}
	function := createTestFunction(t, controller, "duration")
	input := testVersionInput(function.ID, artifactDigest)
	input.ResourceRequest.Timeout = time.Millisecond + time.Nanosecond

	_, err := controller.CreateVersion(context.Background(), input)
	var validationError *problem.Error
	if !errors.As(err, &validationError) || validationError.Field != "resource_request.timeout" {
		t.Fatalf("CreateVersion() error = %v, want whole-millisecond validation", err)
	}
	if len(driver.Requests()) != 0 {
		t.Fatalf("validator request count = %d, want no admission attempt", len(driver.Requests()))
	}
}

func TestControllerDoesNotCreateUploadedVersionWhenStartCommandCannotBeAllocated(t *testing.T) {
	t.Parallel()
	store, err := artifact.Open(artifact.Config{Root: t.TempDir(), MaxArtifactBytes: 1 << 20})
	if err != nil {
		t.Fatalf("artifact.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	commands := &scriptedCommandSource{commands: []commandStep{
		{command: CommandMeta{AppliedIndex: 1, At: time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)}},
		{command: CommandMeta{AppliedIndex: 2, At: time.Date(2026, time.July, 25, 12, 0, 1, 0, time.UTC)}},
		{err: errors.New("command source unavailable")},
	}}
	controller, err := New(Config{
		Artifacts: store,
		Validator: &scriptedValidator{
			steps:    []validatorStep{{report: acceptedReport}},
			requests: []validatorprotocol.Request{},
		},
		Commands: commands,
		IDs:      &sequenceIDSource{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	function := createTestFunction(t, controller, "command-source")
	artifactBytes := []byte{0x0a, 0x0b, 0x0c}
	artifactDigest := digest.Sum(artifactBytes)
	if _, err := controller.PutArtifact(context.Background(), artifactDigest, bytes.NewReader(artifactBytes)); err != nil {
		t.Fatalf("PutArtifact() error = %v", err)
	}

	_, err = controller.CreateVersion(context.Background(), testVersionInput(function.ID, artifactDigest))
	if err == nil || !strings.Contains(err.Error(), "command source unavailable") {
		t.Fatalf("CreateVersion() error = %v, want command source failure", err)
	}
	if snapshot := controller.releases.Snapshot(); len(snapshot.Versions) != 0 || len(snapshot.PendingValidations) != 0 {
		t.Fatalf("release snapshot = %+v, want no stranded uploaded version", snapshot)
	}
}

func TestControllerRejectsInvalidValidationIDBeforeVersionCreation(t *testing.T) {
	t.Parallel()
	store, err := artifact.Open(artifact.Config{Root: t.TempDir(), MaxArtifactBytes: 1 << 20})
	if err != nil {
		t.Fatalf("artifact.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	ids := &scriptedIDSource{ids: []string{
		"function-1",
		"trigger-2",
		"version-3",
		"validation/bad",
	}}
	controller, err := New(Config{
		Artifacts: store,
		Validator: &scriptedValidator{
			steps:    []validatorStep{{report: acceptedReport}},
			requests: []validatorprotocol.Request{},
		},
		IDs: ids,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	function := createTestFunction(t, controller, "invalid-validation-id")
	artifactBytes := []byte{0x0d, 0x0e, 0x0f}
	artifactDigest := digest.Sum(artifactBytes)
	if _, err := controller.PutArtifact(context.Background(), artifactDigest, bytes.NewReader(artifactBytes)); err != nil {
		t.Fatalf("PutArtifact() error = %v", err)
	}

	_, err = controller.CreateVersion(context.Background(), testVersionInput(function.ID, artifactDigest))
	if err == nil || !strings.Contains(err.Error(), "source returned an invalid id") {
		t.Fatalf("CreateVersion() error = %v, want invalid validation id", err)
	}
	if snapshot := controller.releases.Snapshot(); len(snapshot.Versions) != 0 || len(snapshot.PendingValidations) != 0 {
		t.Fatalf("release snapshot = %+v, want no stranded uploaded version", snapshot)
	}
}

func TestControllerRejectsRegressingCommandSource(t *testing.T) {
	t.Parallel()
	store, err := artifact.Open(artifact.Config{Root: t.TempDir(), MaxArtifactBytes: 1 << 20})
	if err != nil {
		t.Fatalf("artifact.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	at := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	commands := &scriptedCommandSource{commands: []commandStep{
		{command: CommandMeta{AppliedIndex: 1, At: at}},
		{command: CommandMeta{AppliedIndex: 1, At: at.Add(time.Second)}},
	}}
	controller, err := New(Config{
		Artifacts: store,
		Validator: &scriptedValidator{
			steps:    []validatorStep{{report: acceptedReport}},
			requests: []validatorprotocol.Request{},
		},
		Commands: commands,
		IDs:      &sequenceIDSource{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := controller.nextCommand(); err != nil {
		t.Fatalf("nextCommand() first error = %v", err)
	}
	if _, err := controller.nextCommand(); err == nil || !strings.Contains(err.Error(), "did not advance applied index") {
		t.Fatalf("nextCommand() second error = %v, want rejected command source", err)
	}
	if snapshot := controller.catalog.Snapshot(); len(snapshot.Functions) != 0 {
		t.Fatalf("catalog snapshot = %+v, want no partial function", snapshot)
	}
}

func TestMonotonicCommandSourcePreventsWallClockRegression(t *testing.T) {
	t.Parallel()
	times := []time.Time{
		time.Date(2026, time.July, 25, 12, 0, 2, 0, time.UTC),
		time.Date(2026, time.July, 25, 12, 0, 1, 0, time.FixedZone("CST", 8*60*60)),
		time.Date(2026, time.July, 25, 12, 0, 2, 0, time.UTC),
	}
	position := 0
	source := NewMonotonicCommandSource(func() time.Time {
		value := times[position]
		position++
		return value
	})

	var previous CommandMeta
	for wantIndex := uint64(1); wantIndex <= uint64(len(times)); wantIndex++ {
		command, err := source.Next()
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		if command.AppliedIndex != wantIndex || command.At.Location() != time.UTC {
			t.Fatalf("Next() = %+v, want index %d and UTC time", command, wantIndex)
		}
		if !previous.At.IsZero() && !previous.At.Before(command.At) {
			t.Fatalf("Next() regressed command time: previous=%s current=%s", previous.At, command.At)
		}
		previous = command
	}
}

func createTestFunction(t *testing.T, controller *Controller, name string) model.Function {
	t.Helper()
	result, err := controller.CreateFunction(context.Background(), CreateFunctionInput{
		Name:       name,
		AuthPolicy: controlplane.AuthPolicyPublic,
	})
	if err != nil {
		t.Fatalf("CreateFunction() error = %v", err)
	}
	return result.Function
}

func assertControllerProblemCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	var classified *problem.Error
	if !errors.As(err, &classified) || classified.Code != want {
		t.Fatalf("error = %v, want problem code %q", err, want)
	}
}

func testVersionInput(functionID string, artifactDigest digest.SHA256) CreateVersionInput {
	return CreateVersionInput{
		FunctionID:     functionID,
		ArtifactDigest: artifactDigest,
		ManifestDigest: digest.Sum([]byte("canonical-manifest")),
		Toolchain: model.ToolchainMetadata{
			Name:       "go",
			Version:    "1.26",
			Provenance: "unverified",
		},
		ResourceRequest: model.ResourceRequest{
			Timeout:        time.Second,
			MemoryMiB:      64,
			MaxConcurrency: 1,
			MaxInputBytes:  1024,
			MaxOutputBytes: 1024,
		},
		RequestedCapabilities: []model.CapabilityRequest{},
	}
}

type controllerHarness struct {
	controller *Controller
	validator  *scriptedValidator
}

func newControllerHarness(t *testing.T, steps []validatorStep) (*Controller, *scriptedValidator) {
	t.Helper()
	store, err := artifact.Open(artifact.Config{Root: t.TempDir(), MaxArtifactBytes: 1 << 20})
	if err != nil {
		t.Fatalf("artifact.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	validator := &scriptedValidator{steps: slices.Clone(steps), requests: []validatorprotocol.Request{}}
	commandTime := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	controller, err := New(Config{
		Artifacts: store,
		Validator: validator,
		Commands: NewMonotonicCommandSource(func() time.Time {
			return commandTime
		}),
		IDs:   &sequenceIDSource{},
		Salts: fixedSaltSource{salt: []byte("0123456789abcdef")},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return controller, validator
}

type validatorStep struct {
	report func(validatorprotocol.Request) validatorprotocol.Report
	err    error
}

type scriptedValidator struct {
	mu sync.Mutex

	steps    []validatorStep
	requests []validatorprotocol.Request
}

func (s *scriptedValidator) Validate(
	ctx context.Context,
	request validatorprotocol.Request,
	artifact io.ReadCloser,
) (validatorprotocol.Report, error) {
	if ctx == nil {
		return validatorprotocol.Report{}, errors.New("test validator context is required")
	}
	if err := ctx.Err(); err != nil {
		return validatorprotocol.Report{}, err
	}
	_, readErr := io.ReadAll(artifact)
	closeErr := artifact.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return validatorprotocol.Report{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	if len(s.steps) == 0 {
		return validatorprotocol.Report{}, errors.New("test validator has no scripted response")
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	if step.err != nil {
		return validatorprotocol.Report{}, step.err
	}
	return step.report(request), nil
}

func (s *scriptedValidator) Requests() []validatorprotocol.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

func acceptedReport(request validatorprotocol.Request) validatorprotocol.Report {
	return validatorprotocol.Report{
		SchemaVersion:         validatorprotocol.SchemaVersion,
		ValidationID:          request.ValidationID,
		Valid:                 true,
		Code:                  validatorprotocol.CodeOK,
		Reason:                "accepted",
		Message:               "module is compatible with the locked v1 runtime profile",
		ArtifactDigest:        request.ArtifactDigest,
		ArtifactSize:          request.ArtifactSize,
		RuntimeName:           validatorprotocol.RuntimeName,
		RuntimeVersion:        validatorprotocol.RuntimeVersion,
		RuntimeFeatureProfile: validatorprotocol.FeatureProfile,
		RuntimeEngine:         request.RuntimeEngine,
		Imports:               []validatorprotocol.Import{},
		Exports:               []string{"_start"},
	}
}

type sequenceIDSource struct {
	mu   sync.Mutex
	next int
}

func (s *sequenceIDSource) NewID(prefix string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return fmt.Sprintf("%s-%d", prefix, s.next), nil
}

type commandStep struct {
	command CommandMeta
	err     error
}

type scriptedCommandSource struct {
	mu sync.Mutex

	commands []commandStep
}

func (s *scriptedCommandSource) Next() (CommandMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.commands) == 0 {
		return CommandMeta{}, errors.New("test command source is exhausted")
	}
	step := s.commands[0]
	s.commands = s.commands[1:]
	return step.command, step.err
}

type scriptedIDSource struct {
	mu sync.Mutex

	ids []string
}

type fixedSaltSource struct {
	salt []byte
}

func (s fixedSaltSource) NewSalt() ([]byte, error) {
	return slices.Clone(s.salt), nil
}

func (s *scriptedIDSource) NewID(prefix string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ids) == 0 {
		return "", errors.New("test id source is exhausted")
	}
	id := s.ids[0]
	s.ids = s.ids[1:]
	return id, nil
}
