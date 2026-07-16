// Package rpc contains transport-neutral contracts shared by MiniCloud's
// internal Controller, Gateway, and Worker RPC implementations.
//
// The package deliberately does not define a wire framing format or an
// authentication mechanism. It provides the version, bounded-input,
// cross-process deadline, and per-peer concurrency rules that every future
// transport must enforce.
package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

const (
	// SchemaVersion is the only v1 internal RPC schema accepted by this
	// package. Every handshake and message header must carry it.
	SchemaVersion = "minicloud-rpc-v1"

	// DefaultMaxMessageBytes is the v1 baseline. A concrete RPC may use a
	// lower limit, but no caller may raise it through this package.
	DefaultMaxMessageBytes int64 = 4 << 20
	HardMaxMessageBytes    int64 = 4 << 20

	// DefaultMaxDeadline is the v1 default and hard upper bound for one RPC
	// hop's remaining budget.
	DefaultMaxDeadline = 10 * time.Second
	HardMaxDeadline    = 10 * time.Second

	// DefaultMaxConcurrent is the per-peer v1 baseline and hard upper bound.
	DefaultMaxConcurrent = 256
	HardMaxConcurrent    = 256
)

var (
	// ErrInvalidLimits indicates that a configured RPC bound is outside v1.
	ErrInvalidLimits = errors.New("rpc limits are invalid")
	// ErrInvalidVersion indicates a missing or malformed schema version.
	ErrInvalidVersion = errors.New("rpc schema version is invalid")
	// ErrIncompatibleVersion indicates that peers do not share one schema.
	ErrIncompatibleVersion = errors.New("rpc schema version is incompatible")
	// ErrMessageTooLarge indicates that a message exceeded its configured
	// bounded-input limit.
	ErrMessageTooLarge = errors.New("rpc message exceeds size limit")
	// ErrInvalidDeadline indicates a non-positive remaining budget.
	ErrInvalidDeadline = errors.New("rpc deadline is invalid")
	// ErrContextRequired indicates a missing context at a cancellation-aware
	// API boundary.
	ErrContextRequired = errors.New("rpc context is required")
	// ErrConcurrentLimit indicates that TryAcquire found no available peer
	// concurrency slot.
	ErrConcurrentLimit = errors.New("rpc peer concurrency limit reached")
	// ErrLimiterUninitialized indicates that a zero-value PeerLimiter was used
	// instead of one returned by NewPeerLimiter.
	ErrLimiterUninitialized = errors.New("rpc peer limiter is uninitialized")
)

// Limits contains the independently configurable v1 RPC bounds. Zero fields
// are filled from DefaultLimits by Normalize; negative fields are invalid.
type Limits struct {
	MaxMessageBytes int64         `json:"max_message_bytes"`
	MaxDeadline     time.Duration `json:"max_deadline"`
	MaxConcurrent   int           `json:"max_concurrent"`
}

// DefaultLimits returns the v1 baseline limits.
func DefaultLimits() Limits {
	return Limits{
		MaxMessageBytes: DefaultMaxMessageBytes,
		MaxDeadline:     DefaultMaxDeadline,
		MaxConcurrent:   DefaultMaxConcurrent,
	}
}

// Validate checks explicit limits without applying defaults.
func (l Limits) Validate() error {
	if l.MaxMessageBytes <= 0 || l.MaxMessageBytes > HardMaxMessageBytes {
		return fmt.Errorf("%w: max message bytes must be between 1 and %d", ErrInvalidLimits, HardMaxMessageBytes)
	}
	if l.MaxDeadline <= 0 || l.MaxDeadline > HardMaxDeadline {
		return fmt.Errorf("%w: max deadline must be positive and at most %s", ErrInvalidLimits, HardMaxDeadline)
	}
	if l.MaxConcurrent <= 0 || l.MaxConcurrent > HardMaxConcurrent {
		return fmt.Errorf("%w: max concurrent must be between 1 and %d", ErrInvalidLimits, HardMaxConcurrent)
	}
	return nil
}

// Normalize fills zero-valued fields with v1 defaults and then validates the
// resulting bounds. It is intended for configuration constructors.
func (l Limits) Normalize() (Limits, error) {
	defaults := DefaultLimits()
	if l.MaxMessageBytes == 0 {
		l.MaxMessageBytes = defaults.MaxMessageBytes
	}
	if l.MaxDeadline == 0 {
		l.MaxDeadline = defaults.MaxDeadline
	}
	if l.MaxConcurrent == 0 {
		l.MaxConcurrent = defaults.MaxConcurrent
	}
	if err := l.Validate(); err != nil {
		return Limits{}, err
	}
	return l, nil
}

// Header is the version-only metadata required on every internal RPC message
// and handshake. Trusted peer identity is supplied by the authenticated
// connection, never by this header.
type Header struct {
	SchemaVersion string `json:"schema_version"`
}

