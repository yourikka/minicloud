package scheduler

import (
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmprofile"
	"github.com/yourikka/minicloud/internal/workercache"
)

func TestPlanRequiresCommittedLeaderBarrier(t *testing.T) {
	planner := newPlanner(t, Config{})
	request := placementRequest("cmd-1", "assignment-1")
	workers := []WorkerSnapshot{readyWorker("worker-1", 0, 0)}
	assertPlanCode(t, planner, request, workers, problem.CodeControlPlaneStale)
	assertErrorCode(t, planner.InstallBarrier(LeaderBarrier{Term: 1, AppliedIndex: 2}), problem.CodeNoQuorum)
	if err := planner.InstallBarrier(LeaderBarrier{Term: 1, AppliedIndex: 2, Ready: true}); err != nil {
		t.Fatalf("InstallBarrier() error = %v", err)
	}
	decision, err := planner.Plan(request, workers)
	if err != nil || decision.Status != StatusApplied {
		t.Fatalf("Plan() = (%+v, %v), want applied decision", decision, err)
	}
}

func TestPlanHardFiltersThenPrefersCacheBeforeLoad(t *testing.T) {
	planner := newPlanner(t, Config{})
	if err := planner.InstallBarrier(LeaderBarrier{Term: 2, AppliedIndex: 10, Ready: true}); err != nil {
		t.Fatalf("InstallBarrier() error = %v", err)
	}
	request := placementRequest("cmd-2", "assignment-2")
	workers := []WorkerSnapshot{
		readyWorkerWithCache("worker-compiled", 8, true, true),
		readyWorkerWithCache("worker-artifact", 1, true, false),
		readyWorker("worker-draining", 0, 0),
		readyWorker("worker-runtime", 0, 0),
		readyWorker("worker-capacity", 0, 0),
	}
	workers[2].Intent = IntentDraining
	workers[3].Runtime.Engine = wasmprofile.EngineInterpreter
	workers[4].Capacity.MemoryMiB = workers[4].Capacity.AllocatedMemoryMiB

	decision, err := planner.Plan(request, workers)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if decision.Assignment.Worker.WorkerID != "worker-compiled" {
		t.Fatalf("selected worker = %q, want compiled-cache worker", decision.Assignment.Worker.WorkerID)
	}
	reasons := make(map[string][]FilterReason, len(decision.Candidates))
	for _, candidate := range decision.Candidates {
		reasons[candidate.WorkerID] = candidate.Reasons
	}
	if !hasReason(reasons["worker-draining"], ReasonIntent) ||
		!hasReason(reasons["worker-runtime"], ReasonRuntime) ||
		!hasReason(reasons["worker-capacity"], ReasonMemory) {
		t.Fatalf("filter explanations = %+v", reasons)
	}

	request.CommandID = "cmd-3"
	request.AssignmentID = "assignment-3"
	shuffled := []WorkerSnapshot{workers[4], workers[2], workers[1], workers[3], workers[0]}
	second, err := planner.Plan(request, shuffled)
	if err != nil {
		t.Fatalf("shuffled Plan() error = %v", err)
	}
	if second.Assignment.Worker.WorkerID != decision.Assignment.Worker.WorkerID {
		t.Fatalf("shuffled selection = %q, want %q", second.Assignment.Worker.WorkerID, decision.Assignment.Worker.WorkerID)
	}
}

func TestPlanReportsNoEligibleWorkerWithReasons(t *testing.T) {
	planner := newPlanner(t, Config{})
	if err := planner.InstallBarrier(LeaderBarrier{Term: 1, AppliedIndex: 3, Ready: true}); err != nil {
		t.Fatalf("InstallBarrier() error = %v", err)
	}
	request := placementRequest("cmd-no-worker", "assignment-no-worker")
	worker := readyWorker("worker-draining", 0, 0)
	worker.Intent = IntentDraining
	decision, err := planner.Plan(request, []WorkerSnapshot{worker})
	assertErrorCode(t, err, problem.CodeNoReadyReplica)
	if len(decision.Candidates) != 1 || !hasReason(decision.Candidates[0].Reasons, ReasonIntent) {
		t.Fatalf("no-worker decision = %+v", decision)
	}
}

