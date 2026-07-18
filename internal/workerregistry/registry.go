package workerregistry

import (
	"errors"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/workercache"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Registry is safe for concurrent Controller, heartbeat, and inventory calls.
type Registry struct {
	clock             servingauth.MonotonicClock
	heartbeatInterval time.Duration
	suspectAfter      time.Duration
	unavailableAfter  time.Duration
	maxWorkers        int

	mu           sync.Mutex
	workers      map[string]*workerRecord
	revision     uint64
	lastElapsed  time.Duration
	clockHealthy bool
}

type workerRecord struct {
	workerID          string
	bootID            string
	maxSessionEpoch   uint64
	current           servingauth.WorkerSession
	registered        bool
	needsNewSession   bool
	intent            scheduler.SchedulingIntent
	drain             scheduler.DrainState
	state             scheduler.SessionState
	runtime           scheduler.RuntimeProfile
	capacity          scheduler.Capacity
	labels            map[string]string
	cache             scheduler.CacheHints
	hasInventory      bool
	inventoryRevision uint64
	lastHeartbeat     time.Duration
}

// New validates timing relationships and creates an empty Registry.
func New(config Config) (*Registry, error) {
	if config.Clock == nil {
		started := time.Now()
		config.Clock = func() time.Duration { return time.Since(started) }
	}
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if config.SuspectAfter == 0 {
		config.SuspectAfter = DefaultSuspectAfter
	}
	if config.UnavailableAfter == 0 {
		config.UnavailableAfter = DefaultUnavailableAfter
	}
	if config.MaxWorkers == 0 {
		config.MaxWorkers = DefaultMaxWorkers
	}
	if config.HeartbeatInterval < 500*time.Millisecond || config.HeartbeatInterval > HardHeartbeatInterval ||
		config.SuspectAfter <= config.HeartbeatInterval || config.SuspectAfter > HardSuspectAfter ||
		config.UnavailableAfter <= config.SuspectAfter || config.UnavailableAfter > HardUnavailableAfter {
		return nil, errors.New("worker registry timing bounds are invalid")
	}
	if config.MaxWorkers < 1 || config.MaxWorkers > HardMaxWorkers {
		return nil, errors.New("worker registry worker limit is outside v1 bounds")
	}
	elapsed := config.Clock()
	if elapsed < 0 {
		return nil, errors.New("worker registry clock returned negative elapsed time")
	}
	return &Registry{
		clock:             config.Clock,
		heartbeatInterval: config.HeartbeatInterval,
		suspectAfter:      config.SuspectAfter,
		unavailableAfter:  config.UnavailableAfter,
		maxWorkers:        config.MaxWorkers,
		workers:           make(map[string]*workerRecord),
		lastElapsed:       elapsed,
		clockHealthy:      true,
	}, nil
}

// CommitSession records the next exact Session Epoch before registration.
func (r *Registry) CommitSession(registration Registration) error {
	if r == nil {
		return errors.New("worker registry is nil")
	}
	if err := registration.Session.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, exists := r.workers[registration.Session.WorkerID]
	if !exists {
		if len(r.workers) >= r.maxWorkers {
			return classified(problem.CodeOverloaded, "worker registry capacity is full")
		}
		if registration.Session.SessionEpoch != 1 {
			return classified(problem.CodeStaleAssignment, "new Worker session must start at epoch one")
		}
		record = &workerRecord{
			workerID: registration.Session.WorkerID,
			intent:   scheduler.IntentSchedulable,
			drain:    scheduler.DrainNotDraining,
			state:    scheduler.SessionJoining,
		}
		r.workers[record.workerID] = record
	}
	if record.intent == scheduler.IntentRemoved {
		return classified(problem.CodeForbidden, "removed Worker identity cannot register")
	}
	want := record.maxSessionEpoch + 1
	if registration.Session.SessionEpoch != want {
		return classified(problem.CodeStaleAssignment, "session epoch is not the next committed high-water mark")
	}
	record.maxSessionEpoch = registration.Session.SessionEpoch
	record.bootID = registration.Session.BootID
	record.current = registration.Session
	record.registered = false
	record.needsNewSession = false
	record.hasInventory = false
	record.inventoryRevision = 0
	record.runtime = scheduler.RuntimeProfile{}
	record.capacity = scheduler.Capacity{}
	record.labels = nil
	record.cache = scheduler.CacheHints{}
	record.state = scheduler.SessionJoining
	r.revision++
	return nil
}

// Register accepts only the exact Session Epoch already committed by the
// Controller. Repeating the exact registration is idempotent.
func (r *Registry) Register(registration Registration) error {
	if r == nil {
		return errors.New("worker registry is nil")
	}
	if err := registration.Session.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, exists := r.workers[registration.Session.WorkerID]
	if !exists {
		return classified(problem.CodeStaleAssignment, "Worker session is not committed")
	}
	if record.intent == scheduler.IntentRemoved {
		return classified(problem.CodeForbidden, "removed Worker identity cannot register")
	}
	if record.maxSessionEpoch != registration.Session.SessionEpoch || record.bootID != registration.Session.BootID {
		return classified(problem.CodeStaleAssignment, "Worker session is stale")
	}
	if record.needsNewSession {
		return classified(problem.CodeStaleAssignment, "Worker must register a newer session")
	}
	if record.registered && record.current == registration.Session {
		return nil
	}
	if record.registered {
		return classified(problem.CodeStaleAssignment, "another registration owns the current session")
	}
	now, err := r.elapsedLocked()
	if err != nil {
		return err
	}
	record.current = registration.Session
	record.registered = true
	record.hasInventory = false
	record.state = scheduler.SessionJoining
	record.lastHeartbeat = now
	r.revision++
	return nil
}

// ReportInventory accepts the complete same-session inventory and makes the
// session Ready. Intent and derived state are always overwritten from Registry
// state; callers cannot promote a Worker by setting those fields in the input.
func (r *Registry) ReportInventory(inventory Inventory) error {
	if r == nil {
		return errors.New("worker registry is nil")
	}
	if err := inventory.Session.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, err := r.currentLocked(inventory.Session)
	if err != nil {
		return err
	}
	if record.needsNewSession {
		return classified(problem.CodeStaleAssignment, "Worker must register a newer session before reporting inventory")
	}
	if record.hasInventory && inventory.Revision < record.inventoryRevision {
		return classified(problem.CodeStaleGeneration, "Worker inventory revision is stale")
	}
	observation := scheduler.WorkerSnapshot{
		Session:  inventory.Session,
		Runtime:  inventory.Runtime,
		Intent:   record.intent,
		State:    scheduler.SessionReady,
		Drain:    record.drain,
		Capacity: inventory.Capacity,
		Labels:   cloneLabels(inventory.Labels),
		Cache:    cloneHints(inventory.Cache),
	}
	if err := observation.Validate(); err != nil {
		return err
	}
	now, err := r.elapsedLocked()
	if err != nil {
		return err
	}
	record.runtime = observation.Runtime
	record.capacity = observation.Capacity
	record.labels = cloneLabels(observation.Labels)
	record.cache = cloneHints(observation.Cache)
	record.hasInventory = true
	record.inventoryRevision = inventory.Revision
	record.state = scheduler.SessionReady
	record.lastHeartbeat = now
	r.revision++
	return nil
}

// Heartbeat refreshes the exact session's local monotonic lease. It does not
// write a persistent command for every heartbeat.
func (r *Registry) Heartbeat(session servingauth.WorkerSession) error {
	if r == nil {
		return errors.New("worker registry is nil")
	}
	if err := session.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, err := r.currentLocked(session)
	if err != nil {
		return err
	}
	if record.needsNewSession {
		return classified(problem.CodeStaleAssignment, "Worker must register a newer session before sending heartbeats")
	}
	now, err := r.elapsedLocked()
	if err != nil {
		return err
	}
	record.lastHeartbeat = now
	if record.hasInventory && !record.needsNewSession &&
		(record.state == scheduler.SessionSuspect || record.state == scheduler.SessionUnavailable) {
		record.state = scheduler.SessionReady
		r.revision++
	}
	return nil
}

// Evaluate derives Suspect/Unavailable from the injected monotonic clock.
// There is no timer goroutine and a clock regression fails closed.
func (r *Registry) Evaluate() error {
	if r == nil {
		return errors.New("worker registry is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now, err := r.elapsedLocked()
	if err != nil {
		return err
	}
	for _, record := range r.workers {
		if !record.registered || record.needsNewSession || !record.hasInventory {
			continue
		}
		elapsed := now - record.lastHeartbeat
		next := record.state
		switch {
		case elapsed >= r.unavailableAfter:
			next = scheduler.SessionUnavailable
		case elapsed >= r.suspectAfter:
			next = scheduler.SessionSuspect
		default:
			next = scheduler.SessionReady
		}
		if next != record.state {
			record.state = next
			r.revision++
		}
	}
	return nil
}

// SetIntent changes only the control intent. Activation forces a new Session
// before the old Assignment/Inventory can become schedulable again.
func (r *Registry) SetIntent(workerID string, intent scheduler.SchedulingIntent) error {
	if r == nil {
		return errors.New("worker registry is nil")
	}
	if !identifierPattern.MatchString(workerID) || !intentValid(intent) {
		return problem.Invalid("worker_intent", "worker ID or intent is invalid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, exists := r.workers[workerID]
	if !exists {
		return problem.Invalid("worker_id", "worker is not registered")
	}
	if record.intent == scheduler.IntentRemoved {
		return classified(problem.CodeForbidden, "removed Worker intent cannot change")
	}
	if intent == scheduler.IntentRemoved {
		if record.intent != scheduler.IntentDraining || record.drain != scheduler.DrainDrained {
			return classified(problem.CodeConflict, "Worker must be Drained before Removed")
		}
		record.intent = scheduler.IntentRemoved
		record.registered = false
		record.state = scheduler.SessionUnavailable
		r.revision++
		return nil
	}
	if record.intent == intent {
		return nil
	}
	record.intent = intent
	if intent == scheduler.IntentDraining {
		record.drain = scheduler.DrainDraining
	} else {
		record.drain = scheduler.DrainNotDraining
		record.needsNewSession = true
		record.state = scheduler.SessionJoining
	}
	r.revision++
	return nil
}

// UpdateDrain derives Drained only after all calls/Assignments are gone and a
// valid fence proof is available from ACKs or an expired authorization window.
func (r *Registry) UpdateDrain(observation DrainObservation) error {
	if r == nil {
		return errors.New("worker registry is nil")
	}
	if err := observation.Session.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, err := r.currentLocked(observation.Session)
	if err != nil {
		return err
	}
	if record.intent != scheduler.IntentDraining {
		return classified(problem.CodeConflict, "Worker is not draining")
	}
	nextDrain := scheduler.DrainDraining
	if observation.ActiveInvocations == 0 && observation.AssignmentsDrained &&
		(observation.GatewayFencesAcked || observation.AuthorizationExpired) {
		nextDrain = scheduler.DrainDrained
	}
	if record.drain != nextDrain {
		record.drain = nextDrain
		r.revision++
	}
	return nil
}

// Snapshot evaluates no wall-clock state; callers should call Evaluate first
// when they need a fresh liveness classification.
func (r *Registry) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	workers := make([]WorkerView, 0, len(r.workers))
	for _, record := range r.workers {
		workers = append(workers, WorkerView{
			Snapshot: scheduler.WorkerSnapshot{
				Session:  record.current,
				Runtime:  record.runtime,
				Intent:   record.intent,
				State:    record.state,
				Drain:    record.drain,
				Capacity: record.capacity,
				Labels:   cloneLabels(record.labels),
				Cache:    cloneHints(record.cache),
			},
			MaxSessionEpoch: record.maxSessionEpoch,
			LastHeartbeat:   record.lastHeartbeat,
			Registered:      record.registered,
		})
	}
	slices.SortFunc(workers, func(left, right WorkerView) int {
		if left.Snapshot.Session.WorkerID < right.Snapshot.Session.WorkerID {
			return -1
		}
		if left.Snapshot.Session.WorkerID > right.Snapshot.Session.WorkerID {
			return 1
		}
		return 0
	})
	return Snapshot{Revision: r.revision, ClockHealthy: r.clockHealthy, Workers: workers}
}

func (r *Registry) currentLocked(session servingauth.WorkerSession) (*workerRecord, error) {
	record, exists := r.workers[session.WorkerID]
	if !exists || !record.registered || record.current != session {
		return nil, classified(problem.CodeStaleAssignment, "Worker session is stale or unavailable")
	}
	return record, nil
}

func (r *Registry) elapsedLocked() (time.Duration, error) {
	now := r.clock()
	if !r.clockHealthy || now < r.lastElapsed {
		r.clockHealthy = false
		for _, record := range r.workers {
			if record.registered && record.intent != scheduler.IntentRemoved {
				record.state = scheduler.SessionUnavailable
			}
		}
		return 0, classified(problem.CodeControlPlaneStale, "worker registry monotonic clock is unhealthy")
	}
	r.lastElapsed = now
	return now, nil
}

func intentValid(intent scheduler.SchedulingIntent) bool {
	return intent == scheduler.IntentSchedulable || intent == scheduler.IntentDraining || intent == scheduler.IntentRemoved
}

func cloneLabels(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func cloneHints(source scheduler.CacheHints) scheduler.CacheHints {
	clone := scheduler.CacheHints{}
	if source.Artifacts != nil {
		clone.Artifacts = make(map[digest.SHA256]struct{}, len(source.Artifacts))
		for key, value := range source.Artifacts {
			clone.Artifacts[key] = value
		}
	}
	if source.Compiled != nil {
		clone.Compiled = make(map[workercache.Key]struct{}, len(source.Compiled))
		for key, value := range source.Compiled {
			clone.Compiled[key] = value
		}
	}
	return clone
}

func classified(code problem.Code, message string) error {
	return &problem.Error{Code: code, Message: message}
}