// Validate checks one message or handshake header.
func (h Header) Validate() error {
	return ValidateVersion(h.SchemaVersion)
}

// ValidateVersion validates one protocol version string.
func ValidateVersion(version string) error {
	if version == "" {
		return fmt.Errorf("%w: schema version is required", ErrInvalidVersion)
	}
	if version != SchemaVersion {
		return fmt.Errorf("%w: got %q", ErrIncompatibleVersion, version)
	}
	return nil
}

// Negotiate accepts a connection only when both peers advertise the exact
// same supported schema version.
func Negotiate(local, peer Header) error {
	if local.SchemaVersion == "" || peer.SchemaVersion == "" {
		return fmt.Errorf("%w: both peers must advertise a schema version", ErrInvalidVersion)
	}
	if local.SchemaVersion != peer.SchemaVersion {
		return fmt.Errorf("%w: local=%q peer=%q", ErrIncompatibleVersion, local.SchemaVersion, peer.SchemaVersion)
	}
	if err := local.Validate(); err != nil {
		return fmt.Errorf("local rpc header: %w", err)
	}
	return nil
}

// ValidateMessage checks a message size before a transport writes it.
func ValidateMessage(message []byte, maxBytes int64) error {
	if err := validateMessageLimit(maxBytes); err != nil {
		return err
	}
	if int64(len(message)) > maxBytes {
		return fmt.Errorf("%w: got %d bytes, limit %d", ErrMessageTooLarge, len(message), maxBytes)
	}
	return nil
}

// ReadMessage reads at most maxBytes+1 bytes. An oversized message is rejected
// without reading or allocating the unbounded remainder.
func ReadMessage(source io.Reader, maxBytes int64) ([]byte, error) {
	if source == nil {
		return nil, errors.New("rpc message source is required")
	}
	if err := validateMessageLimit(maxBytes); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(source, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading rpc message: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: got more than %d bytes", ErrMessageTooLarge, maxBytes)
	}
	return data, nil
}

func validateMessageLimit(maxBytes int64) error {
	if maxBytes <= 0 || maxBytes > HardMaxMessageBytes {
		return fmt.Errorf("%w: max message bytes must be between 1 and %d", ErrInvalidLimits, HardMaxMessageBytes)
	}
	return nil
}

// Budget is the only deadline value that may cross an RPC boundary. It is a
// remaining duration in nanoseconds, never a wall-clock timestamp or a sender
// monotonic timestamp.
type Budget struct {
	RemainingNanos int64 `json:"deadline_remaining_nanos"`
}

// NewBudget validates and clamps a remaining duration to the configured hop
// maximum.
func NewBudget(remaining time.Duration, limits Limits) (Budget, error) {
	normalized, err := limits.Normalize()
	if err != nil {
		return Budget{}, err
	}
	effective, err := clampRemaining(remaining, 0, false, normalized)
	if err != nil {
		return Budget{}, err
	}
	return Budget{RemainingNanos: int64(effective)}, nil
}

// Duration validates and returns the budget after applying the local hop
// maximum. This makes decoding untrusted wire data bounded and deterministic.
func (b Budget) Duration(limits Limits) (time.Duration, error) {
	normalized, err := limits.Normalize()
	if err != nil {
		return 0, err
	}
	return clampRemaining(time.Duration(b.RemainingNanos), 0, false, normalized)
}

// ClampRemaining applies the cross-process rule: a hop may only reduce an
// incoming budget, never extend it. Set parentHasDeadline when the parent
// context has a deadline; parentRemaining is ignored otherwise.
func ClampRemaining(
	incoming time.Duration,
	parentRemaining time.Duration,
	parentHasDeadline bool,
	limits Limits,
) (time.Duration, error) {
	normalized, err := limits.Normalize()
	if err != nil {
		return 0, err
	}
	return clampRemaining(incoming, parentRemaining, parentHasDeadline, normalized)
}

func clampRemaining(
	incoming time.Duration,
	parentRemaining time.Duration,
	parentHasDeadline bool,
	limits Limits,
) (time.Duration, error) {
	if incoming <= 0 {
		return 0, fmt.Errorf("%w: remaining duration must be positive", ErrInvalidDeadline)
	}
	effective := incoming
	if effective > limits.MaxDeadline {
		effective = limits.MaxDeadline
	}
	if parentHasDeadline {
		if parentRemaining <= 0 {
			return 0, fmt.Errorf("%w: parent deadline has expired", context.DeadlineExceeded)
		}
		if parentRemaining < effective {
			effective = parentRemaining
		}
	}
	if effective <= 0 {
		return 0, context.DeadlineExceeded
	}
	return effective, nil
}

