package servingauth

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/problem"
)

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config Config
	}{
		{
			name:   "invalid worker id",
			config: Config{Worker: WorkerProcess{WorkerID: "", BootID: "boot-1"}},
		},
		{
			name:   "invalid boot id",
			config: Config{Worker: WorkerProcess{WorkerID: "worker-1", BootID: "bad boot"}},
		},
		{
			name: "negative ttl maximum",
			config: Config{
				Worker: WorkerProcess{WorkerID: "worker-1", BootID: "boot-1"},
				MaxTTL: -1,
			},
		},
		{
			name: "ttl above hard maximum",
			config: Config{
				Worker: WorkerProcess{WorkerID: "worker-1", BootID: "boot-1"},
				MaxTTL: HardMaxTTL + 1,
			},
		},
		{
			name: "negative authorization capacity",
			config: Config{
				Worker:            WorkerProcess{WorkerID: "worker-1", BootID: "boot-1"},
				MaxAuthorizations: -1,
			},
		},
		{
			name: "authorization capacity above hard maximum",
			config: Config{
				Worker:            WorkerProcess{WorkerID: "worker-1", BootID: "boot-1"},
				MaxAuthorizations: HardMaxAuthorizations + 1,
			},
		},
		{
			name: "negative monotonic clock",
			config: Config{
				Worker: WorkerProcess{WorkerID: "worker-1", BootID: "boot-1"},
				Clock:  newManualClock(-1).Elapsed,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			gate, err := New(test.config)
			if err == nil || gate != nil {
				t.Fatalf("New() = (%v, %v), want nil gate and error", gate, err)
			}
		})
	}
}

func TestControlSessionAdvanceFencesOldWorkerSession(t *testing.T) {
	t.Parallel()
	clock := newManualClock(0)
	gate := newTestGate(t, clock, Config{})
	firstConnection := controlConnection("control-1", 1, 10)
	if err := gate.AcceptAuthoritativeControl(firstConnection); err != nil {
		t.Fatalf("AcceptAuthoritativeControl() error = %v", err)
	}
	firstAuthorization := ttlAuthorization(assignment(1, "assignment-1"), 10, time.Minute)
	if err := gate.Install(firstConnection, firstAuthorization); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if err := gate.AuthorizeSync(firstAuthorization.Fence); err != nil {
		t.Fatalf("AuthorizeSync() error = %v", err)
	}

	gate.DisconnectControl(firstConnection)
	if err := gate.AuthorizeSync(firstAuthorization.Fence); err != nil {
		t.Fatalf("TTL AuthorizeSync() after disconnect error = %v", err)
	}
	invalidHigherEpoch := controlConnection("control-invalid", 1, 11)
	assertProblemCode(t, gate.AcceptAuthoritativeControl(invalidHigherEpoch), problem.CodeStaleAssignment)
	assertProblemCode(t, authorizeError(gate, firstAuthorization.Fence), problem.CodeStaleAssignment)
	if snapshot := gate.Snapshot(); snapshot.HighestDiscoveryEpoch != 11 ||
		snapshot.TrackedAssignments != 1 || snapshot.StoredAuthorizations != 0 || snapshot.ControlConnected {
		t.Fatalf("higher discovery epoch did not fence old state: %+v", snapshot)
	}
	secondConnection := controlConnection("control-2", 2, 11)
	if err := gate.AcceptAuthoritativeControl(secondConnection); err != nil {
		t.Fatalf("higher AcceptAuthoritativeControl() error = %v", err)
	}
	assertProblemCode(t, authorizeError(gate, firstAuthorization.Fence), problem.CodeStaleAssignment)

	for name, connection := range map[string]ControlConnection{
		"lower session":   controlConnection("control-3", 1, 11),
		"lower discovery": controlConnection("control-3", 3, 10),
		"same session":    controlConnection("control-3", 2, 11),
	} {
		t.Run(name, func(t *testing.T) {
			wantCode := problem.CodeStaleAssignment
			if name == "lower discovery" {
				wantCode = problem.CodeStaleGeneration
			}
			assertProblemCode(t, gate.AcceptAuthoritativeControl(connection), wantCode)
		})
	}
	if err := gate.AcceptAuthoritativeControl(secondConnection); err != nil {
		t.Fatalf("idempotent AcceptAuthoritativeControl() error = %v", err)
	}
	snapshot := gate.Snapshot()
	if snapshot.HighestDiscoveryEpoch != 11 || snapshot.CurrentSessionEpoch != 2 ||
		!snapshot.ControlConnected || snapshot.TrackedAssignments != 0 ||
		snapshot.StoredAuthorizations != 0 || !snapshot.ClockHealthy {
		t.Fatalf("Snapshot() = %+v", snapshot)
	}
}

