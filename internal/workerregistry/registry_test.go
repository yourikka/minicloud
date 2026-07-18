package workerregistry

import (
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
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

func newRegistry(t *testing.T, clock *manualClock) *Registry {
	t.Helper()
	registry, err := New(Config{Clock: clock.Elapsed})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return registry
}

func session(workerID, bootID string, epoch uint64) servingauth.WorkerSession {
	return servingauth.WorkerSession{WorkerID: workerID, BootID: bootID, SessionEpoch: epoch}
}

func inventoryFor(s servingauth.WorkerSession) Inventory {
	return Inventory{
		Revision: 1,
		Session:  s,
		Runtime: scheduler.RuntimeProfile{
			Name: wasmprofile.RuntimeName, Version: wasmprofile.RuntimeVersion,
			Engine: wasmprofile.EngineCompiler, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
			ABI: model.ABIWASICommandV1, HostAPI: model.HostAPIProfileNone,
			FeatureProfile: wasmprofile.FeatureProfile, MemoryMiB: 128,
		},
		Capacity: scheduler.Capacity{MemoryMiB: 512, Slots: 8},
		Labels:   map[string]string{"zone": "cn-east"},
		Cache:    scheduler.CacheHints{Artifacts: map[digest.SHA256]struct{}{digest.Sum([]byte("artifact")): {}}},
	}
}

func TestRegistryRejectsStaleInventoryRevision(t *testing.T) {
	clock := &manualClock{}
	registry := newRegistry(t, clock)
	s := session("worker-1", "boot-1", 1)
	registerReady(t, registry, s)
	newer := inventoryFor(s)
	newer.Revision = 2
	newer.Labels["zone"] = "cn-north"
	if err := registry.ReportInventory(newer); err != nil {
		t.Fatalf("newer ReportInventory() error = %v", err)
	}
	stale := inventoryFor(s)
	if err := registry.ReportInventory(stale); err == nil {
		t.Fatal("stale inventory revision was accepted")
	} else {
		assertCode(t, err, problem.CodeStaleGeneration)
	}
	if got := registry.Snapshot().Workers[0].Snapshot.Labels["zone"]; got != "cn-north" {
		t.Fatalf("stale inventory replaced newer snapshot with zone %q", got)
	}
}

func registerReady(t *testing.T, registry *Registry, s servingauth.WorkerSession) {
	t.Helper()
	if err := registry.CommitSession(Registration{Session: s}); err != nil {
		t.Fatalf("CommitSession() error = %v", err)
	}
	if err := registry.Register(Registration{Session: s}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := registry.ReportInventory(inventoryFor(s)); err != nil {
		t.Fatalf("ReportInventory() error = %v", err)
	}
}

func assertCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	var classified *problem.Error
	if !errors.As(err, &classified) || classified.Code != want {
		t.Fatalf("error = %v, want problem code %q", err, want)
	}
}

func TestRegistryRejectsInvalidTimingBounds(t *testing.T) {
	clock := &manualClock{}
	for name, config := range map[string]Config{
		"heartbeat too short":        {Clock: clock.Elapsed, HeartbeatInterval: 499 * time.Millisecond},
		"suspect before heartbeat":   {Clock: clock.Elapsed, HeartbeatInterval: time.Second, SuspectAfter: time.Second, UnavailableAfter: 3 * time.Second},
		"unavailable before suspect": {Clock: clock.Elapsed, HeartbeatInterval: time.Second, SuspectAfter: 2 * time.Second, UnavailableAfter: 2 * time.Second},
		"too many workers":           {Clock: clock.Elapsed, MaxWorkers: HardMaxWorkers + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(config); err == nil {
				t.Fatal("New() succeeded for invalid configuration")
			}
		})
	}
}

