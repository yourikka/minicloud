package workeragent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmexec"
	"github.com/yourikka/minicloud/internal/wasmprofile"
	"github.com/yourikka/minicloud/internal/workercache"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

func TestReplicaStateTransitionGraph(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		previous ReplicaState
		next     ReplicaState
		allowed  bool
	}{
		{name: "assigned fetches", previous: ReplicaAssigned, next: ReplicaFetching, allowed: true},
		{name: "fetch validates", previous: ReplicaFetching, next: ReplicaValidating, allowed: true},
		{name: "validation compiles", previous: ReplicaValidating, next: ReplicaCompiling, allowed: true},
		{name: "compilation readies", previous: ReplicaCompiling, next: ReplicaReady, allowed: true},
		{name: "ready drains", previous: ReplicaReady, next: ReplicaDraining, allowed: true},
		{name: "drain stops", previous: ReplicaDraining, next: ReplicaStopped, allowed: true},
		{name: "fetch fails", previous: ReplicaFetching, next: ReplicaFailed, allowed: true},
		{name: "ready is lost", previous: ReplicaReady, next: ReplicaLost, allowed: true},
		{name: "assigned cannot be ready", previous: ReplicaAssigned, next: ReplicaReady},
		{name: "ready cannot return to fetching", previous: ReplicaReady, next: ReplicaFetching},
		{name: "terminal cannot restart", previous: ReplicaStopped, next: ReplicaAssigned},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := validReplicaTransition(test.previous, test.next); got != test.allowed {
				t.Fatalf("validReplicaTransition(%q, %q) = %t, want %t", test.previous, test.next, got, test.allowed)
			}
		})
	}
}

func TestNewRejectsConcurrencyAboveRuntimeCapacity(t *testing.T) {
	clock := newManualClock(0)
	cache := newFakeCache()
	cache.limitsData.MaxConcurrent = 1
	_, err := newAgent(Config{
		Authorization: servingauth.Config{
			Worker: servingauth.WorkerProcess{WorkerID: "worker-1", BootID: "boot-1"},
			Clock:  clock.Elapsed,
		},
		MaxConcurrent: 2,
	}, cache)
	if err == nil || err.Error() != "worker agent concurrency exceeds runtime capacity" {
		t.Fatalf("newAgent() error = %v, want runtime concurrency rejection", err)
	}
}

func TestPreparePublishesReadyOnlyAfterVerifiedCompilation(t *testing.T) {
	clock := newManualClock(0)
	lease := newFakeLease()
	started := make(chan struct{})
	releaseAcquire := make(chan struct{})
	cache := newFakeCache()
	cache.acquireHook = func(
		ctx context.Context,
		spec workercache.ModuleSpec,
	) (programLease, workercache.Result, error) {
		close(started)
		select {
		case <-releaseAcquire:
			return lease, matchingLoadResult(spec), nil
		case <-ctx.Done():
			return nil, workercache.Result{}, ctx.Err()
		}
	}
	agent := newTestAgent(t, clock, cache)
	connection := testConnection(1, 10)
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}
	request := testPrepareRequest(t, connection, "assignment-1")

	prepared := make(chan prepareOutcome, 1)
	go func() {
		observation, err := agent.Prepare(context.Background(), request)
		prepared <- prepareOutcome{observation: observation, err: err}
	}()
	awaitSignal(t, started, "replica preparation")
	inventory := agent.Inventory()
	if len(inventory.Replicas) != 1 || inventory.Replicas[0].State != ReplicaFetching {
		t.Fatalf("preparation inventory = %+v, want one Fetching replica", inventory.Replicas)
	}
	close(releaseAcquire)
	outcome := <-prepared
	if outcome.err != nil || outcome.observation.State != ReplicaReady {
		t.Fatalf("Prepare() = (%+v, %v), want Ready", outcome.observation, outcome.err)
	}
	if lease.Releases() != 0 {
		t.Fatal("Ready replica released its pinned lease")
	}

	repeated, err := agent.Prepare(context.Background(), request)
	if err != nil || repeated.State != ReplicaReady {
		t.Fatalf("idempotent Prepare() = (%+v, %v), want Ready", repeated, err)
	}
	if cache.AcquireCalls() != 1 {
		t.Fatalf("cache acquire calls = %d, want 1", cache.AcquireCalls())
	}
}

func TestPrepareFailsClosedBeforeArtifactAccess(t *testing.T) {
	clock := newManualClock(0)
	cache := newFakeCache()
	agent := newTestAgent(t, clock, cache)
	connection := testConnection(1, 10)
	request := testPrepareRequest(t, connection, "assignment-1")

	_, err := agent.Prepare(context.Background(), request)
	assertProblemCode(t, err, problem.CodeStaleAssignment)
	if cache.AcquireCalls() != 0 {
		t.Fatal("Prepare() accessed the artifact before authoritative control")
	}
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}

	request.Fence.Assignment.PolicyDigest = digest.Sum([]byte("wrong-policy"))
	_, err = agent.Prepare(context.Background(), request)
	assertProblemCode(t, err, problem.CodeStaleGeneration)
	request = testPrepareRequest(t, connection, "assignment-2")
	cache.limitsData.MaxTimeout = time.Second
	_, err = agent.Prepare(context.Background(), request)
	assertProblemCode(t, err, problem.CodeCapabilityDenied)
	cache.limitsData.MaxTimeout = wasmexec.DefaultMaxTimeout
	request = testPrepareRequest(t, connection, "assignment-3")
	setMaxConcurrency(t, &request, uint32(wasmexec.DefaultMaxConcurrentPerProgram+1))
	_, err = agent.Prepare(context.Background(), request)
	assertProblemCode(t, err, problem.CodeCapabilityDenied)
	cache.limitsData.ABILimits.RawEnvelopeBytes = responseEnvelopeBytes(1024)
	request = testPrepareRequest(t, connection, "assignment-4")
	request.Policy.ResourceLimits.MaxInputBytes = 1 << 20
	policyDigest, digestErr := request.Policy.Digest()
	if digestErr != nil {
		t.Fatalf("EffectivePolicy.Digest() error = %v", digestErr)
	}
	request.Fence.Assignment.PolicyDigest = policyDigest
	_, err = agent.Prepare(context.Background(), request)
	assertProblemCode(t, err, problem.CodeCapabilityDenied)
	if cache.AcquireCalls() != 0 {
		t.Fatal("Prepare() accessed the artifact with an incompatible policy or runtime")
	}
}