func TestTTLAuthorizationRefreshAndExactExpiry(t *testing.T) {
	t.Parallel()
	clock := newManualClock(100 * time.Second)
	gate := newTestGate(t, clock, Config{})
	connection := controlConnection("control-1", 1, 10)
	authorization := ttlAuthorization(assignment(1, "assignment-1"), 10, time.Minute)
	mustAuthenticateAndInstall(t, gate, connection, authorization)

	if err := gate.AuthorizeSync(authorization.Fence); err != nil {
		t.Fatalf("AuthorizeSync() error = %v", err)
	}
	clock.Advance(30 * time.Second)
	if err := gate.Install(connection, authorization); err != nil {
		t.Fatalf("refresh Install() error = %v", err)
	}
	clock.Advance(time.Minute - time.Nanosecond)
	if err := gate.AuthorizeSync(authorization.Fence); err != nil {
		t.Fatalf("AuthorizeSync() just before expiry error = %v", err)
	}
	clock.Advance(time.Nanosecond)
	assertProblemCode(t, authorizeError(gate, authorization.Fence), problem.CodeStaleAssignment)
	snapshot := gate.Snapshot()
	if snapshot.TrackedAssignments != 1 || snapshot.StoredAuthorizations != 0 {
		t.Fatalf("expired authorization state = %+v", snapshot)
	}
}

func TestLiveOnlyAuthorizationFollowsExactControlConnection(t *testing.T) {
	t.Parallel()
	gate := newTestGate(t, newManualClock(0), Config{})
	connection := controlConnection("control-1", 1, 10)
	authorization := liveAuthorization(assignment(1, "assignment-1"), 10)
	mustAuthenticateAndInstall(t, gate, connection, authorization)

	gate.DisconnectControl(controlConnection("another-control", 1, 10))
	if err := gate.AuthorizeSync(authorization.Fence); err != nil {
		t.Fatalf("AuthorizeSync() after unrelated disconnect error = %v", err)
	}
	gate.DisconnectControl(connection)
	assertProblemCode(t, authorizeError(gate, authorization.Fence), problem.CodeStaleAssignment)

	secondConnection := controlConnection("control-2", 2, 10)
	if err := gate.AcceptAuthoritativeControl(secondConnection); err != nil {
		t.Fatalf("reconnect AcceptAuthoritativeControl() error = %v", err)
	}
	secondAuthorization := liveAuthorization(assignment(2, "assignment-2"), 10)
	if err := gate.Install(secondConnection, secondAuthorization); err != nil {
		t.Fatalf("reconnect Install() error = %v", err)
	}
	if err := gate.AuthorizeSync(secondAuthorization.Fence); err != nil {
		t.Fatalf("reconnect AuthorizeSync() error = %v", err)
	}
}

func TestLateDisconnectCannotCloseReplacementControlConnection(t *testing.T) {
	t.Parallel()
	gate := newTestGate(t, newManualClock(0), Config{})
	firstConnection := controlConnection("shared-control-id", 1, 10)
	firstAuthorization := liveAuthorization(assignment(1, "assignment-1"), 10)
	mustAuthenticateAndInstall(t, gate, firstConnection, firstAuthorization)

	secondConnection := controlConnection("shared-control-id", 2, 11)
	if err := gate.AcceptAuthoritativeControl(secondConnection); err != nil {
		t.Fatalf("replacement AcceptAuthoritativeControl() error = %v", err)
	}
	secondAuthorization := liveAuthorization(assignment(2, "assignment-2"), 11)
	if err := gate.Install(secondConnection, secondAuthorization); err != nil {
		t.Fatalf("replacement Install() error = %v", err)
	}

	assertProblemCode(t, gate.Install(firstConnection, secondAuthorization), problem.CodeStaleAssignment)
	gate.DisconnectControl(firstConnection)
	if err := gate.AuthorizeSync(secondAuthorization.Fence); err != nil {
		t.Fatalf("AuthorizeSync() after stale disconnect error = %v", err)
	}
	if !gate.Snapshot().ControlConnected {
		t.Fatal("stale disconnect closed replacement control connection")
	}
}