// OutboundRemaining derives the current local remaining budget. A context
// without a deadline receives the configured maximum; an expired or canceled
// context is rejected.
func OutboundRemaining(ctx context.Context, limits Limits) (time.Duration, error) {
	if ctx == nil {
		return 0, ErrContextRequired
	}
	normalized, err := limits.Normalize()
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		return normalized.MaxDeadline, nil
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, context.DeadlineExceeded
	}
	return clampRemaining(remaining, 0, false, normalized)
}

// OutboundBudget packages OutboundRemaining for a wire field.
func OutboundBudget(ctx context.Context, limits Limits) (Budget, error) {
	remaining, err := OutboundRemaining(ctx, limits)
	if err != nil {
		return Budget{}, err
	}
	return Budget{RemainingNanos: int64(remaining)}, nil
}

// WithRemaining creates a child context using only the supplied remaining
// duration and the parent's cancellation/deadline. It never creates a new
// background context and never extends a parent budget.
func WithRemaining(
	parent context.Context,
	remaining time.Duration,
	limits Limits,
) (context.Context, context.CancelFunc, error) {
	return WithBudget(parent, Budget{RemainingNanos: int64(remaining)}, limits)
}

// WithBudget is the wire-facing form of WithRemaining.
func WithBudget(
	parent context.Context,
	budget Budget,
	limits Limits,
) (context.Context, context.CancelFunc, error) {
	if parent == nil {
		return nil, nil, ErrContextRequired
	}
	normalized, err := limits.Normalize()
	if err != nil {
		return nil, nil, err
	}
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}
	parentDeadline, hasParentDeadline := parent.Deadline()
	var parentRemaining time.Duration
	if hasParentDeadline {
		parentRemaining = time.Until(parentDeadline)
	}
	effective, err := clampRemaining(
		time.Duration(budget.RemainingNanos),
		parentRemaining,
		hasParentDeadline,
		normalized,
	)
	if err != nil {
		return nil, nil, err
	}
	child, cancel := context.WithTimeout(parent, effective)
	return child, cancel, nil
}

// PeerLimiter bounds concurrent work for one already-authenticated peer
// connection. Construct one limiter per connection; no node or role identity
// is accepted from an RPC envelope.
type PeerLimiter struct {
	noCopy noCopy
	slots  chan struct{}
	max    int
}

// noCopy lets go vet flag accidental value copies of stateful limiters.
type noCopy struct{}

func (*noCopy) Lock() {}

func (*noCopy) Unlock() {}

// NewPeerLimiter creates a bounded per-peer limiter. Zero fields in limits use
// v1 defaults; a configured limit may only be lower than the hard maximum.
func NewPeerLimiter(limits Limits) (*PeerLimiter, error) {
	normalized, err := limits.Normalize()
	if err != nil {
		return nil, err
	}
	return &PeerLimiter{
		slots: make(chan struct{}, normalized.MaxConcurrent),
		max:   normalized.MaxConcurrent,
	}, nil
}

// Acquire waits for one slot or returns the parent context's cancellation.
func (l *PeerLimiter) Acquire(ctx context.Context) (*Permit, error) {
	if l == nil || l.slots == nil {
		return nil, ErrLimiterUninitialized
	}
	if ctx == nil {
		return nil, ErrContextRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case l.slots <- struct{}{}:
		// A cancellation and a newly available slot can become ready at the
		// same instant. Recheck after the send so a canceled request never
		// leaves an acquired permit behind.
		if err := ctx.Err(); err != nil {
			<-l.slots
			return nil, err
		}
		return newPermit(l), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TryAcquire obtains a slot without waiting.
func (l *PeerLimiter) TryAcquire() (*Permit, error) {
	if l == nil || l.slots == nil {
		return nil, ErrLimiterUninitialized
	}
	select {
	case l.slots <- struct{}{}:
		return newPermit(l), nil
	default:
		return nil, ErrConcurrentLimit
	}
}

// Inflight reports the current number of acquired permits.
func (l *PeerLimiter) Inflight() int {
	if l == nil {
		return 0
	}
	return len(l.slots)
}

// MaxConcurrent reports the configured per-peer concurrency bound.
func (l *PeerLimiter) MaxConcurrent() int {
	if l == nil {
		return 0
	}
	return l.max
}

// Permit owns one PeerLimiter slot. Release is safe to call more than once.
type Permit struct {
	limiter  *PeerLimiter
	released *atomic.Bool
}

func newPermit(limiter *PeerLimiter) *Permit {
	return &Permit{
		limiter:  limiter,
		released: &atomic.Bool{},
	}
}

// Release returns the slot. A copied Permit still shares its release state,
// so accidental duplicate calls cannot underflow the limiter.
func (p *Permit) Release() {
	if p == nil || p.limiter == nil || p.released == nil {
		return
	}
	if !p.released.CompareAndSwap(false, true) {
		return
	}
	<-p.limiter.slots
}
