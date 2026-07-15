// Package wasmexec executes admitted wasi-command-v1 modules inside a Worker.
package wasmexec

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/wasmprofile"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

const (
	DefaultTimeout                 = 5 * time.Second
	DefaultMaxTimeout              = 30 * time.Second
	DefaultMemoryLimitMiB          = uint32(128)
	DefaultMaxConcurrent           = 4
	DefaultMaxQueue                = 64
	DefaultMaxConcurrentPerProgram = 2
	DefaultMaxQueuePerProgram      = 16
	DefaultMaxLogBytes             = 256 << 10
	DefaultMaxLogLineBytes         = 16 << 10
)

var errOutputLimit = errors.New("wasm stdout exceeds limit")

// ErrAcceptanceStopped reports that a higher-level owner stopped admission
// before the invocation reached its acceptance point.
var ErrAcceptanceStopped = errors.New("wasmexec invocation acceptance stopped")

// Config defines one Worker's bounded runtime pool.
type Config struct {
	Engine                  string
	MemoryLimitMiB          uint32
	DefaultTimeout          time.Duration
	MaxTimeout              time.Duration
	MaxConcurrent           int
	MaxQueue                int
	MaxConcurrentPerProgram int
	MaxQueuePerProgram      int
	MaxLogBytes             int
	MaxLogLineBytes         int
	ABILimits               abi.Limits
	Random                  io.Reader
}

// Metrics separates queueing, compilation, instantiation, and guest execution.
type Metrics struct {
	Queue       time.Duration
	Compile     time.Duration
	Instantiate time.Duration
	Execute     time.Duration
}

// Result contains a validated ABI response and separately bounded guest logs.
type Result struct {
	Response        abi.Response
	GuestLog        []byte
	DroppedLogBytes int64
	Metrics         Metrics
}

// InvocationPolicy contains per-Assignment limits that may only tighten the
// Engine's immutable Worker-wide hard bounds.
type InvocationPolicy struct {
	Timeout         time.Duration
	RequestLimits   abi.Limits
	ResponseLimits  abi.Limits
	MaxLogBytes     int
	MaxLogLineBytes int
}

// InvocationAcceptance coordinates a higher-level admission boundary with the
// runtime queues. Acquire runs after the Program permit and before the Engine
// permit. Stop signals are observed only until Check succeeds, so revocation
// never cancels an already accepted guest.
type InvocationAcceptance struct {
	Stop     <-chan struct{}
	AlsoStop <-chan struct{}
	Acquire  func() (release func(), err error)
	Check    func() error
}

// ExecutionLimits is the immutable Worker runtime envelope applied before any
// per-Assignment policy may tighten it.
type ExecutionLimits struct {
	MaxTimeout              time.Duration
	MaxConcurrent           int
	MaxConcurrentPerProgram int
	ABILimits               abi.Limits
	MaxLogBytes             int
	MaxLogLineBytes         int
}

// Engine owns one wazero runtime and the shared WASI Preview 1 host module.
type Engine struct {
	runtime            wazero.Runtime
	wasi               wazero.CompiledModule
	profile            wasmprofile.Profile
	defaultTimeout     time.Duration
	maxTimeout         time.Duration
	abiLimits          abi.Limits
	maxLogBytes        int
	maxLogLineBytes    int
	maxConcurrent      int
	programConcurrency int
	programQueue       int
	random             io.Reader
	limiter            *limiter
	lifecycle          *lifecycle
}

// Program is a reusable compilation whose instances are never reused.
type Program struct {
	engine    *Engine
	compiled  wazero.CompiledModule
	limiter   *limiter
	lifecycle *lifecycle
}

