// Package workeragent owns boot-local Replica preparation, serving admission,
// and compiled-module leases for one Worker process.
package workeragent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmexec"
	"github.com/yourikka/minicloud/internal/wasmprofile"
	"github.com/yourikka/minicloud/internal/workercache"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

type programLease interface {
	InvokeWithAcceptance(
		context.Context,
		workercache.InvocationRequest,
		wasmexec.InvocationAcceptance,
	) (wasmexec.Result, error)
	Release()
}

type compiledCache interface {
	acquire(context.Context, workercache.ModuleSpec) (programLease, workercache.Result, error)
	close(context.Context) error
	profile() wasmprofile.Profile
	executionLimits() wasmexec.ExecutionLimits
	snapshot() workercache.Stats
}

type cacheAdapter struct {
	cache *workercache.Cache
}

func (c cacheAdapter) acquire(
	ctx context.Context,
	spec workercache.ModuleSpec,
) (programLease, workercache.Result, error) {
	return c.cache.Acquire(ctx, spec)
}

func (c cacheAdapter) close(ctx context.Context) error {
	return c.cache.Close(ctx)
}

func (c cacheAdapter) profile() wasmprofile.Profile {
	return c.cache.Profile()
}

func (c cacheAdapter) executionLimits() wasmexec.ExecutionLimits {
	return c.cache.ExecutionLimits()
}

func (c cacheAdapter) snapshot() workercache.Stats {
	return c.cache.Snapshot()
}

// Agent is safe for concurrent control, invocation, and inventory operations.
type Agent struct {
	cache               compiledCache
	gate                *servingauth.Gate
	maxAssignments      int
	maxConcurrent       int
	maxQueuedPerReplica int
	prepareTimeout      time.Duration
	limiter             *limiter
	stop                chan struct{}

	mu           sync.Mutex
	replicas     map[string]*replica
	revision     uint64
	closing      bool
	closed       bool
	cacheClosing bool
	cacheClose   chan struct{}
	closeErr     error
}

type replica struct {
	request PrepareRequest
	state   ReplicaState
	failure *model.SafeError
	load    workercache.Result
	lease   programLease
	limiter *limiter

	preparing     bool
	prepareCancel context.CancelFunc
	prepareErr    error
	prepared      chan struct{}
	preparedDone  bool
	done          chan struct{}
	doneClosed    bool
	stop          chan struct{}
	stopClosed    bool
	users         int
	active        int
	releasing     bool
}

type releaseAction struct {
	replica *replica
	lease   programLease
}

// New validates all Agent bounds and creates its boot-local authorization gate.
func New(config Config) (*Agent, error) {
	if config.Cache == nil {
		return nil, errors.New("worker agent cache is required")
	}
	return newAgent(config, cacheAdapter{cache: config.Cache})
}

func newAgent(config Config, cache compiledCache) (*Agent, error) {
	if cache == nil {
		return nil, errors.New("worker agent compiled cache is required")
	}
	if config.MaxAssignments == 0 {
		config.MaxAssignments = DefaultMaxAssignments
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = DefaultMaxConcurrent
	}
	if config.MaxQueued == 0 {
		config.MaxQueued = DefaultMaxQueued
	}
	if config.MaxQueuedPerReplica == 0 {
		config.MaxQueuedPerReplica = min(DefaultMaxQueuedPerReplica, config.MaxQueued)
	}
	if config.PrepareTimeout == 0 {
		config.PrepareTimeout = DefaultPrepareTimeout
	}
	if config.MaxAssignments < 1 || config.MaxAssignments > HardMaxAssignments {
		return nil, errors.New("worker agent assignment limit is outside v1 bounds")
	}
	if config.MaxConcurrent < 1 || config.MaxConcurrent > HardMaxConcurrent {
		return nil, errors.New("worker agent concurrency is outside v1 bounds")
	}
	if config.MaxQueued < 1 || config.MaxQueued > HardMaxQueued {
		return nil, errors.New("worker agent queue limit is outside v1 bounds")
	}
	if config.MaxQueuedPerReplica < 1 ||
		config.MaxQueuedPerReplica > HardMaxQueuedPerReplica ||
		config.MaxQueuedPerReplica > config.MaxQueued {
		return nil, errors.New("worker agent replica queue limit is outside v1 bounds")
	}
	if config.PrepareTimeout < time.Millisecond || config.PrepareTimeout > HardMaxPrepareTimeout {
		return nil, errors.New("worker agent prepare timeout is outside v1 bounds")
	}
	if config.MaxConcurrent > cache.executionLimits().MaxConcurrent {
		return nil, errors.New("worker agent concurrency exceeds runtime capacity")
	}
	gate, err := servingauth.New(config.Authorization)
	if err != nil {
		return nil, fmt.Errorf("creating worker authorization gate: %w", err)
	}
	return &Agent{
		cache:               cache,
		gate:                gate,
		maxAssignments:      config.MaxAssignments,
		maxConcurrent:       config.MaxConcurrent,
		maxQueuedPerReplica: config.MaxQueuedPerReplica,
		prepareTimeout:      config.PrepareTimeout,
		limiter:             newLimiter(config.MaxConcurrent, config.MaxQueued),
		stop:                make(chan struct{}),
		replicas:            make(map[string]*replica),
	}, nil
}

