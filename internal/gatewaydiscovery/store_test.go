package gatewaydiscovery

import (
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

type manualClock struct {
	mu      sync.Mutex
	elapsed time.Duration
}

func (c *manualClock) Elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.elapsed
}

func (c *manualClock) Set(elapsed time.Duration) {
	c.mu.Lock()
	c.elapsed = elapsed
	c.mu.Unlock()
}

func newStore(t *testing.T, clock *manualClock, config Config) *Store {
	t.Helper()
	config.Clock = clock.Elapsed
	store, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return store
}

func staleWindow(value time.Duration) *time.Duration {
	return &value
}

func servingSnapshot(t *testing.T, functionID string, epoch, sequence, revision uint64, withEndpoint bool) discovery.Snapshot {
	return servingSnapshotWithSession(
		t,
		functionID,
		epoch,
		sequence,
		revision,
		revision,
		withEndpoint,
	)
}

func servingSnapshotWithSession(
	t *testing.T,
	functionID string,
	epoch, sequence, revision, sessionEpoch uint64,
	withEndpoint bool,
) discovery.Snapshot {
	return servingSnapshotWithMutation(
		t,
		functionID,
		epoch,
		sequence,
		revision,
		sessionEpoch,
		withEndpoint,
		nil,
	)
}

func servingSnapshotWithMutation(
	t *testing.T,
	functionID string,
	epoch, sequence, revision, sessionEpoch uint64,
	withEndpoint bool,
	mutate func(*discovery.Input),
) discovery.Snapshot {
	t.Helper()
	builder, err := discovery.New(discovery.Config{})
	if err != nil {
		t.Fatalf("discovery.New() error = %v", err)
	}
	policy := digest.Sum([]byte("policy-" + functionID))
	verifier := digest.Sum([]byte("verifier-" + functionID))
	input := discovery.Input{
		DiscoveryEpoch:  epoch,
		ServingSequence: sequence,
		GeneratedAt:     time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC),
		Function: discovery.Function{
			ID: functionID, Name: "fn-" + functionID[len("function-"):],
			ResourceRevision: revision, Lifecycle: model.FunctionActive,
		},
		Trigger: discovery.HTTPTrigger{
			ID: "trigger-" + functionID, FunctionID: functionID,
			ResourceRevision: revision, Enabled: true, AuthPolicy: discovery.AuthToken,
			TokenVerifierDigest: &verifier,
		},
		Route: discovery.Route{
			Present: true, FunctionID: functionID, ResourceRevision: revision,
			RouteRevision: revision, Enabled: true,
			Targets: []model.RouteTarget{{
				VersionID: "version-" + functionID, AdmissionEpoch: 1,
				DeploymentGeneration: 1, EffectivePolicyDigest: policy,
				WeightBasisPoints: model.TotalRouteWeightBasisPoints,
			}},
			Affinity: model.AffinityRequestID, HashVersion: model.HashVersionSHA256BPSV1,
			SaltID: "salt-" + functionID, Salt: []byte("0123456789abcdef"),
		},
	}
	if withEndpoint {
		assignment := servingauth.AssignmentIdentity{
			Worker: servingauth.WorkerSession{
				WorkerID: "worker-" + functionID, BootID: "boot-1", SessionEpoch: sessionEpoch,
			},
			AssignmentID: "assignment-" + functionID, VersionID: "version-" + functionID,
			AdmissionEpoch: 1, DeploymentGeneration: 1, PolicyDigest: policy, Mode: servingauth.ModeNormal,
		}
		input.Candidates = []discovery.EndpointCandidate{{
			Assignment: assignment, DesiredState: discovery.AssignmentAssigned,
			Worker: scheduler.WorkerSnapshot{
				Session: assignment.Worker,
				Runtime: scheduler.RuntimeProfile{
					Name: wasmprofile.RuntimeName, Version: wasmprofile.RuntimeVersion,
					Engine: wasmprofile.EngineCompiler, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
					ABI: model.ABIWASICommandV1, HostAPI: model.HostAPIProfileNone,
					FeatureProfile: wasmprofile.FeatureProfile, MemoryMiB: 128,
				},
				Intent: scheduler.IntentSchedulable, State: scheduler.SessionReady,
				Drain:    scheduler.DrainNotDraining,
				Capacity: scheduler.Capacity{MemoryMiB: 512, Slots: 8},
			},
			ReplicaReady: true,
			Authorization: &servingauth.Authorization{
				Fence:    servingauth.InvocationFence{Assignment: assignment, DiscoveryEpoch: epoch},
				Lifetime: servingauth.LifetimeTTL,
				TTL:      time.Minute,
			},
			Address: "worker.internal:7443",
		}}
	}
	if mutate != nil {
		mutate(&input)
	}
	result, err := builder.Build(input)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	return result.Snapshot
}

