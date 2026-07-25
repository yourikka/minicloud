package controlplane

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/problem"
)

func TestRequestDigestCanonicalizesMeaningfulInputs(t *testing.T) {
	t.Parallel()
	zero := uint64(0)
	artifact := digest.Sum([]byte("artifact"))
	base := Request{
		Method: "post",
		Path:   "/v1/functions/echo",
		Preconditions: Preconditions{
			ExpectedActiveRouteRevision: &zero,
		},
		BodyPresent:    true,
		Body:           []byte(`{"labels":{"team":"platform"},"enabled":true}`),
		ArtifactDigest: &artifact,
	}
	got, err := base.Digest()
	if err != nil {
		t.Fatalf("Digest() error = %v", err)
	}
	equivalent := base
	equivalent.Method = "POST"
	equivalent.Body = []byte(`{ "enabled" : true, "labels" : { "team" : "platform" } }`)
	also, err := equivalent.Digest()
	if err != nil {
		t.Fatalf("equivalent Digest() error = %v", err)
	}
	if got != also {
		t.Fatalf("canonical digest = %q, equivalent digest = %q", got, also)
	}

	withoutRouteRevision := base
	withoutRouteRevision.Preconditions.ExpectedActiveRouteRevision = nil
	different, err := withoutRouteRevision.Digest()
	if err != nil {
		t.Fatalf("Digest() without route revision error = %v", err)
	}
	if got == different {
		t.Fatal("nil and zero expected active Route revisions produced the same digest")
	}

	absentBody := base
	absentBody.BodyPresent = false
	absentBody.Body = nil
	absent, err := absentBody.Digest()
	if err != nil {
		t.Fatalf("Digest() absent body error = %v", err)
	}
	nullBody := absentBody
	nullBody.BodyPresent = true
	nullBody.Body = []byte("null")
	null, err := nullBody.Digest()
	if err != nil {
		t.Fatalf("Digest() null body error = %v", err)
	}
	if absent == null {
		t.Fatal("absent and explicit null request bodies produced the same digest")
	}
}

func TestRequestDigestRejectsNonCanonicalOrAmbiguousInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Request)
		field  string
	}{
		{
			name: "lowercase percent encoding",
			mutate: func(request *Request) {
				request.Path = "/v1/functions/%65cho"
			},
			field: "path",
		},
		{
			name: "query in path",
			mutate: func(request *Request) {
				request.Path = "/v1/functions/echo?verbose=true"
			},
			field: "path",
		},
		{
			name: "noncanonical method",
			mutate: func(request *Request) {
				request.Method = "POST "
			},
			field: "method",
		},
		{
			name: "create combined with revision",
			mutate: func(request *Request) {
				revision := uint64(1)
				request.Preconditions = Preconditions{
					IfNoneMatch:              true,
					ExpectedResourceRevision: &revision,
				}
			},
			field: "preconditions",
		},
		{
			name: "duplicate body key",
			mutate: func(request *Request) {
				request.BodyPresent = true
				request.Body = []byte(`{"name":"first","name":"second"}`)
			},
			field: "body",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := validRequest()
			test.mutate(&request)
			_, err := request.Digest()
			assertProblemCodeAndField(t, err, problem.CodeInvalidArgument, test.field)
		})
	}
}