// AcceptControl advances the authoritative control session and atomically
// fences every Replica bound to an older Worker Session Epoch.
func (a *Agent) AcceptControl(connection servingauth.ControlConnection) error {
	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return classified(problem.CodeWorkerLost, "worker agent is closing")
	}
	before := a.gate.Snapshot()
	err := a.gate.AcceptAuthoritativeControl(connection)
	after := a.gate.Snapshot()
	actions := []releaseAction{}
	if after.CurrentSessionEpoch > before.CurrentSessionEpoch {
		for _, current := range a.replicas {
			if current.request.Fence.Assignment.Worker.SessionEpoch == after.CurrentSessionEpoch {
				continue
			}
			actions = append(actions, a.loseReplicaLocked(current)...)
		}
	}
	for _, current := range a.replicas {
		if !current.preparing {
			continue
		}
		if checkErr := a.gate.CheckAssignment(current.request.Connection, current.request.Fence); checkErr != nil {
			actions = append(actions, a.loseReplicaLocked(current)...)
		}
	}
	a.mu.Unlock()
	a.release(actions)
	return err
}

// DisconnectControl invalidates live-only permissions for the exact connection.
func (a *Agent) DisconnectControl(connection servingauth.ControlConnection) {
	if a == nil {
		return
	}
	a.mu.Lock()
	before := a.gate.Snapshot()
	a.gate.DisconnectControl(connection)
	after := a.gate.Snapshot()
	actions := []releaseAction{}
	if before.ControlConnected && !after.ControlConnected {
		for _, current := range a.replicas {
			if current.preparing && current.request.Connection == connection {
				actions = append(actions, a.loseReplicaLocked(current)...)
			}
		}
	}
	a.mu.Unlock()
	a.release(actions)
}

// Prepare verifies, compiles, policy-checks, and pins one Assignment before it
// becomes Ready. Concurrent exact retries join the first preparation.
func (a *Agent) Prepare(
	ctx context.Context,
	request PrepareRequest,
) (Observation, error) {
	if ctx == nil {
		return Observation{}, errors.New("worker agent prepare context is required")
	}
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	if err := a.validatePrepare(request); err != nil {
		return Observation{}, err
	}

	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return Observation{}, classified(problem.CodeWorkerLost, "worker agent is closing")
	}
	if err := a.gate.CheckAssignment(request.Connection, request.Fence); err != nil {
		a.mu.Unlock()
		return Observation{}, err
	}
	assignmentID := request.Fence.Assignment.AssignmentID
	if current, exists := a.replicas[assignmentID]; exists {
		if !samePreparation(current.request, request) {
			a.mu.Unlock()
			return Observation{}, incompatibleAssignment(current.request.Fence.Assignment, request.Fence.Assignment)
		}
		prepared := current.prepared
		if current.preparedDone {
			observation := a.observeLocked(current)
			err := current.prepareErr
			a.mu.Unlock()
			return observation, err
		}
		a.mu.Unlock()
		select {
		case <-prepared:
			return a.preparationResult(current)
		case <-ctx.Done():
			return Observation{}, ctx.Err()
		}
	}
	if len(a.replicas) >= a.maxAssignments {
		a.mu.Unlock()
		return Observation{}, classified(problem.CodeOverloaded, "worker replica capacity is full")
	}
	prepareContext, cancelPrepare := a.newPrepareContext(ctx)
	current := &replica{
		request:       clonePrepareRequest(request),
		state:         ReplicaAssigned,
		limiter:       newLimiter(int(request.Policy.MaxConcurrency), a.maxQueuedPerReplica),
		preparing:     true,
		prepareCancel: cancelPrepare,
		prepared:      make(chan struct{}),
		done:          make(chan struct{}),
		stop:          make(chan struct{}),
	}
	a.replicas[assignmentID] = current
	a.bumpLocked()
	a.transitionLocked(current, ReplicaFetching)
	a.mu.Unlock()

	go a.prepareReplica(prepareContext, cancelPrepare, current)
	select {
	case <-current.prepared:
		return a.preparationResult(current)
	case <-ctx.Done():
		return Observation{}, ctx.Err()
	}
}

