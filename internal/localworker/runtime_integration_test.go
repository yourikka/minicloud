//go:build integration

package localworker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/gatewaydiscovery"
	"github.com/yourikka/minicloud/internal/gatewayinvoke"
	"github.com/yourikka/minicloud/internal/localserving"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmexec"
	"github.com/yourikka/minicloud/internal/workeragent"
	"github.com/yourikka/minicloud/internal/workercache"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

func TestLocalRuntimeReconcilesPublishesAndInvokesRealWorker(t *testing.T) {
	wasm := buildLocalStandardGoFixture(t)
	store, err := artifact.Open(artifact.Config{
		Root:             t.TempDir(),
		MaxArtifactBytes: model.MaxArtifactBytes,
	})
	if err != nil {
		t.Fatalf("artifact.Open() error = %v", err)
	}
	engine, err := wasmexec.New(context.Background(), wasmexec.Config{})
	if err != nil {
		_ = store.Close()
		t.Fatalf("wasmexec.New() error = %v", err)
	}
	cache, err := workercache.New(workercache.Config{Artifacts: store, Compiler: engine})
	if err != nil {
		_ = engine.Close(context.Background())
		_ = store.Close()
		t.Fatalf("workercache.New() error = %v", err)
	}
	worker := validWorkerSnapshot()
	agent, err := workeragent.New(workeragent.Config{
		Cache: cache,
		Authorization: servingauth.Config{Worker: servingauth.WorkerProcess{
			WorkerID: worker.Session.WorkerID,
			BootID:   worker.Session.BootID,
		}},
	})
	if err != nil {
		_ = cache.Close(context.Background())
		_ = engine.Close(context.Background())
		_ = store.Close()
		t.Fatalf("workeragent.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := agent.Close(context.Background()); err != nil {
			t.Errorf("Agent.Close() error = %v", err)
		}
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("Engine.Close() error = %v", err)
		}
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})

	state, assignment := localRuntimeState(t, worker.Session, wasm)
	if _, err := store.Put(t.Context(), state.Version.ArtifactDigest, bytes.NewReader(wasm)); err != nil {
		t.Fatalf("Store.Put() error = %v", err)
	}
	assignments := &assignmentSource{records: []controlplane.AssignmentRecord{assignment}}
	states := &servingStateSource{states: []controlplane.ServingState{state}}
	connection := servingauth.ControlConnection{
		ConnectionID: "control-a", SessionEpoch: worker.Session.SessionEpoch, DiscoveryEpoch: 101,
	}
	reconciler, err := NewReconciler(ReconcilerConfig{
		Assignments: assignments,
		States:      states,
		Workers:     workerSource{snapshot: worker},
		Agent:       agent,
		Connection:  connection,
		Address:     "worker-a.internal:7443",
	})
	if err != nil {
		t.Fatalf("NewReconciler() error = %v", err)
	}
	discoveryStore, synchronizer := newLocalServingRuntime(t, states, reconciler)
	resolver, err := NewResolver("worker-a.internal:7443", agent)
	if err != nil {
		t.Fatalf("NewResolver() error = %v", err)
	}
	gateway, err := gatewayinvoke.New(gatewayinvoke.Config{
		Discovery: discoveryStore,
		Resolver:  resolver,
	})
	if err != nil {
		t.Fatalf("gatewayinvoke.New() error = %v", err)
	}

	if err := reconciler.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if err := synchronizer.FullSync(t.Context()); err != nil {
		t.Fatalf("FullSync() error = %v", err)
	}
	result, err := gateway.Invoke(t.Context(), gatewayinvoke.Request{
		FunctionID:  state.Function.ID,
		AffinityKey: []byte("request-a"),
		Invocation:  localABIRequest("inv-local-runtime", []byte("local-runtime")),
		Timeout:     2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Gateway.Invoke() error = %v", err)
	}
	fields := strings.Split(string(result.Execution.Response.Body), "|")
	if result.DiscoveryEpoch != connection.DiscoveryEpoch || result.VersionID != state.Version.VersionID ||
		result.Endpoint.Assignment != assignment.Identity() ||
		len(fields) != 6 || fields[0] != "1" || fields[5] != "local-runtime" {
		t.Fatalf("Gateway.Invoke() result = %+v, response = %q", result, result.Execution.Response.Body)
	}

	assignment.DesiredState = controlplane.AssignmentCancelled
	assignment.ResourceRevision++
	assignments.Set([]controlplane.AssignmentRecord{assignment})
	if err := reconciler.Reconcile(t.Context()); err != nil {
		t.Fatalf("cancel Reconcile() error = %v", err)
	}
	if err := synchronizer.FullSync(t.Context()); err != nil {
		t.Fatalf("cancel FullSync() error = %v", err)
	}
	_, err = gateway.Invoke(t.Context(), gatewayinvoke.Request{
		FunctionID:  state.Function.ID,
		AffinityKey: []byte("request-b"),
		Invocation:  localABIRequest("inv-local-runtime-cancelled", []byte("blocked")),
		Timeout:     2 * time.Second,
	})
	var invocationProblem *problem.Error
	if !errors.As(err, &invocationProblem) || invocationProblem.Code != problem.CodeNoReadyReplica {
		t.Fatalf("cancelled Gateway.Invoke() error = %v, want no_ready_replica", err)
	}
}

