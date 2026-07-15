//go:build integration

package wasmexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/problem"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

func TestProgramInvokesStandardGoWithFreshStateAndNoHostInheritance(t *testing.T) {
	engine, program := openStandardGoProgram(t, Config{})
	defer closeEngineAndProgram(t, engine, program)

	for range 2 {
		result, err := program.Invoke(context.Background(), validRequest("echo"), time.Second)
		if err != nil {
			t.Fatalf("Invoke() error = %v", err)
		}
		fields := strings.Split(string(result.Response.Body), "|")
		if len(fields) != 6 {
			t.Fatalf("response body = %q, want six fixture fields", result.Response.Body)
		}
		if fields[0] != "1" {
			t.Fatalf("guest global count = %q, want fresh value 1", fields[0])
		}
		if fields[1] != "0" || fields[2] != "0" {
			t.Fatalf("guest inherited args or environment: args=%s env=%s", fields[1], fields[2])
		}
		if fields[3] != "true" {
			t.Fatal("guest unexpectedly read a host filesystem path")
		}
		if fields[4] != "true" || fields[5] != "echo" {
			t.Fatalf("deadline/body fields = %q|%q, want true|echo", fields[4], fields[5])
		}
		if len(result.GuestLog) != 0 || result.DroppedLogBytes != 0 {
			t.Fatalf("healthy call produced guest logs: %+v", result)
		}
	}
}