func TestPrepareRetrySurvivesFirstCallerCancellation(t *testing.T) {
	clock := newManualClock(0)
	lease := newFakeLease()
	started := make(chan struct{})
	releaseAcquire := make(chan struct{})
	cache := newFakeCache()
	cache.acquireHook = func(
		ctx context.Context,
		spec workercache.ModuleSpec,
	) (programLease, workercache.Result, error) {
		close(started)
		select {
		case <-releaseAcquire:
			return lease, matchingLoadResult(spec), nil
		case <-ctx.Done():
			return nil, workercache.Result{}, ctx.Err()
		}
	}
	agent := newTestAgent(t, clock, cache)
	connection := testConnection(1, 10)
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}
	request := testPrepareRequest(t, connection, "assignment-1")

	firstContext, cancelFirst := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := agent.Prepare(firstContext, request)
		first <- err
	}()
	awaitSignal(t, started, "shared replica preparation")
	cancelFirst()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Prepare() error = %v, want context cancellation", err)
	}

	second := make(chan prepareOutcome, 1)
	go func() {
		observation, err := agent.Prepare(context.Background(), request)
		second <- prepareOutcome{observation: observation, err: err}
	}()
	close(releaseAcquire)
	outcome := <-second
	if outcome.err != nil || outcome.observation.State != ReplicaReady {
		t.Fatalf("retry Prepare() = (%+v, %v), want Ready", outcome.observation, outcome.err)
	}
	if cache.AcquireCalls() != 1 {
		t.Fatalf("cache acquire calls = %d, want one Agent-owned preparation", cache.AcquireCalls())
	}
}

func TestInvokeRequiresExactReadyAuthorization(t *testing.T) {
	clock := newManualClock(0)
	lease := newFakeLease()
	cache := newFakeCacheWithLease(lease)
	agent, connection, prepared := prepareReadyReplica(t, clock, cache, "assignment-1")
	fence := prepared.Fence
	request := testABIRequest([]byte("hello"))

	_, err := agent.Invoke(context.Background(), fence, request, time.Second)
	assertProblemCode(t, err, problem.CodeStaleAssignment)
	if lease.Guests() != 0 {
		t.Fatal("unauthorized invocation created a guest")
	}
	installTTL(t, agent, connection, fence, time.Minute)

	result, err := agent.Invoke(context.Background(), fence, request, time.Second)
	if err != nil || string(result.Response.Body) != "ok" {
		t.Fatalf("Invoke() = (%q, %v), want ok", result.Response.Body, err)
	}
	if lease.Guests() != 1 || lease.Invocations() != 2 {
		t.Fatalf("lease calls = invocations %d, guests %d; want 2 and 1", lease.Invocations(), lease.Guests())
	}

	wrongFence := fence
	wrongFence.Assignment.DeploymentGeneration++
	_, err = agent.Invoke(context.Background(), wrongFence, request, time.Second)
	assertProblemCode(t, err, problem.CodeStaleAssignment)
	if lease.Invocations() != 2 {
		t.Fatal("mismatched fence reached the compiled program")
	}
}

func TestAuthorizationExpiresWhileInvocationWaitsBeforeAcceptance(t *testing.T) {
	clock := newManualClock(0)
	lease := newFakeLease()
	lease.beforeAcceptance = make(chan struct{})
	cache := newFakeCacheWithLease(lease)
	agent, connection, prepared := prepareReadyReplica(t, clock, cache, "assignment-1")
	installTTL(t, agent, connection, prepared.Fence, time.Second)

	invoked := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(
			context.Background(),
			prepared.Fence,
			testABIRequest([]byte("queued")),
			2*time.Second,
		)
		invoked <- err
	}()
	awaitSignal(t, lease.invokeStarted, "invocation to reach the runtime queue")
	clock.Advance(time.Second)
	close(lease.beforeAcceptance)
	assertProblemCode(t, <-invoked, problem.CodeStaleAssignment)
	if lease.Guests() != 0 {
		t.Fatal("expired queued invocation created a guest")
	}
}

func TestCancelLetsAcceptedInvocationFinishThenReleasesLease(t *testing.T) {
	clock := newManualClock(0)
	lease := newFakeLease()
	lease.guestRelease = make(chan struct{})
	cache := newFakeCacheWithLease(lease)
	agent, connection, prepared := prepareReadyReplica(t, clock, cache, "assignment-1")
	installTTL(t, agent, connection, prepared.Fence, time.Minute)

	invoked := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(
			context.Background(),
			prepared.Fence,
			testABIRequest([]byte("running")),
			2*time.Second,
		)
		invoked <- err
	}()
	awaitSignal(t, lease.guestStarted, "accepted guest")
	if err := agent.Cancel(CancelRequest{Connection: connection, Fence: prepared.Fence}); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if got := replicaState(t, agent, "assignment-1"); got != ReplicaDraining {
		t.Fatalf("state after Cancel = %q, want Draining", got)
	}
	if lease.Releases() != 0 {
		t.Fatal("Cancel() released a lease used by an accepted invocation")
	}
	_, err := agent.Invoke(
		context.Background(),
		prepared.Fence,
		testABIRequest([]byte("late")),
		time.Second,
	)
	assertProblemCode(t, err, problem.CodeNoReadyReplica)

	close(lease.guestRelease)
	if err := <-invoked; err != nil {
		t.Fatalf("accepted Invoke() error after Cancel = %v", err)
	}
	awaitCondition(t, func() bool {
		return replicaState(t, agent, "assignment-1") == ReplicaStopped && lease.Releases() == 1
	}, "cancelled replica to stop")
	if lease.ReleasedWhileActive() {
		t.Fatal("Agent released the lease while Invoke was active")
	}
}