func newLocalServingRuntime(
	t *testing.T,
	states *servingStateSource,
	reconciler *Reconciler,
) (*gatewaydiscovery.Store, *localserving.Synchronizer) {
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
	synchronizer, err := localserving.New(localserving.Config{
		States: states, Candidates: reconciler, Publisher: publisher, Store: store,
	})
	if err != nil {
		t.Fatalf("localserving.New() error = %v", err)
	}
	return store, synchronizer
}

func localRuntimeState(
	t *testing.T,
	session servingauth.WorkerSession,
	wasm []byte,
) (controlplane.ServingState, controlplane.AssignmentRecord) {
	t.Helper()
	state, assignment := validReconcileState(t, session)
	artifactDigest := digest.Sum(wasm)
	state.Version.ArtifactDigest = artifactDigest
	state.Version.ArtifactSize = int64(len(wasm))
	state.Version.ResourceRequest.MemoryMiB = 128
	state.Deployment.ResourceLimits.MemoryMiB = 128
	policy := model.EffectivePolicy{
		VersionID:             state.Version.VersionID,
		AdmissionEpoch:        state.Version.AdmissionEpoch,
		DeploymentGeneration:  state.Deployment.Generation,
		ArtifactDigest:        artifactDigest,
		ArtifactSize:          int64(len(wasm)),
		ABI:                   state.Version.ABI,
		HostAPIProfile:        state.Version.HostAPIProfile,
		RuntimeFeatureProfile: state.Version.RuntimeFeatureProfile,
		ResourceLimits:        state.Deployment.ResourceLimits,
		MaxConcurrency:        state.Version.ResourceRequest.MaxConcurrency,
		GrantedCapabilities:   []model.CapabilityRequest{},
	}
	policyDigest, err := policy.Digest()
	if err != nil {
		t.Fatalf("EffectivePolicy.Digest() error = %v", err)
	}
	state.Deployment.EffectivePolicyDigest = policyDigest
	state.Route.Targets[0].EffectivePolicyDigest = policyDigest
	assignment.Placement.ArtifactDigest = artifactDigest
	assignment.Placement.ArtifactSize = int64(len(wasm))
	assignment.Placement.MemoryMiB = state.Deployment.ResourceLimits.MemoryMiB
	assignment.Placement.PolicyDigest = policyDigest
	return state, assignment
}

func localABIRequest(invocationID string, body []byte) abi.Request {
	return abi.Request{
		SpecVersion: abi.Version, InvocationID: invocationID, Method: "POST", Path: "/invoke",
		Query: abi.Query{}, Headers: abi.RequestHeaders{}, Body: bytes.Clone(body),
		DeadlineUnixMS: time.Now().Add(2 * time.Second).UnixMilli(),
		Trigger:        abi.Trigger{Type: "http", ID: "trigger-a", ResourceRevision: 4},
	}
}

func buildLocalStandardGoFixture(t *testing.T) []byte {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repository root: %v", err)
	}
	wasmPath := filepath.Join(t.TempDir(), "runtime.wasm")
	command := exec.Command("go", "build", "-trimpath", "-o", wasmPath, "./test/fixtures/wasm/runtime")
	command.Dir = root
	command.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("building standard Go fixture: %v\n%s", err, output)
	}
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("reading standard Go fixture: %v", err)
	}
	return wasm
}