func TestInvocationFenceRejectsEveryMismatchedField(t *testing.T) {
	t.Parallel()
	gate := newTestGate(t, newManualClock(0), Config{})
	connection := controlConnection("control-1", 7, 20)
	authorization := ttlAuthorization(assignment(7, "assignment-1"), 20, time.Minute)
	mustAuthenticateAndInstall(t, gate, connection, authorization)

	tests := []struct {
		name   string
		mutate func(*InvocationFence)
	}{
		{name: "worker id", mutate: func(fence *InvocationFence) { fence.Assignment.Worker.WorkerID = "worker-2" }},
		{name: "boot id", mutate: func(fence *InvocationFence) { fence.Assignment.Worker.BootID = "boot-2" }},
		{name: "session epoch", mutate: func(fence *InvocationFence) { fence.Assignment.Worker.SessionEpoch++ }},
		{name: "assignment id", mutate: func(fence *InvocationFence) { fence.Assignment.AssignmentID = "assignment-2" }},
		{name: "version id", mutate: func(fence *InvocationFence) { fence.Assignment.VersionID = "version-2" }},
		{name: "admission epoch", mutate: func(fence *InvocationFence) { fence.Assignment.AdmissionEpoch++ }},
		{name: "deployment generation", mutate: func(fence *InvocationFence) { fence.Assignment.DeploymentGeneration++ }},
		{name: "policy digest", mutate: func(fence *InvocationFence) { fence.Assignment.PolicyDigest = digest.Sum([]byte("other-policy")) }},
		{name: "mode", mutate: func(fence *InvocationFence) { fence.Assignment.Mode = ModeDrainOnly }},
		{name: "lower discovery epoch", mutate: func(fence *InvocationFence) { fence.DiscoveryEpoch-- }},
		{name: "higher discovery epoch", mutate: func(fence *InvocationFence) { fence.DiscoveryEpoch++ }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fence := authorization.Fence
			test.mutate(&fence)
			assertProblemCode(t, authorizeError(gate, fence), problem.CodeStaleAssignment)
		})
	}
}

func TestAuthorizationRefreshCannotChangeAssignmentIdentity(t *testing.T) {
	t.Parallel()
	gate := newTestGate(t, newManualClock(0), Config{})
	connection := controlConnection("control-1", 1, 10)
	authorization := ttlAuthorization(assignment(1, "assignment-1"), 10, time.Minute)
	mustAuthenticateAndInstall(t, gate, connection, authorization)

	generationChanges := []struct {
		name   string
		mutate func(*Authorization)
	}{
		{name: "version", mutate: func(auth *Authorization) { auth.Fence.Assignment.VersionID = "version-2" }},
		{name: "admission", mutate: func(auth *Authorization) { auth.Fence.Assignment.AdmissionEpoch++ }},
		{name: "generation", mutate: func(auth *Authorization) { auth.Fence.Assignment.DeploymentGeneration++ }},
		{name: "policy", mutate: func(auth *Authorization) { auth.Fence.Assignment.PolicyDigest = digest.Sum([]byte("other")) }},
	}
	for _, test := range generationChanges {
		t.Run(test.name, func(t *testing.T) {
			changed := authorization
			test.mutate(&changed)
			assertProblemCode(t, gate.Install(connection, changed), problem.CodeStaleGeneration)
		})
	}
	changedMode := authorization
	changedMode.Fence.Assignment.Mode = ModeDrainOnly
	assertProblemCode(t, gate.Install(connection, changedMode), problem.CodeStaleAssignment)
	if err := gate.AuthorizeSync(authorization.Fence); err != nil {
		t.Fatalf("failed refresh changed existing permission: %v", err)
	}
}

func TestFailedRefreshDoesNotExtendAuthorization(t *testing.T) {
	t.Parallel()
	clock := newManualClock(0)
	gate := newTestGate(t, clock, Config{})
	connection := controlConnection("control-1", 1, 10)
	authorization := ttlAuthorization(assignment(1, "assignment-1"), 10, time.Minute)
	mustAuthenticateAndInstall(t, gate, connection, authorization)

	clock.Advance(30 * time.Second)
	changed := authorization
	changed.Fence.Assignment.PolicyDigest = digest.Sum([]byte("changed-policy"))
	assertProblemCode(t, gate.Install(connection, changed), problem.CodeStaleGeneration)
	clock.Advance(30 * time.Second)
	assertProblemCode(t, authorizeError(gate, authorization.Fence), problem.CodeStaleAssignment)
	assertProblemCode(t, gate.Install(connection, changed), problem.CodeStaleGeneration)
	if err := gate.Install(connection, authorization); err != nil {
		t.Fatalf("valid refresh after expiry error = %v", err)
	}
	if err := gate.AuthorizeSync(authorization.Fence); err != nil {
		t.Fatalf("AuthorizeSync() after valid refresh error = %v", err)
	}
}