// New initializes the locked WASI host profile without inheriting process
// arguments, environment variables, filesystems, or standard streams.
func New(ctx context.Context, config Config) (*Engine, error) {
	if ctx == nil {
		return nil, errors.New("wasmexec context is required")
	}
	config, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	runtimeConfig, err := wasmprofile.RuntimeConfig(config.Engine, config.MemoryLimitMiB)
	if err != nil {
		return nil, fmt.Errorf("configuring wasm runtime: %w", err)
	}
	runtimeInstance := wazero.NewRuntimeWithConfig(ctx, runtimeConfig)
	wasiCompiled, err := wasi_snapshot_preview1.NewBuilder(runtimeInstance).Compile(ctx)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("compiling locked wasi profile: %w", err),
			runtimeInstance.Close(context.WithoutCancel(ctx)),
		)
	}
	if _, err := runtimeInstance.InstantiateModule(ctx, wasiCompiled, wazero.NewModuleConfig()); err != nil {
		return nil, errors.Join(
			fmt.Errorf("instantiating locked wasi profile: %w", err),
			wasiCompiled.Close(context.WithoutCancel(ctx)),
			runtimeInstance.Close(context.WithoutCancel(ctx)),
		)
	}
	randomSource := config.Random
	if randomSource == nil {
		randomSource = cryptorand.Reader
	}
	return &Engine{
		runtime:            runtimeInstance,
		wasi:               wasiCompiled,
		profile:            wasmprofile.Profile{Engine: config.Engine, MemoryLimitMiB: config.MemoryLimitMiB},
		defaultTimeout:     config.DefaultTimeout,
		maxTimeout:         config.MaxTimeout,
		abiLimits:          config.ABILimits,
		maxLogBytes:        config.MaxLogBytes,
		maxLogLineBytes:    config.MaxLogLineBytes,
		maxConcurrent:      config.MaxConcurrent,
		programConcurrency: config.MaxConcurrentPerProgram,
		programQueue:       config.MaxQueuePerProgram,
		random:             &lockedReader{source: randomSource},
		limiter:            newLimiter(config.MaxConcurrent, config.MaxQueue),
		lifecycle:          newLifecycle(),
	}, nil
}

// Profile returns the immutable compilation profile owned by this Engine.
func (e *Engine) Profile() wasmprofile.Profile {
	return e.profile
}

// ExecutionLimits returns the immutable runtime hard bounds owned by Engine.
func (e *Engine) ExecutionLimits() ExecutionLimits {
	return ExecutionLimits{
		MaxTimeout:              e.maxTimeout,
		MaxConcurrent:           e.maxConcurrent,
		MaxConcurrentPerProgram: e.programConcurrency,
		ABILimits:               e.abiLimits,
		MaxLogBytes:             e.maxLogBytes,
		MaxLogLineBytes:         e.maxLogLineBytes,
	}
}

// Compile compiles and rechecks one admitted module for repeated fresh-instance
// invocation. This local check prevents Validator and Worker profile drift.
func (e *Engine) Compile(ctx context.Context, wasm []byte) (*Program, Metrics, error) {
	if ctx == nil {
		return nil, Metrics{}, errors.New("wasmexec compile context is required")
	}
	if len(wasm) == 0 {
		return nil, Metrics{}, errors.New("wasmexec module is required")
	}
	if !e.lifecycle.begin() {
		return nil, Metrics{}, errors.New("wasmexec engine is closed")
	}
	defer e.lifecycle.end()
	started := time.Now()
	compiled, err := e.runtime.CompileModule(ctx, wasm)
	metrics := Metrics{Compile: time.Since(started)}
	if err != nil {
		return nil, metrics, invocationError(
			problem.CodeInvalidModule,
			"module failed runtime compilation",
		)
	}
	binaryMetadata, inspectErr := wasmprofile.InspectBinary(wasm)
	if inspectErr != nil {
		return nil, metrics, errors.Join(
			invocationError(problem.CodeInvalidModule, "module failed profile inspection"),
			compiled.Close(context.WithoutCancel(ctx)),
		)
	}
	if _, compatibilityErr := wasmprofile.ValidateCommand(compiled, e.wasi, binaryMetadata); compatibilityErr != nil {
		return nil, metrics, errors.Join(
			invocationError(problem.CodeInvalidModule, "module is incompatible with the runtime profile"),
			compiled.Close(context.WithoutCancel(ctx)),
		)
	}
	return &Program{
		engine:    e,
		compiled:  compiled,
		limiter:   newLimiter(e.programConcurrency, e.programQueue),
		lifecycle: newLifecycle(),
	}, metrics, nil
}

// Invoke executes exactly one fresh anonymous instance and validates its only
// stdout value as a wasi-command-v1 ResponseEnvelope.
func (p *Program) Invoke(
	ctx context.Context,
	request abi.Request,
	timeout time.Duration,
) (result Result, err error) {
	return p.invoke(ctx, request, InvocationPolicy{Timeout: timeout}, nil)
}

// InvokeWithPolicy applies per-Assignment limits without an authorization
// callback. Higher-level Worker serving paths use InvokeWithAcceptance.
func (p *Program) InvokeWithPolicy(
	ctx context.Context,
	request abi.Request,
	policy InvocationPolicy,
) (result Result, err error) {
	return p.invoke(ctx, request, policy, nil)
}