func TestHotReplicaDoesNotReserveWorkerSlotsWhileLocallyQueued(t *testing.T) {
	clock := newManualClock(0)
	hotLease := newFakeLease()
	hotLease.guestRelease = make(chan struct{})
	otherLease := newFakeLease()
	cache := newFakeCache()
	cache.acquireHook = func(
		_ context.Context,
		spec workercache.ModuleSpec,
	) (programLease, workercache.Result, error) {
		lease := programLease(otherLease)
		if spec.ArtifactDigest == digest.Sum([]byte("wasm-hot")) {
			lease = hotLease
		}
		return lease, matchingLoadResult(spec), nil
	}
	agent := newTestAgentWithBounds(t, clock, cache, 2, 4)
	connection := testConnection(1, 10)
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}
	hot := testPrepareRequest(t, connection, "hot")
	other := testPrepareRequest(t, connection, "other")
	setMaxConcurrency(t, &hot, 1)
	setMaxConcurrency(t, &other, 1)
	for _, request := range []PrepareRequest{hot, other} {
		if _, err := agent.Prepare(context.Background(), request); err != nil {
			t.Fatalf("Prepare(%s) error = %v", request.Fence.Assignment.AssignmentID, err)
		}
		installTTL(t, agent, connection, request.Fence, time.Minute)
	}

	firstHot := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(context.Background(), hot.Fence, testABIRequest([]byte("hot-1")), time.Second)
		firstHot <- err
	}()
	awaitSignal(t, hotLease.guestStarted, "first hot guest")
	secondHot := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(context.Background(), hot.Fence, testABIRequest([]byte("hot-2")), time.Second)
		secondHot <- err
	}()
	awaitCondition(t, func() bool { return queuedForReplica(agent, "hot") == 1 }, "second hot call to queue")

	if _, err := agent.Invoke(
		context.Background(),
		other.Fence,
		testABIRequest([]byte("other")),
		200*time.Millisecond,
	); err != nil {
		t.Fatalf("other replica Invoke() error while hot replica queued = %v", err)
	}
	close(hotLease.guestRelease)
	if err := <-firstHot; err != nil {
		t.Fatalf("first hot Invoke() error = %v", err)
	}
	if err := <-secondHot; err != nil {
		t.Fatalf("second hot Invoke() error = %v", err)
	}
}

func TestProgramQueuedReplicaDoesNotReserveWorkerSlot(t *testing.T) {
	clock := newManualClock(0)
	runtimeLimiter := newLimiter(1, 4)
	firstLease := newFakeLease()
	firstLease.runtimeLimiter = runtimeLimiter
	firstLease.guestRelease = make(chan struct{})
	queuedLease := newFakeLease()
	queuedLease.runtimeLimiter = runtimeLimiter
	otherLease := newFakeLease()
	cache := newFakeCache()
	cache.limitsData.MaxConcurrent = 2
	cache.limitsData.MaxConcurrentPerProgram = 1
	cache.acquireHook = func(
		_ context.Context,
		spec workercache.ModuleSpec,
	) (programLease, workercache.Result, error) {
		lease := programLease(otherLease)
		switch spec.ArtifactDigest {
		case digest.Sum([]byte("wasm-first")):
			lease = firstLease
		case digest.Sum([]byte("wasm-queued")):
			lease = queuedLease
		}
		return lease, matchingLoadResult(spec), nil
	}
	agent := newTestAgentWithBounds(t, clock, cache, 2, 4)
	connection := testConnection(1, 10)
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}
	first := testPrepareRequest(t, connection, "first")
	queued := testPrepareRequest(t, connection, "queued")
	other := testPrepareRequest(t, connection, "other")
	for _, request := range []*PrepareRequest{&first, &queued, &other} {
		setMaxConcurrency(t, request, 1)
		if _, err := agent.Prepare(context.Background(), *request); err != nil {
			t.Fatalf("Prepare(%s) error = %v", request.Fence.Assignment.AssignmentID, err)
		}
		installTTL(t, agent, connection, request.Fence, time.Minute)
	}

	firstCall := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(context.Background(), first.Fence, testABIRequest([]byte("first")), time.Second)
		firstCall <- err
	}()
	awaitSignal(t, firstLease.guestStarted, "first shared-program guest")
	queuedCall := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(context.Background(), queued.Fence, testABIRequest([]byte("queued")), time.Second)
		queuedCall <- err
	}()
	awaitCondition(t, func() bool { return len(runtimeLimiter.queue) == 1 }, "shared Program queue")
	if occupied := len(agent.limiter.slots); occupied != 1 {
		t.Fatalf("Program-queued call occupied %d Worker slots, want only the active guest", occupied)
	}

	if _, err := agent.Invoke(
		context.Background(),
		other.Fence,
		testABIRequest([]byte("other")),
		200*time.Millisecond,
	); err != nil {
		t.Fatalf("other Program Invoke() error while shared Program queued = %v", err)
	}
	close(firstLease.guestRelease)
	if err := <-firstCall; err != nil {
		t.Fatalf("first Invoke() error = %v", err)
	}
	if err := <-queuedCall; err != nil {
		t.Fatalf("queued Invoke() error = %v", err)
	}
}

