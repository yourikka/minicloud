package rpc

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLimitsNormalizeAndValidate(t *testing.T) {
	t.Parallel()

	defaults := DefaultLimits()
	if defaults.MaxMessageBytes != 4<<20 ||
		defaults.MaxDeadline != 10*time.Second ||
		defaults.MaxConcurrent != 256 {
		t.Fatalf("DefaultLimits() = %+v, want v1 baseline", defaults)
	}

	normalized, err := (Limits{}).Normalize()
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if normalized != defaults {
		t.Fatalf("Normalize() = %+v, want %+v", normalized, defaults)
	}
	if err := (Limits{}).Validate(); !errors.Is(err, ErrInvalidLimits) {
		t.Fatalf("Validate() error = %v, want ErrInvalidLimits", err)
	}

	tests := []struct {
		name   string
		limits Limits
	}{
		{
			name:   "message below one",
			limits: Limits{MaxMessageBytes: -1},
		},
		{
			name:   "message above hard maximum",
			limits: Limits{MaxMessageBytes: HardMaxMessageBytes + 1},
		},
		{
			name:   "deadline below one",
			limits: Limits{MaxDeadline: -time.Nanosecond},
		},
		{
			name:   "deadline above hard maximum",
			limits: Limits{MaxDeadline: HardMaxDeadline + time.Nanosecond},
		},
		{
			name:   "concurrency below one",
			limits: Limits{MaxConcurrent: -1},
		},
		{
			name:   "concurrency above hard maximum",
			limits: Limits{MaxConcurrent: HardMaxConcurrent + 1},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.limits.Validate(); !errors.Is(err, ErrInvalidLimits) {
				t.Fatalf("Validate() error = %v, want ErrInvalidLimits", err)
			}
			if _, err := test.limits.Normalize(); !errors.Is(err, ErrInvalidLimits) {
				t.Fatalf("Normalize() error = %v, want ErrInvalidLimits", err)
			}
		})
	}
}

func TestHeaderNegotiationRejectsUnsupportedVersions(t *testing.T) {
	t.Parallel()

	valid := Header{SchemaVersion: SchemaVersion}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Header.Validate() error = %v", err)
	}
	if err := Negotiate(valid, valid); err != nil {
		t.Fatalf("Negotiate() error = %v", err)
	}

	tests := []struct {
		name    string
		local   Header
		peer    Header
		wantErr error
	}{
		{
			name:    "missing local version",
			local:   Header{},
			peer:    valid,
			wantErr: ErrInvalidVersion,
		},
		{
			name:    "missing peer version",
			local:   valid,
			peer:    Header{},
			wantErr: ErrInvalidVersion,
		},
		{
			name:    "different versions",
			local:   valid,
			peer:    Header{SchemaVersion: "minicloud-rpc-v2"},
			wantErr: ErrIncompatibleVersion,
		},
		{
			name:    "both unsupported",
			local:   Header{SchemaVersion: "minicloud-rpc-v2"},
			peer:    Header{SchemaVersion: "minicloud-rpc-v2"},
			wantErr: ErrIncompatibleVersion,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := Negotiate(test.local, test.peer); !errors.Is(err, test.wantErr) {
				t.Fatalf("Negotiate() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestReadMessageBoundsInputBeforeDecode(t *testing.T) {
	t.Parallel()

	const maxBytes = int64(32)
	message := bytes.Repeat([]byte{'m'}, int(maxBytes))
	got, err := ReadMessage(bytes.NewReader(message), maxBytes)
	if err != nil {
		t.Fatalf("ReadMessage(exact limit) error = %v", err)
	}
	if !bytes.Equal(got, message) {
		t.Fatalf("ReadMessage(exact limit) = %d bytes, want %d", len(got), len(message))
	}

	tooLarge := append(append([]byte(nil), message...), 'x')
	got, err = ReadMessage(bytes.NewReader(tooLarge), maxBytes)
	if !errors.Is(err, ErrMessageTooLarge) || got != nil {
		t.Fatalf("ReadMessage(oversized) = (%d bytes, %v), want nil and ErrMessageTooLarge", len(got), err)
	}
	if err := ValidateMessage(message, maxBytes); err != nil {
		t.Fatalf("ValidateMessage(exact limit) error = %v", err)
	}
	if err := ValidateMessage(tooLarge, maxBytes); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("ValidateMessage(oversized) error = %v, want ErrMessageTooLarge", err)
	}
	if _, err := ReadMessage(strings.NewReader("ignored"), HardMaxMessageBytes+1); !errors.Is(err, ErrInvalidLimits) {
		t.Fatalf("ReadMessage(invalid limit) error = %v, want ErrInvalidLimits", err)
	}
	if _, err := ReadMessage(nil, maxBytes); err == nil {
		t.Fatal("ReadMessage(nil) returned nil error")
	}

	limitedSource := &countingReader{source: bytes.NewReader(bytes.Repeat([]byte{'x'}, 1024))}
	if _, err := ReadMessage(limitedSource, maxBytes); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("ReadMessage(large source) error = %v, want ErrMessageTooLarge", err)
	}
	if limitedSource.readBytes > int(maxBytes)+1 {
		t.Fatalf("ReadMessage consumed %d bytes, want at most %d", limitedSource.readBytes, maxBytes+1)
	}
}

type countingReader struct {
	source    *bytes.Reader
	readBytes int
}

func (r *countingReader) Read(p []byte) (int, error) {
	read, err := r.source.Read(p)
	r.readBytes += read
	return read, err
}

func TestClampRemainingNeverExtendsBudget(t *testing.T) {
	t.Parallel()
	limits := Limits{MaxDeadline: 10 * time.Second}
	tests := []struct {
		name              string
		incoming          time.Duration
		parentRemaining   time.Duration
		parentHasDeadline bool
		want              time.Duration
		wantErr           error
	}{
		{
			name:              "incoming is lower",
			incoming:          2 * time.Second,
			parentRemaining:   5 * time.Second,
			parentHasDeadline: true,
			want:              2 * time.Second,
		},
		{
			name:              "parent is lower",
			incoming:          8 * time.Second,
			parentRemaining:   3 * time.Second,
			parentHasDeadline: true,
			want:              3 * time.Second,
		},
		{
			name:     "hop maximum clamps",
			incoming: 20 * time.Second,
			want:     10 * time.Second,
		},
		{
			name:     "zero incoming",
			incoming: 0,
			wantErr:  ErrInvalidDeadline,
		},
		{
			name:              "expired parent",
			incoming:          time.Second,
			parentHasDeadline: true,
			wantErr:           context.DeadlineExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ClampRemaining(
				test.incoming,
				test.parentRemaining,
				test.parentHasDeadline,
				limits,
			)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("ClampRemaining() error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("ClampRemaining() = (%s, %v), want (%s, nil)", got, err, test.want)
			}
		})
	}
}

