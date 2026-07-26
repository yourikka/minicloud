package localserving

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/gatewaydiscovery"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

func TestFullSyncProjectsConsistentStateAndEndpoint(t *testing.T) {
	t.Parallel()
	state := servingState("function-a", true)
	generatedAt := time.Date(2026, time.July, 26, 9, 8, 7, 654321000, time.UTC)
	syncer, store, publisher := newTestSynchronizer(t, Config{
		States: stateSourceFunc(func(context.Context) ([]controlplane.ServingState, error) {
			return []controlplane.ServingState{state}, nil
		}),
		Candidates: candidateSourceFunc(func(
			_ context.Context,
			got controlplane.ServingState,
			epoch uint64,
		) ([]discovery.EndpointCandidate, error) {
			if got.Function.ID != state.Function.ID || epoch != 101 {
				t.Fatalf("Candidates() function = %q, epoch = %d", got.Function.ID, epoch)
			}
			candidate := readyCandidate(got, epoch)
			got.Route.Salt[0] = 'x'
			*got.Trigger.TokenVerifierDigest = digest.Sum([]byte("candidate-mutated"))
			return []discovery.EndpointCandidate{candidate}, nil
		}),
		Now: func() time.Time { return generatedAt },
	})

	if err := syncer.FullSync(t.Context()); err != nil {
		t.Fatalf("FullSync() error = %v", err)
	}
	if epoch, sequence := publisher.Position(); epoch != 101 || sequence != 1 {
		t.Fatalf("Publisher.Position() = (%d, %d)", epoch, sequence)
	}
	view, err := store.Lookup(state.Function.ID)
	if err != nil {
		t.Fatalf("Store.Lookup() error = %v", err)
	}
	if view.Snapshot.GeneratedAt != generatedAt || view.Snapshot.ServingSequence != 1 ||
		view.Snapshot.Function.ResourceRevision != state.Function.ResourceRevision ||
		view.Snapshot.Trigger.ResourceRevision != state.Trigger.ResourceRevision ||
		view.Snapshot.Route.ResourceRevision != state.Route.ResourceRevision {
		t.Fatalf("projected snapshot = %+v", view.Snapshot)
	}
	if len(view.Endpoints) != 1 || view.Endpoints[0].Assignment.AssignmentID != "assignment-function-a" ||
		view.Endpoints[0].Assignment.PolicyDigest != state.Route.Targets[0].EffectivePolicyDigest {
		t.Fatalf("projected endpoints = %+v", view.Endpoints)
	}
	if string(view.Snapshot.Route.Salt) != "0123456789abcdef" ||
		*view.Snapshot.Trigger.TokenVerifierDigest == *state.Trigger.TokenVerifierDigest {
		t.Fatalf("candidate source mutated the fixed projection: %+v", view.Snapshot)
	}

	view.Snapshot.Route.Salt[0] = 'x'
	*view.Snapshot.Trigger.TokenVerifierDigest = digest.Sum([]byte("view-mutated"))
	again, err := store.Lookup(state.Function.ID)
	if err != nil {
		t.Fatalf("second Store.Lookup() error = %v", err)
	}
	if string(again.Snapshot.Route.Salt) != "0123456789abcdef" ||
		*again.Snapshot.Trigger.TokenVerifierDigest == *view.Snapshot.Trigger.TokenVerifierDigest {
		t.Fatalf("snapshot retained caller-owned projection memory: %+v", again.Snapshot)
	}
}

func TestFullSyncReplacesFunctionSetAndAdvancesSequence(t *testing.T) {
	t.Parallel()
	states := []controlplane.ServingState{
		servingState("function-a", false),
		servingState("function-b", false),
	}
	var statesMu sync.Mutex
	syncer, store, publisher := newTestSynchronizer(t, Config{
		States: stateSourceFunc(func(context.Context) ([]controlplane.ServingState, error) {
			statesMu.Lock()
			defer statesMu.Unlock()
			return slices.Clone(states), nil
		}),
	})
	if err := syncer.FullSync(t.Context()); err != nil {
		t.Fatalf("first FullSync() error = %v", err)
	}
	statesMu.Lock()
	states = []controlplane.ServingState{servingState("function-b", false)}
	statesMu.Unlock()
	if err := syncer.FullSync(t.Context()); err != nil {
		t.Fatalf("second FullSync() error = %v", err)
	}
	views, err := store.LookupAll()
	if err != nil {
		t.Fatalf("Store.LookupAll() error = %v", err)
	}
	if len(views) != 1 || views[0].Snapshot.FunctionID != "function-b" ||
		views[0].Snapshot.Route.Present || views[0].Snapshot.ServingSequence != 2 {
		t.Fatalf("Store.LookupAll() = %+v", views)
	}
	if _, err := store.Lookup("function-a"); err == nil {
		t.Fatal("removed function remained in the Full Sync view")
	}
	if epoch, sequence := publisher.Position(); epoch != 101 || sequence != 2 {
		t.Fatalf("Publisher.Position() = (%d, %d)", epoch, sequence)
	}
}