// InstallAuthorization refreshes permission only for an exact Ready Replica.
func (a *Agent) InstallAuthorization(
	connection servingauth.ControlConnection,
	authorization servingauth.Authorization,
) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return classified(problem.CodeWorkerLost, "worker agent is closing")
	}
	current, exists := a.replicas[authorization.Fence.Assignment.AssignmentID]
	if !exists || current.state != ReplicaReady {
		return classified(problem.CodeNoReadyReplica, "assignment replica is not ready")
	}
	if current.request.Fence.Assignment != authorization.Fence.Assignment {
		return incompatibleAssignment(current.request.Fence.Assignment, authorization.Fence.Assignment)
	}
	return a.gate.Install(connection, authorization)
}

// Invoke runs one request against the pinned Ready Replica. Serving permission
// is checked exactly once after every queue and before guest instantiation.
func (a *Agent) Invoke(
	ctx context.Context,
	fence servingauth.InvocationFence,
	request abi.Request,
	timeout time.Duration,
) (result wasmexec.Result, err error) {
	started := time.Now()
	if ctx == nil {
		return wasmexec.Result{}, errors.New("worker agent invocation context is required")
	}
	a.mu.Lock()
	current, exists := a.replicas[fence.Assignment.AssignmentID]
	if a.closing || !exists || current.state != ReplicaReady {
		a.mu.Unlock()
		return wasmexec.Result{}, classified(problem.CodeNoReadyReplica, "assignment replica is not ready")
	}
	if current.request.Fence.Assignment != fence.Assignment {
		a.mu.Unlock()
		return wasmexec.Result{}, classified(problem.CodeStaleAssignment, "invocation fence does not match ready replica")
	}
	if int64(len(request.Body)) > current.request.Policy.ResourceLimits.MaxInputBytes {
		a.mu.Unlock()
		return wasmexec.Result{}, problem.Invalid("body", "exceeds the assignment input limit")
	}
	runtimePolicy, err := runtimeInvocationPolicy(current.request.Policy, timeout)
	if err != nil {
		a.mu.Unlock()
		return wasmexec.Result{}, err
	}
	a.mu.Unlock()

	remaining := runtimePolicy.Timeout - time.Since(started)
	if remaining <= 0 {
		return wasmexec.Result{}, classified(problem.CodeFunctionTimeout, "invocation deadline was exceeded before admission")
	}
	invocationContext, cancelInvocation := context.WithTimeout(ctx, remaining)
	defer cancelInvocation()

	a.mu.Lock()
	latest, exists := a.replicas[fence.Assignment.AssignmentID]
	if a.closing || !exists || latest != current || current.state != ReplicaReady {
		a.mu.Unlock()
		return wasmexec.Result{}, classified(problem.CodeNoReadyReplica, "assignment replica is not ready")
	}
	lease := current.lease
	current.users++
	a.bumpLocked()
	a.mu.Unlock()

	accepted := false
	defer func() { a.finishInvocation(current, accepted) }()
	if err := current.limiter.acquire(invocationContext, current.stop, a.stop); err != nil {
		return wasmexec.Result{}, a.replicaAdmissionError(current, err)
	}
	defer current.limiter.release()
	a.mu.Lock()
	ready := !a.closing && current.state == ReplicaReady &&
		a.replicas[fence.Assignment.AssignmentID] == current
	a.mu.Unlock()
	if !ready {
		return wasmexec.Result{}, classified(problem.CodeStaleAssignment, "assignment stopped before invocation acceptance")
	}

	result, err = lease.InvokeWithAcceptance(
		invocationContext,
		workercache.InvocationRequest{
			Request: request,
			Policy:  runtimePolicy,
		},
		wasmexec.InvocationAcceptance{
			Stop:     current.stop,
			AlsoStop: a.stop,
			Acquire: func() (func(), error) {
				if err := a.limiter.acquire(invocationContext, a.stop, current.stop); err != nil {
					return nil, a.globalAdmissionError(current, err)
				}
				return a.limiter.release, nil
			},
			Check: func() error {
				a.mu.Lock()
				defer a.mu.Unlock()
				if invocationContext.Err() != nil {
					return classified(problem.CodeFunctionTimeout, "invocation deadline was exceeded before acceptance")
				}
				ready := !a.closing && current.state == ReplicaReady &&
					a.replicas[fence.Assignment.AssignmentID] == current
				if !ready {
					return classified(problem.CodeStaleAssignment, "assignment stopped before invocation acceptance")
				}
				if err := a.gate.AuthorizeSync(fence); err != nil {
					return err
				}
				if invocationContext.Err() != nil {
					return classified(problem.CodeFunctionTimeout, "invocation deadline was exceeded before acceptance")
				}
				current.active++
				accepted = true
				a.bumpLocked()
				return nil
			},
		},
	)
	if errors.Is(err, wasmexec.ErrAcceptanceStopped) {
		return result, a.stoppingAdmissionError(current)
	}
	return result, err
}