func event(full bool, snapshots ...discovery.Snapshot) Event {
	result := Event{Full: full, Snapshots: snapshots}
	if len(snapshots) > 0 {
		result.DiscoveryEpoch = snapshots[0].DiscoveryEpoch
		result.ServingSequence = snapshots[0].ServingSequence
	}
	return result
}

func assertProblemCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	var classified *problem.Error
	if !errors.As(err, &classified) || classified.Code != want {
		t.Fatalf("error = %v, want problem code %q", err, want)
	}
}

func TestStoreRequiresFullSyncAndLatchesSequenceGap(t *testing.T) {
	clock := &manualClock{}
	store := newStore(t, clock, Config{})
	initial := servingSnapshot(t, "function-a", 1, 5, 1, false)
	assertProblemCode(t, store.Apply(event(false, initial)), problem.CodeControlPlaneStale)
	if err := store.Apply(event(true, initial)); err != nil {
		t.Fatalf("Apply(full) error = %v", err)
	}
	next := servingSnapshot(t, "function-a", 1, 6, 2, false)
	if err := store.Apply(event(false, next)); err != nil {
		t.Fatalf("Apply(next) error = %v", err)
	}
	gap := servingSnapshot(t, "function-a", 1, 8, 3, false)
	assertProblemCode(t, store.Apply(event(false, gap)), problem.CodeControlPlaneStale)
	if !store.Status().NeedsFullSync {
		t.Fatal("sequence gap did not latch Full Sync requirement")
	}
	late := servingSnapshot(t, "function-a", 1, 7, 3, false)
	assertProblemCode(t, store.Apply(event(false, late)), problem.CodeControlPlaneStale)
	if err := store.Apply(event(true, gap)); err != nil {
		t.Fatalf("Apply(recovery Full Sync) error = %v", err)
	}
	if store.Status().NeedsFullSync {
		t.Fatal("Full Sync did not clear gap latch")
	}
	oldEpoch := servingSnapshot(t, "function-a", 1, 9, 4, false)
	newEpoch := servingSnapshot(t, "function-a", 2, 3, 4, false)
	if err := store.Apply(event(true, newEpoch)); err != nil {
		t.Fatalf("Apply(new epoch Full Sync) error = %v", err)
	}
	assertProblemCode(t, store.Apply(event(false, oldEpoch)), problem.CodeStaleGeneration)
}

func TestStoreExactReplayDoesNotRefreshMaxStale(t *testing.T) {
	clock := &manualClock{}
	store := newStore(t, clock, Config{MaxStale: staleWindow(10 * time.Second)})
	snapshot := servingSnapshot(t, "function-a", 1, 1, 1, false)
	full := event(true, snapshot)
	if err := store.Apply(full); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	clock.Set(9 * time.Second)
	if err := store.Apply(full); err != nil {
		t.Fatalf("exact replay error = %v", err)
	}
	clock.Set(10*time.Second - time.Nanosecond)
	if _, err := store.Lookup("function-a"); err != nil {
		t.Fatalf("Lookup() before exact expiry error = %v", err)
	}
	clock.Set(10 * time.Second)
	_, err := store.Lookup("function-a")
	assertProblemCode(t, err, problem.CodeControlPlaneStale)
}

func TestStoreZeroMaxStaleRequiresConnectedWatch(t *testing.T) {
	clock := &manualClock{}
	store := newStore(t, clock, Config{MaxStale: staleWindow(0)})
	snapshot := servingSnapshot(t, "function-a", 1, 1, 1, false)
	if err := store.Apply(event(true, snapshot)); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	clock.Set(time.Hour)
	if _, err := store.Lookup("function-a"); err != nil {
		t.Fatalf("connected live-only Lookup() error = %v", err)
	}
	store.SetWatchConnected(false)
	_, err := store.Lookup("function-a")
	assertProblemCode(t, err, problem.CodeControlPlaneStale)
	store.SetWatchConnected(true)
	if _, err := store.Lookup("function-a"); err != nil {
		t.Fatalf("reconnected live-only Lookup() error = %v", err)
	}
}

func TestStoreNilMaxStaleUsesDefaultWindow(t *testing.T) {
	clock := &manualClock{}
	store := newStore(t, clock, Config{})
	snapshot := servingSnapshot(t, "function-a", 1, 1, 1, false)
	if err := store.Apply(event(true, snapshot)); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	clock.Set(DefaultMaxStale - time.Nanosecond)
	if _, err := store.Lookup(snapshot.FunctionID); err != nil {
		t.Fatalf("Lookup() before default expiry error = %v", err)
	}
	clock.Set(DefaultMaxStale)
	_, err := store.Lookup(snapshot.FunctionID)
	assertProblemCode(t, err, problem.CodeControlPlaneStale)
}