func TestInstallRejectsInvalidLifetimeWithoutChangingState(t *testing.T) {
	t.Parallel()
	gate := newTestGate(t, newManualClock(0), Config{MaxTTL: time.Minute})
	connection := controlConnection("control-1", 1, 10)
	if err := gate.AcceptAuthoritativeControl(connection); err != nil {
		t.Fatalf("AcceptAuthoritativeControl() error = %v", err)
	}
	valid := ttlAuthorization(assignment(1, "assignment-1"), 10, time.Minute)
	tests := []struct {
		name   string
		mutate func(*Authorization)
	}{
		{name: "zero ttl", mutate: func(auth *Authorization) { auth.TTL = 0 }},
		{name: "ttl above maximum", mutate: func(auth *Authorization) { auth.TTL = time.Minute + 1 }},
		{name: "live-only with ttl", mutate: func(auth *Authorization) { auth.Lifetime = LifetimeLiveOnly }},
		{name: "unknown lifetime", mutate: func(auth *Authorization) { auth.Lifetime = Lifetime("unknown") }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authorization := valid
			test.mutate(&authorization)
			assertProblemCode(t, gate.Install(connection, authorization), problem.CodeInvalidArgument)
		})
	}
	if gate.Snapshot().StoredAuthorizations != 0 {
		t.Fatal("invalid authorization changed gate state")
	}
}

func TestDrainOnlyAuthorizationCannotBeginSynchronousWork(t *testing.T) {
	t.Parallel()
	gate := newTestGate(t, newManualClock(0), Config{})
	connection := controlConnection("control-1", 1, 10)
	identity := assignment(1, "assignment-1")
	identity.Mode = ModeDrainOnly
	authorization := ttlAuthorization(identity, 10, time.Minute)
	mustAuthenticateAndInstall(t, gate, connection, authorization)
	assertProblemCode(t, authorizeError(gate, authorization.Fence), problem.CodeStaleAssignment)
}

func TestAuthorizationCapacityRequiresAssignmentRevoke(t *testing.T) {
	t.Parallel()
	clock := newManualClock(0)
	gate := newTestGate(t, clock, Config{MaxAuthorizations: 1})
	connection := controlConnection("control-1", 1, 10)
	if err := gate.AcceptAuthoritativeControl(connection); err != nil {
		t.Fatalf("AcceptAuthoritativeControl() error = %v", err)
	}
	first := ttlAuthorization(assignment(1, "assignment-1"), 10, time.Second)
	second := ttlAuthorization(assignment(1, "assignment-2"), 10, time.Second)
	if err := gate.Install(connection, first); err != nil {
		t.Fatalf("first Install() error = %v", err)
	}
	assertProblemCode(t, gate.Install(connection, second), problem.CodeOverloaded)
	clock.Advance(time.Second)
	assertProblemCode(t, authorizeError(gate, first.Fence), problem.CodeStaleAssignment)
	assertProblemCode(t, gate.Install(connection, second), problem.CodeOverloaded)
	gate.Revoke(first.Fence.Assignment.AssignmentID)
	if err := gate.Install(connection, second); err != nil {
		t.Fatalf("Install() after Revoke error = %v", err)
	}
	gate.Revoke(second.Fence.Assignment.AssignmentID)
	if snapshot := gate.Snapshot(); snapshot.TrackedAssignments != 0 || snapshot.StoredAuthorizations != 0 {
		t.Fatal("Revoke() did not remove authorization")
	}
}

func TestMonotonicClockRegressionFailsClosed(t *testing.T) {
	t.Parallel()
	clock := newManualClock(10 * time.Second)
	gate := newTestGate(t, clock, Config{})
	connection := controlConnection("control-1", 1, 10)
	authorization := ttlAuthorization(assignment(1, "assignment-1"), 10, time.Minute)
	mustAuthenticateAndInstall(t, gate, connection, authorization)

	clock.Set(9 * time.Second)
	assertProblemCode(t, authorizeError(gate, authorization.Fence), problem.CodeStaleAssignment)
	clock.Set(20 * time.Second)
	assertProblemCode(t, authorizeError(gate, authorization.Fence), problem.CodeStaleAssignment)
	if gate.Snapshot().ClockHealthy {
		t.Fatal("clock recovered after a monotonic regression")
	}
}