func TestBudgetAndContextPropagation(t *testing.T) {
	t.Parallel()
	limits := Limits{MaxDeadline: 5 * time.Second}

	budget, err := NewBudget(10*time.Second, limits)
	if err != nil {
		t.Fatalf("NewBudget() error = %v", err)
	}
	if budget.RemainingNanos != int64(5*time.Second) {
		t.Fatalf("NewBudget() = %+v, want five-second clamp", budget)
	}
	if got, err := budget.Duration(limits); err != nil || got != 5*time.Second {
		t.Fatalf("Budget.Duration() = (%s, %v), want five seconds", got, err)
	}
	if _, err := (Budget{}).Duration(limits); !errors.Is(err, ErrInvalidDeadline) {
		t.Fatalf("zero Budget.Duration() error = %v, want ErrInvalidDeadline", err)
	}

	backgroundRemaining, err := OutboundRemaining(context.Background(), limits)
	if err != nil || backgroundRemaining != 5*time.Second {
		t.Fatalf("OutboundRemaining(background) = (%s, %v), want five seconds", backgroundRemaining, err)
	}
	outbound, err := OutboundBudget(context.Background(), limits)
	if err != nil || outbound.RemainingNanos != int64(5*time.Second) {
		t.Fatalf("OutboundBudget(background) = (%+v, %v), want five-second budget", outbound, err)
	}

	parent, cancelParent := context.WithCancel(context.Background())
	child, cancelChild, err := WithBudget(parent, outbound, limits)
	if err != nil {
		t.Fatalf("WithBudget() error = %v", err)
	}
	cancelParent()
	select {
	case <-child.Done():
		if !errors.Is(child.Err(), context.Canceled) {
			t.Fatalf("child.Err() = %v, want context.Canceled", child.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("child context did not observe parent cancellation")
	}
	cancelChild()

	deadlineParent, cancelDeadlineParent := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDeadlineParent()
	deadline, ok := deadlineParent.Deadline()
	if !ok {
		t.Fatal("parent context has no deadline")
	}
	child, cancelChild, err = WithRemaining(deadlineParent, 4*time.Second, Limits{MaxDeadline: 10 * time.Second})
	if err != nil {
		t.Fatalf("WithRemaining() error = %v", err)
	}
	childDeadline, ok := child.Deadline()
	if !ok || childDeadline.After(deadline) {
		t.Fatalf("child deadline = %v, parent deadline = %v; child extended parent", childDeadline, deadline)
	}
	cancelChild()

	expiredParent, cancelExpiredParent := context.WithCancel(context.Background())
	cancelExpiredParent()
	if _, _, err := WithRemaining(expiredParent, time.Second, limits); !errors.Is(err, context.Canceled) {
		t.Fatalf("WithRemaining(canceled parent) error = %v, want context.Canceled", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := OutboundRemaining(canceled, limits); !errors.Is(err, context.Canceled) {
		t.Fatalf("OutboundRemaining(canceled) error = %v, want context.Canceled", err)
	}
	if _, _, err := WithRemaining(nil, time.Second, limits); !errors.Is(err, ErrContextRequired) {
		t.Fatalf("WithRemaining(nil) error = %v, want ErrContextRequired", err)
	}
}

func TestPeerLimiterBoundsAndIdempotentRelease(t *testing.T) {
	t.Parallel()
	var zero PeerLimiter
	if permit, err := zero.Acquire(context.Background()); permit != nil || !errors.Is(err, ErrLimiterUninitialized) {
		t.Fatalf("zero PeerLimiter.Acquire() = (%v, %v), want nil and ErrLimiterUninitialized", permit, err)
	}
	if permit, err := zero.TryAcquire(); permit != nil || !errors.Is(err, ErrLimiterUninitialized) {
		t.Fatalf("zero PeerLimiter.TryAcquire() = (%v, %v), want nil and ErrLimiterUninitialized", permit, err)
	}

	limiter, err := NewPeerLimiter(Limits{MaxConcurrent: 2})
	if err != nil {
		t.Fatalf("NewPeerLimiter() error = %v", err)
	}
	if limiter.MaxConcurrent() != 2 {
		t.Fatalf("MaxConcurrent() = %d, want 2", limiter.MaxConcurrent())
	}

	first, err := limiter.TryAcquire()
	if err != nil {
		t.Fatalf("TryAcquire(first) error = %v", err)
	}
	second, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire(second) error = %v", err)
	}
	if limiter.Inflight() != 2 {
		t.Fatalf("Inflight() = %d, want 2", limiter.Inflight())
	}
	if _, err := limiter.TryAcquire(); !errors.Is(err, ErrConcurrentLimit) {
		t.Fatalf("TryAcquire(full) error = %v, want ErrConcurrentLimit", err)
	}

	firstCopy := *first
	firstCopy.Release()
	first.Release()
	if limiter.Inflight() != 1 {
		t.Fatalf("Inflight() after copied/double release = %d, want 1", limiter.Inflight())
	}
	second.Release()
	second.Release()
	if limiter.Inflight() != 0 {
		t.Fatalf("Inflight() after release = %d, want 0", limiter.Inflight())
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if permit, err := limiter.Acquire(canceled); permit != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire(canceled) = (%v, %v), want nil and context.Canceled", permit, err)
	}
	if permit, err := limiter.Acquire(nil); permit != nil || !errors.Is(err, ErrContextRequired) {
		t.Fatalf("Acquire(nil) = (%v, %v), want nil and ErrContextRequired", permit, err)
	}

	waitingLimiter, err := NewPeerLimiter(Limits{MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("NewPeerLimiter(waiting) error = %v", err)
	}
	held, err := waitingLimiter.TryAcquire()
	if err != nil {
		t.Fatalf("TryAcquire(held) error = %v", err)
	}
	base, cancel := context.WithCancel(context.Background())
	observed := &observingContext{
		Context: base,
		started: make(chan struct{}),
	}
	result := make(chan acquireResult, 1)
	go func() {
		permit, acquireErr := waitingLimiter.Acquire(observed)
		result <- acquireResult{permit: permit, err: acquireErr}
	}()
	select {
	case <-observed.started:
	case <-time.After(time.Second):
		t.Fatal("Acquire did not enter its wait select")
	}
	cancel()
	held.Release()
	var got acquireResult
	select {
	case got = <-result:
	case <-time.After(time.Second):
		t.Fatal("Acquire did not return after cancellation")
	}
	if got.permit != nil || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("Acquire(waiting canceled) = (%v, %v), want nil and context.Canceled", got.permit, got.err)
	}
}

type acquireResult struct {
	permit *Permit
	err    error
}

type observingContext struct {
	context.Context
	started chan struct{}
	once    sync.Once
}

func (c *observingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.started) })
	return c.Context.Done()
}