func TestStoreClockRegressionFailsClosedPermanently(t *testing.T) {
	clock := &manualClock{}
	store := newStore(t, clock, Config{})
	snapshot := servingSnapshot(t, "function-a", 1, 1, 1, false)
	if err := store.Apply(event(true, snapshot)); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	clock.Set(time.Second)
	if _, err := store.Lookup("function-a"); err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	clock.Set(time.Second - time.Nanosecond)
	_, err := store.Lookup("function-a")
	assertProblemCode(t, err, problem.CodeControlPlaneStale)
	clock.Set(2 * time.Second)
	_, err = store.Lookup("function-a")
	assertProblemCode(t, err, problem.CodeControlPlaneStale)
	if store.Status().ClockHealthy {
		t.Fatal("clock recovered after a monotonic regression")
	}
}

func TestStoreSuppressionKeepsAuthoritativeChecksumAndFencesReplacement(t *testing.T) {
	clock := &manualClock{}
	store := newStore(t, clock, Config{})
	first := servingSnapshot(t, "function-a", 1, 1, 1, true)
	if err := store.Apply(event(true, first)); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	assignment := first.Endpoints[0].Assignment
	if err := store.SuppressEndpoint(first.FunctionID, assignment); err != nil {
		t.Fatalf("SuppressEndpoint() error = %v", err)
	}
	view, err := store.Lookup(first.FunctionID)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if len(view.Endpoints) != 0 {
		t.Fatalf("visible endpoints = %d, want zero", len(view.Endpoints))
	}
	if err := view.Snapshot.Validate(); err != nil {
		t.Fatalf("authoritative snapshot checksum was changed: %v", err)
	}
	fullRefresh := servingSnapshotWithSession(t, "function-a", 1, 2, 2, 1, true)
	if err := store.Apply(event(true, fullRefresh)); err != nil {
		t.Fatalf("Apply(same-fence Full Sync) error = %v", err)
	}
	view, err = store.Lookup(first.FunctionID)
	if err != nil {
		t.Fatalf("same-fence Full Sync Lookup() error = %v", err)
	}
	if len(view.Endpoints) != 0 {
		t.Fatalf("Full Sync restored a locally suppressed exact endpoint: %+v", view.Endpoints)
	}
	replacement := servingSnapshot(t, "function-a", 1, 3, 3, true)
	if err := store.Apply(event(false, replacement)); err != nil {
		t.Fatalf("Apply(replacement) error = %v", err)
	}
	assertProblemCode(t, store.SuppressEndpoint(first.FunctionID, assignment), problem.CodeStaleAssignment)
	view, err = store.Lookup(first.FunctionID)
	if err != nil {
		t.Fatalf("replacement Lookup() error = %v", err)
	}
	if len(view.Endpoints) != 1 || view.Endpoints[0].Assignment != replacement.Endpoints[0].Assignment {
		t.Fatalf("replacement endpoint was suppressed by stale failure: %+v", view.Endpoints)
	}
}

func TestStoreRejectsChecksumAndRevisionRegressionWithoutReplacingView(t *testing.T) {
	clock := &manualClock{}
	store := newStore(t, clock, Config{})
	current := servingSnapshot(t, "function-a", 1, 1, 2, false)
	if err := store.Apply(event(true, current)); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	tampered := servingSnapshot(t, "function-a", 1, 2, 3, false)
	tampered.Function.Name = "changed"
	assertProblemCode(t, store.Apply(event(false, tampered)), problem.CodeInvalidArgument)
	regressed := servingSnapshot(t, "function-a", 1, 2, 1, false)
	assertProblemCode(t, store.Apply(event(false, regressed)), problem.CodeStaleGeneration)
	view, err := store.Lookup("function-a")
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if view.Snapshot.Function.ResourceRevision != 2 || store.Status().ServingSequence != 1 {
		t.Fatalf("rejected event replaced state: view=%+v status=%+v", view, store.Status())
	}
}

func TestStoreRejectsSameRevisionContentConflictWithoutReplacingView(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*discovery.Input)
	}{
		{
			name: "function projection",
			mutate: func(input *discovery.Input) {
				input.Function.Name = "changed"
			},
		},
		{
			name: "trigger projection",
			mutate: func(input *discovery.Input) {
				input.Trigger.Enabled = false
			},
		},
		{
			name: "route projection",
			mutate: func(input *discovery.Input) {
				input.Route.Salt = []byte("fedcba9876543210")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := &manualClock{}
			store := newStore(t, clock, Config{})
			current := servingSnapshot(t, "function-a", 1, 1, 1, false)
			if err := store.Apply(event(true, current)); err != nil {
				t.Fatalf("Apply(current) error = %v", err)
			}
			conflict := servingSnapshotWithMutation(
				t,
				"function-a",
				1,
				2,
				1,
				1,
				false,
				test.mutate,
			)
			assertProblemCode(t, store.Apply(event(false, conflict)), problem.CodeStaleGeneration)
			view, err := store.Lookup(current.FunctionID)
			if err != nil {
				t.Fatalf("Lookup() error = %v", err)
			}
			if view.Snapshot.Checksum != current.Checksum || store.Status().ServingSequence != 1 {
				t.Fatalf("same-revision conflict replaced state: view=%+v status=%+v", view, store.Status())
			}
		})
	}
}

