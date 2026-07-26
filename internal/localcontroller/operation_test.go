package localcontroller

import (
	"bytes"
	"context"
	"testing"

	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/validator"
)

func TestCreateFunctionOperationAppliesOnceAndReplaysSameRequest(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, nil)
	input := testCreateFunctionOperationInput("op-1", "echo", controlplane.AuthPolicyPublic)

	applied, err := controller.CreateFunctionOperation(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateFunctionOperation() error = %v", err)
	}
	if applied.Disposition != controlplane.CompletionApplied {
		t.Fatalf("CreateFunctionOperation() disposition = %q, want %q",
			applied.Disposition, controlplane.CompletionApplied)
	}
	if applied.View.Function.Name != "echo" || applied.View.Function.Lifecycle != model.FunctionActive {
		t.Fatalf("CreateFunctionOperation() function = %+v, want active echo", applied.View.Function)
	}
	if applied.Record.Outcome.Status != controlplane.OutcomeSucceeded ||
		applied.Record.Outcome.CredentialIssued {
		t.Fatalf("CreateFunctionOperation() outcome = %+v, want success without credential",
			applied.Record.Outcome)
	}
	if len(applied.Record.Outcome.AffectedResources) != 2 {
		t.Fatalf("CreateFunctionOperation() affected = %+v, want function and trigger",
			applied.Record.Outcome.AffectedResources)
	}

	replayed, err := controller.CreateFunctionOperation(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateFunctionOperation() replay error = %v", err)
	}
	if replayed.Disposition != controlplane.CompletionReplay {
		t.Fatalf("CreateFunctionOperation() replay disposition = %q, want %q",
			replayed.Disposition, controlplane.CompletionReplay)
	}
	if replayed.View.Function.ID != applied.View.Function.ID ||
		replayed.View.Function.ResourceRevision != applied.View.Function.ResourceRevision {
		t.Fatalf("CreateFunctionOperation() replay function = %+v, want the original resource",
			replayed.View.Function)
	}
	if replayed.Record.AppliedIndex != applied.Record.AppliedIndex {
		t.Fatalf("CreateFunctionOperation() replay applied index = %d, want %d",
			replayed.Record.AppliedIndex, applied.Record.AppliedIndex)
	}

	functions, err := controller.ListFunctions(context.Background())
	if err != nil {
		t.Fatalf("ListFunctions() error = %v", err)
	}
	if len(functions) != 1 {
		t.Fatalf("ListFunctions() = %d functions, want exactly one after replay", len(functions))
	}
}

func TestCreateFunctionOperationDoesNotReplayIssuedCredential(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, nil)
	input := testCreateFunctionOperationInput("op-1", "echo", controlplane.AuthPolicyToken)

	applied, err := controller.CreateFunctionOperation(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateFunctionOperation() error = %v", err)
	}
	if applied.View.InvocationToken == "" || !applied.Record.Outcome.CredentialIssued {
		t.Fatalf("CreateFunctionOperation() = %+v, want a one-time credential", applied)
	}

	replayed, err := controller.CreateFunctionOperation(context.Background(), input)
	assertControllerProblemCode(t, err, problem.CodeCredentialNotReplayable)
	if replayed.Disposition != controlplane.CompletionCredentialNotReplayable {
		t.Fatalf("CreateFunctionOperation() replay disposition = %q, want %q",
			replayed.Disposition, controlplane.CompletionCredentialNotReplayable)
	}
	if replayed.View.InvocationToken != "" {
		t.Fatalf("CreateFunctionOperation() replay returned a credential, want none")
	}
	if replayed.View.Function.ID != applied.View.Function.ID ||
		replayed.View.Function.ResourceRevision != applied.View.Function.ResourceRevision {
		t.Fatalf("CreateFunctionOperation() replay function = %+v, want current resource identity",
			replayed.View.Function)
	}
}

