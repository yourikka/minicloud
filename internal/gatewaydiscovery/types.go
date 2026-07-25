// Package gatewaydiscovery atomically applies complete ServingSnapshot events
// and enforces one process-local monotonic LKG window.
package gatewaydiscovery

import (
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/servingauth"
)

const (
	DefaultMaxStale     = 5 * time.Minute
	HardMaxStale        = 10 * time.Minute
	DefaultMaxFunctions = 10_000
	HardMaxFunctions    = 10_000
)

// Config bounds the in-memory Gateway serving view.
type Config struct {
	Clock servingauth.MonotonicClock
	// MaxStale is nil for the v1 default. A non-nil zero duration means that
	// serving is allowed only while the control Watch is connected.
	MaxStale     *time.Duration
	MaxFunctions int
}

// Event is one global Serving Watch position. Full events replace every
// Function atomically; incremental events replace exactly one Function.
type Event struct {
	Full            bool
	DiscoveryEpoch  uint64
	ServingSequence uint64
	Snapshots       []discovery.Snapshot
}

// Status is a non-sensitive view of the Gateway cache state.
type Status struct {
	DiscoveryEpoch  uint64
	ServingSequence uint64
	Functions       int
	FullSynced      bool
	NeedsFullSync   bool
	WatchConnected  bool
	ClockHealthy    bool
}

// View retains the complete checksummed Snapshot and applies local health only
// to Endpoints. A Gateway cannot mint a new authoritative checksum.
type View struct {
	Snapshot  discovery.Snapshot
	Endpoints []discovery.Endpoint
}

type functionRecord struct {
	snapshot   discovery.Snapshot
	receivedAt time.Duration
	suppressed map[string]servingauth.AssignmentIdentity
}

type eventPosition struct {
	epoch       uint64
	sequence    uint64
	fingerprint digest.SHA256
}