func TestRegistrySessionHighWaterAndRemovedIdentity(t *testing.T) {
	clock := &manualClock{}
	registry := newRegistry(t, clock)
	s1 := session("worker-1", "boot-1", 1)
	if err := registry.Register(Registration{Session: s1}); err == nil {
		t.Fatal("Register() accepted an uncommitted session")
	}
	registerReady(t, registry, s1)
	if err := registry.CommitSession(Registration{Session: session("worker-1", "boot-2", 1)}); err == nil {
		t.Fatal("CommitSession() accepted a reused epoch")
	} else {
		assertCode(t, err, problem.CodeStaleAssignment)
	}
	if err := registry.SetIntent("worker-1", scheduler.IntentDraining); err != nil {
		t.Fatalf("SetIntent(Draining) error = %v", err)
	}
	if err := registry.UpdateDrain(DrainObservation{Session: s1, AssignmentsDrained: true, GatewayFencesAcked: true}); err != nil {
		t.Fatalf("UpdateDrain() error = %v", err)
	}
	if err := registry.SetIntent("worker-1", scheduler.IntentRemoved); err != nil {
		t.Fatalf("SetIntent(Removed) error = %v", err)
	}
	if err := registry.CommitSession(Registration{Session: session("worker-1", "boot-3", 3)}); err == nil {
		t.Fatal("removed Worker accepted a new session")
	} else {
		assertCode(t, err, problem.CodeForbidden)
	}
}

func TestRegistryNewWorkerMustStartAtEpochOne(t *testing.T) {
	clock := &manualClock{}
	registry := newRegistry(t, clock)
	if err := registry.CommitSession(Registration{Session: session("worker-1", "boot-1", 2)}); err == nil {
		t.Fatal("new Worker accepted an epoch greater than one")
	} else {
		assertCode(t, err, problem.CodeStaleAssignment)
	}
	if got := len(registry.Snapshot().Workers); got != 0 {
		t.Fatalf("invalid first commit created %d Worker records", got)
	}
}

func TestRegistryHigherSessionFencesOldSession(t *testing.T) {
	clock := &manualClock{}
	registry := newRegistry(t, clock)
	s1 := session("worker-1", "boot-1", 1)
	registerReady(t, registry, s1)
	s2 := session("worker-1", "boot-2", 2)
	if err := registry.CommitSession(Registration{Session: s2}); err != nil {
		t.Fatalf("CommitSession() error = %v", err)
	}
	if err := registry.Register(Registration{Session: s1}); err == nil {
		t.Fatal("old session was accepted after a higher commit")
	} else {
		assertCode(t, err, problem.CodeStaleAssignment)
	}
	view := registry.Snapshot().Workers[0]
	if view.Snapshot.State != scheduler.SessionJoining || view.Snapshot.Capacity != (scheduler.Capacity{}) || view.Snapshot.Runtime != (scheduler.RuntimeProfile{}) {
		t.Fatalf("higher session retained old inventory: %+v", view.Snapshot)
	}
}

func TestRegistryInventoryAndHeartbeatThresholds(t *testing.T) {
	clock := &manualClock{}
	registry := newRegistry(t, clock)
	s := session("worker-1", "boot-1", 1)
	registerReady(t, registry, s)
	clock.Set(DefaultSuspectAfter - time.Nanosecond)
	if err := registry.Evaluate(); err != nil {
		t.Fatalf("Evaluate() before suspect threshold error = %v", err)
	}
	if got := registry.Snapshot().Workers[0].Snapshot.State; got != scheduler.SessionReady {
		t.Fatalf("state at suspect threshold - epsilon = %q", got)
	}
	clock.Set(DefaultSuspectAfter)
	if err := registry.Evaluate(); err != nil {
		t.Fatalf("Evaluate() at suspect threshold error = %v", err)
	}
	if got := registry.Snapshot().Workers[0].Snapshot.State; got != scheduler.SessionSuspect {
		t.Fatalf("state at suspect threshold = %q", got)
	}
	if err := registry.Heartbeat(s); err != nil {
		t.Fatalf("Heartbeat() recovery error = %v", err)
	}
	if got := registry.Snapshot().Workers[0].Snapshot.State; got != scheduler.SessionReady {
		t.Fatalf("state after heartbeat recovery = %q", got)
	}
	clock.Set(DefaultUnavailableAfter + DefaultSuspectAfter)
	if err := registry.Evaluate(); err != nil {
		t.Fatalf("Evaluate() at unavailable threshold error = %v", err)
	}
	if got := registry.Snapshot().Workers[0].Snapshot.State; got != scheduler.SessionUnavailable {
		t.Fatalf("state at unavailable threshold = %q", got)
	}
	if err := registry.Heartbeat(s); err != nil {
		t.Fatalf("Heartbeat() unavailable recovery error = %v", err)
	}
	if got := registry.Snapshot().Workers[0].Snapshot.State; got != scheduler.SessionReady {
		t.Fatalf("state after unavailable recovery = %q", got)
	}
}