func TestCreateFunctionOperationRejectsReusedIDWithDifferentRequest(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, nil)
	first := testCreateFunctionOperationInput("op-1", "echo", controlplane.AuthPolicyPublic)
	if _, err := controller.CreateFunctionOperation(context.Background(), first); err != nil {
		t.Fatalf("CreateFunctionOperation() error = %v", err)
	}

	second := testCreateFunctionOperationInput("op-1", "other", controlplane.AuthPolicyPublic)
	_, err := controller.CreateFunctionOperation(context.Background(), second)
	assertControllerProblemCode(t, err, problem.CodeConflict)

	functions, err := controller.ListFunctions(context.Background())
	if err != nil {
		t.Fatalf("ListFunctions() error = %v", err)
	}
	if len(functions) != 1 {
		t.Fatalf("ListFunctions() = %d functions, want no state change for a reused id", len(functions))
	}
}

func TestCreateFunctionOperationRetainsTerminalFailureForExactRetry(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, nil)
	if _, err := controller.CreateFunctionOperation(
		context.Background(),
		testCreateFunctionOperationInput("op-1", "echo", controlplane.AuthPolicyPublic),
	); err != nil {
		t.Fatalf("CreateFunctionOperation() error = %v", err)
	}

	duplicate := testCreateFunctionOperationInput("op-2", "echo", controlplane.AuthPolicyPublic)
	failed, err := controller.CreateFunctionOperation(context.Background(), duplicate)
	assertControllerProblemCode(t, err, problem.CodeConflict)
	if failed.Record.Outcome.Status != controlplane.OutcomeFailed ||
		failed.Record.Outcome.Failure == nil ||
		failed.Record.Outcome.Failure.Code != problem.CodeConflict {
		t.Fatalf("CreateFunctionOperation() outcome = %+v, want a retained terminal conflict",
			failed.Record.Outcome)
	}
	if len(failed.Record.Outcome.AffectedResources) != 0 {
		t.Fatalf("CreateFunctionOperation() affected = %+v, want no resources for a failed operation",
			failed.Record.Outcome.AffectedResources)
	}

	retried, err := controller.CreateFunctionOperation(context.Background(), duplicate)
	assertControllerProblemCode(t, err, problem.CodeConflict)
	if retried.Disposition != controlplane.CompletionReplay {
		t.Fatalf("CreateFunctionOperation() retry disposition = %q, want %q",
			retried.Disposition, controlplane.CompletionReplay)
	}
	if retried.Record.AppliedIndex != failed.Record.AppliedIndex {
		t.Fatalf("CreateFunctionOperation() retry applied index = %d, want the original %d",
			retried.Record.AppliedIndex, failed.Record.AppliedIndex)
	}
}

func TestGetOperationReturnsRetainedRecordAndRejectsUnknownID(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, nil)
	input := testCreateFunctionOperationInput("op-1", "echo", controlplane.AuthPolicyPublic)
	applied, err := controller.CreateFunctionOperation(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateFunctionOperation() error = %v", err)
	}

	record, err := controller.GetOperation(context.Background(), input.Operation)
	if err != nil {
		t.Fatalf("GetOperation() error = %v", err)
	}
	if record.Digest != applied.Record.Digest || record.AppliedIndex != applied.Record.AppliedIndex {
		t.Fatalf("GetOperation() = %+v, want the retained completion record", record)
	}

	unknown := controlplane.OperationKey{
		Principal:   input.Operation.Principal,
		Namespace:   controlplane.DefaultNamespace,
		OperationID: "op-missing",
	}
	_, err = controller.GetOperation(context.Background(), unknown)
	assertControllerProblemCode(t, err, problem.CodeNotFound)
}

