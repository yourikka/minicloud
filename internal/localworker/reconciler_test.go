package localworker

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
	"github.com/yourikka/minicloud/internal/localserving"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmprofile"
	"github.com/yourikka/minicloud/internal/workeragent"
	"github.com/yourikka/minicloud/internal/workercache"
)

func TestReconcilerPreparesAuthorizesAndPublishesReadyCandidate(t *testing.T) {
	t.Parallel()
	fixture := newReconcilerFixture(t)
	if err := fixture.reconciler.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	candidates, err := fixture.reconciler.Candidates(t.Context(), fixture.state, 101)
	if err != nil {
		t.Fatalf("Candidates() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].Assignment != fixture.assignment.Identity() ||
		candidates[0].Authorization == nil ||
		candidates[0].Authorization.Lifetime != servingauth.LifetimeLiveOnly ||
		candidates[0].Authorization.Fence.DiscoveryEpoch != 101 ||
		candidates[0].Address != "worker-a.internal:7443" {
		t.Fatalf("Candidates() = %+v", candidates)
	}
	prepareCalls, installCalls, cancelCalls, acknowledgeCalls := fixture.agent.Counts()
	if prepareCalls != 1 || installCalls != 1 || cancelCalls != 0 || acknowledgeCalls != 0 {
		t.Fatalf(
			"agent calls prepare=%d install=%d cancel=%d acknowledge=%d",
			prepareCalls,
			installCalls,
			cancelCalls,
			acknowledgeCalls,
		)
	}

	candidates[0].Worker.Labels["zone"] = "mutated"
	candidates[0].Authorization.Fence.DiscoveryEpoch = 999
	again, err := fixture.reconciler.Candidates(t.Context(), fixture.state, 101)
	if err != nil {
		t.Fatalf("second Candidates() error = %v", err)
	}
	if again[0].Worker.Labels["zone"] != "test" || again[0].Authorization.Fence.DiscoveryEpoch != 101 {
		t.Fatalf("Candidates() exposed internal state: %+v", again)
	}
	if err := fixture.reconciler.Reconcile(t.Context()); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	prepareCalls, installCalls, _, _ = fixture.agent.Counts()
	if prepareCalls != 1 || installCalls != 2 {
		t.Fatalf("repeat calls prepare=%d install=%d", prepareCalls, installCalls)
	}
}

func TestReconcilerCandidatesPublishThroughLocalServingFullSync(t *testing.T) {
	t.Parallel()
	fixture := newReconcilerFixture(t)
	if err := fixture.reconciler.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
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
	synchronizer, err := localserving.New(localserving.Config{
		States:     fixture.states,
		Candidates: fixture.reconciler,
		Publisher:  publisher,
		Store:      store,
		Now:        func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("localserving.New() error = %v", err)
	}
	if err := synchronizer.FullSync(t.Context()); err != nil {
		t.Fatalf("FullSync() error = %v", err)
	}
	view, err := store.Lookup(fixture.state.Function.ID)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if view.Snapshot.DiscoveryEpoch != 101 || view.Snapshot.ServingSequence != 1 ||
		len(view.Snapshot.Endpoints) != 1 ||
		view.Snapshot.Endpoints[0].Assignment != fixture.assignment.Identity() {
		t.Fatalf("published snapshot = %+v", view.Snapshot)
	}
}

func TestReconcilerCancelsThenAcknowledgesCommittedCancellation(t *testing.T) {
	t.Parallel()
	fixture := newReconcilerFixture(t)
	if err := fixture.reconciler.Reconcile(t.Context()); err != nil {
		t.Fatalf("assigned Reconcile() error = %v", err)
	}
	fixture.assignments.Set([]controlplane.AssignmentRecord{{
		FunctionID: fixture.assignment.FunctionID, Placement: fixture.assignment.Placement,
		DesiredState: controlplane.AssignmentCancelled, ResourceRevision: 2,
	}})
	if err := fixture.reconciler.Reconcile(t.Context()); err != nil {
		t.Fatalf("cancelled Reconcile() error = %v", err)
	}
	candidates, err := fixture.reconciler.Candidates(t.Context(), fixture.state, 101)
	if err != nil {
		t.Fatalf("Candidates() error = %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("cancelled Candidates() = %+v", candidates)
	}
	_, _, cancelCalls, acknowledgeCalls := fixture.agent.Counts()
	if cancelCalls != 1 || acknowledgeCalls != 1 || len(fixture.agent.Inventory().Replicas) != 0 {
		t.Fatalf(
			"cancel calls=%d acknowledge calls=%d inventory=%+v",
			cancelCalls,
			acknowledgeCalls,
			fixture.agent.Inventory(),
		)
	}
}

func TestReconcilerRejectsPolicyMismatchBeforePrepare(t *testing.T) {
	t.Parallel()
	fixture := newReconcilerFixture(t)
	fixture.state.Deployment.EffectivePolicyDigest = digest.Sum([]byte("mismatched-policy"))
	fixture.states.Set([]controlplane.ServingState{fixture.state})
	if err := fixture.reconciler.Reconcile(t.Context()); err == nil {
		t.Fatal("Reconcile() accepted a mismatched effective policy")
	}
	prepareCalls, installCalls, _, _ := fixture.agent.Counts()
	if prepareCalls != 0 || installCalls != 0 {
		t.Fatalf("mismatch calls prepare=%d install=%d", prepareCalls, installCalls)
	}
	candidates, err := fixture.reconciler.Candidates(t.Context(), fixture.state, 101)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("Candidates() = %+v, %v", candidates, err)
	}
}

func TestReconcilerJoinsExistingPreparationButDoesNotReuseTerminalReplica(t *testing.T) {
	t.Parallel()
	t.Run("joins existing preparation", func(t *testing.T) {
		t.Parallel()
		fixture := newReconcilerFixture(t)
		fixture.agent.SetReplica(workeragent.Observation{
			Identity: fixture.assignment.Identity(), State: workeragent.ReplicaFetching,
		})
		if err := fixture.reconciler.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
		candidates, err := fixture.reconciler.Candidates(t.Context(), fixture.state, 101)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("Candidates() = %+v, %v", candidates, err)
		}
		prepareCalls, _, _, _ := fixture.agent.Counts()
		if prepareCalls != 1 {
			t.Fatalf("Prepare() calls = %d, want 1", prepareCalls)
		}
	})
	t.Run("rejects terminal assigned replica", func(t *testing.T) {
		t.Parallel()
		fixture := newReconcilerFixture(t)
		fixture.agent.SetReplica(workeragent.Observation{
			Identity: fixture.assignment.Identity(), State: workeragent.ReplicaFailed,
		})
		if err := fixture.reconciler.Reconcile(t.Context()); err == nil {
			t.Fatal("Reconcile() reused a terminal Assignment identity")
		}
		prepareCalls, installCalls, _, _ := fixture.agent.Counts()
		if prepareCalls != 0 || installCalls != 0 {
			t.Fatalf("terminal calls prepare=%d install=%d", prepareCalls, installCalls)
		}
	})
}

func TestReconcilerSerializesControlAndAllowsConcurrentCandidateReads(t *testing.T) {
	t.Parallel()
	fixture := newReconcilerFixture(t)
	const callers = 32
	errorsByCall := make(chan error, callers*2)
	var wait sync.WaitGroup
	for range callers {
		wait.Go(func() {
			errorsByCall <- fixture.reconciler.Reconcile(t.Context())
		})
		wait.Go(func() {
			_, err := fixture.reconciler.Candidates(t.Context(), fixture.state, 101)
			errorsByCall <- err
		})
	}
	wait.Wait()
	close(errorsByCall)
	for err := range errorsByCall {
		if err != nil {
			t.Fatalf("concurrent operation error = %v", err)
		}
	}
	prepareCalls, installCalls, _, _ := fixture.agent.Counts()
	if prepareCalls != 1 || installCalls != callers {
		t.Fatalf("concurrent calls prepare=%d install=%d", prepareCalls, installCalls)
	}
}

func TestReconcilerFailsClosedForEpochAndMissingDependencies(t *testing.T) {
	t.Parallel()
	if _, err := NewReconciler(ReconcilerConfig{}); err == nil {
		t.Fatal("NewReconciler() accepted missing dependencies")
	}
	fixture := newReconcilerFixture(t)
	if err := fixture.reconciler.Reconcile(nil); err == nil {
		t.Fatal("Reconcile() accepted nil context")
	}
	if err := fixture.reconciler.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	candidates, err := fixture.reconciler.Candidates(t.Context(), fixture.state, 102)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("mismatched epoch Candidates() = %+v, %v", candidates, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := fixture.reconciler.Candidates(ctx, fixture.state, 101); !errors.Is(err, context.Canceled) {
		t.Fatalf("Candidates() error = %v, want cancellation", err)
	}
}

type reconcilerFixture struct {
	reconciler  *Reconciler
	agent       *fakeAgent
	assignments *assignmentSource
	states      *servingStateSource
	assignment  controlplane.AssignmentRecord
	state       controlplane.ServingState
}

func newReconcilerFixture(t *testing.T) reconcilerFixture {
	t.Helper()
	worker := validWorkerSnapshot()
	state, assignment := validReconcileState(t, worker.Session)
	assignments := &assignmentSource{records: []controlplane.AssignmentRecord{assignment}}
	states := &servingStateSource{states: []controlplane.ServingState{state}}
	agent := newFakeAgent()
	reconciler, err := NewReconciler(ReconcilerConfig{
		Assignments: assignments,
		States:      states,
		Workers:     workerSource{snapshot: worker},
		Agent:       agent,
		Connection: servingauth.ControlConnection{
			ConnectionID: "control-a", SessionEpoch: worker.Session.SessionEpoch, DiscoveryEpoch: 101,
		},
		Address: "worker-a.internal:7443",
	})
	if err != nil {
		t.Fatalf("NewReconciler() error = %v", err)
	}
	return reconcilerFixture{
		reconciler: reconciler, agent: agent, assignments: assignments, states: states,
		assignment: assignment, state: state,
	}
}

func validReconcileState(
	t *testing.T,
	session servingauth.WorkerSession,
) (controlplane.ServingState, controlplane.AssignmentRecord) {
	t.Helper()
	artifactDigest := digest.Sum([]byte("artifact-a"))
	version := model.Version{
		VersionID: "version-a", ArtifactDigest: artifactDigest, ArtifactSize: 1024,
		ABI: model.ABIWASICommandV1, HostAPIProfile: model.HostAPIProfileNone,
		RuntimeFeatureProfile: wasmprofile.FeatureProfile, AdmissionEpoch: 1,
		ResourceRequest: model.ResourceRequest{
			Timeout: time.Second, MemoryMiB: 64, MaxConcurrency: 1,
			MaxInputBytes: 1024, MaxOutputBytes: 1024,
		},
	}
	limits := model.ResourceLimits{
		Timeout: time.Second, MemoryMiB: 64, MaxInputBytes: 1024,
		MaxOutputBytes: 1024, MaxLogBytes: 1024,
	}
	policy := model.EffectivePolicy{
		VersionID: version.VersionID, AdmissionEpoch: version.AdmissionEpoch,
		DeploymentGeneration: 1, ArtifactDigest: artifactDigest, ArtifactSize: version.ArtifactSize,
		ABI: version.ABI, HostAPIProfile: version.HostAPIProfile,
		RuntimeFeatureProfile: version.RuntimeFeatureProfile, ResourceLimits: limits,
		MaxConcurrency:      version.ResourceRequest.MaxConcurrency,
		GrantedCapabilities: []model.CapabilityRequest{},
	}
	policyDigest, err := policy.Digest()
	if err != nil {
		t.Fatalf("EffectivePolicy.Digest() error = %v", err)
	}
	deployment := model.Deployment{
		VersionID: version.VersionID, Generation: 1, ResourceLimits: limits,
		GrantedCapabilities: []model.CapabilityRequest{}, EffectivePolicyDigest: policyDigest,
	}
	route := model.Route{
		Metadata:   model.Metadata{ID: "route-a", ResourceRevision: 5},
		FunctionID: "function-a", RouteRevision: 1,
		Targets: []model.RouteTarget{{
			VersionID: version.VersionID, AdmissionEpoch: version.AdmissionEpoch,
			DeploymentGeneration: deployment.Generation, EffectivePolicyDigest: policyDigest,
			WeightBasisPoints: model.TotalRouteWeightBasisPoints,
		}},
		Affinity: model.AffinityRequestID, HashVersion: model.HashVersionSHA256BPSV1,
		SaltID: "salt-a", Salt: []byte("0123456789abcdef"), Enabled: true,
	}
	state := controlplane.ServingState{
		Function: model.Function{
			Metadata: model.Metadata{ID: "function-a", ResourceRevision: 3},
			Name:     "function-a", Lifecycle: model.FunctionActive,
		},
		Trigger: controlplane.HTTPTrigger{
			Metadata:   model.Metadata{ID: "trigger-a", ResourceRevision: 4},
			FunctionID: "function-a", Enabled: true, AuthPolicy: controlplane.AuthPolicyPublic,
		},
		Route: &route, Version: &version, Deployment: &deployment,
	}
	assignment := controlplane.AssignmentRecord{
		FunctionID: "function-a",
		Placement: scheduler.Assignment{
			CommandID: "command-a", AssignmentID: "assignment-a", Worker: session,
			VersionID: version.VersionID, ArtifactDigest: version.ArtifactDigest,
			ArtifactSize: version.ArtifactSize, ABI: version.ABI, HostAPI: version.HostAPIProfile,
			FeatureProfile: version.RuntimeFeatureProfile, MemoryMiB: limits.MemoryMiB, RequiredSlots: 1,
			AdmissionEpoch: version.AdmissionEpoch, DeploymentGeneration: deployment.Generation,
			PolicyDigest: policyDigest,
		},
		DesiredState: controlplane.AssignmentAssigned,
	}
	return state, assignment
}

func validWorkerSnapshot() scheduler.WorkerSnapshot {
	return scheduler.WorkerSnapshot{
		Session: servingauth.WorkerSession{WorkerID: "worker-a", BootID: "boot-a", SessionEpoch: 1},
		Runtime: scheduler.RuntimeProfile{
			Name: wasmprofile.RuntimeName, Version: wasmprofile.RuntimeVersion,
			Engine: wasmprofile.EngineCompiler, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
			ABI: model.ABIWASICommandV1, HostAPI: model.HostAPIProfileNone,
			FeatureProfile: wasmprofile.FeatureProfile, MemoryMiB: 64,
		},
		Intent: scheduler.IntentSchedulable, State: scheduler.SessionReady,
		Drain: scheduler.DrainNotDraining, Capacity: scheduler.Capacity{MemoryMiB: 512, Slots: 8},
		Labels: map[string]string{"zone": "test"},
		Cache: scheduler.CacheHints{
			Artifacts: map[digest.SHA256]struct{}{},
			Compiled:  map[workercache.Key]struct{}{},
		},
	}
}

type assignmentSource struct {
	mu      sync.Mutex
	records []controlplane.AssignmentRecord
}

func (s *assignmentSource) ListAssignments(context.Context) ([]controlplane.AssignmentRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.records), nil
}

func (s *assignmentSource) Set(records []controlplane.AssignmentRecord) {
	s.mu.Lock()
	s.records = slices.Clone(records)
	s.mu.Unlock()
}

type servingStateSource struct {
	mu     sync.Mutex
	states []controlplane.ServingState
}

func (s *servingStateSource) ListServingStates(context.Context) ([]controlplane.ServingState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.states), nil
}

func (s *servingStateSource) Set(states []controlplane.ServingState) {
	s.mu.Lock()
	s.states = slices.Clone(states)
	s.mu.Unlock()
}

type workerSource struct {
	snapshot scheduler.WorkerSnapshot
}

func (s workerSource) WorkerSnapshot(context.Context) (scheduler.WorkerSnapshot, error) {
	return cloneWorkerSnapshot(s.snapshot), nil
}

type fakeAgent struct {
	mu sync.Mutex

	control          servingauth.ControlConnection
	replicas         map[string]workeragent.Observation
	authorizations   map[string]servingauth.Authorization
	prepareCalls     int
	installCalls     int
	cancelCalls      int
	acknowledgeCalls int
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{
		replicas:       make(map[string]workeragent.Observation),
		authorizations: make(map[string]servingauth.Authorization),
	}
}

func (a *fakeAgent) AcceptControl(connection servingauth.ControlConnection) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.control = connection
	return nil
}