// InvokeWithAcceptance runs higher-level admission after the Program permit,
// then checks acceptance after the Engine permit and immediately before guest
// creation. A rejected or stopped call never instantiates guest code.
func (p *Program) InvokeWithAcceptance(
	ctx context.Context,
	request abi.Request,
	policy InvocationPolicy,
	acceptance InvocationAcceptance,
) (result Result, err error) {
	if acceptance.Check == nil {
		return Result{}, errors.New("wasmexec invocation acceptance is required")
	}
	return p.invoke(ctx, request, policy, &acceptance)
}

func (p *Program) invoke(
	ctx context.Context,
	request abi.Request,
	policy InvocationPolicy,
	acceptance *InvocationAcceptance,
) (result Result, err error) {
	if ctx == nil {
		return Result{}, errors.New("wasmexec invocation context is required")
	}
	if !p.engine.lifecycle.begin() {
		return Result{}, errors.New("wasmexec engine is closed")
	}
	defer p.engine.lifecycle.end()
	if !p.lifecycle.begin() {
		return Result{}, errors.New("wasmexec program is closed")
	}
	defer p.lifecycle.end()
	policy, err = p.engine.normalizeInvocationPolicy(policy)
	if err != nil {
		return Result{}, err
	}
	invocationContext, cancel := isolatedContext(ctx, policy.Timeout)
	defer cancel()
	deadline, _ := invocationContext.Deadline()
	request.DeadlineUnixMS = deadline.UnixMilli()

	var stdin bytes.Buffer
	if err := abi.EncodeRequest(&stdin, request, policy.RequestLimits); err != nil {
		return Result{}, invocationError(problem.CodeInvalidArgument, "invalid invocation request")
	}
	queueStarted := time.Now()
	var stop, alsoStop <-chan struct{}
	if acceptance != nil {
		stop = acceptance.Stop
		alsoStop = acceptance.AlsoStop
	}
	if err := p.limiter.acquire(invocationContext, stop, alsoStop); err != nil {
		return Result{}, err
	}
	defer p.limiter.release()
	if acceptance != nil && acceptance.Acquire != nil {
		release, acquireErr := acceptance.Acquire()
		if acquireErr != nil {
			return Result{}, acquireErr
		}
		if release == nil {
			return Result{}, errors.New("wasmexec invocation admission release is required")
		}
		defer release()
	}
	if err := p.engine.limiter.acquire(invocationContext, stop, alsoStop); err != nil {
		return Result{}, err
	}
	defer p.engine.limiter.release()
	result.Metrics.Queue = time.Since(queueStarted)
	if invocationContext.Err() != nil {
		return Result{}, invocationError(problem.CodeFunctionTimeout, "function execution deadline was exceeded while queued")
	}

	stdout := newBoundedOutput(rawEnvelopeLimit(policy.ResponseLimits), cancel)
	guestLog := newGuestLog(policy.MaxLogBytes, policy.MaxLogLineBytes)
	moduleConfig := wazero.NewModuleConfig().
		WithName("").
		WithStartFunctions().
		WithStdin(&stdin).
		WithStdout(stdout).
		WithStderr(guestLog).
		WithSysWalltime().
		WithSysNanotime().
		WithNanosleep(cancelAwareNanosleep(invocationContext)).
		WithOsyield(runtime.Gosched).
		WithRandSource(p.engine.random)
	if acceptance != nil {
		if acceptanceStopped(stop, alsoStop) {
			return Result{}, ErrAcceptanceStopped
		}
		if err := acceptance.Check(); err != nil {
			return result, err
		}
	}

	instantiateStarted := time.Now()
	module, instantiateErr := p.engine.runtime.InstantiateModule(invocationContext, p.compiled, moduleConfig)
	result.Metrics.Instantiate = time.Since(instantiateStarted)
	if instantiateErr != nil {
		result = finishResult(result, guestLog)
		return result, classifyExecutionError(invocationContext, stdout, instantiateErr)
	}
	defer func() {
		if closeErr := module.Close(context.WithoutCancel(invocationContext)); closeErr != nil && err == nil {
			err = invocationError(problem.CodeWorkerLost, "runtime failed to release the function instance")
		}
	}()

	start := module.ExportedFunction("_start")
	if start == nil {
		result = finishResult(result, guestLog)
		return result, invocationError(problem.CodeInvalidModule, "module command entrypoint is unavailable")
	}
	executeStarted := time.Now()
	_, executionErr := start.Call(invocationContext)
	result.Metrics.Execute = time.Since(executeStarted)
	result = finishResult(result, guestLog)
	if executionErr != nil && !successfulExit(executionErr) {
		return result, classifyExecutionError(invocationContext, stdout, executionErr)
	}
	if stdout.Exceeded() {
		return result, invocationError(problem.CodeOutputLimit, "function output exceeded its limit")
	}
	response, decodeErr := abi.DecodeResponse(bytes.NewReader(stdout.Bytes()), request.Method, policy.ResponseLimits)
	if decodeErr != nil {
		return result, invocationError(problem.CodeInvalidFunctionResponse, "function returned an invalid response")
	}
	result.Response = response
	return result, nil
}