func TestLedgerCompletesOnceReplaysAndRejectsDigestConflict(t *testing.T) {
	t.Parallel()
	ledger := newTestLedger(t)
	completion := validCompletion()
	completion.Outcome.AffectedResources = []AffectedResource{
		{Kind: "trigger", ID: "trigger-01", ResourceRevision: revision(2)},
		{Kind: "function", ID: "function-01", ResourceRevision: revision(3)},
	}
	first, err := ledger.Complete(completion)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if first.Disposition != CompletionApplied {
		t.Fatalf("first disposition = %q, want %q", first.Disposition, CompletionApplied)
	}
	if first.Record.Outcome.AffectedResources[0].Kind != "function" {
		t.Fatalf("record resources were not canonicalized: %+v", first.Record.Outcome.AffectedResources)
	}

	replayInput := completion
	replayInput.CompletedAt = completion.CompletedAt.Add(time.Hour)
	replayInput.AppliedIndex++
	replayInput.Outcome = Outcome{
		Status: OutcomeFailed,
		Failure: &Failure{
			Code:    problem.CodeInvalidModule,
			Message: "module failed validation",
		},
		AffectedResources: []AffectedResource{{
			Kind: "function", ID: "function-01", ResourceRevision: revision(99),
		}},
	}
	replay, err := ledger.Complete(replayInput)
	if err != nil {
		t.Fatalf("Complete(replay) error = %v", err)
	}
	if replay.Disposition != CompletionReplay ||
		replay.Record.Digest != first.Record.Digest ||
		replay.Record.AppliedIndex != first.Record.AppliedIndex ||
		replay.Record.CompletedAt != first.Record.CompletedAt ||
		replay.Record.Outcome.Status != OutcomeSucceeded ||
		len(replay.Record.Outcome.AffectedResources) != 2 ||
		*replay.Record.Outcome.AffectedResources[0].ResourceRevision != 3 {
		t.Fatalf("replay = %+v, want the original successful record", replay)
	}

	conflict := completion
	conflict.Request.Method = "PATCH"
	_, err = ledger.Complete(conflict)
	assertProblemCodeAndField(t, err, problem.CodeConflict, "")
	status := ledger.Status()
	if status.Records != 1 || status.Tombstones != 0 {
		t.Fatalf("Status() = %+v, want one completed record", status)
	}
}

func TestLedgerDoesNotReplayOneTimeCredential(t *testing.T) {
	t.Parallel()
	ledger := newTestLedger(t)
	completion := validCompletion()
	completion.Outcome.CredentialIssued = true
	if _, err := ledger.Complete(completion); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	replay, err := ledger.Complete(completion)
	if err != nil {
		t.Fatalf("Complete(replay) error = %v", err)
	}
	if replay.Disposition != CompletionCredentialNotReplayable ||
		len(replay.Record.Outcome.AffectedResources) != 1 ||
		replay.Record.Outcome.AffectedResources[0].ID != "function-01" {
		t.Fatalf("credential replay = %+v, want safe resource reference", replay)
	}

	record, err := ledger.Lookup(completion.Key)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if !record.Outcome.CredentialIssued || len(record.Outcome.AffectedResources) != 1 {
		t.Fatalf("Lookup() safe record = %+v", record)
	}
}

func TestLedgerReplaysBeforeValidatingNewCompletionFields(t *testing.T) {
	t.Parallel()
	ledger := newTestLedger(t)
	completion := validCompletion()
	if _, err := ledger.Complete(completion); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	retry := completion
	retry.AppliedIndex = 0
	retry.CompletedAt = time.Time{}
	retry.Outcome = Outcome{Status: "invalid"}
	result, err := ledger.Complete(retry)
	if err != nil {
		t.Fatalf("Complete(retry) error = %v", err)
	}
	if result.Disposition != CompletionReplay || result.Record.AppliedIndex != completion.AppliedIndex {
		t.Fatalf("Complete(retry) = %+v, want original retained record", result)
	}
}

func TestLedgerRetainsFailedOutcomeWithoutAffectedResources(t *testing.T) {
	t.Parallel()
	ledger := newTestLedger(t)
	completion := validCompletion()
	completion.Outcome = Outcome{
		Status: OutcomeFailed,
		Failure: &Failure{
			Code:    problem.CodeInvalidArgument,
			Message: "request did not satisfy a precondition",
		},
		AffectedResources: []AffectedResource{},
	}
	first, err := ledger.Complete(completion)
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if first.Disposition != CompletionApplied || len(first.Record.Outcome.AffectedResources) != 0 {
		t.Fatalf("Complete() = %+v, want retained empty affected-resource set", first)
	}
	replay, err := ledger.Complete(completion)
	if err != nil || replay.Disposition != CompletionReplay || len(replay.Record.Outcome.AffectedResources) != 0 {
		t.Fatalf("Complete(replay) = %+v, %v", replay, err)
	}
}

