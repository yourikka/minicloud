// Package servingauth enforces Worker-side Assignment and Leader fencing for
// new invocations.
package servingauth

import (
	"errors"
	"sync"
	"time"

	"github.com/yourikka/minicloud/internal/problem"
)

const (
	DefaultMaxTTL            = 5 * time.Minute
	HardMaxTTL               = 10 * time.Minute
	DefaultMaxAuthorizations = 4096
	HardMaxAuthorizations    = 65536
)

// Config defines one boot-scoped authorization gate.
type Config struct {
	Worker            WorkerProcess
	Clock             MonotonicClock
	MaxTTL            time.Duration
	MaxAuthorizations int
}

// Snapshot is a non-sensitive view of the current fencing state.
type Snapshot struct {
	HighestDiscoveryEpoch uint64
	CurrentSessionEpoch   uint64
	ControlConnected      bool
	TrackedAssignments    int
	StoredAuthorizations  int
	ClockHealthy          bool
}

// Gate is safe for concurrent use.
type Gate struct {
	worker            WorkerProcess
	clock             MonotonicClock
	maxTTL            time.Duration
	maxAuthorizations int

	mu                    sync.Mutex
	highestDiscoveryEpoch uint64
	currentSessionEpoch   uint64
	control               controlState
	assignments           map[string]AssignmentIdentity
	authorizations        map[string]authorizationRecord
	lastElapsed           time.Duration
	clockHealthy          bool
}

type controlState struct {
	connection ControlConnection
	connected  bool
}

type authorizationRecord struct {
	authorization Authorization
	connection    ControlConnection
	receivedAt    time.Duration
}

// New validates the Worker identity and all memory/time bounds.
func New(config Config) (*Gate, error) {
	if err := config.Worker.validate(); err != nil {
		return nil, err
	}
	if config.Clock == nil {
		config.Clock = newProcessClock()
	}
	if config.MaxTTL == 0 {
		config.MaxTTL = DefaultMaxTTL
	}
	if config.MaxAuthorizations == 0 {
		config.MaxAuthorizations = DefaultMaxAuthorizations
	}
	if config.MaxTTL <= 0 || config.MaxTTL > HardMaxTTL {
		return nil, errors.New("serving authorization ttl limit is outside v1 bounds")
	}
	if config.MaxAuthorizations < 1 || config.MaxAuthorizations > HardMaxAuthorizations {
		return nil, errors.New("serving authorization entry limit is outside v1 bounds")
	}
	elapsed := config.Clock()
	if elapsed < 0 {
		return nil, errors.New("serving authorization monotonic clock returned negative elapsed time")
	}
	return &Gate{
		worker:            config.Worker,
		clock:             config.Clock,
		maxTTL:            config.MaxTTL,
		maxAuthorizations: config.MaxAuthorizations,
		assignments:       make(map[string]AssignmentIdentity),
		authorizations:    make(map[string]authorizationRecord),
		lastElapsed:       elapsed,
		clockHealthy:      true,
	}, nil
}

// AcceptAuthoritativeControl records a connection only after the caller has
// authenticated the peer, verified its current Leader barrier, and confirmed
// that the Worker Registry committed its Session Epoch.
func (g *Gate) AcceptAuthoritativeControl(connection ControlConnection) error {
	if err := connection.validate(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if connection.DiscoveryEpoch < g.highestDiscoveryEpoch {
		return classified(problem.CodeStaleGeneration, "control discovery epoch is stale")
	}
	if connection.DiscoveryEpoch > g.highestDiscoveryEpoch {
		g.highestDiscoveryEpoch = connection.DiscoveryEpoch
		clear(g.authorizations)
		g.control.connected = false
	}
	if connection.SessionEpoch < g.currentSessionEpoch {
		return classified(problem.CodeStaleAssignment, "worker session epoch is stale")
	}
	if connection.SessionEpoch == g.currentSessionEpoch {
		idempotent := g.control.connected && g.control.connection == connection
		if !idempotent {
			return classified(problem.CodeStaleAssignment, "worker session epoch must increase on reconnect")
		}
		return nil
	}
	g.currentSessionEpoch = connection.SessionEpoch
	clear(g.assignments)
	clear(g.authorizations)
	g.control = controlState{connection: connection, connected: true}
	return nil
}

// DisconnectControl invalidates live-only permissions bound to this exact
// connection. TTL permissions continue until their local monotonic expiry.
func (g *Gate) DisconnectControl(connection ControlConnection) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.control.connection == connection {
		g.control.connected = false
	}
}