func TestCancelWakesInvocationWaitingForWorkerAdmission(t *testing.T) {
	clock := newManualClock(0)
	blockerLease := newFakeLease()
	blockerLease.guestRelease = make(chan struct{})
	targetLease := newFakeLease()
	cache := newFakeCache()
	blockerDigest := digest.Sum([]byte("wasm-blocker"))
	cache.acquireHook = func(
		_ context.Context,
		spec workercache.ModuleSpec,
	) (programLease, workercache.Result, error) {
		lease := programLease(targetLease)
		if spec.ArtifactDigest == blockerDigest {
			lease = blockerLease
		}
		return lease, matchingLoadResult(spec), nil
	}
	agent := newTestAgentWithBounds(t, clock, cache, 1, 2)
	connection := testConnection(1, 10)
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}
	blocker := testPrepareRequest(t, connection, "blocker")
	target := testPrepareRequest(t, connection, "target")
	setMaxConcurrency(t, &blocker, 1)
	setMaxConcurrency(t, &target, 1)
	for _, request := range []PrepareRequest{blocker, target} {
		if _, err := agent.Prepare(context.Background(), request); err != nil {
			t.Fatalf("Prepare(%s) error = %v", request.Fence.Assignment.AssignmentID, err)
		}
		installTTL(t, agent, connection, request.Fence, time.Minute)
	}

	blockingCall := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(context.Background(), blocker.Fence, testABIRequest([]byte("block")), time.Second)
		blockingCall <- err
	}()
	awaitSignal(t, blockerLease.guestStarted, "blocking guest")
	targetCall := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(context.Background(), target.Fence, testABIRequest([]byte("target")), time.Second)
		targetCall <- err
	}()
	awaitCondition(t, func() bool { return len(agent.limiter.queue) == 1 }, "target call to wait for Worker admission")
	if err := agent.Cancel(CancelRequest{Connection: connection, Fence: target.Fence}); err != nil {
		t.Fatalf("Cancel(target) error = %v", err)
	}
	assertProblemCode(t, <-targetCall, problem.CodeStaleAssignment)
	if targetLease.Guests() != 0 {
		t.Fatal("cancelled Worker-queued call created a guest")
	}
	close(blockerLease.guestRelease)
	if err := <-blockingCall; err != nil {
		t.Fatalf("blocking Invoke() error = %v", err)
	}
}

func TestCancelWakesInvocationWaitingForRuntimeAdmission(t *testing.T) {
	clock := newManualClock(0)
	runtimeLimiter := newLimiter(1, 2)
	blockerLease := newFakeLease()
	blockerLease.runtimeLimiter = runtimeLimiter
	blockerLease.guestRelease = make(chan struct{})
	targetLease := newFakeLease()
	targetLease.runtimeLimiter = runtimeLimiter
	cache := newFakeCache()
	cache.limitsData.MaxConcurrent = 2
	cache.limitsData.MaxConcurrentPerProgram = 1
	blockerDigest := digest.Sum([]byte("wasm-blocker"))
	cache.acquireHook = func(
		_ context.Context,
		spec workercache.ModuleSpec,
	) (programLease, workercache.Result, error) {
		lease := programLease(targetLease)
		if spec.ArtifactDigest == blockerDigest {
			lease = blockerLease
		}
		return lease, matchingLoadResult(spec), nil
	}
	agent := newTestAgentWithBounds(t, clock, cache, 2, 2)
	connection := testConnection(1, 10)
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}
	blocker := testPrepareRequest(t, connection, "blocker")
	target := testPrepareRequest(t, connection, "target")
	for _, request := range []*PrepareRequest{&blocker, &target} {
		setMaxConcurrency(t, request, 1)
		if _, err := agent.Prepare(context.Background(), *request); err != nil {
			t.Fatalf("Prepare(%s) error = %v", request.Fence.Assignment.AssignmentID, err)
		}
		installTTL(t, agent, connection, request.Fence, time.Minute)
	}

	blockingCall := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(context.Background(), blocker.Fence, testABIRequest([]byte("block")), time.Second)
		blockingCall <- err
	}()
	awaitSignal(t, blockerLease.guestStarted, "runtime-blocking guest")
	targetCall := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(context.Background(), target.Fence, testABIRequest([]byte("target")), time.Second)
		targetCall <- err
	}()
	awaitCondition(t, func() bool { return len(runtimeLimiter.queue) == 1 }, "target runtime admission queue")
	if occupied := len(agent.limiter.slots); occupied != 1 {
		t.Fatalf("runtime-queued call occupied %d Worker slots, want 1", occupied)
	}
	if err := agent.Cancel(CancelRequest{Connection: connection, Fence: target.Fence}); err != nil {
		t.Fatalf("Cancel(target) error = %v", err)
	}
	assertProblemCode(t, <-targetCall, problem.CodeStaleAssignment)
	if targetLease.Guests() != 0 {
		t.Fatal("cancelled runtime-queued call created a guest")
	}
	awaitCondition(t, func() bool {
		return replicaState(t, agent, "target") == ReplicaStopped && targetLease.Releases() == 1
	}, "runtime-queued Replica cancellation")

	close(blockerLease.guestRelease)
	if err := <-blockingCall; err != nil {
		t.Fatalf("blocking Invoke() error = %v", err)
	}
}