// Cancel prevents new acceptance immediately. Calls already accepted may
// finish, after which the pinned compiled lease is released.
func (a *Agent) Cancel(request CancelRequest) error {
	a.mu.Lock()
	if a.closing {
		a.mu.Unlock()
		return classified(problem.CodeWorkerLost, "worker agent is closing")
	}
	if err := a.gate.CheckAssignment(request.Connection, request.Fence); err != nil {
		a.mu.Unlock()
		return err
	}
	current, exists := a.replicas[request.Fence.Assignment.AssignmentID]
	if !exists {
		a.mu.Unlock()
		return nil
	}
	if current.request.Fence.Assignment != request.Fence.Assignment {
		a.mu.Unlock()
		return incompatibleAssignment(current.request.Fence.Assignment, request.Fence.Assignment)
	}
	actions := a.drainReplicaLocked(current)
	a.mu.Unlock()
	a.release(actions)
	return nil
}

// AcknowledgeTerminal removes one fully released terminal observation after
// the current Controller has incorporated it into a full Inventory exchange.
// Assignment ID global uniqueness and non-reuse remain Controller/Raft rules.
func (a *Agent) AcknowledgeTerminal(
	connection servingauth.ControlConnection,
	assignmentID string,
) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return classified(problem.CodeWorkerLost, "worker agent is closing")
	}
	if err := a.gate.CheckControl(connection); err != nil {
		return err
	}
	current, exists := a.replicas[assignmentID]
	if !exists {
		return nil
	}
	if !current.state.terminal() || !current.doneClosed {
		return classified(problem.CodeConflict, "assignment replica has not fully stopped")
	}
	delete(a.replicas, assignmentID)
	a.bumpLocked()
	return nil
}

// Inventory returns Replica state, bounded queue occupancy, cache metrics, and
// authorization high-water marks without exposing mutable internal storage.
func (a *Agent) Inventory() Inventory {
	a.mu.Lock()
	defer a.mu.Unlock()
	replicas := make([]Observation, 0, len(a.replicas))
	for _, current := range a.replicas {
		replicas = append(replicas, a.observeLocked(current))
	}
	slices.SortFunc(replicas, func(left, right Observation) int {
		return compareString(left.Identity.AssignmentID, right.Identity.AssignmentID)
	})
	return Inventory{
		Revision:      a.revision,
		Replicas:      replicas,
		Cache:         a.cache.snapshot(),
		Authorization: a.gate.Snapshot(),
		Closing:       a.closing,
		Closed:        a.closed,
	}
}