func TestFullSyncBuildFailureDoesNotAdvanceOrReplaceView(t *testing.T) {
	t.Parallel()
	state := servingState("function-a", false)
	syncer, store, publisher := newTestSynchronizer(t, Config{
		States: stateSourceFunc(func(context.Context) ([]controlplane.ServingState, error) {
			return []controlplane.ServingState{state}, nil
		}),
	})
	if err := syncer.FullSync(t.Context()); err != nil {
		t.Fatalf("first FullSync() error = %v", err)
	}
	state.Function.Name = "INVALID_NAME"
	if err := syncer.FullSync(t.Context()); err == nil {
		t.Fatal("FullSync() accepted an invalid projected Function")
	}
	if epoch, sequence := publisher.Position(); epoch != 101 || sequence != 1 {
		t.Fatalf("failed build advanced position to (%d, %d)", epoch, sequence)
	}
	view, err := store.Lookup("function-a")
	if err != nil {
		t.Fatalf("Store.Lookup() after failed build error = %v", err)
	}
	if view.Snapshot.Function.Name != "function-a" || view.Snapshot.ServingSequence != 1 {
		t.Fatalf("failed build replaced the serving view: %+v", view.Snapshot)
	}
}

func TestFullSyncCandidateFailureDoesNotAdvancePublisher(t *testing.T) {
	t.Parallel()
	want := errors.New("worker inventory unavailable")
	syncer, _, publisher := newTestSynchronizer(t, Config{
		States: stateSourceFunc(func(context.Context) ([]controlplane.ServingState, error) {
			return []controlplane.ServingState{servingState("function-a", true)}, nil
		}),
		Candidates: candidateSourceFunc(func(
			context.Context,
			controlplane.ServingState,
			uint64,
		) ([]discovery.EndpointCandidate, error) {
			return nil, want
		}),
	})
	if err := syncer.FullSync(t.Context()); !errors.Is(err, want) {
		t.Fatalf("FullSync() error = %v, want wrapped candidate error", err)
	}
	if _, sequence := publisher.Position(); sequence != 0 {
		t.Fatalf("candidate failure advanced sequence to %d", sequence)
	}
}

func TestFullSyncSerializesConcurrentPublications(t *testing.T) {
	t.Parallel()
	syncer, store, publisher := newTestSynchronizer(t, Config{
		States: stateSourceFunc(func(context.Context) ([]controlplane.ServingState, error) {
			return []controlplane.ServingState{servingState("function-a", false)}, nil
		}),
	})
	const publications = 8
	errorsByCall := make(chan error, publications)
	var wait sync.WaitGroup
	for range publications {
		wait.Go(func() {
			errorsByCall <- syncer.FullSync(t.Context())
		})
	}
	wait.Wait()
	close(errorsByCall)
	for err := range errorsByCall {
		if err != nil {
			t.Fatalf("concurrent FullSync() error = %v", err)
		}
	}
	if _, sequence := publisher.Position(); sequence != publications {
		t.Fatalf("Publisher sequence = %d, want %d", sequence, publications)
	}
	status := store.Status()
	if status.ServingSequence != publications || !status.FullSynced || status.NeedsFullSync {
		t.Fatalf("Store.Status() = %+v", status)
	}
}

func TestSynchronizerRejectsMissingDependenciesAndContext(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); err == nil {
		t.Fatal("New() accepted missing dependencies")
	}
	syncer, _, _ := newTestSynchronizer(t, Config{
		States: stateSourceFunc(func(context.Context) ([]controlplane.ServingState, error) {
			return []controlplane.ServingState{}, nil
		}),
	})
	if err := syncer.FullSync(nil); err == nil {
		t.Fatal("FullSync() accepted a nil context")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := syncer.FullSync(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("FullSync() error = %v, want context cancellation", err)
	}
}