func TestInvocationTimeoutIncludesWorkerAdmissionQueue(t *testing.T) {
	clock := newManualClock(0)
	blockerLease := newFakeLease()
	blockerLease.guestRelease = make(chan struct{})
	targetLease := newFakeLease()
	cache := newFakeCache()
	blockerDigest := digest.Sum([]byte("wasm-blocker"))
	cache.acquireHook = func(
		_ context.Context,
		spec workercache.ModuleSpec,
	) (programLease, workercache.Result, error) {
		lease := programLease(targetLease)
		if spec.ArtifactDigest == blockerDigest {
			lease = blockerLease
		}
		return lease, matchingLoadResult(spec), nil
	}
	agent := newTestAgentWithBounds(t, clock, cache, 1, 2)
	connection := testConnection(1, 10)
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}
	blocker := testPrepareRequest(t, connection, "blocker")
	target := testPrepareRequest(t, connection, "target")
	setMaxConcurrency(t, &blocker, 1)
	setMaxConcurrency(t, &target, 1)
	for _, request := range []PrepareRequest{blocker, target} {
		if _, err := agent.Prepare(context.Background(), request); err != nil {
			t.Fatalf("Prepare(%s) error = %v", request.Fence.Assignment.AssignmentID, err)
		}
		installTTL(t, agent, connection, request.Fence, time.Minute)
	}

	blockingCall := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(context.Background(), blocker.Fence, testABIRequest([]byte("block")), time.Second)
		blockingCall <- err
	}()
	awaitSignal(t, blockerLease.guestStarted, "blocking guest")
	started := time.Now()
	_, err := agent.Invoke(
		context.Background(),
		target.Fence,
		testABIRequest([]byte("target")),
		20*time.Millisecond,
	)
	assertProblemCode(t, err, problem.CodeFunctionTimeout)
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("queued timeout took %s, want bounded by invocation budget", elapsed)
	}
	if targetLease.Guests() != 0 {
		t.Fatal("timed out Worker-queued call created a guest")
	}
	close(blockerLease.guestRelease)
	if err := <-blockingCall; err != nil {
		t.Fatalf("blocking Invoke() error = %v", err)
	}
}

func TestInvocationDeadlineExpiresWhileAcceptanceWaitsForAgentLock(t *testing.T) {
	clock := newManualClock(0)
	lease := newFakeLease()
	lease.beforeAcceptance = make(chan struct{})
	lease.acceptanceStarted = make(chan struct{})
	cache := newFakeCacheWithLease(lease)
	agent, connection, prepared := prepareReadyReplica(t, clock, cache, "assignment-1")
	installTTL(t, agent, connection, prepared.Fence, time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	invoked := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(ctx, prepared.Fence, testABIRequest([]byte("hello")), time.Second)
		invoked <- err
	}()
	awaitSignal(t, lease.invokeStarted, "compiled program invocation")

	agent.mu.Lock()
	locked := true
	defer func() {
		if locked {
			agent.mu.Unlock()
		}
	}()
	close(lease.beforeAcceptance)
	awaitSignal(t, lease.acceptanceStarted, "invocation acceptance")
	awaitSignal(t, ctx.Done(), "invocation deadline")
	agent.mu.Unlock()
	locked = false

	assertProblemCode(t, <-invoked, problem.CodeFunctionTimeout)
	if lease.Guests() != 0 {
		t.Fatal("expired invocation created a guest after waiting for the Agent lock")
	}
	observation := agent.Inventory().Replicas[0]
	if observation.ActiveInvocations != 0 {
		t.Fatalf("active invocations = %d, want 0", observation.ActiveInvocations)
	}
}

func TestSessionAdvanceLosesOldReplicas(t *testing.T) {
	clock := newManualClock(0)
	lease := newFakeLease()
	cache := newFakeCacheWithLease(lease)
	agent, _, prepared := prepareReadyReplica(t, clock, cache, "assignment-1")

	if err := agent.AcceptControl(testConnection(2, 11)); err != nil {
		t.Fatalf("AcceptControl(new session) error = %v", err)
	}
	if got := replicaState(t, agent, "assignment-1"); got != ReplicaLost {
		t.Fatalf("old replica state = %q, want Lost", got)
	}
	if lease.Releases() != 1 {
		t.Fatalf("old replica lease releases = %d, want 1", lease.Releases())
	}
	_, err := agent.Invoke(
		context.Background(),
		prepared.Fence,
		testABIRequest([]byte("stale")),
		time.Second,
	)
	assertProblemCode(t, err, problem.CodeNoReadyReplica)
}

func TestTerminalAcknowledgementReclaimsBoundedReplicaCapacity(t *testing.T) {
	clock := newManualClock(0)
	cache := newFakeCache()
	cache.acquireHook = func(
		_ context.Context,
		spec workercache.ModuleSpec,
	) (programLease, workercache.Result, error) {
		return newFakeLease(), matchingLoadResult(spec), nil
	}
	agent, err := newAgent(Config{
		Authorization: servingauth.Config{
			Worker: servingauth.WorkerProcess{WorkerID: "worker-1", BootID: "boot-1"},
			Clock:  clock.Elapsed,
		},
		MaxAssignments: 1,
	}, cache)
	if err != nil {
		t.Fatalf("newAgent() error = %v", err)
	}
	t.Cleanup(func() {
		if err := agent.Close(context.Background()); err != nil {
			t.Errorf("Agent.Close() error = %v", err)
		}
	})
	connection := testConnection(1, 10)
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}
	first := testPrepareRequest(t, connection, "assignment-1")
	if _, err := agent.Prepare(context.Background(), first); err != nil {
		t.Fatalf("Prepare(first) error = %v", err)
	}
	if err := agent.Cancel(CancelRequest{Connection: connection, Fence: first.Fence}); err != nil {
		t.Fatalf("Cancel(first) error = %v", err)
	}
	if err := agent.AcknowledgeTerminal(testConnection(1, 11), "assignment-1"); err == nil {
		t.Fatal("AcknowledgeTerminal() accepted a stale control connection")
	}
	if err := agent.AcknowledgeTerminal(connection, "assignment-1"); err != nil {
		t.Fatalf("AcknowledgeTerminal() error = %v", err)
	}
	second := testPrepareRequest(t, connection, "assignment-2")
	if _, err := agent.Prepare(context.Background(), second); err != nil {
		t.Fatalf("Prepare(second) after acknowledgement error = %v", err)
	}
}