func (a *fakeAgent) Prepare(
	_ context.Context,
	request workeragent.PrepareRequest,
) (workeragent.Observation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prepareCalls++
	if observation, exists := a.replicas[request.Fence.Assignment.AssignmentID]; exists {
		observation.State = workeragent.ReplicaReady
		a.replicas[request.Fence.Assignment.AssignmentID] = observation
		return observation, nil
	}
	observation := workeragent.Observation{
		Identity: request.Fence.Assignment, Module: request.Module, State: workeragent.ReplicaReady,
	}
	a.replicas[request.Fence.Assignment.AssignmentID] = observation
	return observation, nil
}

func (a *fakeAgent) InstallAuthorization(
	_ servingauth.ControlConnection,
	authorization servingauth.Authorization,
) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.installCalls++
	a.authorizations[authorization.Fence.Assignment.AssignmentID] = authorization
	return nil
}

func (a *fakeAgent) Cancel(request workeragent.CancelRequest) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelCalls++
	observation, exists := a.replicas[request.Fence.Assignment.AssignmentID]
	if !exists {
		return nil
	}
	observation.State = workeragent.ReplicaStopped
	a.replicas[request.Fence.Assignment.AssignmentID] = observation
	delete(a.authorizations, request.Fence.Assignment.AssignmentID)
	return nil
}

func (a *fakeAgent) AcknowledgeTerminal(_ servingauth.ControlConnection, assignmentID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acknowledgeCalls++
	delete(a.replicas, assignmentID)
	return nil
}

func (a *fakeAgent) Inventory() workeragent.Inventory {
	a.mu.Lock()
	defer a.mu.Unlock()
	replicas := make([]workeragent.Observation, 0, len(a.replicas))
	for _, observation := range a.replicas {
		replicas = append(replicas, observation)
	}
	return workeragent.Inventory{Replicas: replicas}
}

func (a *fakeAgent) SetReplica(observation workeragent.Observation) {
	a.mu.Lock()
	a.replicas[observation.Identity.AssignmentID] = observation
	a.mu.Unlock()
}

func (a *fakeAgent) Counts() (prepare, install, cancel, acknowledge int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prepareCalls, a.installCalls, a.cancelCalls, a.acknowledgeCalls
}

var _ Agent = (*fakeAgent)(nil)
