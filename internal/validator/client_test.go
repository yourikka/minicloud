package validator

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/validator/protocol"
)

func TestClientWithRealValidatorAndStandardGoModule(t *testing.T) {
	root := repositoryRoot(t)
	binDirectory := t.TempDir()
	validatorPath := filepath.Join(binDirectory, executableName("minicloud-validator"))
	buildCommand(t, root, nil, validatorPath, "./cmd/minicloud-validator")
	assertBinaryDependencyVersion(t, validatorPath, "github.com/tetratelabs/wazero", protocol.RuntimeVersion)
	wasmPath := filepath.Join(binDirectory, "echo.wasm")
	buildCommand(t, root, []string{"GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0"}, wasmPath, "./test/fixtures/wasm/echo")
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", wasmPath, err)
	}

	tempRoot := filepath.Join(t.TempDir(), "validator-temp")
	client, err := New(Config{
		Command:          validatorPath,
		TempRoot:         tempRoot,
		Deadline:         10 * time.Second,
		MaxConcurrent:    2,
		MaxArtifactBytes: int64(len(wasm)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	report, err := client.Validate(context.Background(), requestFor("standard-go", wasm), readCloser(wasm))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !report.Valid || report.Code != protocol.CodeOK {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(report.Imports) == 0 || !slices.Contains(report.Exports, "_start") {
		t.Fatalf("standard Go module metadata is incomplete: %+v", report)
	}
	if !report.Isolation.ProcessBoundary || !report.Isolation.Deadline {
		t.Fatalf("process watchdog was not reported: %+v", report.Isolation)
	}
	if report.Isolation.MemoryLimit || report.Isolation.TempDiskLimit {
		t.Fatalf("unenforced hard limits must not be claimed: %+v", report.Isolation)
	}
	if runtime.GOOS == "linux" && (!report.Isolation.CPULimit || !report.Isolation.FileSizeLimit) {
		t.Fatalf("linux rlimits were not reported: %+v", report.Isolation)
	}
	assertDirectoryEmpty(t, tempRoot)

	invalid := append([]byte(nil), wasm...)
	invalid[0] = 0xff
	rejected, err := client.Validate(context.Background(), requestFor("invalid", invalid), readCloser(invalid))
	if err != nil {
		t.Fatalf("Validate(invalid) error = %v", err)
	}
	if rejected.Valid || rejected.Reason != "compile_failed" {
		t.Fatalf("invalid module was not rejected: %+v", rejected)
	}
}

func TestClientWatchdogAndOutputBoundaries(t *testing.T) {
	root := repositoryRoot(t)
	helperPath := filepath.Join(t.TempDir(), executableName("validator-helper"))
	buildCommand(t, root, nil, helperPath, "./internal/validator/testdata/helper")

	t.Run("watchdog timeout", func(t *testing.T) {
		tempRoot := t.TempDir()
		client := newTestClient(t, helperPath, tempRoot, 1, time.Second)
		_, err := client.Validate(context.Background(), requestFor("sleep", []byte{1}), readCloser([]byte{1}))
		if !errors.Is(err, ErrTimedOut) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Validate() error = %v, want timeout classification", err)
		}
		assertDirectoryEmpty(t, tempRoot)
	})

	t.Run("caller cancellation", func(t *testing.T) {
		tempRoot := t.TempDir()
		client := newTestClient(t, helperPath, tempRoot, 1, 5*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			_, err := client.Validate(ctx, requestFor("sleep", []byte{1}), readCloser([]byte{1}))
			result <- err
		}()
		waitForTempDirectories(t, tempRoot, 1)
		cancel()
		err := <-result
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimedOut) {
			t.Fatalf("Validate() error = %v, want caller cancellation only", err)
		}
		assertDirectoryEmpty(t, tempRoot)
	})

	t.Run("immediate overload", func(t *testing.T) {
		tempRoot := t.TempDir()
		client := newTestClient(t, helperPath, tempRoot, 2, 5*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var group sync.WaitGroup
		results := make(chan error, 2)
		for _, validationID := range []string{"sleep-one", "sleep-two"} {
			group.Add(1)
			go func() {
				defer group.Done()
				_, err := client.Validate(ctx, requestFor(validationID, []byte{1}), readCloser([]byte{1}))
				results <- err
			}()
		}
		waitForTempDirectories(t, tempRoot, 2)
		_, err := client.Validate(ctx, requestFor("third", []byte{1}), readCloser([]byte{1}))
		if !errors.Is(err, ErrOverloaded) {
			t.Fatalf("Validate() error = %v, want ErrOverloaded", err)
		}
		cancel()
		group.Wait()
		close(results)
		for err := range results {
			if !errors.Is(err, context.Canceled) {
				t.Errorf("background Validate() error = %v, want context.Canceled", err)
			}
		}
		assertDirectoryEmpty(t, tempRoot)
	})

	t.Run("blocked artifact reader is canceled", func(t *testing.T) {
		tempRoot := t.TempDir()
		client := newTestClient(t, helperPath, tempRoot, 1, 5*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		artifactReader, artifactWriter := io.Pipe()
		result := make(chan error, 1)
		go func() {
			_, err := client.Validate(ctx, requestFor("sleep", []byte{1}), artifactReader)
			result <- err
		}()
		waitForTempDirectories(t, tempRoot, 1)
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Validate() error = %v, want context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Validate() did not release a blocked artifact reader")
		}
		_ = artifactWriter.Close()
		assertDirectoryEmpty(t, tempRoot)
		_, err := client.Validate(context.Background(), requestFor("invalid-report", []byte{1}), readCloser([]byte{1}))
		if !errors.Is(err, ErrInvalidReport) {
			t.Fatalf("validator slot was not reusable: %v", err)
		}
	})

	t.Run("invalid report", func(t *testing.T) {
		client := newTestClient(t, helperPath, t.TempDir(), 1, 5*time.Second)
		_, err := client.Validate(context.Background(), requestFor("invalid-report", []byte{1}), readCloser([]byte{1}))
		if !errors.Is(err, ErrInvalidReport) {
			t.Fatalf("Validate() error = %v, want ErrInvalidReport", err)
		}
	})

	t.Run("stdout limit", func(t *testing.T) {
		client := newTestClient(t, helperPath, t.TempDir(), 1, 5*time.Second)
		_, err := client.Validate(context.Background(), requestFor("output-limit", []byte{1}), readCloser([]byte{1}))
		if !errors.Is(err, ErrOutputLimit) {
			t.Fatalf("Validate() error = %v, want ErrOutputLimit", err)
		}
	})

	t.Run("stderr is not exposed", func(t *testing.T) {
		client := newTestClient(t, helperPath, t.TempDir(), 1, 5*time.Second)
		_, err := client.Validate(context.Background(), requestFor("crash", []byte{1}), readCloser([]byte{1}))
		if !errors.Is(err, ErrProcessFailed) {
			t.Fatalf("Validate() error = %v, want ErrProcessFailed", err)
		}
		if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "artifact.wasm") {
			t.Fatalf("child stderr leaked through error: %v", err)
		}
	})
}

func TestLimitedBuffer(t *testing.T) {
	t.Parallel()
	buffer := newLimitedBuffer(3)
	written, err := buffer.Write([]byte("abcd"))
	if written != 3 || !errors.Is(err, ErrOutputLimit) || string(buffer.Bytes()) != "abc" {
		t.Fatalf("Write() = (%d, %v, %q), want bounded partial write", written, err, buffer.Bytes())
	}
	if written, err = buffer.Write([]byte("x")); written != 0 || !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("second Write() = (%d, %v), want sticky limit error", written, err)
	}
}