func TestSetFunctionLifecycleOperationAppliesCASOnceAndReplays(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, nil)
	created, err := controller.CreateFunctionOperation(
		context.Background(),
		testCreateFunctionOperationInput("op-create", "echo", controlplane.AuthPolicyPublic),
	)
	if err != nil {
		t.Fatalf("CreateFunctionOperation() error = %v", err)
	}

	input := SetFunctionLifecycleOperationInput{
		Operation: testOperationKey("op-disable"),
		Lifecycle: SetFunctionLifecycleInput{
			FunctionID:               created.View.Function.ID,
			ExpectedResourceRevision: created.View.Function.ResourceRevision,
			Lifecycle:                model.FunctionDisabled,
		},
	}
	disabled, err := controller.SetFunctionLifecycleOperation(context.Background(), input)
	if err != nil {
		t.Fatalf("SetFunctionLifecycleOperation() error = %v", err)
	}
	if disabled.Disposition != controlplane.CompletionApplied ||
		disabled.View.Function.Lifecycle != model.FunctionDisabled ||
		disabled.View.Function.ResourceRevision != created.View.Function.ResourceRevision+1 {
		t.Fatalf("SetFunctionLifecycleOperation() = %+v, want an applied disable", disabled)
	}

	replayed, err := controller.SetFunctionLifecycleOperation(context.Background(), input)
	if err != nil {
		t.Fatalf("SetFunctionLifecycleOperation() replay error = %v", err)
	}
	if replayed.Disposition != controlplane.CompletionReplay ||
		replayed.View.Function.Lifecycle != model.FunctionDisabled {
		t.Fatalf("SetFunctionLifecycleOperation() replay = %+v, want the retained disable", replayed)
	}

	stale, err := controller.SetFunctionLifecycleOperation(context.Background(), SetFunctionLifecycleOperationInput{
		Operation: testOperationKey("op-stale"),
		Lifecycle: SetFunctionLifecycleInput{
			FunctionID:               created.View.Function.ID,
			ExpectedResourceRevision: created.View.Function.ResourceRevision,
			Lifecycle:                model.FunctionActive,
		},
	})
	assertControllerProblemCode(t, err, problem.CodeRevisionConflict)
	if stale.Record.Outcome.Status != controlplane.OutcomeFailed {
		t.Fatalf("SetFunctionLifecycleOperation() stale outcome = %+v, want a retained terminal conflict",
			stale.Record.Outcome)
	}
}

func TestRotateInvocationTokenOperationIssuesCredentialExactlyOnce(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, nil)
	created, err := controller.CreateFunctionOperation(
		context.Background(),
		testCreateFunctionOperationInput("op-create", "echo", controlplane.AuthPolicyToken),
	)
	if err != nil {
		t.Fatalf("CreateFunctionOperation() error = %v", err)
	}

	input := RotateInvocationTokenOperationInput{
		Operation: testOperationKey("op-rotate"),
		Rotation: RotateInvocationTokenInput{
			FunctionID:               created.View.Function.ID,
			ExpectedResourceRevision: created.View.Trigger.ResourceRevision,
		},
	}
	rotated, err := controller.RotateInvocationTokenOperation(context.Background(), input)
	if err != nil {
		t.Fatalf("RotateInvocationTokenOperation() error = %v", err)
	}
	if rotated.Disposition != controlplane.CompletionApplied ||
		rotated.View.InvocationToken == "" ||
		rotated.View.InvocationToken == created.View.InvocationToken {
		t.Fatalf("RotateInvocationTokenOperation() = %+v, want a fresh one-time credential", rotated)
	}
	if rotated.View.Trigger.ResourceRevision != created.View.Trigger.ResourceRevision+1 {
		t.Fatalf("RotateInvocationTokenOperation() trigger = %+v, want an advanced revision",
			rotated.View.Trigger)
	}

	replayed, err := controller.RotateInvocationTokenOperation(context.Background(), input)
	assertControllerProblemCode(t, err, problem.CodeCredentialNotReplayable)
	if replayed.Disposition != controlplane.CompletionCredentialNotReplayable ||
		replayed.View.InvocationToken != "" {
		t.Fatalf("RotateInvocationTokenOperation() replay = %+v, want no replayed credential", replayed)
	}
	if replayed.View.Trigger.ResourceRevision != rotated.View.Trigger.ResourceRevision {
		t.Fatalf("RotateInvocationTokenOperation() replay trigger = %+v, want current resource identity",
			replayed.View.Trigger)
	}
}

