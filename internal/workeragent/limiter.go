package workeragent

import (
	"context"
	"errors"
)

var (
	errQueueFull = errors.New("worker agent invocation queue is full")
	errStopping  = errors.New("worker agent admission is stopping")
)

type limiter struct {
	slots chan struct{}
	queue chan struct{}
}

func newLimiter(concurrent, queued int) *limiter {
	return &limiter{
		slots: make(chan struct{}, concurrent),
		queue: make(chan struct{}, queued),
	}
}

func (l *limiter) acquire(
	ctx context.Context,
	stop <-chan struct{},
	alsoStop <-chan struct{},
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-stop:
		return errStopping
	case <-alsoStop:
		return errStopping
	default:
	}
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-stop:
		return errStopping
	case <-alsoStop:
		return errStopping
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	select {
	case l.queue <- struct{}{}:
		defer func() { <-l.queue }()
	case <-stop:
		return errStopping
	case <-alsoStop:
		return errStopping
	case <-ctx.Done():
		return ctx.Err()
	default:
		return errQueueFull
	}
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-stop:
		return errStopping
	case <-alsoStop:
		return errStopping
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *limiter) release() {
	<-l.slots
}