// Install adds or refreshes one independent authorization received on the
// current authoritative control connection.
func (g *Gate) Install(connection ControlConnection, authorization Authorization) error {
	if err := authorization.validate(g.maxTTL); err != nil {
		return err
	}
	if err := connection.validate(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.currentControlLocked(connection) ||
		authorization.Fence.DiscoveryEpoch != connection.DiscoveryEpoch {
		return classified(problem.CodeStaleAssignment, "authenticated control connection is unavailable")
	}
	if authorization.Fence.Assignment.Worker != g.currentWorkerLocked() {
		return classified(problem.CodeStaleAssignment, "authorization targets another worker session")
	}
	now, err := g.elapsedLocked()
	if err != nil {
		return err
	}
	assignmentID := authorization.Fence.Assignment.AssignmentID
	identity := authorization.Fence.Assignment
	if previous, exists := g.assignments[assignmentID]; exists {
		if err := compatibleRefresh(previous, identity); err != nil {
			return err
		}
	} else if len(g.assignments) >= g.maxAuthorizations {
		return classified(problem.CodeOverloaded, "serving authorization capacity is full")
	}
	g.assignments[assignmentID] = identity
	g.authorizations[assignmentID] = authorizationRecord{
		authorization: authorization,
		connection:    connection,
		receivedAt:    now,
	}
	return nil
}

// AuthorizeSync checks one Invocation RPC at its acceptance point. It returns
// no reusable capability, and drain-only Assignments cannot accept synchronous
// work.
func (g *Gate) AuthorizeSync(fence InvocationFence) error {
	if err := fence.validate(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if fence.DiscoveryEpoch < g.highestDiscoveryEpoch {
		return classified(problem.CodeStaleAssignment, "invocation discovery epoch is stale")
	}
	if fence.Assignment.Mode != ModeNormal {
		return classified(problem.CodeStaleAssignment, "assignment does not accept synchronous work")
	}
	record, exists := g.authorizations[fence.Assignment.AssignmentID]
	if !exists || record.authorization.Fence != fence {
		return classified(problem.CodeStaleAssignment, "invocation fence does not match serving authorization")
	}
	now, err := g.elapsedLocked()
	if err != nil {
		return err
	}
	if !g.validLocked(record, now) {
		delete(g.authorizations, fence.Assignment.AssignmentID)
		return classified(problem.CodeStaleAssignment, "serving authorization is no longer valid")
	}
	return nil
}

// Revoke removes one local permission when its Replica stops or is replaced.
func (g *Gate) Revoke(assignmentID string) {
	if g == nil || assignmentID == "" {
		return
	}
	g.mu.Lock()
	delete(g.assignments, assignmentID)
	delete(g.authorizations, assignmentID)
	g.mu.Unlock()
}

// Snapshot returns bounded state without exposing policy digests or IDs.
func (g *Gate) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Snapshot{
		HighestDiscoveryEpoch: g.highestDiscoveryEpoch,
		CurrentSessionEpoch:   g.currentSessionEpoch,
		ControlConnected:      g.control.connected,
		TrackedAssignments:    len(g.assignments),
		StoredAuthorizations:  len(g.authorizations),
		ClockHealthy:          g.clockHealthy,
	}
}

func (g *Gate) currentWorkerLocked() WorkerSession {
	return WorkerSession{
		WorkerID:     g.worker.WorkerID,
		BootID:       g.worker.BootID,
		SessionEpoch: g.currentSessionEpoch,
	}
}

func (g *Gate) currentControlLocked(connection ControlConnection) bool {
	return g.control.connected &&
		g.control.connection == connection &&
		connection.DiscoveryEpoch == g.highestDiscoveryEpoch
}

func (g *Gate) elapsedLocked() (time.Duration, error) {
	now := g.clock()
	if !g.clockHealthy || now < g.lastElapsed {
		g.clockHealthy = false
		return 0, classified(problem.CodeStaleAssignment, "worker monotonic clock is unhealthy")
	}
	g.lastElapsed = now
	return now, nil
}

func (g *Gate) validLocked(record authorizationRecord, now time.Duration) bool {
	authorization := record.authorization
	if authorization.Fence.DiscoveryEpoch != g.highestDiscoveryEpoch || now < record.receivedAt {
		return false
	}
	if authorization.Lifetime == LifetimeLiveOnly {
		return g.currentControlLocked(record.connection)
	}
	return now-record.receivedAt < authorization.TTL
}

func compatibleRefresh(previous, next AssignmentIdentity) error {
	if previous.AssignmentID != next.AssignmentID {
		return classified(problem.CodeStaleAssignment, "authorization assignment id changed")
	}
	generationChanged := previous.VersionID != next.VersionID ||
		previous.AdmissionEpoch != next.AdmissionEpoch ||
		previous.DeploymentGeneration != next.DeploymentGeneration ||
		previous.PolicyDigest != next.PolicyDigest
	if generationChanged {
		return classified(problem.CodeStaleGeneration, "authorization generation identity changed")
	}
	if previous != next {
		return classified(problem.CodeStaleAssignment, "authorization assignment identity changed")
	}
	return nil
}

func classified(code problem.Code, message string) error {
	return &problem.Error{Code: code, Message: message}
}
