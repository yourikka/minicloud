package controlplane

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
)

func TestCatalogCreatesFunctionAndDefaultTriggerAtomically(t *testing.T) {
	t.Parallel()
	catalog := NewCatalog()
	command := validCreateFunction(3, "function-01", "echo")
	result, err := catalog.CreateFunction(command)
	if err != nil {
		t.Fatalf("CreateFunction() error = %v", err)
	}
	if len(result.Functions) != 1 || len(result.Triggers) != 1 ||
		result.Functions[0].ID != command.Function.ID || result.Triggers[0].FunctionID != command.Function.ID {
		t.Fatalf("CreateFunction() = %+v, want one Function and its Trigger", result)
	}
	function, trigger, err := catalog.GetFunctionByName("echo")
	if err != nil {
		t.Fatalf("GetFunctionByName() error = %v", err)
	}
	if function.ResourceRevision != 1 || trigger.ResourceRevision != 1 || !trigger.Enabled ||
		trigger.AuthPolicy != AuthPolicyToken || trigger.TokenVerifierDigest == nil {
		t.Fatalf("persisted values = %+v %+v", function, trigger)
	}
	function.Labels["team"] = "mutated"
	*trigger.TokenVerifierDigest = digest.Sum([]byte("mutated"))
	again, againTrigger, err := catalog.GetFunction(command.Function.ID)
	if err != nil {
		t.Fatalf("GetFunction() error = %v", err)
	}
	if again.Labels["team"] != "platform" || *againTrigger.TokenVerifierDigest != *command.Trigger.TokenVerifierDigest {
		t.Fatalf("GetFunction() exposed catalog storage: %+v %+v", again, againTrigger)
	}
}

func TestCatalogCreateRejectsDuplicateAndPartialCommands(t *testing.T) {
	t.Parallel()
	catalog := NewCatalog()
	first := validCreateFunction(1, "function-01", "echo")
	if _, err := catalog.CreateFunction(first); err != nil {
		t.Fatalf("CreateFunction(first) error = %v", err)
	}
	duplicateName := validCreateFunction(2, "function-02", "echo")
	_, err := catalog.CreateFunction(duplicateName)
	assertCatalogProblemCode(t, err, problem.CodeConflict, "")
	if snapshot := catalog.Snapshot(); len(snapshot.Functions) != 1 || len(snapshot.Triggers) != 1 {
		t.Fatalf("duplicate create changed catalog: %+v", snapshot)
	}

	missingCreateCAS := validCreateFunction(2, "function-02", "other")
	missingCreateCAS.IfNoneMatch = false
	_, err = catalog.CreateFunction(missingCreateCAS)
	assertCatalogProblemCode(t, err, problem.CodeInvalidArgument, "if_none_match")
	badTrigger := validCreateFunction(2, "function-02", "other")
	badTrigger.Trigger.FunctionID = "wrong-function"
	_, err = catalog.CreateFunction(badTrigger)
	assertCatalogProblemCode(t, err, problem.CodeInvalidArgument, "trigger")
}

func TestCatalogLifecycleUsesExactResourceRevision(t *testing.T) {
	t.Parallel()
	catalog := NewCatalog()
	create := validCreateFunction(1, "function-01", "echo")
	if _, err := catalog.CreateFunction(create); err != nil {
		t.Fatalf("CreateFunction() error = %v", err)
	}
	updatedAt := create.Function.CreatedAt.Add(time.Minute)
	disabled, err := catalog.SetFunctionLifecycle(SetFunctionLifecycleCommand{
		FunctionID:               create.Function.ID,
		ExpectedResourceRevision: 1,
		Lifecycle:                model.FunctionDisabled,
		UpdatedAt:                updatedAt,
		AppliedIndex:             2,
	})
	if err != nil {
		t.Fatalf("SetFunctionLifecycle(disable) error = %v", err)
	}
	if disabled.Lifecycle != model.FunctionDisabled || disabled.ResourceRevision != 2 || !disabled.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("disabled function = %+v", disabled)
	}
	_, err = catalog.SetFunctionLifecycle(SetFunctionLifecycleCommand{
		FunctionID:               create.Function.ID,
		ExpectedResourceRevision: 1,
		Lifecycle:                model.FunctionActive,
		UpdatedAt:                updatedAt.Add(time.Minute),
		AppliedIndex:             3,
	})
	assertCatalogProblemCode(t, err, problem.CodeRevisionConflict, "")
	var conflict *RevisionConflict
	if !errors.As(err, &conflict) || conflict.RevisionKind != "resource_revision" || conflict.Expected != 1 || conflict.Actual != 2 {
		t.Fatalf("revision conflict = %#v, want resource_revision expected=1 actual=2", conflict)
	}
	if details := conflict.Details(); details["revision_kind"] != "resource_revision" || details["expected_revision"] != uint64(1) || details["actual_revision"] != uint64(2) {
		t.Fatalf("revision details = %#v", details)
	}
	got, _, err := catalog.GetFunction(create.Function.ID)
	if err != nil {
		t.Fatalf("GetFunction() error = %v", err)
	}
	if got.Lifecycle != model.FunctionDisabled || got.ResourceRevision != 2 {
		t.Fatalf("failed CAS changed function: %+v", got)
	}
	restored, err := catalog.SetFunctionLifecycle(SetFunctionLifecycleCommand{
		FunctionID:               create.Function.ID,
		ExpectedResourceRevision: 2,
		Lifecycle:                model.FunctionActive,
		UpdatedAt:                updatedAt.Add(time.Minute),
		AppliedIndex:             3,
	})
	if err != nil || restored.Lifecycle != model.FunctionActive || restored.ResourceRevision != 3 {
		t.Fatalf("SetFunctionLifecycle(restore) = %+v, %v", restored, err)
	}
}