func TestCreateVersionOperationCommitsVersionBeforeAdmissionAndReplays(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, []validatorStep{{report: acceptedReport}})
	artifactBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	artifactDigest := digest.Sum(artifactBytes)
	if _, err := controller.PutArtifact(
		context.Background(), artifactDigest, bytes.NewReader(artifactBytes),
	); err != nil {
		t.Fatalf("PutArtifact() error = %v", err)
	}
	function := createTestFunction(t, controller, "echo")

	input := CreateVersionOperationInput{
		Operation: testOperationKey("op-version"),
		Version:   testVersionInput(function.ID, artifactDigest),
	}
	applied, err := controller.CreateVersionOperation(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateVersionOperation() error = %v", err)
	}
	if applied.Disposition != controlplane.CompletionApplied ||
		applied.Version.State != model.VersionReady || applied.Deployment == nil {
		t.Fatalf("CreateVersionOperation() = %+v, want an admitted ready version", applied)
	}

	replayed, err := controller.CreateVersionOperation(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateVersionOperation() replay error = %v", err)
	}
	if replayed.Disposition != controlplane.CompletionReplay ||
		replayed.Version.VersionID != applied.Version.VersionID ||
		replayed.Version.State != model.VersionReady {
		t.Fatalf("CreateVersionOperation() replay = %+v, want the original ready version", replayed)
	}
}

func TestCreateVersionOperationRetryAfterValidatorFailureReplaysSameVersion(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, []validatorStep{
		{err: validator.ErrProcessFailed},
		{report: acceptedReport},
	})
	artifactBytes := []byte{0x10, 0x11, 0x12}
	artifactDigest := digest.Sum(artifactBytes)
	if _, err := controller.PutArtifact(
		context.Background(), artifactDigest, bytes.NewReader(artifactBytes),
	); err != nil {
		t.Fatalf("PutArtifact() error = %v", err)
	}
	function := createTestFunction(t, controller, "echo")

	input := CreateVersionOperationInput{
		Operation: testOperationKey("op-version"),
		Version:   testVersionInput(function.ID, artifactDigest),
	}
	failed, err := controller.CreateVersionOperation(context.Background(), input)
	if err == nil {
		t.Fatalf("CreateVersionOperation() error = nil, want a validator infrastructure failure")
	}
	if failed.Disposition != controlplane.CompletionApplied ||
		failed.Version.State != model.VersionValidating {
		t.Fatalf("CreateVersionOperation() = %+v, want a committed validating version", failed)
	}

	replayed, err := controller.CreateVersionOperation(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateVersionOperation() retry error = %v", err)
	}
	if replayed.Disposition != controlplane.CompletionReplay ||
		replayed.Version.VersionID != failed.Version.VersionID ||
		replayed.Version.State != model.VersionValidating {
		t.Fatalf("CreateVersionOperation() retry = %+v, want the original validating version", replayed)
	}

	if err := controller.ResumePendingValidation(context.Background(), 1); err != nil {
		t.Fatalf("ResumePendingValidation() error = %v", err)
	}
	version, deployment, err := controller.GetVersion(context.Background(), failed.Version.VersionID)
	if err != nil || version.State != model.VersionReady || deployment == nil {
		t.Fatalf("GetVersion() = (%+v, %+v, %v), want a resumed ready version", version, deployment, err)
	}
}