func TestLedgerGCRetainsTombstoneThenAllowsReuse(t *testing.T) {
	t.Parallel()
	operationTTL := time.Hour
	tombstoneTTL := 24 * time.Hour
	ledger := newTestLedgerWithConfig(t, ledgerConfig{OperationTTL: operationTTL, TombstoneTTL: tombstoneTTL, MaxOperations: DefaultMaxOperations})
	completion := validCompletion()
	if _, err := ledger.Complete(completion); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	before, err := ledger.GC(completion.CompletedAt.Add(operationTTL - time.Nanosecond))
	if err != nil {
		t.Fatalf("GC(before expiry) error = %v", err)
	}
	if before != (GCResult{}) {
		t.Fatalf("GC(before expiry) = %+v, want no changes", before)
	}

	atExpiry, err := ledger.GC(completion.CompletedAt.Add(operationTTL))
	if err != nil {
		t.Fatalf("GC(at expiry) error = %v", err)
	}
	if atExpiry != (GCResult{Tombstoned: 1}) {
		t.Fatalf("GC(at expiry) = %+v, want one tombstone", atExpiry)
	}
	_, err = ledger.Lookup(completion.Key)
	assertProblemCodeAndField(t, err, problem.CodeOperationExpired, "")
	_, err = ledger.Complete(completion)
	assertProblemCodeAndField(t, err, problem.CodeOperationExpired, "")

	snapshot := ledger.Snapshot()
	if len(snapshot.Records) != 0 || len(snapshot.Tombstones) != 1 || snapshot.Tombstones[0].Digest == "" {
		t.Fatalf("tombstoned Snapshot() = %+v", snapshot)
	}

	deleted, err := ledger.GC(completion.CompletedAt.Add(operationTTL + tombstoneTTL))
	if err != nil {
		t.Fatalf("GC(tombstone expiry) error = %v", err)
	}
	if deleted != (GCResult{Deleted: 1}) {
		t.Fatalf("GC(tombstone expiry) = %+v, want one delete", deleted)
	}
	_, err = ledger.Lookup(completion.Key)
	assertProblemCodeAndField(t, err, problem.CodeNotFound, "")
	if result, err := ledger.Complete(completion); err != nil || result.Disposition != CompletionApplied {
		t.Fatalf("Complete(reused after tombstone expiry) = %+v, %v", result, err)
	}
}

func TestLedgerTombstonePrecedesRetryValidation(t *testing.T) {
	t.Parallel()
	ledger := newTestLedgerWithConfig(t, ledgerConfig{
		MaxOperations: DefaultMaxOperations,
		OperationTTL:  time.Hour,
		TombstoneTTL:  DefaultOperationTombstoneTTL,
	})
	completion := validCompletion()
	if _, err := ledger.Complete(completion); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if _, err := ledger.GC(completion.CompletedAt.Add(time.Hour)); err != nil {
		t.Fatalf("GC() error = %v", err)
	}
	retry := completion
	retry.Request.Body = []byte(`{"name":"first","name":"second"}`)
	retry.AppliedIndex = 0
	_, err := ledger.Complete(retry)
	assertProblemCodeAndField(t, err, problem.CodeOperationExpired, "")
}

func TestLedgerRequiresExplicitGCBeforeCapacityCanRecover(t *testing.T) {
	t.Parallel()
	ledger := newTestLedgerWithConfig(t, ledgerConfig{MaxOperations: 1, OperationTTL: DefaultOperationTTL, TombstoneTTL: DefaultOperationTombstoneTTL})
	first := validCompletion()
	if _, err := ledger.Complete(first); err != nil {
		t.Fatalf("Complete(first) error = %v", err)
	}
	second := validCompletion()
	second.Key.OperationID = "operation-02"
	_, err := ledger.Complete(second)
	assertProblemCodeAndField(t, err, problem.CodeOverloaded, "")
}