func TestCancelInterruptsReplicaPreparation(t *testing.T) {
	clock := newManualClock(0)
	started := make(chan struct{})
	cache := newFakeCache()
	cache.acquireHook = func(
		ctx context.Context,
		_ workercache.ModuleSpec,
	) (programLease, workercache.Result, error) {
		close(started)
		<-ctx.Done()
		return nil, workercache.Result{}, ctx.Err()
	}
	agent := newTestAgent(t, clock, cache)
	connection := testConnection(1, 10)
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}
	request := testPrepareRequest(t, connection, "assignment-1")
	prepared := make(chan error, 1)
	go func() {
		_, err := agent.Prepare(context.Background(), request)
		prepared <- err
	}()
	awaitSignal(t, started, "artifact preparation")
	if err := agent.Cancel(CancelRequest{Connection: connection, Fence: request.Fence}); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	assertProblemCode(t, <-prepared, problem.CodeStaleAssignment)
	awaitCondition(t, func() bool {
		return replicaState(t, agent, "assignment-1") == ReplicaStopped
	}, "cancelled preparation to stop")
}

func TestControlDisconnectCancelsOnlyUnreadyPreparation(t *testing.T) {
	clock := newManualClock(0)
	started := make(chan struct{})
	cache := newFakeCache()
	cache.acquireHook = func(
		ctx context.Context,
		_ workercache.ModuleSpec,
	) (programLease, workercache.Result, error) {
		close(started)
		<-ctx.Done()
		return nil, workercache.Result{}, ctx.Err()
	}
	agent := newTestAgent(t, clock, cache)
	connection := testConnection(1, 10)
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}
	request := testPrepareRequest(t, connection, "assignment-1")
	prepared := make(chan error, 1)
	go func() {
		_, err := agent.Prepare(context.Background(), request)
		prepared <- err
	}()
	awaitSignal(t, started, "artifact preparation")
	agent.DisconnectControl(connection)
	assertProblemCode(t, <-prepared, problem.CodeStaleAssignment)
	if got := replicaState(t, agent, "assignment-1"); got != ReplicaLost {
		t.Fatalf("disconnected preparation state = %q, want Lost", got)
	}
}

func TestCloseIsRetryableWhileAcceptedInvocationDrains(t *testing.T) {
	clock := newManualClock(0)
	lease := newFakeLease()
	lease.guestRelease = make(chan struct{})
	cache := newFakeCacheWithLease(lease)
	agent, connection, prepared := prepareReadyReplica(t, clock, cache, "assignment-1")
	installTTL(t, agent, connection, prepared.Fence, time.Minute)

	invoked := make(chan error, 1)
	go func() {
		_, err := agent.Invoke(
			context.Background(),
			prepared.Fence,
			testABIRequest([]byte("running")),
			2*time.Second,
		)
		invoked <- err
	}()
	awaitSignal(t, lease.guestStarted, "accepted guest")
	closeContext, cancelClose := context.WithCancel(context.Background())
	cancelClose()
	if err := agent.Close(closeContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() error = %v, want context cancellation", err)
	}
	if cache.CloseCalls() != 0 {
		t.Fatal("Agent closed the cache before Replica drain")
	}
	close(lease.guestRelease)
	if err := <-invoked; err != nil {
		t.Fatalf("accepted Invoke() error during Close = %v", err)
	}
	if err := agent.Close(context.Background()); err != nil {
		t.Fatalf("Close() retry error = %v", err)
	}
	if cache.CloseCalls() != 1 || lease.Releases() != 1 {
		t.Fatalf("Close() calls = cache %d, lease %d; want 1 and 1", cache.CloseCalls(), lease.Releases())
	}
	inventory := agent.Inventory()
	if !inventory.Closed || inventory.Replicas[0].State != ReplicaStopped {
		t.Fatalf("closed inventory = %+v", inventory)
	}
}

func newTestAgent(t *testing.T, clock *manualClock, cache compiledCache) *Agent {
	t.Helper()
	return newTestAgentWithBounds(t, clock, cache, 0, 0)
}

func newTestAgentWithBounds(
	t *testing.T,
	clock *manualClock,
	cache compiledCache,
	maxConcurrent int,
	maxQueued int,
) *Agent {
	t.Helper()
	agent, err := newAgent(Config{
		Authorization: servingauth.Config{
			Worker: servingauth.WorkerProcess{WorkerID: "worker-1", BootID: "boot-1"},
			Clock:  clock.Elapsed,
		},
		MaxConcurrent: maxConcurrent,
		MaxQueued:     maxQueued,
	}, cache)
	if err != nil {
		t.Fatalf("newAgent() error = %v", err)
	}
	t.Cleanup(func() {
		if err := agent.Close(context.Background()); err != nil {
			t.Errorf("Agent.Close() error = %v", err)
		}
	})
	return agent
}

func setMaxConcurrency(t *testing.T, request *PrepareRequest, maximum uint32) {
	t.Helper()
	request.Policy.MaxConcurrency = maximum
	policyDigest, err := request.Policy.Digest()
	if err != nil {
		t.Fatalf("EffectivePolicy.Digest() error = %v", err)
	}
	request.Fence.Assignment.PolicyDigest = policyDigest
}