func TestClientRejectsArtifactOverConfiguredLimitBeforeStartingChild(t *testing.T) {
	t.Parallel()
	client := &Client{
		maxArtifactBytes: 1,
		slots:            make(chan struct{}, 1),
	}
	request := requestFor("too-large", []byte{1, 2})
	_, err := client.Validate(context.Background(), request, readCloser([]byte{1, 2}))
	if err == nil || !strings.Contains(err.Error(), "exceeds client limit") {
		t.Fatalf("Validate() error = %v, want client size rejection", err)
	}
	if len(client.slots) != 0 {
		t.Fatal("preflight rejection acquired a validator slot")
	}
}

func newTestClient(t *testing.T, command, tempRoot string, concurrent int, deadline time.Duration) *Client {
	t.Helper()
	client, err := New(Config{
		Command:          command,
		TempRoot:         tempRoot,
		Deadline:         deadline,
		MaxConcurrent:    concurrent,
		MaxArtifactBytes: 1024,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

func requestFor(validationID string, artifact []byte) protocol.Request {
	return protocol.Request{
		SchemaVersion:         protocol.SchemaVersion,
		ValidationID:          validationID,
		ArtifactDigest:        digest.Sum(artifact),
		ArtifactSize:          int64(len(artifact)),
		ABI:                   model.ABIWASICommandV1,
		HostAPIProfile:        model.HostAPIProfileNone,
		RuntimeFeatureProfile: protocol.FeatureProfile,
		RuntimeEngine:         protocol.EngineCompiler,
		MemoryLimitMiB:        128,
		RequestedCapabilities: []model.CapabilityRequest{},
	}
}

func readCloser(data []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(data))
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repository root: %v", err)
	}
	return root
}

func buildCommand(t *testing.T, root string, extraEnvironment []string, output, packagePath string) {
	t.Helper()
	command := exec.Command("go", "build", "-trimpath", "-o", output, packagePath)
	command.Dir = root
	command.Env = append(os.Environ(), extraEnvironment...)
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", packagePath, err, combined)
	}
}

func assertBinaryDependencyVersion(t *testing.T, binaryPath, dependencyPath, wantVersion string) {
	t.Helper()
	info, err := buildinfo.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("reading build info from %q: %v", binaryPath, err)
	}
	for _, dependency := range info.Deps {
		if dependency.Path == dependencyPath {
			if dependency.Version != wantVersion {
				t.Fatalf("%s version = %q, want %q", dependencyPath, dependency.Version, wantVersion)
			}
			return
		}
	}
	t.Fatalf("%s is absent from validator build metadata", dependencyPath)
}

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func waitForTempDirectories(t *testing.T, root string, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(root)
		if err == nil && len(entries) >= count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("validator did not create %d temporary directories", count)
}

func assertDirectoryEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", root, err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary root contains %d entries after validation", len(entries))
	}
}
