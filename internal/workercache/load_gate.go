package workercache

import "context"

type loadGate struct {
	active chan struct{}
	queued chan struct{}
}

type loadReservation struct {
	gate   *loadGate
	active bool
	queued bool
}

func newLoadGate(maxConcurrent, maxQueued int) *loadGate {
	return &loadGate{
		active: make(chan struct{}, maxConcurrent),
		queued: make(chan struct{}, maxQueued),
	}
}

func (g *loadGate) reserve() (*loadReservation, error) {
	reservation := &loadReservation{gate: g}
	select {
	case g.active <- struct{}{}:
		reservation.active = true
		return reservation, nil
	default:
	}
	select {
	case g.queued <- struct{}{}:
		reservation.queued = true
		return reservation, nil
	default:
		return nil, ErrLoadQueueFull
	}
}

func (r *loadReservation) wait(ctx context.Context) error {
	if r.active {
		return nil
	}
	select {
	case r.gate.active <- struct{}{}:
		<-r.gate.queued
		r.queued = false
		r.active = true
		return nil
	case <-ctx.Done():
		<-r.gate.queued
		r.queued = false
		return ctx.Err()
	}
}

func (r *loadReservation) release() {
	if r.active {
		<-r.gate.active
		r.active = false
	}
	if r.queued {
		<-r.gate.queued
		r.queued = false
	}
}