func prepareReadyReplica(
	t *testing.T,
	clock *manualClock,
	cache compiledCache,
	assignmentID string,
) (*Agent, servingauth.ControlConnection, PrepareRequest) {
	t.Helper()
	agent := newTestAgent(t, clock, cache)
	connection := testConnection(1, 10)
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}
	request := testPrepareRequest(t, connection, assignmentID)
	observation, err := agent.Prepare(context.Background(), request)
	if err != nil || observation.State != ReplicaReady {
		t.Fatalf("Prepare() = (%+v, %v), want Ready", observation, err)
	}
	return agent, connection, request
}

func testPrepareRequest(
	t *testing.T,
	connection servingauth.ControlConnection,
	assignmentID string,
) PrepareRequest {
	t.Helper()
	moduleBytes := []byte("wasm-" + assignmentID)
	policy := model.EffectivePolicy{
		VersionID:             "version-1",
		AdmissionEpoch:        3,
		DeploymentGeneration:  4,
		ArtifactDigest:        digest.Sum(moduleBytes),
		ArtifactSize:          int64(len(moduleBytes)),
		ABI:                   model.ABIWASICommandV1,
		HostAPIProfile:        model.HostAPIProfileNone,
		RuntimeFeatureProfile: wasmprofile.FeatureProfile,
		ResourceLimits: model.ResourceLimits{
			Timeout:        2 * time.Second,
			MemoryMiB:      128,
			MaxInputBytes:  1024,
			MaxOutputBytes: 1024,
			MaxLogBytes:    1024,
		},
		MaxConcurrency:      2,
		GrantedCapabilities: []model.CapabilityRequest{},
	}
	policyDigest, err := policy.Digest()
	if err != nil {
		t.Fatalf("EffectivePolicy.Digest() error = %v", err)
	}
	identity := servingauth.AssignmentIdentity{
		Worker: servingauth.WorkerSession{
			WorkerID:     "worker-1",
			BootID:       "boot-1",
			SessionEpoch: connection.SessionEpoch,
		},
		AssignmentID:         assignmentID,
		VersionID:            "version-1",
		AdmissionEpoch:       3,
		DeploymentGeneration: 4,
		PolicyDigest:         policyDigest,
		Mode:                 servingauth.ModeNormal,
	}
	return PrepareRequest{
		Connection: connection,
		Fence: servingauth.InvocationFence{
			Assignment:     identity,
			DiscoveryEpoch: connection.DiscoveryEpoch,
		},
		Module: workercache.ModuleSpec{
			ArtifactDigest:        digest.Sum(moduleBytes),
			ArtifactSize:          int64(len(moduleBytes)),
			ABI:                   policy.ABI,
			HostAPIProfile:        policy.HostAPIProfile,
			RuntimeFeatureProfile: policy.RuntimeFeatureProfile,
		},
		Policy: policy,
	}
}

func testConnection(sessionEpoch, discoveryEpoch uint64) servingauth.ControlConnection {
	return servingauth.ControlConnection{
		ConnectionID:   "control-1",
		SessionEpoch:   sessionEpoch,
		DiscoveryEpoch: discoveryEpoch,
	}
}

func installTTL(
	t *testing.T,
	agent *Agent,
	connection servingauth.ControlConnection,
	fence servingauth.InvocationFence,
	ttl time.Duration,
) {
	t.Helper()
	err := agent.InstallAuthorization(connection, servingauth.Authorization{
		Fence:    fence,
		Lifetime: servingauth.LifetimeTTL,
		TTL:      ttl,
	})
	if err != nil {
		t.Fatalf("InstallAuthorization() error = %v", err)
	}
}

func testABIRequest(body []byte) abi.Request {
	return abi.Request{
		SpecVersion:  abi.Version,
		InvocationID: "inv-worker-agent",
		Method:       "POST",
		Path:         "/invoke",
		Query:        abi.Query{},
		Headers:      abi.RequestHeaders{},
		Body:         body,
		Trigger:      abi.Trigger{Type: "http", ID: "trigger-1"},
	}
}

func matchingLoadResult(spec workercache.ModuleSpec) workercache.Result {
	return workercache.Result{
		Key: workercache.Key{
			ArtifactDigest:        spec.ArtifactDigest,
			ArtifactSize:          spec.ArtifactSize,
			ABI:                   spec.ABI,
			HostAPIProfile:        spec.HostAPIProfile,
			RuntimeFeatureProfile: spec.RuntimeFeatureProfile,
			MemoryLimitMiB:        128,
		},
	}
}

func replicaState(t *testing.T, agent *Agent, assignmentID string) ReplicaState {
	t.Helper()
	for _, observation := range agent.Inventory().Replicas {
		if observation.Identity.AssignmentID == assignmentID {
			return observation.State
		}
	}
	t.Fatalf("assignment %q not found", assignmentID)
	return ""
}

func queuedForReplica(agent *Agent, assignmentID string) int {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	current := agent.replicas[assignmentID]
	if current == nil {
		return 0
	}
	return len(current.limiter.queue)
}

func assertProblemCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	var classifiedError *problem.Error
	if !errors.As(err, &classifiedError) || classifiedError.Code != want {
		t.Fatalf("error = %v, want problem code %q", err, want)
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func awaitCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

type prepareOutcome struct {
	observation Observation
	err         error
}

type manualClock struct {
	mu      sync.Mutex
	elapsed time.Duration
}

func newManualClock(elapsed time.Duration) *manualClock {
	return &manualClock{elapsed: elapsed}
}

func (c *manualClock) Elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.elapsed
}

func (c *manualClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.elapsed += delta
	c.mu.Unlock()
}

type fakeCache struct {
	mu          sync.Mutex
	profileData wasmprofile.Profile
	limitsData  wasmexec.ExecutionLimits
	acquireHook func(context.Context, workercache.ModuleSpec) (programLease, workercache.Result, error)
	acquires    int
	closes      int
}

