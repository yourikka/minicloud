package gatewayinvoke

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/gatewaydiscovery"
	"github.com/yourikka/minicloud/internal/gatewayroute"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmexec"
	"github.com/yourikka/minicloud/internal/wasmprofile"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

func TestGatewayInvokesSelectedEndpointWithExactFence(t *testing.T) {
	t.Parallel()
	policy := digest.Sum([]byte("policy-a"))
	endpoint := candidate("assignment-a", "worker-a", "version-a", policy)
	store := servingStore(t, []discovery.EndpointCandidate{endpoint})
	invoker := &recordingInvoker{result: wasmexec.Result{Response: abi.Response{SpecVersion: abi.Version, Status: 204}}}
	resolver := &recordingResolver{invokers: map[string]EndpointInvoker{
		endpoint.Address: invoker,
	}}
	balancer := gatewayroute.New()
	gateway, err := New(Config{Discovery: store, Balancer: balancer, Resolver: resolver})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	request := Request{
		FunctionID:  "function-a",
		AffinityKey: []byte("request-123"),
		Invocation: abi.Request{
			InvocationID: "invocation-1",
			Method:       "POST",
			Path:         "/echo",
			Body:         []byte("request-body"),
		},
		Timeout: time.Second,
	}
	result, err := gateway.Invoke(t.Context(), request)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if result.InvocationID != request.Invocation.InvocationID || result.VersionID != "version-a" ||
		result.RouteRevision != 6 || result.DiscoveryEpoch != 101 || result.Endpoint.Assignment != endpoint.Assignment ||
		result.Execution.Response.Status != 204 {
		t.Fatalf("Invoke() = %+v", result)
	}
	calls := invoker.Calls()
	if len(calls) != 1 || calls[0].fence.Assignment != endpoint.Assignment || calls[0].fence.DiscoveryEpoch != 101 ||
		calls[0].request.InvocationID != request.Invocation.InvocationID || calls[0].timeout != time.Second {
		t.Fatalf("worker calls = %+v", calls)
	}
	if calls[0].request.Body[0] != 'r' {
		t.Fatalf("worker received unexpected request body: %+v", calls[0].request)
	}
	resolverCalls := resolver.Calls()
	if !slices.Equal(resolverCalls, []string{endpoint.Address}) {
		t.Fatalf("resolver calls = %v", resolverCalls)
	}
	if inflight := balancer.Inflight(endpoint.Assignment); inflight != 0 {
		t.Fatalf("Balancer.Inflight() = %d after invocation", inflight)
	}
}

func TestGatewayFailsClosedBeforeEndpointResolution(t *testing.T) {
	t.Parallel()
	resolver := &recordingResolver{invokers: map[string]EndpointInvoker{}}
	store, err := gatewaydiscovery.New(gatewaydiscovery.Config{})
	if err != nil {
		t.Fatalf("gatewaydiscovery.New() error = %v", err)
	}
	gateway, err := New(Config{Discovery: store, Resolver: resolver})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = gateway.Invoke(t.Context(), Request{FunctionID: "function-a"})
	assertProblemCode(t, err, problem.CodeControlPlaneStale)
	if len(resolver.Calls()) != 0 {
		t.Fatalf("resolver was called with stale serving state: %v", resolver.Calls())
	}
}

func TestGatewayRejectsUnavailableServingViewBeforeResolution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*discovery.Input)
		code   problem.Code
	}{
		{
			name: "function disabled",
			mutate: func(input *discovery.Input) {
				input.Function.Lifecycle = model.FunctionDisabled
			},
			code: problem.CodeFunctionDisabled,
		},
		{
			name: "trigger disabled",
			mutate: func(input *discovery.Input) {
				input.Trigger.Enabled = false
			},
			code: problem.CodeFunctionDisabled,
		},
		{
			name: "route absent",
			mutate: func(input *discovery.Input) {
				input.Route = discovery.Route{FunctionID: input.Function.ID}
			},
			code: problem.CodeFunctionDisabled,
		},
		{
			name: "route disabled",
			mutate: func(input *discovery.Input) {
				input.Route.Enabled = false
				input.Route.Targets = []model.RouteTarget{}
			},
			code: problem.CodeFunctionDisabled,
		},
		{
			name: "no ready endpoint",
			mutate: func(input *discovery.Input) {
				input.Candidates = []discovery.EndpointCandidate{}
			},
			code: problem.CodeNoReadyReplica,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resolver := &recordingResolver{invokers: map[string]EndpointInvoker{}}
			store := servingStoreWithMutation(
				t,
				[]discovery.EndpointCandidate{
					candidate("assignment-a", "worker-a", "version-a", digest.Sum([]byte("policy-a"))),
				},
				test.mutate,
			)
			gateway, err := New(Config{Discovery: store, Resolver: resolver})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			_, err = gateway.Invoke(t.Context(), Request{FunctionID: "function-a"})
			assertProblemCode(t, err, test.code)
			if len(resolver.Calls()) != 0 {
				t.Fatalf("resolver was called for unavailable serving view: %v", resolver.Calls())
			}
		})
	}
}