// Close rejects new work, drains every Replica, releases all compiled leases,
// and then closes the owned Cache. A context failure leaves Close retryable.
func (a *Agent) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("worker agent close context is required")
	}
	actions, waits := a.beginClose()
	a.release(actions)
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return a.closeCache(ctx)
}

func (a *Agent) newPrepareContext(parent context.Context) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(a.prepareTimeout)
	if parentDeadline, exists := parent.Deadline(); exists && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	return context.WithDeadline(context.WithoutCancel(parent), deadline)
}

func (a *Agent) prepareReplica(
	ctx context.Context,
	cancel context.CancelFunc,
	current *replica,
) {
	defer cancel()
	lease, result, err := a.cache.acquire(ctx, current.request.Module)
	_, _ = a.finishPrepare(current, lease, result, err)
}

func (a *Agent) validatePrepare(request PrepareRequest) error {
	if err := request.Policy.Validate(); err != nil {
		return err
	}
	policyDigest, err := request.Policy.Digest()
	if err != nil {
		return fmt.Errorf("digesting effective policy: %w", err)
	}
	if policyDigest != request.Fence.Assignment.PolicyDigest {
		return classified(problem.CodeStaleGeneration, "assignment policy digest does not match effective policy")
	}
	identity := request.Fence.Assignment
	identityMatchesPolicy := identity.VersionID == request.Policy.VersionID &&
		identity.AdmissionEpoch == request.Policy.AdmissionEpoch &&
		identity.DeploymentGeneration == request.Policy.DeploymentGeneration
	if !identityMatchesPolicy {
		return classified(problem.CodeStaleGeneration, "assignment identity does not match effective policy")
	}
	moduleMatchesPolicy := request.Module.ArtifactDigest == request.Policy.ArtifactDigest &&
		request.Module.ArtifactSize == request.Policy.ArtifactSize &&
		request.Module.ABI == request.Policy.ABI &&
		request.Module.HostAPIProfile == request.Policy.HostAPIProfile &&
		request.Module.RuntimeFeatureProfile == request.Policy.RuntimeFeatureProfile
	if !moduleMatchesPolicy {
		return classified(problem.CodeStaleGeneration, "assignment module does not match effective policy")
	}
	if len(request.Policy.GrantedCapabilities) != 0 {
		return classified(problem.CodeCapabilityDenied, "worker runtime supports no optional host capabilities")
	}
	if request.Policy.MaxConcurrency > uint32(a.maxConcurrent) {
		return classified(problem.CodeCapabilityDenied, "assignment concurrency exceeds worker capacity")
	}
	profile := a.cache.profile()
	if profile.MemoryLimitMiB != request.Policy.ResourceLimits.MemoryMiB {
		return classified(problem.CodeCapabilityDenied, "assignment memory tier does not match worker runtime")
	}
	if !executionLimitsSatisfy(request.Policy, a.cache.executionLimits()) {
		return classified(problem.CodeCapabilityDenied, "assignment limits do not match worker runtime")
	}
	return nil
}