func newTestSynchronizer(
	t *testing.T,
	config Config,
) (*Synchronizer, *gatewaydiscovery.Store, *discovery.Publisher) {
	t.Helper()
	builder, err := discovery.New(discovery.Config{})
	if err != nil {
		t.Fatalf("discovery.New() error = %v", err)
	}
	publisher, err := discovery.NewPublisher(101, builder)
	if err != nil {
		t.Fatalf("discovery.NewPublisher() error = %v", err)
	}
	store, err := gatewaydiscovery.New(gatewaydiscovery.Config{})
	if err != nil {
		t.Fatalf("gatewaydiscovery.New() error = %v", err)
	}
	config.Publisher = publisher
	config.Store = store
	if config.Now == nil {
		config.Now = func() time.Time {
			return time.Date(2026, time.July, 26, 10, 0, 0, 0, time.UTC)
		}
	}
	syncer, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return syncer, store, publisher
}

func servingState(functionID string, routePresent bool) controlplane.ServingState {
	createdAt := time.Date(2026, time.July, 26, 8, 0, 0, 0, time.UTC)
	metadata := func(id string, revision uint64) model.Metadata {
		return model.Metadata{
			ID: id, Namespace: model.DefaultNamespace, CreatedAt: createdAt, UpdatedAt: createdAt,
			CreatedRaftIndex: 1, ResourceRevision: revision,
		}
	}
	verifier := digest.Sum([]byte("verifier-" + functionID))
	state := controlplane.ServingState{
		Function: model.Function{
			Metadata: metadata(functionID, 3), Name: functionID,
			Labels: map[string]string{}, Lifecycle: model.FunctionActive,
		},
		Trigger: controlplane.HTTPTrigger{
			Metadata: metadata("trigger-"+functionID, 4), FunctionID: functionID,
			Enabled: true, AuthPolicy: controlplane.AuthPolicyToken, TokenVerifierDigest: &verifier,
		},
	}
	if !routePresent {
		return state
	}
	policy := digest.Sum([]byte("policy-" + functionID))
	state.Route = &model.Route{
		Metadata: metadata("route-"+functionID, 5), FunctionID: functionID, RouteRevision: 1,
		Targets: []model.RouteTarget{{
			VersionID: "version-" + functionID, AdmissionEpoch: 1, DeploymentGeneration: 1,
			EffectivePolicyDigest: policy, WeightBasisPoints: model.TotalRouteWeightBasisPoints,
		}},
		Affinity: model.AffinityRequestID, HashVersion: model.HashVersionSHA256BPSV1,
		SaltID: "salt-" + functionID, Salt: []byte("0123456789abcdef"), Enabled: true,
	}
	return state
}

func readyCandidate(state controlplane.ServingState, epoch uint64) discovery.EndpointCandidate {
	target := state.Route.Targets[0]
	session := servingauth.WorkerSession{WorkerID: "worker-a", BootID: "boot-a", SessionEpoch: 1}
	assignment := servingauth.AssignmentIdentity{
		Worker: session, AssignmentID: "assignment-" + state.Function.ID, VersionID: target.VersionID,
		AdmissionEpoch: target.AdmissionEpoch, DeploymentGeneration: target.DeploymentGeneration,
		PolicyDigest: target.EffectivePolicyDigest, Mode: servingauth.ModeNormal,
	}
	return discovery.EndpointCandidate{
		Assignment: assignment, DesiredState: discovery.AssignmentAssigned,
		Worker: scheduler.WorkerSnapshot{
			Session: session,
			Runtime: scheduler.RuntimeProfile{
				Name: wasmprofile.RuntimeName, Version: wasmprofile.RuntimeVersion,
				Engine: wasmprofile.EngineCompiler, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
				ABI: model.ABIWASICommandV1, HostAPI: model.HostAPIProfileNone,
				FeatureProfile: wasmprofile.FeatureProfile, MemoryMiB: 128,
			},
			Intent: scheduler.IntentSchedulable, State: scheduler.SessionReady,
			Drain: scheduler.DrainNotDraining, Capacity: scheduler.Capacity{MemoryMiB: 512, Slots: 8},
			Labels: map[string]string{"zone": "test"},
		},
		ReplicaReady: true,
		Authorization: &servingauth.Authorization{
			Fence:    servingauth.InvocationFence{Assignment: assignment, DiscoveryEpoch: epoch},
			Lifetime: servingauth.LifetimeTTL, TTL: time.Minute,
		},
		Address: "worker-a.internal:7443",
	}
}

type stateSourceFunc func(context.Context) ([]controlplane.ServingState, error)

func (f stateSourceFunc) ListServingStates(ctx context.Context) ([]controlplane.ServingState, error) {
	return f(ctx)
}

type candidateSourceFunc func(
	context.Context,
	controlplane.ServingState,
	uint64,
) ([]discovery.EndpointCandidate, error)

func (f candidateSourceFunc) Candidates(
	ctx context.Context,
	state controlplane.ServingState,
	epoch uint64,
) ([]discovery.EndpointCandidate, error) {
	return f(ctx, state, epoch)
}
