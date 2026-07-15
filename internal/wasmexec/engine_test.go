package wasmexec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/problem"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

func TestProgramInvokeWithAcceptanceRejectsNilHook(t *testing.T) {
	t.Parallel()
	program := &Program{}

	_, err := program.InvokeWithAcceptance(
		context.Background(),
		abi.Request{},
		InvocationPolicy{Timeout: time.Second},
		InvocationAcceptance{},
	)
	if err == nil || err.Error() != "wasmexec invocation acceptance is required" {
		t.Fatalf("InvokeWithAcceptance() error = %v, want required acceptance error", err)
	}
}

func TestCompileRejectsIncompatibleCommandModules(t *testing.T) {
	engine, err := New(context.Background(), Config{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("Engine.Close() error = %v", err)
		}
	}()

	for _, wasm := range [][]byte{moduleWithStartSection(), moduleWithUnknownImport()} {
		_, _, err = engine.Compile(context.Background(), wasm)
		assertProblemCode(t, err, problem.CodeInvalidModule)
	}
}

func TestLimiterRejectsAFullQueue(t *testing.T) {
	limiter := newLimiter(1, 1)
	if err := limiter.acquire(context.Background(), nil, nil); err != nil {
		t.Fatalf("first acquire error = %v", err)
	}
	defer limiter.release()

	queuedContext, cancelQueued := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() { queued <- limiter.acquire(queuedContext, nil, nil) }()
	deadline := time.Now().Add(time.Second)
	for len(limiter.queue) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(limiter.queue) != 1 {
		t.Fatal("second acquire did not enter the bounded queue")
	}
	err := limiter.acquire(context.Background(), nil, nil)
	assertProblemCode(t, err, problem.CodeOverloaded)
	cancelQueued()
	assertProblemCode(t, <-queued, problem.CodeFunctionTimeout)
}

func TestLimiterWakesWhenPreAcceptanceStops(t *testing.T) {
	limiter := newLimiter(1, 1)
	if err := limiter.acquire(context.Background(), nil, nil); err != nil {
		t.Fatalf("first acquire error = %v", err)
	}
	defer limiter.release()

	stop := make(chan struct{})
	queued := make(chan error, 1)
	go func() { queued <- limiter.acquire(context.Background(), stop, nil) }()
	deadline := time.Now().Add(time.Second)
	for len(limiter.queue) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(limiter.queue) != 1 {
		t.Fatal("second acquire did not enter the bounded queue")
	}
	close(stop)
	if err := <-queued; !errors.Is(err, ErrAcceptanceStopped) {
		t.Fatalf("queued acquire error = %v, want acceptance stopped", err)
	}
}

func TestExecutionLimitsExposeRuntimeAdmissionCapacity(t *testing.T) {
	engine, err := New(context.Background(), Config{
		MaxConcurrent:           7,
		MaxConcurrentPerProgram: 3,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() {
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("Engine.Close() error = %v", err)
		}
	}()

	limits := engine.ExecutionLimits()
	if limits.MaxConcurrent != 7 || limits.MaxConcurrentPerProgram != 3 {
		t.Fatalf(
			"ExecutionLimits() concurrency = global %d, program %d; want 7 and 3",
			limits.MaxConcurrent,
			limits.MaxConcurrentPerProgram,
		)
	}
}

func TestLifecycleCloseWaitsWithContextAndRetriesRelease(t *testing.T) {
	t.Parallel()
	lifecycle := newLifecycle()
	if !lifecycle.begin() {
		t.Fatal("begin() rejected an open lifecycle")
	}
	shortContext, cancel := context.WithCancel(context.Background())
	cancel()
	releases := 0
	releaseErr := errors.New("release failed")
	err := lifecycle.close(shortContext, func(context.Context) error {
		releases++
		return nil
	})
	if !errors.Is(err, context.Canceled) || releases != 0 {
		t.Fatalf("first close = (%v, releases %d), want canceled wait", err, releases)
	}
	if lifecycle.begin() {
		t.Fatal("begin() accepted work after closing started")
	}
	lifecycle.end()
	err = lifecycle.close(context.Background(), func(context.Context) error {
		releases++
		return releaseErr
	})
	if !errors.Is(err, releaseErr) {
		t.Fatalf("second close error = %v, want release failure", err)
	}
	err = lifecycle.close(context.Background(), func(context.Context) error {
		releases++
		return nil
	})
	if err != nil || releases != 2 {
		t.Fatalf("retry close = (%v, releases %d), want success after two releases", err, releases)
	}
}

func TestOutputAndGuestLogWriters(t *testing.T) {
	t.Parallel()
	canceled := false
	output := newBoundedOutput(3, func() { canceled = true })
	if written, err := output.Write([]byte("abc")); written != 3 || err != nil {
		t.Fatalf("exact Write() = (%d, %v)", written, err)
	}
	if written, err := output.Write([]byte("d")); written != 0 || !errors.Is(err, errOutputLimit) {
		t.Fatalf("overflow Write() = (%d, %v)", written, err)
	}
	if !canceled || !output.Exceeded() || string(output.Bytes()) != "abc" {
		t.Fatalf("output state = canceled %t, exceeded %t, bytes %q", canceled, output.Exceeded(), output.Bytes())
	}

	logs := newGuestLog(8, 3)
	written, err := logs.Write([]byte("abcdef\nxy"))
	if written != 9 || err != nil || string(logs.Bytes()) != "abc\nxy" || logs.Dropped() != 3 {
		t.Fatalf("guest log = (%d, %v, %q, dropped %d)", written, err, logs.Bytes(), logs.Dropped())
	}
}

func TestInvocationUsesEarlierParentDeadline(t *testing.T) {
	t.Parallel()
	parent, cancelParent := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelParent()
	ctx, cancel := isolatedContext(parent, time.Second)
	defer cancel()
	deadline, exists := ctx.Deadline()
	if !exists || time.Until(deadline) > 100*time.Millisecond {
		t.Fatalf("isolated deadline = %v, want earlier parent deadline", deadline)
	}
	select {
	case <-ctx.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("isolated context did not follow parent cancellation")
	}
}

func TestConfigAllowsLowerGlobalConcurrencyWithProgramDefaults(t *testing.T) {
	t.Parallel()
	config, err := normalizeConfig(Config{MaxConcurrent: 1, MaxQueue: 1})
	if err != nil {
		t.Fatalf("normalizeConfig() error = %v", err)
	}
	if config.MaxConcurrentPerProgram != 1 || config.MaxQueuePerProgram != 1 {
		t.Fatalf(
			"program defaults = concurrent %d, queue %d; want 1 and 1",
			config.MaxConcurrentPerProgram,
			config.MaxQueuePerProgram,
		)
	}
}

func assertProblemCode(t *testing.T, err error, want problem.Code) {
	t.Helper()
	var problemErr *problem.Error
	if !errors.As(err, &problemErr) || problemErr.Code != want {
		t.Fatalf("error = %v, want problem code %q", err, want)
	}
}

func moduleWithStartSection() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x0a, 0x01, 0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x00,
		0x08, 0x01, 0x00,
		0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b,
	}
}

func moduleWithUnknownImport() []byte {
	return []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		0x02, 0x0c, 0x01, 0x04, 0x65, 0x76, 0x69, 0x6c, 0x03, 0x70, 0x77, 0x6e, 0x00, 0x00,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x0a, 0x01, 0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x01,
		0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b,
	}
}