func TestProgramInvokeWithAcceptanceCallsHookOnce(t *testing.T) {
	engine, program := openStandardGoProgram(t, Config{})
	defer closeEngineAndProgram(t, engine, program)

	acceptCalls := 0
	result, err := program.InvokeWithAcceptance(
		context.Background(),
		validRequest("accepted"),
		InvocationPolicy{Timeout: time.Second},
		InvocationAcceptance{
			Check: func() error {
				acceptCalls++
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("InvokeWithAcceptance() error = %v", err)
	}
	if acceptCalls != 1 {
		t.Fatalf("accept calls = %d, want 1", acceptCalls)
	}
	if !strings.HasPrefix(string(result.Response.Body), "1|") {
		t.Fatalf("response body = %q, want healthy fixture response", result.Response.Body)
	}
}

func TestProgramInvokeWithAcceptanceRejectsCanceledParentBeforeHook(t *testing.T) {
	engine, program := openStandardGoProgram(t, Config{})
	defer closeEngineAndProgram(t, engine, program)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	acceptCalls := 0
	_, err := program.InvokeWithAcceptance(
		ctx,
		validRequest("canceled"),
		InvocationPolicy{Timeout: time.Second},
		InvocationAcceptance{
			Check: func() error {
				acceptCalls++
				return nil
			},
		},
	)
	assertProblemCode(t, err, problem.CodeFunctionTimeout)
	if acceptCalls != 0 {
		t.Fatalf("accept calls after parent cancellation = %d, want 0", acceptCalls)
	}
}

func TestProgramInvokeWithAcceptanceStopsWhileWaitingForRuntimePermit(t *testing.T) {
	engine, program := openStandardGoProgram(t, Config{
		MaxConcurrent:           2,
		MaxConcurrentPerProgram: 1,
	})
	defer closeEngineAndProgram(t, engine, program)

	blockerContext, cancelBlocker := context.WithCancel(context.Background())
	defer cancelBlocker()
	blockerDone := make(chan error, 1)
	go func() {
		_, err := program.Invoke(blockerContext, validRequest("sleep"), 5*time.Second)
		blockerDone <- err
	}()
	waitForCount(t, func() int { return len(program.limiter.slots) }, 1)

	stop := make(chan struct{})
	var acceptCalls atomic.Int32
	queuedDone := make(chan error, 1)
	go func() {
		_, err := program.InvokeWithAcceptance(
			context.Background(),
			validRequest("queued"),
			InvocationPolicy{Timeout: 5 * time.Second},
			InvocationAcceptance{
				Stop: stop,
				Check: func() error {
					acceptCalls.Add(1)
					return nil
				},
			},
		)
		queuedDone <- err
	}()
	waitForCount(t, func() int { return len(program.limiter.queue) }, 1)
	close(stop)
	if err := <-queuedDone; !errors.Is(err, ErrAcceptanceStopped) {
		t.Fatalf("queued InvokeWithAcceptance() error = %v, want acceptance stopped", err)
	}
	if calls := acceptCalls.Load(); calls != 0 {
		t.Fatalf("accept calls after admission stop = %d, want 0", calls)
	}

	cancelBlocker()
	assertProblemCode(t, <-blockerDone, problem.CodeFunctionTimeout)
}

func TestProgramInvokeWithAcceptanceRunsAfterProgramAndWorkerPermits(t *testing.T) {
	wasm := buildStandardGoFixture(t)
	engine, err := New(context.Background(), Config{
		MaxConcurrent:           1,
		MaxQueue:                1,
		MaxConcurrentPerProgram: 1,
		MaxQueuePerProgram:      1,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	programA, _, err := engine.Compile(context.Background(), wasm)
	if err != nil {
		_ = engine.Close(context.Background())
		t.Fatalf("Compile(A) error = %v", err)
	}
	programB, _, err := engine.Compile(context.Background(), wasm)
	if err != nil {
		_ = programA.Close(context.Background())
		_ = engine.Close(context.Background())
		t.Fatalf("Compile(B) error = %v", err)
	}
	defer func() {
		if err := programA.Close(context.Background()); err != nil {
			t.Errorf("Program A Close() error = %v", err)
		}
		if err := programB.Close(context.Background()); err != nil {
			t.Errorf("Program B Close() error = %v", err)
		}
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("Engine Close() error = %v", err)
		}
	}()

	workerContext, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	workerDone := make(chan error, 1)
	go func() {
		_, err := programB.Invoke(workerContext, validRequest("sleep"), 5*time.Second)
		workerDone <- err
	}()
	waitForCount(t, func() int { return len(programB.limiter.slots) }, 1)
	waitForCount(t, func() int { return len(engine.limiter.slots) }, 1)

	programContext, cancelProgram := context.WithCancel(context.Background())
	defer cancelProgram()
	programDone := make(chan error, 1)
	go func() {
		_, err := programA.Invoke(programContext, validRequest("queued"), 5*time.Second)
		programDone <- err
	}()
	waitForCount(t, func() int { return len(programA.limiter.slots) }, 1)
	waitForCount(t, func() int { return len(engine.limiter.queue) }, 1)

	rejection := errors.New("invocation rejected at acceptance")
	var acquireCalls atomic.Int32
	var releaseCalls atomic.Int32
	var acceptCalls atomic.Int32
	type invocationOutcome struct {
		result Result
		err    error
	}
	acceptContext, cancelAccept := context.WithCancel(context.Background())
	defer cancelAccept()
	acceptDone := make(chan invocationOutcome, 1)
	go func() {
		result, err := programA.InvokeWithAcceptance(
			acceptContext,
			validRequest("rejected"),
			InvocationPolicy{Timeout: 5 * time.Second},
			InvocationAcceptance{
				Acquire: func() (func(), error) {
					acquireCalls.Add(1)
					return func() { releaseCalls.Add(1) }, nil
				},
				Check: func() error {
					acceptCalls.Add(1)
					return rejection
				},
			},
		)
		acceptDone <- invocationOutcome{result: result, err: err}
	}()
	waitForCount(t, func() int { return len(programA.limiter.queue) }, 1)
	if calls := acceptCalls.Load(); calls != 0 {
		t.Fatalf("accept calls while waiting for Program permit = %d, want 0", calls)
	}
	if calls := acquireCalls.Load(); calls != 0 {
		t.Fatalf("admission acquire calls while waiting for Program permit = %d, want 0", calls)
	}

	cancelProgram()
	assertProblemCode(t, <-programDone, problem.CodeFunctionTimeout)
	waitForCount(t, func() int { return len(programA.limiter.queue) }, 0)
	waitForCount(t, func() int { return len(programA.limiter.slots) }, 1)
	waitForCount(t, func() int { return len(engine.limiter.queue) }, 1)
	if calls := acceptCalls.Load(); calls != 0 {
		t.Fatalf("accept calls while waiting for Worker permit = %d, want 0", calls)
	}
	if calls := acquireCalls.Load(); calls != 1 {
		t.Fatalf("admission acquire calls after Program permit = %d, want 1", calls)
	}

	cancelWorker()
	assertProblemCode(t, <-workerDone, problem.CodeFunctionTimeout)
	outcome := <-acceptDone
	if outcome.err != rejection {
		t.Fatalf("InvokeWithAcceptance() error = %v, want original rejection %v", outcome.err, rejection)
	}
	if calls := acceptCalls.Load(); calls != 1 {
		t.Fatalf("accept calls = %d, want 1", calls)
	}
	if calls := releaseCalls.Load(); calls != 1 {
		t.Fatalf("admission release calls = %d, want 1", calls)
	}
	if outcome.result.Metrics.Instantiate != 0 || outcome.result.Metrics.Execute != 0 {
		t.Fatalf(
			"rejected invocation metrics = instantiate %s, execute %s; want both zero",
			outcome.result.Metrics.Instantiate,
			outcome.result.Metrics.Execute,
		)
	}

	healthy, healthyErr := programA.Invoke(context.Background(), validRequest("healthy"), time.Second)
	if healthyErr != nil || !strings.HasPrefix(string(healthy.Response.Body), "1|") {
		t.Fatalf("healthy call after rejection = (%q, %v)", healthy.Response.Body, healthyErr)
	}
}

func TestProgramContainsGuestFailuresAndRemainsReusable(t *testing.T) {
	engine, program := openStandardGoProgram(t, Config{})
	defer closeEngineAndProgram(t, engine, program)

	tests := []struct {
		name    string
		mode    string
		timeout time.Duration
		code    problem.Code
	}{
		{name: "invalid response", mode: "invalid", timeout: time.Second, code: problem.CodeInvalidFunctionResponse},
		{name: "non-zero exit", mode: "exit", timeout: time.Second, code: problem.CodeFunctionTrap},
		{name: "panic", mode: "panic", timeout: time.Second, code: problem.CodeFunctionTrap},
		{name: "output overflow", mode: "output", timeout: time.Second, code: problem.CodeOutputLimit},
		{name: "infinite loop", mode: "loop", timeout: 50 * time.Millisecond, code: problem.CodeFunctionTimeout},
		{name: "cancellable sleep", mode: "sleep", timeout: 50 * time.Millisecond, code: problem.CodeFunctionTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			started := time.Now()
			result, err := program.Invoke(context.Background(), validRequest(test.mode), test.timeout)
			assertProblemCode(t, err, test.code)
			if time.Since(started) > 750*time.Millisecond && test.timeout < time.Second {
				t.Fatalf("deadline call took %s", time.Since(started))
			}
			if strings.Contains(err.Error(), "fixture panic") || strings.Contains(err.Error(), "wasm stack") {
				t.Fatalf("guest diagnostic leaked through public error: %v", err)
			}
			if test.mode == "panic" && len(result.GuestLog) == 0 {
				t.Fatal("guest panic stderr was not captured separately")
			}

			healthy, healthyErr := program.Invoke(context.Background(), validRequest("healthy"), time.Second)
			if healthyErr != nil || !strings.HasPrefix(string(healthy.Response.Body), "1|") {
				t.Fatalf("healthy call after %s = (%q, %v)", test.mode, healthy.Response.Body, healthyErr)
			}
		})
	}
}

func TestGuestLogIsBoundedWithoutFailingInvocation(t *testing.T) {
	engine, program := openStandardGoProgram(t, Config{
		MaxLogBytes:     1024,
		MaxLogLineBytes: 128,
	})
	defer closeEngineAndProgram(t, engine, program)

	result, err := program.Invoke(context.Background(), validRequest("stderr"), time.Second)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if len(result.GuestLog) > 1024 || result.DroppedLogBytes == 0 {
		t.Fatalf("guest log bounds = stored %d, dropped %d", len(result.GuestLog), result.DroppedLogBytes)
	}
	for _, line := range strings.Split(string(result.GuestLog), "\n") {
		if len(line) > 128 {
			t.Fatalf("stored guest log line has %d bytes, want <= 128", len(line))
		}
	}
}

func TestProgramRunsConcurrentFreshInstancesWithoutIOCrossTalk(t *testing.T) {
	engine, program := openStandardGoProgram(t, Config{
		MaxConcurrent:           4,
		MaxConcurrentPerProgram: 4,
	})
	defer closeEngineAndProgram(t, engine, program)

	const invocations = 20
	var group sync.WaitGroup
	errorsByInvocation := make(chan error, invocations)
	for index := range invocations {
		group.Add(1)
		go func() {
			defer group.Done()
			body := fmt.Sprintf("concurrent-%d", index)
			request := validRequest(body)
			request.InvocationID = fmt.Sprintf("inv_concurrent-%d", index)
			result, err := program.Invoke(context.Background(), request, 2*time.Second)
			if err != nil {
				errorsByInvocation <- fmt.Errorf("%s: %w", body, err)
				return
			}
			fields := strings.Split(string(result.Response.Body), "|")
			if len(fields) != 6 || fields[0] != "1" || fields[5] != body {
				errorsByInvocation <- fmt.Errorf("%s: unexpected response %q", body, result.Response.Body)
			}
		}()
	}
	group.Wait()
	close(errorsByInvocation)
	for err := range errorsByInvocation {
		t.Error(err)
	}
}

func TestProgramQueueDoesNotReserveIdleWorkerSlots(t *testing.T) {
	wasm := buildStandardGoFixture(t)
	engine, err := New(context.Background(), Config{
		MaxConcurrent:           4,
		MaxConcurrentPerProgram: 2,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	programA, _, err := engine.Compile(context.Background(), wasm)
	if err != nil {
		t.Fatalf("Compile(A) error = %v", err)
	}
	programB, _, err := engine.Compile(context.Background(), wasm)
	if err != nil {
		t.Fatalf("Compile(B) error = %v", err)
	}
	defer func() {
		if err := programA.Close(context.Background()); err != nil {
			t.Errorf("Program A Close() error = %v", err)
		}
		if err := programB.Close(context.Background()); err != nil {
			t.Errorf("Program B Close() error = %v", err)
		}
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("Engine Close() error = %v", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	var group sync.WaitGroup
	for index := range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			request := validRequest("sleep")
			request.InvocationID = fmt.Sprintf("inv_flood-%d", index)
			_, _ = programA.Invoke(ctx, request, 2*time.Second)
		}()
	}
	waitForCount(t, func() int { return len(programA.limiter.queue) }, 2)
	if occupied := len(engine.limiter.slots); occupied != 2 {
		t.Fatalf("Program A occupied %d Worker slots, want only its 2 active guests", occupied)
	}
	result, err := programB.Invoke(context.Background(), validRequest("healthy"), 500*time.Millisecond)
	if err != nil || !strings.HasPrefix(string(result.Response.Body), "1|") {
		t.Fatalf("healthy Program B call = (%q, %v)", result.Response.Body, err)
	}
	cancel()
	group.Wait()
}

func TestProgramCloseHonorsContextAndCanRetryAfterDrain(t *testing.T) {
	engine, program := openStandardGoProgram(t, Config{})
	defer func() {
		if err := program.Close(context.Background()); err != nil {
			t.Errorf("Program.Close() error = %v", err)
		}
		if err := engine.Close(context.Background()); err != nil {
			t.Errorf("Engine.Close() error = %v", err)
		}
	}()

	invocationContext, cancelInvocation := context.WithCancel(context.Background())
	invocationDone := make(chan error, 1)
	go func() {
		_, err := program.Invoke(invocationContext, validRequest("sleep"), 2*time.Second)
		invocationDone <- err
	}()
	waitForCount(t, func() int { return len(program.limiter.slots) }, 1)

	closeContext, cancelClose := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := program.Close(closeContext)
	cancelClose()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Program.Close() error = %v, want context deadline", err)
	}
	if _, err := program.Invoke(context.Background(), validRequest("rejected"), time.Second); err == nil {
		t.Fatal("Program.Invoke() accepted work after Close started")
	}

	cancelInvocation()
	assertProblemCode(t, <-invocationDone, problem.CodeFunctionTimeout)
	if err := program.Close(context.Background()); err != nil {
		t.Fatalf("Program.Close() retry error = %v", err)
	}
}

func TestEngineCloseHonorsContextAndCanRetryAfterDrain(t *testing.T) {
	engine, program := openStandardGoProgram(t, Config{})
	invocationContext, cancelInvocation := context.WithCancel(context.Background())
	invocationDone := make(chan error, 1)
	go func() {
		_, err := program.Invoke(invocationContext, validRequest("sleep"), 2*time.Second)
		invocationDone <- err
	}()
	waitForCount(t, func() int { return len(program.limiter.slots) }, 1)

	closeContext, cancelClose := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := engine.Close(closeContext)
	cancelClose()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Engine.Close() error = %v, want context deadline", err)
	}
	if _, _, err := engine.Compile(context.Background(), moduleWithStartSection()); err == nil {
		t.Fatal("Engine.Compile() accepted work after Close started")
	}

	cancelInvocation()
	assertProblemCode(t, <-invocationDone, problem.CodeFunctionTimeout)
	if err := engine.Close(context.Background()); err != nil {
		t.Fatalf("Engine.Close() retry error = %v", err)
	}
}

func openStandardGoProgram(t *testing.T, config Config) (*Engine, *Program) {
	t.Helper()
	wasm := buildStandardGoFixture(t)
	engine, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	program, _, err := engine.Compile(context.Background(), wasm)
	if err != nil {
		_ = engine.Close(context.Background())
		t.Fatalf("Compile() error = %v", err)
	}
	return engine, program
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

func closeEngineAndProgram(t *testing.T, engine *Engine, program *Program) {
	t.Helper()
	if err := program.Close(context.Background()); err != nil {
		t.Errorf("Program.Close() error = %v", err)
	}
	if err := engine.Close(context.Background()); err != nil {
		t.Errorf("Engine.Close() error = %v", err)
	}
}

func validRequest(body string) abi.Request {
	return abi.Request{
		SpecVersion:  abi.Version,
		InvocationID: "inv_runtime-test",
		Method:       "POST",
		Path:         "/runtime",
		Query:        abi.Query{},
		Headers:      abi.RequestHeaders{},
		Body:         []byte(body),
		Trigger:      abi.Trigger{Type: "http", ID: "trg_runtime-test"},
	}
}

func waitForCount(t *testing.T, current func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if current() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("count = %d, want %d", current(), want)
}