func (e *Engine) normalizeInvocationPolicy(policy InvocationPolicy) (InvocationPolicy, error) {
	timeout, err := e.normalizeTimeout(policy.Timeout)
	if err != nil {
		return InvocationPolicy{}, err
	}
	requestLimits, err := e.abiLimits.Tighten(policy.RequestLimits)
	if err != nil {
		return InvocationPolicy{}, errors.New("wasmexec request limits are outside configured bounds")
	}
	responseLimits, err := e.abiLimits.Tighten(policy.ResponseLimits)
	if err != nil {
		return InvocationPolicy{}, errors.New("wasmexec response limits are outside configured bounds")
	}
	if policy.MaxLogBytes == 0 {
		policy.MaxLogBytes = e.maxLogBytes
	}
	if policy.MaxLogLineBytes == 0 {
		policy.MaxLogLineBytes = e.maxLogLineBytes
	}
	invalidLogs := policy.MaxLogBytes < 1 || policy.MaxLogBytes > e.maxLogBytes ||
		policy.MaxLogLineBytes < 1 || policy.MaxLogLineBytes > e.maxLogLineBytes
	if invalidLogs {
		return InvocationPolicy{}, errors.New("wasmexec invocation log limits are outside configured bounds")
	}
	policy.Timeout = timeout
	policy.RequestLimits = requestLimits
	policy.ResponseLimits = responseLimits
	return policy, nil
}

// Close releases the compiled program after in-flight invocations finish.
func (p *Program) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("wasmexec close context is required")
	}
	if !p.engine.lifecycle.begin() {
		return errors.New("wasmexec engine is closed")
	}
	defer p.engine.lifecycle.end()
	err := p.lifecycle.close(ctx, p.compiled.Close)
	if err != nil {
		return fmt.Errorf("closing compiled wasm program: %w", err)
	}
	return nil
}

// Close waits for active Engine operations and releases all runtime resources.
func (e *Engine) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("wasmexec close context is required")
	}
	err := e.lifecycle.close(ctx, func(closeContext context.Context) error {
		return errors.Join(e.wasi.Close(closeContext), e.runtime.Close(closeContext))
	})
	if err != nil {
		return fmt.Errorf("closing wasm runtime: %w", err)
	}
	return nil
}

func (e *Engine) normalizeTimeout(timeout time.Duration) (time.Duration, error) {
	if timeout == 0 {
		return e.defaultTimeout, nil
	}
	if timeout < time.Millisecond || timeout > e.maxTimeout {
		return 0, errors.New("wasmexec timeout is outside configured bounds")
	}
	return timeout, nil
}

func isolatedContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(timeout)
	if parentDeadline, exists := parent.Deadline(); exists && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	return context.WithDeadline(parent, deadline)
}

func cancelAwareNanosleep(ctx context.Context) sys.Nanosleep {
	return func(nanoseconds int64) {
		if nanoseconds <= 0 {
			return
		}
		timer := time.NewTimer(time.Duration(nanoseconds))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
	}
}

func classifyExecutionError(ctx context.Context, output *boundedOutput, executionErr error) error {
	if output.Exceeded() || errors.Is(executionErr, errOutputLimit) {
		return invocationError(problem.CodeOutputLimit, "function output exceeded its limit")
	}
	if ctx.Err() != nil {
		return invocationError(problem.CodeFunctionTimeout, "function execution deadline was exceeded")
	}
	return invocationError(problem.CodeFunctionTrap, "function trapped or exited unsuccessfully")
}

func successfulExit(err error) bool {
	var exitErr *sys.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 0
}

func invocationError(code problem.Code, message string) error {
	return &problem.Error{Code: code, Message: message}
}

func finishResult(result Result, logs *guestLog) Result {
	result.GuestLog = logs.Bytes()
	result.DroppedLogBytes = logs.Dropped()
	return result
}

type lockedReader struct {
	mu     sync.Mutex
	source io.Reader
}

func (r *lockedReader) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.source.Read(buffer)
}