func TestGatewayDoesNotFallbackAfterWorkerFailure(t *testing.T) {
	t.Parallel()
	policy := digest.Sum([]byte("policy-a"))
	first := candidate("assignment-a", "worker-a", "version-a", policy)
	second := candidate("assignment-b", "worker-b", "version-a", policy)
	store := servingStore(t, []discovery.EndpointCandidate{second, first})
	workerFailure := &problem.Error{Code: problem.CodeWorkerLost, Message: "worker connection failed"}
	firstInvoker := &recordingInvoker{err: workerFailure}
	secondInvoker := &recordingInvoker{}
	resolver := &recordingResolver{invokers: map[string]EndpointInvoker{
		first.Address:  firstInvoker,
		second.Address: secondInvoker,
	}}
	balancer := gatewayroute.New()
	gateway, err := New(Config{Discovery: store, Balancer: balancer, Resolver: resolver})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = gateway.Invoke(t.Context(), Request{
		FunctionID:  "function-a",
		AffinityKey: []byte("request-456"),
		Invocation:  abi.Request{InvocationID: "invocation-2"},
		Timeout:     time.Second,
	})
	if !errors.Is(err, workerFailure) {
		t.Fatalf("Invoke() error = %v, want worker error", err)
	}
	if calls := resolver.Calls(); !slices.Equal(calls, []string{first.Address}) {
		t.Fatalf("resolver calls = %v, want only selected endpoint", calls)
	}
	if len(firstInvoker.Calls()) != 1 || len(secondInvoker.Calls()) != 0 {
		t.Fatalf("worker calls = first:%+v second:%+v", firstInvoker.Calls(), secondInvoker.Calls())
	}
	if inflight := balancer.Inflight(first.Assignment); inflight != 0 {
		t.Fatalf("Balancer.Inflight() = %d after worker failure", inflight)
	}
}

func TestGatewayRejectsMissingDependenciesAndResolverResult(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); err == nil {
		t.Fatal("New() succeeded without dependencies")
	}
	store := servingStore(t, []discovery.EndpointCandidate{
		candidate("assignment-a", "worker-a", "version-a", digest.Sum([]byte("policy-a"))),
	})
	balancer := gatewayroute.New()
	gateway, err := New(Config{
		Discovery: store,
		Balancer:  balancer,
		Resolver: resolverFunc(func(context.Context, string) (EndpointInvoker, error) {
			return nil, nil
		}),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = gateway.Invoke(t.Context(), Request{FunctionID: "function-a"})
	if err == nil || !strings.Contains(err.Error(), "resolver returned no worker invoker") {
		t.Fatalf("Invoke() error = %v, want nil resolver result", err)
	}
	identity := candidate("assignment-a", "worker-a", "version-a", digest.Sum([]byte("policy-a"))).Assignment
	if inflight := balancer.Inflight(identity); inflight != 0 {
		t.Fatalf("Balancer.Inflight() = %d after resolver failure", inflight)
	}
}

func servingStore(t *testing.T, candidates []discovery.EndpointCandidate) *gatewaydiscovery.Store {
	t.Helper()
	return servingStoreWithMutation(t, candidates, nil)
}

func servingStoreWithMutation(
	t *testing.T,
	candidates []discovery.EndpointCandidate,
	mutate func(*discovery.Input),
) *gatewaydiscovery.Store {
	t.Helper()
	builder, err := discovery.New(discovery.Config{})
	if err != nil {
		t.Fatalf("discovery.New() error = %v", err)
	}
	policy := digest.Sum([]byte("policy-a"))
	input := discovery.Input{
		DiscoveryEpoch:  101,
		ServingSequence: 6,
		GeneratedAt:     time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC),
		Function: discovery.Function{
			ID: "function-a", Name: "function-a", ResourceRevision: 3, Lifecycle: model.FunctionActive,
		},
		Trigger: discovery.HTTPTrigger{
			ID: "trigger-a", FunctionID: "function-a", ResourceRevision: 4, Enabled: true, AuthPolicy: discovery.AuthPublic,
		},
		Route: discovery.Route{
			Present: true, FunctionID: "function-a", ResourceRevision: 5, RouteRevision: 6, Enabled: true,
			Targets: []model.RouteTarget{{
				VersionID: "version-a", AdmissionEpoch: 1, DeploymentGeneration: 1,
				EffectivePolicyDigest: policy, WeightBasisPoints: model.TotalRouteWeightBasisPoints,
			}},
			Affinity: model.AffinityRequestID, HashVersion: model.HashVersionSHA256BPSV1,
			SaltID: "salt-a", Salt: []byte("0123456789abcdef"),
		},
		Candidates: candidates,
	}
	if mutate != nil {
		mutate(&input)
	}
	result, err := builder.Build(input)
	if err != nil {
		t.Fatalf("Builder.Build() error = %v", err)
	}
	store, err := gatewaydiscovery.New(gatewaydiscovery.Config{})
	if err != nil {
		t.Fatalf("gatewaydiscovery.New() error = %v", err)
	}
	if err := store.Apply(gatewaydiscovery.Event{
		Full: true, DiscoveryEpoch: 101, ServingSequence: 6, Snapshots: []discovery.Snapshot{result.Snapshot},
	}); err != nil {
		t.Fatalf("Store.Apply() error = %v", err)
	}
	return store
}