func TestPlanIsIdempotentAndExplicitACKReclaimsCapacity(t *testing.T) {
	planner := newPlanner(t, Config{MaxDecisions: 1})
	if err := planner.InstallBarrier(LeaderBarrier{Term: 1, AppliedIndex: 2, Ready: true}); err != nil {
		t.Fatalf("InstallBarrier() error = %v", err)
	}
	request := placementRequest("cmd-4", "assignment-4")
	workers := []WorkerSnapshot{readyWorker("worker-1", 0, 0)}
	first, err := planner.Plan(request, workers)
	if err != nil {
		t.Fatalf("first Plan() error = %v", err)
	}
	duplicate, err := planner.Plan(request, []WorkerSnapshot{})
	if err != nil || duplicate.Status != StatusDuplicate || duplicate.Assignment != first.Assignment {
		t.Fatalf("duplicate Plan() = (%+v, %v)", duplicate, err)
	}
	already := request
	already.CommandID = "cmd-5"
	decision, err := planner.Plan(already, workers)
	if err != nil || decision.Status != StatusAlreadySatisfied {
		t.Fatalf("already satisfied Plan() = (%+v, %v)", decision, err)
	}
	conflict := request
	conflict.VersionID = "version-2"
	assertPlanCode(t, planner, conflict, workers, problem.CodeConflict)
	full := placementRequest("cmd-6", "assignment-6")
	assertPlanCode(t, planner, full, workers, problem.CodeOverloaded)
	if err := planner.Acknowledge(request.CommandID); err != nil {
		t.Fatalf("Acknowledge() error = %v", err)
	}
	if planner.DecisionCount() != 0 {
		t.Fatalf("DecisionCount() = %d, want zero after ACK", planner.DecisionCount())
	}
	if _, err := planner.Plan(full, workers); err != nil {
		t.Fatalf("Plan() after ACK error = %v", err)
	}
}

func TestPlanConcurrentSameCommandCreatesOneDecision(t *testing.T) {
	planner := newPlanner(t, Config{})
	if err := planner.InstallBarrier(LeaderBarrier{Term: 1, AppliedIndex: 1, Ready: true}); err != nil {
		t.Fatalf("InstallBarrier() error = %v", err)
	}
	request := placementRequest("cmd-concurrent", "assignment-concurrent")
	workers := []WorkerSnapshot{readyWorker("worker-1", 0, 0)}
	const calls = 100
	results := make(chan DecisionStatus, calls)
	errorsSeen := make(chan error, calls)
	var wait sync.WaitGroup
	for range calls {
		wait.Go(func() {
			decision, err := planner.Plan(request, workers)
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- decision.Status
		})
	}
	wait.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent Plan() error = %v", err)
	}
	var applied, duplicate int
	for status := range results {
		switch status {
		case StatusApplied:
			applied++
		case StatusDuplicate:
			duplicate++
		default:
			t.Errorf("unexpected concurrent status %q", status)
		}
	}
	if applied != 1 || duplicate != calls-1 || planner.DecisionCount() != 1 {
		t.Fatalf("statuses applied=%d duplicate=%d decisions=%d", applied, duplicate, planner.DecisionCount())
	}
}

func TestInstallBarrierRejectsRegressionAndDuplicateWorkers(t *testing.T) {
	planner := newPlanner(t, Config{})
	if err := planner.InstallBarrier(LeaderBarrier{Term: 4, AppliedIndex: 20, Ready: true}); err != nil {
		t.Fatalf("InstallBarrier() error = %v", err)
	}
	assertErrorCode(t, planner.InstallBarrier(LeaderBarrier{Term: 4, AppliedIndex: 19, Ready: true}), problem.CodeStaleGeneration)
	request := placementRequest("cmd-duplicate", "assignment-duplicate")
	workers := []WorkerSnapshot{readyWorker("worker-1", 0, 0), readyWorker("worker-1", 0, 0)}
	assertPlanCode(t, planner, request, workers, problem.CodeInvalidArgument)
}