func TestCatalogSnapshotAndStateDigestAreStable(t *testing.T) {
	t.Parallel()
	first := validCreateFunction(1, "function-01", "alpha")
	second := validCreateFunction(2, "function-02", "bravo")
	left := NewCatalog()
	right := NewCatalog()
	for _, command := range []CreateFunctionCommand{first, second} {
		if _, err := left.CreateFunction(command); err != nil {
			t.Fatalf("left CreateFunction() error = %v", err)
		}
	}
	for _, command := range []CreateFunctionCommand{second, first} {
		if _, err := right.CreateFunction(command); err != nil {
			t.Fatalf("right CreateFunction() error = %v", err)
		}
	}
	leftDigest, err := left.StateDigest()
	if err != nil {
		t.Fatalf("left StateDigest() error = %v", err)
	}
	rightDigest, err := right.StateDigest()
	if err != nil {
		t.Fatalf("right StateDigest() error = %v", err)
	}
	if leftDigest != rightDigest {
		t.Fatalf("state digests differ: left=%s right=%s", leftDigest, rightDigest)
	}
	snapshot := left.Snapshot()
	if snapshot.Functions[0].ID != first.Function.ID || snapshot.Triggers[1].FunctionID != second.Function.ID {
		t.Fatalf("Snapshot() ordering = %+v", snapshot)
	}
	snapshot.Functions[0].Labels["team"] = "changed"
	if again := left.Snapshot(); again.Functions[0].Labels["team"] != "platform" {
		t.Fatalf("Snapshot() exposed labels: %+v", again)
	}
}

func TestHTTPTriggerRejectsUnsupportedSecretShapes(t *testing.T) {
	t.Parallel()
	command := validCreateFunction(1, "function-01", "echo")
	command.Trigger.AuthPolicy = AuthPolicyPublic
	_, err := NewCatalog().CreateFunction(command)
	assertCatalogProblemCode(t, err, problem.CodeInvalidArgument, "token_verifier_digest")
	command = validCreateFunction(1, "function-01", "echo")
	command.Trigger.TokenVerifierDigest = nil
	_, err = NewCatalog().CreateFunction(command)
	assertCatalogProblemCode(t, err, problem.CodeInvalidArgument, "token_verifier_digest")
}

func TestCatalogRejectsCreationBeyondFunctionSafetyMaximum(t *testing.T) {
	catalog := NewCatalog()
	for index := uint64(1); index <= DefaultMaxFunctions; index++ {
		id := "function-" + formatIndex(index)
		name := "f" + formatIndex(index)
		if _, err := catalog.CreateFunction(validCreateFunction(index, id, name)); err != nil {
			t.Fatalf("CreateFunction(%d) error = %v", index, err)
		}
	}
	_, err := catalog.CreateFunction(validCreateFunction(DefaultMaxFunctions+1, "function-overflow", "overflow"))
	assertCatalogProblemCode(t, err, problem.CodeOverloaded, "")
	if status := len(catalog.Snapshot().Functions); status != DefaultMaxFunctions {
		t.Fatalf("Function count = %d, want %d", status, DefaultMaxFunctions)
	}
}

func validCreateFunction(index uint64, id, name string) CreateFunctionCommand {
	createdAt := time.Date(2026, 7, 25, 0, 0, int(index), 0, time.UTC)
	verifier := digest.Sum([]byte("verifier-" + id))
	return CreateFunctionCommand{
		IfNoneMatch:  true,
		AppliedIndex: index,
		Function: model.Function{
			Metadata: model.Metadata{
				ID: id, Namespace: model.DefaultNamespace, CreatedAt: createdAt, UpdatedAt: createdAt,
				CreatedRaftIndex: index, ResourceRevision: 1,
			},
			Name: name, Labels: map[string]string{"team": "platform"}, Lifecycle: model.FunctionActive,
		},
		Trigger: HTTPTrigger{
			Metadata: model.Metadata{
				ID: "trigger-" + id, Namespace: model.DefaultNamespace, CreatedAt: createdAt, UpdatedAt: createdAt,
				CreatedRaftIndex: index, ResourceRevision: 1,
			},
			FunctionID: id, Enabled: true, AuthPolicy: AuthPolicyToken, TokenVerifierDigest: &verifier,
		},
	}
}

func formatIndex(value uint64) string {
	return fmt.Sprintf("%d", value)
}

func assertCatalogProblemCode(t *testing.T, err error, wantCode problem.Code, wantField string) {
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
