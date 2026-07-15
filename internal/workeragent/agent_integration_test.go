//go:build integration

package workeragent

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmexec"
	"github.com/yourikka/minicloud/internal/wasmprofile"
	"github.com/yourikka/minicloud/internal/workercache"
)

func TestAgentPreparesAndInvokesStandardGoModule(t *testing.T) {
	wasm := buildStandardGoFixture(t)
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
	cache, err := workercache.New(workercache.Config{
		Artifacts: store,
		Compiler:  engine,
	})
	if err != nil {
		_ = engine.Close(context.Background())
		_ = store.Close()
		t.Fatalf("workercache.New() error = %v", err)
	}
	agent, err := New(Config{
		Cache: cache,
		Authorization: servingauth.Config{
			Worker: servingauth.WorkerProcess{WorkerID: "worker-1", BootID: "boot-1"},
		},
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

	module := workercache.ModuleSpec{
		ArtifactDigest:        digest.Sum(wasm),
		ArtifactSize:          int64(len(wasm)),
		ABI:                   model.ABIWASICommandV1,
		HostAPIProfile:        model.HostAPIProfileNone,
		RuntimeFeatureProfile: wasmprofile.FeatureProfile,
	}
	if _, err := store.Put(context.Background(), module.ArtifactDigest, bytes.NewReader(wasm)); err != nil {
		t.Fatalf("Store.Put() error = %v", err)
	}
	policy := model.EffectivePolicy{
		VersionID:             "version-1",
		AdmissionEpoch:        3,
		DeploymentGeneration:  4,
		ArtifactDigest:        module.ArtifactDigest,
		ArtifactSize:          module.ArtifactSize,
		ABI:                   module.ABI,
		HostAPIProfile:        module.HostAPIProfile,
		RuntimeFeatureProfile: module.RuntimeFeatureProfile,
		ResourceLimits: model.ResourceLimits{
			Timeout:        2 * time.Second,
			MemoryMiB:      128,
			MaxInputBytes:  1024,
			MaxOutputBytes: 1024,
			MaxLogBytes:    128,
		},
		MaxConcurrency:      2,
		GrantedCapabilities: []model.CapabilityRequest{},
	}
	policyDigest, err := policy.Digest()
	if err != nil {
		t.Fatalf("EffectivePolicy.Digest() error = %v", err)
	}
	connection := testConnection(1, 10)
	if err := agent.AcceptControl(connection); err != nil {
		t.Fatalf("AcceptControl() error = %v", err)
	}
	fence := servingauth.InvocationFence{
		Assignment: servingauth.AssignmentIdentity{
			Worker: servingauth.WorkerSession{
				WorkerID:     "worker-1",
				BootID:       "boot-1",
				SessionEpoch: connection.SessionEpoch,
			},
			AssignmentID:         "assignment-1",
			VersionID:            policy.VersionID,
			AdmissionEpoch:       policy.AdmissionEpoch,
			DeploymentGeneration: policy.DeploymentGeneration,
			PolicyDigest:         policyDigest,
			Mode:                 servingauth.ModeNormal,
		},
		DiscoveryEpoch: connection.DiscoveryEpoch,
	}
	observation, err := agent.Prepare(context.Background(), PrepareRequest{
		Connection: connection,
		Fence:      fence,
		Module:     module,
		Policy:     policy,
	})
	if err != nil || observation.State != ReplicaReady {
		t.Fatalf("Prepare() = (%+v, %v), want Ready", observation, err)
	}
	installTTL(t, agent, connection, fence, time.Minute)

	request := testABIRequest([]byte("worker-agent"))
	result, err := agent.Invoke(context.Background(), fence, request, 2*time.Second)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	fields := strings.Split(string(result.Response.Body), "|")
	if len(fields) != 6 || fields[0] != "1" || fields[5] != "worker-agent" {
		t.Fatalf("response body = %q, want fresh standard Go guest response", result.Response.Body)
	}

	request.InvocationID = "inv-worker-agent-output"
	request.Body = []byte("output")
	_, err = agent.Invoke(context.Background(), fence, request, 2*time.Second)
	assertProblemCode(t, err, problem.CodeOutputLimit)

	request.InvocationID = "inv-worker-agent-stderr"
	request.Body = []byte("stderr")
	result, err = agent.Invoke(context.Background(), fence, request, 2*time.Second)
	if err != nil {
		t.Fatalf("stderr Invoke() error = %v", err)
	}
	if len(result.GuestLog) > int(policy.ResourceLimits.MaxLogBytes) || result.DroppedLogBytes == 0 {
		t.Fatalf("policy log bounds = stored %d, dropped %d", len(result.GuestLog), result.DroppedLogBytes)
	}
}

func buildStandardGoFixture(t *testing.T) []byte {
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