func TestLedgerConcurrentSameOperationAppliesExactlyOnce(t *testing.T) {
	ledger := newTestLedger(t)
	completion := validCompletion()
	const callers = 64
	start := make(chan struct{})
	results := make(chan CompletionDisposition, callers)
	errorsSeen := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Go(func() {
			<-start
			result, err := ledger.Complete(completion)
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- result.Disposition
		})
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("Complete() concurrent error = %v", err)
	}
	var applied, replayed int
	for disposition := range results {
		switch disposition {
		case CompletionApplied:
			applied++
		case CompletionReplay:
			replayed++
		default:
			t.Errorf("unexpected disposition %q", disposition)
		}
	}
	if applied != 1 || replayed != callers-1 {
		t.Fatalf("applied=%d replayed=%d, want 1 and %d", applied, replayed, callers-1)
	}
	if status := ledger.Status(); status.Records != 1 {
		t.Fatalf("Status() = %+v, want one record", status)
	}
}

func TestLedgerSnapshotDefensivelyCopiesOutcome(t *testing.T) {
	t.Parallel()
	ledger := newTestLedger(t)
	completion := validCompletion()
	if _, err := ledger.Complete(completion); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	snapshot := ledger.Snapshot()
	*snapshot.Records[0].Outcome.AffectedResources[0].ResourceRevision = 99
	snapshot.Records[0].Outcome.AffectedResources[0].Kind = "mutated"
	again := ledger.Snapshot()
	if *again.Records[0].Outcome.AffectedResources[0].ResourceRevision != 1 ||
		again.Records[0].Outcome.AffectedResources[0].Kind != "function" {
		t.Fatalf("Snapshot() exposed internal storage: %+v", again)
	}
}

func TestLedgerRejectsInvalidDeterministicInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Completion)
		field  string
	}{
		{
			name: "zero applied index",
			mutate: func(completion *Completion) {
				completion.AppliedIndex = 0
			},
			field: "applied_index",
		},
		{
			name: "non UTC completed at",
			mutate: func(completion *Completion) {
				completion.CompletedAt = time.Date(2026, 7, 25, 0, 0, 0, 0, time.FixedZone("CST", 8*60*60))
			},
			field: "completed_at",
		},
		{
			name: "failed without safe error",
			mutate: func(completion *Completion) {
				completion.Outcome.Status = OutcomeFailed
				completion.Outcome.Failure = nil
			},
			field: "failure",
		},
		{
			name: "credential without affected resource",
			mutate: func(completion *Completion) {
				completion.Outcome.AffectedResources = []AffectedResource{}
				completion.Outcome.CredentialIssued = true
			},
			field: "credential_issued",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ledger := newTestLedger(t)
			completion := validCompletion()
			test.mutate(&completion)
			_, err := ledger.Complete(completion)
			assertProblemCodeAndField(t, err, problem.CodeInvalidArgument, test.field)
		})
	}
}

func validRequest() Request {
	return Request{
		Method: "POST",
		Path:   "/v1/functions/echo",
		Preconditions: Preconditions{
			IfNoneMatch: true,
		},
		BodyPresent: true,
		Body:        []byte(`{"name":"echo"}`),
	}
}

func validCompletion() Completion {
	return Completion{
		Key: OperationKey{
			Principal:   "local-admin",
			Namespace:   DefaultNamespace,
			OperationID: "operation-01",
		},
		Request: validRequest(),
		Outcome: Outcome{
			Status: OutcomeSucceeded,
			AffectedResources: []AffectedResource{{
				Kind:             "function",
				ID:               "function-01",
				ResourceRevision: revision(1),
			}},
		},
		CompletedAt:  time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC),
		AppliedIndex: 1,
	}
}

func newTestLedger(t *testing.T) *Ledger {
	t.Helper()
	return New()
}

func newTestLedgerWithConfig(t *testing.T, config ledgerConfig) *Ledger {
	t.Helper()
	return newLedgerWithConfig(config)
}

func revision(value uint64) *uint64 {
	return &value
}

func assertProblemCodeAndField(t *testing.T, err error, wantCode problem.Code, wantField string) {
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