func TestStoreFullSyncAtomicallyReplacesFunctionSet(t *testing.T) {
	clock := &manualClock{}
	store := newStore(t, clock, Config{})
	a := servingSnapshot(t, "function-a", 1, 1, 1, false)
	b := servingSnapshot(t, "function-b", 1, 1, 1, false)
	if err := store.Apply(event(true, b, a)); err != nil {
		t.Fatalf("Apply(first Full Sync) error = %v", err)
	}
	views, err := store.LookupAll()
	if err != nil {
		t.Fatalf("LookupAll() error = %v", err)
	}
	if len(views) != 2 || views[0].Snapshot.FunctionID != "function-a" || views[1].Snapshot.FunctionID != "function-b" {
		t.Fatalf("sorted Full Sync view = %+v", views)
	}
	c := servingSnapshot(t, "function-c", 1, 2, 1, false)
	if err := store.Apply(event(true, c)); err != nil {
		t.Fatalf("Apply(replacement Full Sync) error = %v", err)
	}
	views, err = store.LookupAll()
	if err != nil {
		t.Fatalf("replacement LookupAll() error = %v", err)
	}
	if len(views) != 1 || views[0].Snapshot.FunctionID != "function-c" {
		t.Fatalf("replacement Full Sync view = %+v", views)
	}
}

func TestStoreEmptyFullSyncAndDefensiveViews(t *testing.T) {
	clock := &manualClock{}
	store := newStore(t, clock, Config{})
	snapshot := servingSnapshot(t, "function-a", 1, 1, 1, true)
	if err := store.Apply(event(true, snapshot)); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	view, err := store.Lookup("function-a")
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	view.Snapshot.Route.Salt[0] ^= 0xff
	view.Endpoints[0].Address = "mutated.internal:7443"
	again, err := store.Lookup("function-a")
	if err != nil {
		t.Fatalf("second Lookup() error = %v", err)
	}
	if err := again.Snapshot.Validate(); err != nil || again.Endpoints[0].Address == view.Endpoints[0].Address {
		t.Fatalf("Lookup() exposed mutable storage: snapshot error=%v endpoint=%q", err, again.Endpoints[0].Address)
	}
	empty := Event{Full: true, DiscoveryEpoch: 1, ServingSequence: 2, Snapshots: []discovery.Snapshot{}}
	if err := store.Apply(empty); err != nil {
		t.Fatalf("Apply(empty Full Sync) error = %v", err)
	}
	views, err := store.LookupAll()
	if err != nil {
		t.Fatalf("LookupAll() after empty Full Sync error = %v", err)
	}
	if views == nil || len(views) != 0 {
		t.Fatalf("empty Full Sync views = %#v, want initialized empty slice", views)
	}
}

func TestStoreConcurrentApplyAndLookupNeverMixesSnapshotFields(t *testing.T) {
	clock := &manualClock{}
	store := newStore(t, clock, Config{})
	const updates = 100
	snapshots := make([]discovery.Snapshot, updates)
	for index := range updates {
		snapshots[index] = servingSnapshot(t, "function-a", 1, uint64(index+1), uint64(index+1), true)
	}
	if err := store.Apply(event(true, snapshots[0])); err != nil {
		t.Fatalf("Apply(initial) error = %v", err)
	}
	start := make(chan struct{})
	done := make(chan struct{})
	errorsSeen := make(chan error, 8)
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			<-start
			for {
				select {
				case <-done:
					return
				default:
				}
				view, err := store.Lookup("function-a")
				if err != nil {
					errorsSeen <- err
					return
				}
				if err := view.Snapshot.Validate(); err != nil {
					errorsSeen <- err
					return
				}
				if len(view.Endpoints) != 1 || view.Endpoints[0].Assignment != view.Snapshot.Endpoints[0].Assignment {
					errorsSeen <- errors.New("lookup mixed authoritative and local endpoint views")
					return
				}
			}
		})
	}
	close(start)
	for index := 1; index < updates; index++ {
		if err := store.Apply(event(false, snapshots[index])); err != nil {
			t.Fatalf("Apply(sequence=%d) error = %v", index+1, err)
		}
	}
	close(done)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent lookup error = %v", err)
	}
}
