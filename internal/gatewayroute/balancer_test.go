package gatewayroute

import (
	"errors"
	"sync"
	"testing"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/servingauth"
)

func routeTarget(version string, generation uint64) model.RouteTarget {
	return model.RouteTarget{
		VersionID: version, AdmissionEpoch: 1, DeploymentGeneration: generation,
		EffectivePolicyDigest: digest.Sum([]byte(version)), WeightBasisPoints: model.TotalRouteWeightBasisPoints,
	}
}

func endpoint(id string, target model.RouteTarget) discovery.Endpoint {
	return discovery.Endpoint{
		Assignment: servingauth.AssignmentIdentity{
			Worker:       servingauth.WorkerSession{WorkerID: "worker-" + id, BootID: "boot-1", SessionEpoch: 1},
			AssignmentID: id, VersionID: target.VersionID, AdmissionEpoch: target.AdmissionEpoch,
			DeploymentGeneration: target.DeploymentGeneration, PolicyDigest: target.EffectivePolicyDigest,
			Mode: servingauth.ModeNormal,
		},
		Address: "worker.internal:7443", State: discovery.EndpointReady,
	}
}

func assertCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	var classified *problem.Error
	if !errors.As(err, &classified) || classified.Code != want {
		t.Fatalf("error = %v, want code %q", err, want)
	}
}

func TestBalancerLeastInflightThenRoundRobin(t *testing.T) {
	t.Parallel()
	target := routeTarget("version-a", 1)
	endpoints := []discovery.Endpoint{endpoint("assignment-b", target), endpoint("assignment-a", target)}
	balancer := New()
	first, err := balancer.Acquire("function-a", target, endpoints)
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	second, err := balancer.Acquire("function-a", target, endpoints)
	if err != nil {
		t.Fatalf("second Acquire() error = %v", err)
	}
	if first.Endpoint.Assignment.AssignmentID != "assignment-a" || second.Endpoint.Assignment.AssignmentID != "assignment-b" {
		t.Fatalf("selections = (%q,%q)", first.Endpoint.Assignment.AssignmentID, second.Endpoint.Assignment.AssignmentID)
	}
	first.Release()
	third, err := balancer.Acquire("function-a", target, endpoints)
	if err != nil {
		t.Fatalf("third Acquire() error = %v", err)
	}
	if third.Endpoint.Assignment != first.Endpoint.Assignment {
		t.Fatalf("least-inflight selected %q, want %q", third.Endpoint.Assignment.AssignmentID, first.Endpoint.Assignment.AssignmentID)
	}
	second.Release()
	third.Release()
}

func TestBalancerDoesNotFallbackAcrossRouteTargets(t *testing.T) {
	t.Parallel()
	targetA := routeTarget("version-a", 1)
	targetB := routeTarget("version-b", 2)
	balancer := New()
	_, err := balancer.Acquire("function-a", targetA, []discovery.Endpoint{endpoint("assignment-b", targetB)})
	assertCode(t, err, problem.CodeNoReadyReplica)
}

func TestLeaseReleaseIsIdempotentAcrossCopies(t *testing.T) {
	t.Parallel()
	target := routeTarget("version-a", 1)
	balancer := New()
	lease, err := balancer.Acquire("function-a", target, []discovery.Endpoint{endpoint("assignment-a", target)})
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	copyOfLease := lease
	var wait sync.WaitGroup
	for range 100 {
		wait.Go(func() { lease.Release() })
		wait.Go(func() { copyOfLease.Release() })
	}
	wait.Wait()
	if got := balancer.Inflight(lease.Endpoint.Assignment); got != 0 {
		t.Fatalf("Inflight() = %d after repeated release", got)
	}
}

func TestBalancerConcurrentAcquiresRemainBalanced(t *testing.T) {
	t.Parallel()
	target := routeTarget("version-a", 1)
	endpoints := []discovery.Endpoint{endpoint("assignment-a", target), endpoint("assignment-b", target)}
	balancer := New()
	const calls = 100
	selected := make(chan string, calls)
	leases := make(chan Lease, calls)
	var wait sync.WaitGroup
	for range calls {
		wait.Go(func() {
			lease, err := balancer.Acquire("function-a", target, endpoints)
			if err != nil {
				t.Errorf("Acquire() error = %v", err)
				return
			}
			selected <- lease.Endpoint.Assignment.AssignmentID
			leases <- lease
		})
	}
	wait.Wait()
	close(selected)
	close(leases)
	counts := make(map[string]int)
	for id := range selected {
		counts[id]++
	}
	if counts["assignment-a"] != calls/2 || counts["assignment-b"] != calls/2 {
		t.Fatalf("selection counts = %+v", counts)
	}
	for lease := range leases {
		lease.Release()
	}
}

func TestBalancerRejectsInvalidInputsAndSupportsZeroValue(t *testing.T) {
	t.Parallel()
	target := routeTarget("version-a", 1)
	var balancer Balancer
	if _, err := balancer.Acquire("", target, nil); err == nil {
		t.Fatal("Acquire() accepted an invalid Function ID")
	} else {
		assertCode(t, err, problem.CodeInvalidArgument)
	}
	invalid := target
	invalid.EffectivePolicyDigest = "invalid"
	if _, err := balancer.Acquire("function-a", invalid, nil); err == nil {
		t.Fatal("Acquire() accepted an invalid Route target")
	} else {
		assertCode(t, err, problem.CodeInvalidArgument)
	}
	lease, err := balancer.Acquire("function-a", target, []discovery.Endpoint{endpoint("assignment-a", target)})
	if err != nil {
		t.Fatalf("zero-value Acquire() error = %v", err)
	}
	lease.Release()
}

func TestBalancerRejectsDuplicateAssignmentID(t *testing.T) {
	t.Parallel()
	target := routeTarget("version-a", 1)
	first := endpoint("assignment-a", target)
	second := first
	second.Assignment.Worker.BootID = "boot-2"
	_, err := New().Acquire("function-a", target, []discovery.Endpoint{first, second})
	assertCode(t, err, problem.CodeInvalidArgument)
}