func TestInvalidatedBarrierStopsNewPlans(t *testing.T) {
	planner := newPlanner(t, Config{})
	if err := planner.InstallBarrier(LeaderBarrier{Term: 1, AppliedIndex: 2, Ready: true}); err != nil {
		t.Fatalf("InstallBarrier() error = %v", err)
	}
	request := placementRequest("cmd-invalidate-1", "assignment-invalidate-1")
	workers := []WorkerSnapshot{readyWorker("worker-1", 0, 0)}
	if _, err := planner.Plan(request, workers); err != nil {
		t.Fatalf("initial Plan() error = %v", err)
	}
	planner.InvalidateBarrier()
	assertPlanCode(t, planner, placementRequest("cmd-invalidate-2", "assignment-invalidate-2"), workers, problem.CodeControlPlaneStale)
	assertErrorCode(t, planner.InstallBarrier(LeaderBarrier{Term: 1, AppliedIndex: 1, Ready: true}), problem.CodeStaleGeneration)
	duplicate, err := planner.Plan(request, nil)
	if err != nil || duplicate.Status != StatusDuplicate {
		t.Fatalf("duplicate after barrier invalidation = (%+v, %v)", duplicate, err)
	}
	if err := planner.InstallBarrier(LeaderBarrier{Term: 2, AppliedIndex: 3, Ready: true}); err != nil {
		t.Fatalf("reinstall barrier error = %v", err)
	}
}

func newPlanner(t *testing.T, config Config) *Planner {
	t.Helper()
	planner, err := New(config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return planner
}

func placementRequest(commandID, assignmentID string) PlacementRequest {
	artifactDigest := digest.Sum([]byte("artifact"))
	return PlacementRequest{
		CommandID:            commandID,
		AssignmentID:         assignmentID,
		VersionID:            "version-1",
		ArtifactDigest:       artifactDigest,
		ArtifactSize:         64,
		ABI:                  model.ABIWASICommandV1,
		HostAPI:              model.HostAPIProfileNone,
		FeatureProfile:       wasmprofile.FeatureProfile,
		RuntimeName:          wasmprofile.RuntimeName,
		RuntimeVersion:       wasmprofile.RuntimeVersion,
		RuntimeEngine:        wasmprofile.EngineCompiler,
		GOOS:                 runtime.GOOS,
		GOARCH:               runtime.GOARCH,
		MemoryMiB:            128,
		RequiredSlots:        1,
		AdmissionEpoch:       1,
		DeploymentGeneration: 1,
		PolicyDigest:         digest.Sum([]byte("policy")),
		CompiledCacheKey: workercache.Key{
			ArtifactDigest:        artifactDigest,
			ArtifactSize:          64,
			RuntimeName:           wasmprofile.RuntimeName,
			RuntimeVersion:        wasmprofile.RuntimeVersion,
			ABI:                   model.ABIWASICommandV1,
			HostAPIProfile:        model.HostAPIProfileNone,
			RuntimeFeatureProfile: wasmprofile.FeatureProfile,
			Engine:                wasmprofile.EngineCompiler,
			MemoryLimitMiB:        128,
			GOOS:                  runtime.GOOS,
			GOARCH:                runtime.GOARCH,
		},
	}
}

func readyWorker(id string, allocatedMemory, allocatedSlots uint64) WorkerSnapshot {
	return WorkerSnapshot{
		Session: servingauth.WorkerSession{WorkerID: id, BootID: "boot-1", SessionEpoch: 1},
		Runtime: RuntimeProfile{
			Name: wasmprofile.RuntimeName, Version: wasmprofile.RuntimeVersion,
			Engine: wasmprofile.EngineCompiler, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
			ABI: model.ABIWASICommandV1, HostAPI: model.HostAPIProfileNone,
			FeatureProfile: wasmprofile.FeatureProfile, MemoryMiB: 128,
		},
		Intent:   IntentSchedulable,
		State:    SessionReady,
		Capacity: Capacity{MemoryMiB: 512, AllocatedMemoryMiB: allocatedMemory, Slots: 10, AllocatedSlots: allocatedSlots},
	}
}

func readyWorkerWithCache(id string, allocatedSlots uint64, artifact, compiled bool) WorkerSnapshot {
	worker := readyWorker(id, 0, allocatedSlots)
	request := placementRequest("cache-command", "cache-assignment")
	if artifact {
		worker.Cache.Artifacts = map[digest.SHA256]struct{}{request.ArtifactDigest: {}}
	}
	if compiled {
		worker.Cache.Compiled = map[workercache.Key]struct{}{request.CompiledCacheKey: {}}
	}
	return worker
}

func hasReason(reasons []FilterReason, want FilterReason) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func assertPlanCode(t *testing.T, planner *Planner, request PlacementRequest, workers []WorkerSnapshot, want problem.Code) {
	t.Helper()
	result, err := planner.Plan(request, workers)
	assertErrorCode(t, err, want)
	_ = result
}

func assertErrorCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	var classifiedError *problem.Error
	if !errors.As(err, &classifiedError) || classifiedError.Code != want {
		t.Fatalf("error=%v, want problem code %q", err, want)
	}
}