func newFakeCache() *fakeCache {
	effective, err := (abi.Limits{}).Effective()
	if err != nil {
		panic(err)
	}
	return &fakeCache{
		profileData: wasmprofile.Profile{
			Engine:         wasmprofile.EngineCompiler,
			MemoryLimitMiB: 128,
		},
		limitsData: wasmexec.ExecutionLimits{
			MaxTimeout:              wasmexec.DefaultMaxTimeout,
			MaxConcurrent:           wasmexec.DefaultMaxConcurrent,
			MaxConcurrentPerProgram: wasmexec.DefaultMaxConcurrentPerProgram,
			ABILimits:               effective,
			MaxLogBytes:             wasmexec.DefaultMaxLogBytes,
			MaxLogLineBytes:         wasmexec.DefaultMaxLogLineBytes,
		},
	}
}

func newFakeCacheWithLease(lease programLease) *fakeCache {
	cache := newFakeCache()
	cache.acquireHook = func(
		_ context.Context,
		spec workercache.ModuleSpec,
	) (programLease, workercache.Result, error) {
		return lease, matchingLoadResult(spec), nil
	}
	return cache
}

func (c *fakeCache) acquire(
	ctx context.Context,
	spec workercache.ModuleSpec,
) (programLease, workercache.Result, error) {
	c.mu.Lock()
	c.acquires++
	hook := c.acquireHook
	c.mu.Unlock()
	if hook == nil {
		return nil, workercache.Result{}, errors.New("fake cache has no acquire result")
	}
	return hook(ctx, spec)
}

func (c *fakeCache) close(context.Context) error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	return nil
}

func (c *fakeCache) profile() wasmprofile.Profile {
	return c.profileData
}

func (c *fakeCache) executionLimits() wasmexec.ExecutionLimits {
	return c.limitsData
}

func (c *fakeCache) snapshot() workercache.Stats {
	return workercache.Stats{}
}

func (c *fakeCache) AcquireCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acquires
}

func (c *fakeCache) CloseCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

type fakeLease struct {
	mu                  sync.Mutex
	invokeOnce          sync.Once
	guestOnce           sync.Once
	acceptanceOnce      sync.Once
	invokeStarted       chan struct{}
	guestStarted        chan struct{}
	beforeAcceptance    chan struct{}
	acceptanceStarted   chan struct{}
	guestRelease        chan struct{}
	runtimeLimiter      *limiter
	invocations         int
	guests              int
	releases            int
	active              int
	releasedWhileActive bool
}

func newFakeLease() *fakeLease {
	return &fakeLease{
		invokeStarted: make(chan struct{}),
		guestStarted:  make(chan struct{}),
	}
}

func (l *fakeLease) InvokeWithAcceptance(
	ctx context.Context,
	request workercache.InvocationRequest,
	acceptance wasmexec.InvocationAcceptance,
) (wasmexec.Result, error) {
	l.mu.Lock()
	l.invocations++
	l.active++
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.active--
		l.mu.Unlock()
	}()
	l.invokeOnce.Do(func() { close(l.invokeStarted) })
	if acceptance.Check == nil {
		return wasmexec.Result{}, errors.New("missing acceptance hook")
	}
	if l.runtimeLimiter != nil {
		if err := l.runtimeLimiter.acquire(ctx, acceptance.Stop, acceptance.AlsoStop); err != nil {
			if errors.Is(err, errStopping) {
				return wasmexec.Result{}, wasmexec.ErrAcceptanceStopped
			}
			return wasmexec.Result{}, err
		}
		defer l.runtimeLimiter.release()
	}
	if acceptance.Acquire != nil {
		release, err := acceptance.Acquire()
		if err != nil {
			return wasmexec.Result{}, err
		}
		if release == nil {
			return wasmexec.Result{}, errors.New("missing admission release")
		}
		defer release()
	}
	if l.beforeAcceptance != nil {
		select {
		case <-l.beforeAcceptance:
		case <-acceptance.Stop:
			return wasmexec.Result{}, wasmexec.ErrAcceptanceStopped
		case <-acceptance.AlsoStop:
			return wasmexec.Result{}, wasmexec.ErrAcceptanceStopped
		case <-ctx.Done():
			return wasmexec.Result{}, ctx.Err()
		}
	}
	if fakeAcceptanceStopped(acceptance) {
		return wasmexec.Result{}, wasmexec.ErrAcceptanceStopped
	}
	if l.acceptanceStarted != nil {
		l.acceptanceOnce.Do(func() { close(l.acceptanceStarted) })
	}
	if err := acceptance.Check(); err != nil {
		return wasmexec.Result{}, err
	}
	l.mu.Lock()
	l.guests++
	l.mu.Unlock()
	l.guestOnce.Do(func() { close(l.guestStarted) })
	if l.guestRelease != nil {
		select {
		case <-l.guestRelease:
		case <-ctx.Done():
			return wasmexec.Result{}, ctx.Err()
		}
	}
	return wasmexec.Result{
		Response: abi.Response{
			SpecVersion: abi.Version,
			Status:      200,
			Headers:     abi.ResponseHeaders{},
			Body:        []byte("ok"),
		},
	}, nil
}

func fakeAcceptanceStopped(acceptance wasmexec.InvocationAcceptance) bool {
	select {
	case <-acceptance.Stop:
		return true
	case <-acceptance.AlsoStop:
		return true
	default:
		return false
	}
}

func (l *fakeLease) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active != 0 {
		l.releasedWhileActive = true
	}
	l.releases++
}

func (l *fakeLease) Invocations() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.invocations
}

func (l *fakeLease) Guests() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.guests
}

func (l *fakeLease) Releases() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releases
}

func (l *fakeLease) ReleasedWhileActive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releasedWhileActive
}