func TestConcurrentRefreshAndBeginIsRaceSafe(t *testing.T) {
	gate := newTestGate(t, newManualClock(0), Config{})
	connection := controlConnection("control-1", 1, 10)
	authorization := ttlAuthorization(assignment(1, "assignment-1"), 10, time.Minute)
	mustAuthenticateAndInstall(t, gate, connection, authorization)

	const operations = 100
	errorsSeen := make(chan error, operations*2)
	var wait sync.WaitGroup
	for range operations {
		wait.Go(func() {
			errorsSeen <- gate.AuthorizeSync(authorization.Fence)
		})
		wait.Go(func() {
			errorsSeen <- gate.Install(connection, authorization)
		})
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Errorf("concurrent operation error = %v", err)
		}
	}
}

func TestInstallRejectsUntrustedConnectionAndWrongWorker(t *testing.T) {
	t.Parallel()
	gate := newTestGate(t, newManualClock(0), Config{})
	connection := controlConnection("control-1", 1, 10)
	authorization := ttlAuthorization(assignment(1, "assignment-1"), 10, time.Minute)
	assertProblemCode(t, gate.Install(connection, authorization), problem.CodeStaleAssignment)
	if err := gate.AcceptAuthoritativeControl(connection); err != nil {
		t.Fatalf("AcceptAuthoritativeControl() error = %v", err)
	}
	assertProblemCode(t, gate.Install(controlConnection("control-2", 1, 10), authorization), problem.CodeStaleAssignment)
	wrongWorker := authorization
	wrongWorker.Fence.Assignment.Worker.WorkerID = "worker-2"
	assertProblemCode(t, gate.Install(connection, wrongWorker), problem.CodeStaleAssignment)
	if gate.Snapshot().StoredAuthorizations != 0 {
		t.Fatal("rejected authorization was stored")
	}
}

func newTestGate(t *testing.T, clock *manualClock, overrides Config) *Gate {
	t.Helper()
	config := overrides
	config.Worker = WorkerProcess{WorkerID: "worker-1", BootID: "boot-1"}
	config.Clock = clock.Elapsed
	gate, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return gate
}

func controlConnection(id string, sessionEpoch, discoveryEpoch uint64) ControlConnection {
	return ControlConnection{
		ConnectionID:   id,
		SessionEpoch:   sessionEpoch,
		DiscoveryEpoch: discoveryEpoch,
	}
}

func assignment(sessionEpoch uint64, assignmentID string) AssignmentIdentity {
	return AssignmentIdentity{
		Worker: WorkerSession{
			WorkerID:     "worker-1",
			BootID:       "boot-1",
			SessionEpoch: sessionEpoch,
		},
		AssignmentID:         assignmentID,
		VersionID:            "version-1",
		AdmissionEpoch:       3,
		DeploymentGeneration: 4,
		PolicyDigest:         digest.Sum([]byte("policy")),
		Mode:                 ModeNormal,
	}
}

func ttlAuthorization(identity AssignmentIdentity, discoveryEpoch uint64, ttl time.Duration) Authorization {
	return Authorization{
		Fence: InvocationFence{
			Assignment:     identity,
			DiscoveryEpoch: discoveryEpoch,
		},
		Lifetime: LifetimeTTL,
		TTL:      ttl,
	}
}

func liveAuthorization(identity AssignmentIdentity, discoveryEpoch uint64) Authorization {
	return Authorization{
		Fence: InvocationFence{
			Assignment:     identity,
			DiscoveryEpoch: discoveryEpoch,
		},
		Lifetime: LifetimeLiveOnly,
	}
}

func mustAuthenticateAndInstall(
	t *testing.T,
	gate *Gate,
	connection ControlConnection,
	authorization Authorization,
) {
	t.Helper()
	if err := gate.AcceptAuthoritativeControl(connection); err != nil {
		t.Fatalf("AcceptAuthoritativeControl() error = %v", err)
	}
	if err := gate.Install(connection, authorization); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
}

func authorizeError(gate *Gate, fence InvocationFence) error {
	return gate.AuthorizeSync(fence)
}

func assertProblemCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	var classified *problem.Error
	if !errors.As(err, &classified) || classified.Code != want {
		t.Fatalf("error = %v, want problem code %q", err, want)
	}
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

func (c *manualClock) Set(elapsed time.Duration) {
	c.mu.Lock()
	c.elapsed = elapsed
	c.mu.Unlock()
}