func TestPublishRouteOperationAppliesExactCASOnceAndReplays(t *testing.T) {
	t.Parallel()
	controller, _ := newControllerHarness(t, []validatorStep{{report: acceptedReport}})
	artifactBytes := []byte{0x20, 0x21, 0x22}
	artifactDigest := digest.Sum(artifactBytes)
	if _, err := controller.PutArtifact(
		context.Background(), artifactDigest, bytes.NewReader(artifactBytes),
	); err != nil {
		t.Fatalf("PutArtifact() error = %v", err)
	}
	function := createTestFunction(t, controller, "echo")
	admission, err := controller.CreateVersion(context.Background(), testVersionInput(function.ID, artifactDigest))
	if err != nil {
		t.Fatalf("CreateVersion() error = %v", err)
	}

	input := PublishRouteOperationInput{
		Operation: testOperationKey("op-route"),
		Route: PublishRouteInput{
			FunctionID:                  function.ID,
			VersionID:                   admission.Version.VersionID,
			ExpectedActiveRouteRevision: 0,
		},
	}
	published, err := controller.PublishRouteOperation(context.Background(), input)
	if err != nil {
		t.Fatalf("PublishRouteOperation() error = %v", err)
	}
	if published.Disposition != controlplane.CompletionApplied ||
		published.Route.RouteRevision != 1 || !published.Route.Enabled {
		t.Fatalf("PublishRouteOperation() = %+v, want an applied enabled route", published)
	}
	if published.Function.ActiveRouteRevision != 1 {
		t.Fatalf("PublishRouteOperation() function = %+v, want an advanced active pointer",
			published.Function)
	}

	replayed, err := controller.PublishRouteOperation(context.Background(), input)
	if err != nil {
		t.Fatalf("PublishRouteOperation() replay error = %v", err)
	}
	if replayed.Disposition != controlplane.CompletionReplay ||
		replayed.Route.ID != published.Route.ID ||
		replayed.Route.RouteRevision != published.Route.RouteRevision {
		t.Fatalf("PublishRouteOperation() replay = %+v, want the original route", replayed)
	}

	stale, err := controller.PublishRouteOperation(context.Background(), PublishRouteOperationInput{
		Operation: testOperationKey("op-stale"),
		Route: PublishRouteInput{
			FunctionID:                  function.ID,
			VersionID:                   admission.Version.VersionID,
			ExpectedActiveRouteRevision: 0,
		},
	})
	assertControllerProblemCode(t, err, problem.CodeRevisionConflict)
	if stale.Record.Outcome.Status != controlplane.OutcomeFailed {
		t.Fatalf("PublishRouteOperation() stale outcome = %+v, want a retained terminal conflict",
			stale.Record.Outcome)
	}

	retried, err := controller.PublishRouteOperation(context.Background(), PublishRouteOperationInput{
		Operation: testOperationKey("op-stale"),
		Route: PublishRouteInput{
			FunctionID:                  function.ID,
			VersionID:                   admission.Version.VersionID,
			ExpectedActiveRouteRevision: 0,
		},
	})
	assertControllerProblemCode(t, err, problem.CodeRevisionConflict)
	if retried.Disposition != controlplane.CompletionReplay ||
		retried.Record.AppliedIndex != stale.Record.AppliedIndex {
		t.Fatalf("PublishRouteOperation() retry = %+v, want the retained failure", retried)
	}
}

func testOperationKey(operationID string) controlplane.OperationKey {
	return controlplane.OperationKey{
		Principal:   "admin",
		Namespace:   controlplane.DefaultNamespace,
		OperationID: operationID,
	}
}

func testCreateFunctionOperationInput(
	operationID string,
	name string,
	policy controlplane.AuthPolicy,
) CreateFunctionOperationInput {
	return CreateFunctionOperationInput{
		Operation: controlplane.OperationKey{
			Principal:   "admin",
			Namespace:   controlplane.DefaultNamespace,
			OperationID: operationID,
		},
		Function: CreateFunctionInput{Name: name, AuthPolicy: policy},
	}
}