func candidate(assignmentID, workerID, versionID string, policy digest.SHA256) discovery.EndpointCandidate {
	session := servingauth.WorkerSession{WorkerID: workerID, BootID: "boot-a", SessionEpoch: 1}
	assignment := servingauth.AssignmentIdentity{
		Worker: session, AssignmentID: assignmentID, VersionID: versionID, AdmissionEpoch: 1,
		DeploymentGeneration: 1, PolicyDigest: policy, Mode: servingauth.ModeNormal,
	}
	return discovery.EndpointCandidate{
		Assignment: assignment, DesiredState: discovery.AssignmentAssigned,
		Worker: scheduler.WorkerSnapshot{
			Session: session,
			Runtime: scheduler.RuntimeProfile{
				Name: wasmprofile.RuntimeName, Version: wasmprofile.RuntimeVersion, Engine: wasmprofile.EngineCompiler,
				GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, ABI: model.ABIWASICommandV1,
				HostAPI: model.HostAPIProfileNone, FeatureProfile: wasmprofile.FeatureProfile, MemoryMiB: 128,
			},
			Intent: scheduler.IntentSchedulable, State: scheduler.SessionReady, Drain: scheduler.DrainNotDraining,
			Capacity: scheduler.Capacity{MemoryMiB: 512, Slots: 8}, Labels: map[string]string{"zone": "test"},
		},
		ReplicaReady: true,
		Authorization: &servingauth.Authorization{
			Fence:    servingauth.InvocationFence{Assignment: assignment, DiscoveryEpoch: 101},
			Lifetime: servingauth.LifetimeTTL,
			TTL:      time.Minute,
		},
		Address: workerID + ".internal:7443",
	}
}

type invocationCall struct {
	fence   servingauth.InvocationFence
	request abi.Request
	timeout time.Duration
}

type recordingInvoker struct {
	mu     sync.Mutex
	calls  []invocationCall
	result wasmexec.Result
	err    error
}

func (i *recordingInvoker) Invoke(
	_ context.Context,
	fence servingauth.InvocationFence,
	request abi.Request,
	timeout time.Duration,
) (wasmexec.Result, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls = append(i.calls, invocationCall{fence: fence, request: request, timeout: timeout})
	return i.result, i.err
}

func (i *recordingInvoker) Calls() []invocationCall {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.calls)
}

type recordingResolver struct {
	mu       sync.Mutex
	invokers map[string]EndpointInvoker
	calls    []string
}

func (r *recordingResolver) Resolve(_ context.Context, address string) (EndpointInvoker, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, address)
	return r.invokers[address], nil
}

func (r *recordingResolver) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

type resolverFunc func(context.Context, string) (EndpointInvoker, error)

func (f resolverFunc) Resolve(ctx context.Context, address string) (EndpointInvoker, error) {
	return f(ctx, address)
}

func assertProblemCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	var classified *problem.Error
	if !errors.As(err, &classified) || classified.Code != want {
		t.Fatalf("error = %v, want problem code %q", err, want)
	}
}