func TestRegistryClockRegressionFailsClosedAndSnapshotIsDefensive(t *testing.T) {
	clock := &manualClock{}
	registry := newRegistry(t, clock)
	s := session("worker-1", "boot-1", 1)
	registerReady(t, registry, s)
	snapshot := registry.Snapshot()
	snapshot.Workers[0].Snapshot.Labels["zone"] = "mutated"
	for key := range snapshot.Workers[0].Snapshot.Cache.Artifacts {
		delete(snapshot.Workers[0].Snapshot.Cache.Artifacts, key)
	}
	defensive := registry.Snapshot().Workers[0].Snapshot
	if defensive.Labels["zone"] != "cn-east" || len(defensive.Cache.Artifacts) != 1 {
		t.Fatalf("snapshot was not defensive: %+v", defensive)
	}
	clock.Set(-time.Nanosecond)
	if err := registry.Evaluate(); err == nil {
		t.Fatal("Evaluate() succeeded after clock regression")
	} else {
		assertCode(t, err, problem.CodeControlPlaneStale)
	}
	if snapshot := registry.Snapshot(); snapshot.ClockHealthy {
		t.Fatal("Snapshot() reported a healthy clock after regression")
	}
}

func TestRegistryDrainActivationRequiresNewSession(t *testing.T) {
	clock := &manualClock{}
	registry := newRegistry(t, clock)
	s1 := session("worker-1", "boot-1", 1)
	registerReady(t, registry, s1)
	if err := registry.SetIntent("worker-1", scheduler.IntentDraining); err != nil {
		t.Fatalf("SetIntent(Draining) error = %v", err)
	}
	if err := registry.UpdateDrain(DrainObservation{Session: s1, AssignmentsDrained: true, GatewayFencesAcked: true}); err != nil {
		t.Fatalf("UpdateDrain() error = %v", err)
	}
	if got := registry.Snapshot().Workers[0].Snapshot.Drain; got != scheduler.DrainDrained {
		t.Fatalf("drain state = %q", got)
	}
	if err := registry.SetIntent("worker-1", scheduler.IntentSchedulable); err != nil {
		t.Fatalf("SetIntent(Schedulable) error = %v", err)
	}
	if err := registry.ReportInventory(inventoryFor(s1)); err == nil {
		t.Fatal("old session inventory revived an activated Worker")
	} else {
		assertCode(t, err, problem.CodeStaleAssignment)
	}
	if err := registry.Heartbeat(s1); err == nil {
		t.Fatal("old session heartbeat revived an activated Worker")
	} else {
		assertCode(t, err, problem.CodeStaleAssignment)
	}
	s2 := session("worker-1", "boot-1", 2)
	if err := registry.CommitSession(Registration{Session: s2}); err != nil {
		t.Fatalf("CommitSession() activation error = %v", err)
	}
	if err := registry.Register(Registration{Session: s2}); err != nil {
		t.Fatalf("Register() activation error = %v", err)
	}
	if err := registry.ReportInventory(inventoryFor(s2)); err != nil {
		t.Fatalf("ReportInventory() activation error = %v", err)
	}
	view := registry.Snapshot().Workers[0]
	if view.Snapshot.Intent != scheduler.IntentSchedulable || view.Snapshot.State != scheduler.SessionReady || view.Snapshot.Drain != scheduler.DrainNotDraining {
		t.Fatalf("activated view = %+v", view)
	}
}

func TestRegistryConcurrentOperations(t *testing.T) {
	clock := &manualClock{}
	registry := newRegistry(t, clock)
	s := session("worker-1", "boot-1", 1)
	registerReady(t, registry, s)
	var wait sync.WaitGroup
	for range 100 {
		wait.Go(func() {
			_ = registry.Heartbeat(s)
			_ = registry.Evaluate()
			_ = registry.Snapshot()
		})
	}
	wait.Wait()
}