func (a *Agent) finishPrepare(
	current *replica,
	lease programLease,
	result workercache.Result,
	prepareErr error,
) (Observation, error) {
	a.mu.Lock()
	current.preparing = false
	current.prepareCancel = nil
	current.load = result
	if prepareErr != nil {
		a.finishFailedPreparationLocked(current, prepareErr)
		a.mu.Unlock()
		if lease != nil {
			lease.Release()
		}
		return a.preparationResult(current)
	}

	if current.state == ReplicaDraining || current.state == ReplicaLost || a.closing {
		stateErr := classified(problem.CodeStaleAssignment, "assignment stopped during replica preparation")
		current.prepareErr = stateErr
		if current.state == ReplicaDraining {
			a.transitionLocked(current, ReplicaStopped)
		}
		a.closePreparedLocked(current)
		current.releasing = true
		a.mu.Unlock()
		a.release([]releaseAction{{replica: current, lease: lease}})
		return a.preparationResult(current)
	}
	if err := a.gate.CheckAssignment(current.request.Connection, current.request.Fence); err != nil {
		current.prepareErr = err
		current.failure = safeFailure(err)
		a.transitionLocked(current, ReplicaLost)
		a.closeStopLocked(current)
		a.closePreparedLocked(current)
		current.releasing = true
		a.mu.Unlock()
		a.release([]releaseAction{{replica: current, lease: lease}})
		return a.preparationResult(current)
	}
	if !cacheResultMatches(current.request, result) {
		profileErr := classified(problem.CodeCapabilityDenied, "compiled runtime profile does not match assignment policy")
		current.prepareErr = profileErr
		current.failure = safeFailure(profileErr)
		a.transitionLocked(current, ReplicaFailed)
		a.closeStopLocked(current)
		a.closePreparedLocked(current)
		current.releasing = true
		a.mu.Unlock()
		a.release([]releaseAction{{replica: current, lease: lease}})
		return a.preparationResult(current)
	}

	current.lease = lease
	a.transitionLocked(current, ReplicaValidating)
	a.transitionLocked(current, ReplicaCompiling)
	a.transitionLocked(current, ReplicaReady)
	a.closePreparedLocked(current)
	observation := a.observeLocked(current)
	a.mu.Unlock()
	return observation, nil
}

func (a *Agent) finishFailedPreparationLocked(current *replica, err error) {
	if current.state == ReplicaDraining {
		current.prepareErr = classified(problem.CodeStaleAssignment, "assignment was cancelled during preparation")
		a.transitionLocked(current, ReplicaStopped)
	} else if current.state == ReplicaLost {
		current.prepareErr = classified(problem.CodeStaleAssignment, "worker session changed during preparation")
	} else {
		current.prepareErr = err
		current.failure = safeFailure(err)
		a.transitionLocked(current, ReplicaFailed)
	}
	a.closeStopLocked(current)
	a.closePreparedLocked(current)
	a.finishTerminalLocked(current)
}

func (a *Agent) preparationResult(current *replica) (Observation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.observeLocked(current), current.prepareErr
}

func (a *Agent) finishInvocation(current *replica, accepted bool) {
	a.mu.Lock()
	if accepted {
		current.active--
	}
	current.users--
	a.bumpLocked()
	actions := a.finalizeIdleLocked(current)
	a.mu.Unlock()
	a.release(actions)
}

func (a *Agent) drainReplicaLocked(current *replica) []releaseAction {
	if current.state.terminal() {
		return nil
	}
	a.transitionLocked(current, ReplicaDraining)
	a.closeStopLocked(current)
	if current.prepareCancel != nil {
		current.prepareCancel()
	}
	current.prepareErr = classified(problem.CodeStaleAssignment, "assignment was cancelled")
	if !current.preparing {
		a.closePreparedLocked(current)
	}
	a.gate.Revoke(current.request.Fence.Assignment.AssignmentID)
	return a.finalizeIdleLocked(current)
}

func (a *Agent) loseReplicaLocked(current *replica) []releaseAction {
	if current.state.terminal() {
		return nil
	}
	a.transitionLocked(current, ReplicaLost)
	a.closeStopLocked(current)
	if current.prepareCancel != nil {
		current.prepareCancel()
	}
	current.prepareErr = classified(problem.CodeStaleAssignment, "worker session changed")
	current.failure = safeFailure(current.prepareErr)
	if !current.preparing {
		a.closePreparedLocked(current)
	}
	a.gate.Revoke(current.request.Fence.Assignment.AssignmentID)
	return a.finalizeIdleLocked(current)
}

func (a *Agent) finalizeIdleLocked(current *replica) []releaseAction {
	if current.users != 0 || current.preparing {
		return nil
	}
	if current.state == ReplicaDraining {
		a.transitionLocked(current, ReplicaStopped)
	}
	if !current.state.terminal() {
		return nil
	}
	if current.lease != nil && !current.releasing {
		lease := current.lease
		current.lease = nil
		current.releasing = true
		return []releaseAction{{replica: current, lease: lease}}
	}
	a.finishTerminalLocked(current)
	return nil
}

