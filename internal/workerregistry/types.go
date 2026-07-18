// Package workerregistry models the bounded Worker Registry and Leader-derived
// heartbeat observations. Persistent Raft integration is intentionally outside
// this package.
package workerregistry

import (
	"time"

	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
)

const (
	DefaultHeartbeatInterval = 2 * time.Second
	DefaultSuspectAfter      = 6 * time.Second
	DefaultUnavailableAfter  = 10 * time.Second
	HardHeartbeatInterval    = 30 * time.Second
	HardSuspectAfter         = 10 * time.Minute
	HardUnavailableAfter     = 10 * time.Minute
	DefaultMaxWorkers        = 1_000
	HardMaxWorkers           = 1_000
)

// Config defines bounded Worker Registry timing and cardinality.
type Config struct {
	Clock             servingauth.MonotonicClock
	HeartbeatInterval time.Duration
	SuspectAfter      time.Duration
	UnavailableAfter  time.Duration
	MaxWorkers        int
}

// Registration is the session identity committed by the Controller before a
// Worker may be accepted. The Registry never increments this epoch itself.
type Registration struct {
	Session servingauth.WorkerSession
}

// Inventory is a complete same-session Worker observation.
type Inventory struct {
	Revision uint64
	Session  servingauth.WorkerSession
	Runtime  scheduler.RuntimeProfile
	Capacity scheduler.Capacity
	Labels   map[string]string
	Cache    scheduler.CacheHints
}

// DrainObservation contains only local/Leader observations used to derive the
// orthogonal Drain state. ACK and expiry proofs come from their respective
// owners and are not inferred by this package.
type DrainObservation struct {
	Session              servingauth.WorkerSession
	ActiveInvocations    uint64
	AssignmentsDrained   bool
	GatewayFencesAcked   bool
	AuthorizationExpired bool
}

// WorkerView is a defensive registry view suitable for Scheduler conversion.
type WorkerView struct {
	Snapshot        scheduler.WorkerSnapshot
	MaxSessionEpoch uint64
	LastHeartbeat   time.Duration
	Registered      bool
}

// Snapshot is a bounded, sorted registry view.
type Snapshot struct {
	Revision     uint64
	ClockHealthy bool
	Workers      []WorkerView
}