func (a *Agent) release(actions []releaseAction) {
	for _, action := range actions {
		action.lease.Release()
		a.mu.Lock()
		action.replica.releasing = false
		a.finishTerminalLocked(action.replica)
		a.mu.Unlock()
	}
}

func (a *Agent) finishTerminalLocked(current *replica) {
	if !current.state.terminal() || current.preparing || current.users != 0 ||
		current.lease != nil || current.releasing || current.doneClosed {
		return
	}
	close(current.done)
	current.doneClosed = true
	a.bumpLocked()
}

func (a *Agent) beginClose() ([]releaseAction, []<-chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.closing {
		a.closing = true
		close(a.stop)
		a.bumpLocked()
	}
	actions := []releaseAction{}
	waits := make([]<-chan struct{}, 0, len(a.replicas))
	for _, current := range a.replicas {
		actions = append(actions, a.drainReplicaLocked(current)...)
		waits = append(waits, current.done)
	}
	return actions, waits
}

func (a *Agent) closeCache(ctx context.Context) error {
	for {
		a.mu.Lock()
		if a.closed {
			err := a.closeErr
			a.mu.Unlock()
			return err
		}
		if a.cacheClosing {
			done := a.cacheClose
			a.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		a.cacheClosing = true
		a.cacheClose = make(chan struct{})
		done := a.cacheClose
		a.mu.Unlock()

		err := a.cache.close(ctx)
		a.mu.Lock()
		a.cacheClosing = false
		a.closeErr = err
		if err == nil {
			a.closed = true
			a.bumpLocked()
		}
		close(done)
		a.mu.Unlock()
		return err
	}
}

func (a *Agent) observeLocked(current *replica) Observation {
	var failure *model.SafeError
	if current.failure != nil {
		copied := *current.failure
		failure = &copied
	}
	return Observation{
		Identity:          current.request.Fence.Assignment,
		Module:            current.request.Module,
		State:             current.state,
		Failure:           failure,
		Load:              current.load,
		ActiveInvocations: current.active,
		QueuedInvocations: len(current.limiter.queue),
	}
}

func (a *Agent) transitionLocked(current *replica, state ReplicaState) {
	if current.state == state {
		return
	}
	if !validReplicaTransition(current.state, state) {
		panic(fmt.Sprintf("workeragent: invalid replica transition %s -> %s", current.state, state))
	}
	current.state = state
	a.bumpLocked()
}

func (a *Agent) closePreparedLocked(current *replica) {
	if current.preparedDone {
		return
	}
	close(current.prepared)
	current.preparedDone = true
}

func (a *Agent) closeStopLocked(current *replica) {
	if current.stopClosed {
		return
	}
	close(current.stop)
	current.stopClosed = true
}

func (a *Agent) bumpLocked() {
	a.revision++
}

func clonePrepareRequest(request PrepareRequest) PrepareRequest {
	request.Policy.GrantedCapabilities = slices.Clone(request.Policy.GrantedCapabilities)
	return request
}

func samePreparation(left, right PrepareRequest) bool {
	return left.Connection == right.Connection && left.Fence == right.Fence && left.Module == right.Module
}

func cacheResultMatches(request PrepareRequest, result workercache.Result) bool {
	key := result.Key
	return key.ArtifactDigest == request.Module.ArtifactDigest &&
		key.ArtifactSize == request.Module.ArtifactSize &&
		key.ABI == request.Policy.ABI &&
		key.HostAPIProfile == request.Policy.HostAPIProfile &&
		key.RuntimeFeatureProfile == request.Policy.RuntimeFeatureProfile &&
		key.MemoryLimitMiB == request.Policy.ResourceLimits.MemoryMiB
}

func executionLimitsSatisfy(
	policy model.EffectivePolicy,
	limits wasmexec.ExecutionLimits,
) bool {
	abiLimits := limits.ABILimits
	standardMetadata := abiLimits.MetadataBytes >= abi.DefaultMetadataBytes &&
		abiLimits.HeaderCount >= abi.DefaultHeaderCount &&
		abiLimits.HeaderBytes >= abi.DefaultHeaderBytes &&
		abiLimits.HeaderValueBytes >= abi.DefaultHeaderValueBytes &&
		abiLimits.QueryPairs >= abi.DefaultQueryPairs &&
		abiLimits.QueryBytes >= abi.DefaultQueryBytes &&
		abiLimits.JSONDepth >= abi.DefaultJSONDepth &&
		abiLimits.MethodBytes >= abi.DefaultMethodBytes &&
		abiLimits.PathBytes >= abi.DefaultPathBytes
	requiredRawEnvelope := max(
		responseEnvelopeBytes(policy.ResourceLimits.MaxInputBytes),
		responseEnvelopeBytes(policy.ResourceLimits.MaxOutputBytes),
	)
	return limits.MaxTimeout >= policy.ResourceLimits.Timeout &&
		limits.MaxConcurrentPerProgram >= int(policy.MaxConcurrency) &&
		int64(abiLimits.BodyBytes) >= policy.ResourceLimits.MaxInputBytes &&
		int64(abiLimits.BodyBytes) >= policy.ResourceLimits.MaxOutputBytes &&
		abiLimits.RawEnvelopeBytes >= requiredRawEnvelope &&
		standardMetadata &&
		int64(limits.MaxLogBytes) >= policy.ResourceLimits.MaxLogBytes &&
		limits.MaxLogLineBytes >= wasmexec.DefaultMaxLogLineBytes
}

func incompatibleAssignment(previous, next servingauth.AssignmentIdentity) error {
	generationChanged := previous.VersionID != next.VersionID ||
		previous.AdmissionEpoch != next.AdmissionEpoch ||
		previous.DeploymentGeneration != next.DeploymentGeneration ||
		previous.PolicyDigest != next.PolicyDigest
	if generationChanged {
		return classified(problem.CodeStaleGeneration, "assignment generation identity changed")
	}
	return classified(problem.CodeStaleAssignment, "assignment identity changed")
}

func safeFailure(err error) *model.SafeError {
	var classifiedError *problem.Error
	if errors.As(err, &classifiedError) && problem.Known(classifiedError.Code) {
		return &model.SafeError{Code: classifiedError.Code, Message: classifiedError.Message}
	}
	switch {
	case errors.Is(err, workercache.ErrFull),
		errors.Is(err, workercache.ErrEntryTooLarge),
		errors.Is(err, workercache.ErrLoadQueueFull):
		return &model.SafeError{Code: problem.CodeOverloaded, Message: "worker runtime capacity is unavailable"}
	case errors.Is(err, artifact.ErrCorrupt):
		return &model.SafeError{Code: problem.CodeArtifactUnavailable, Message: "artifact verification failed"}
	default:
		return &model.SafeError{Code: problem.CodeArtifactUnavailable, Message: "replica preparation failed"}
	}
}

func (a *Agent) globalAdmissionError(current *replica, err error) error {
	switch {
	case errors.Is(err, errQueueFull):
		return classified(problem.CodeOverloaded, "worker invocation queue is full")
	case errors.Is(err, errStopping):
		return a.stoppingAdmissionError(current)
	default:
		return classified(problem.CodeFunctionTimeout, "invocation deadline was exceeded while queued")
	}
}

func (a *Agent) replicaAdmissionError(current *replica, err error) error {
	switch {
	case errors.Is(err, errQueueFull):
		return classified(problem.CodeOverloaded, "replica invocation queue is full")
	case errors.Is(err, errStopping):
		return a.stoppingAdmissionError(current)
	default:
		return classified(problem.CodeFunctionTimeout, "invocation deadline was exceeded while queued")
	}
}

func (a *Agent) stoppingAdmissionError(current *replica) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closing {
		return classified(problem.CodeWorkerLost, "worker agent is closing")
	}
	if current.state != ReplicaReady {
		return classified(problem.CodeStaleAssignment, "assignment stopped while queued")
	}
	return classified(problem.CodeStaleAssignment, "invocation admission was fenced")
}

func classified(code problem.Code, message string) error {
	return &problem.Error{Code: code, Message: message}
}

func classifiedInvalid(field, message string) error {
	return problem.Invalid(field, message)
}

func compareString(left, right string) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}
