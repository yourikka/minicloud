package wasmexec

import (
	"context"

	"github.com/yourikka/minicloud/internal/problem"
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
	if ctx.Err() != nil {
		return invocationError(problem.CodeFunctionTimeout, "function execution deadline was exceeded while queued")
	}
	if acceptanceStopped(stop, alsoStop) {
		return ErrAcceptanceStopped
	}
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-stop:
		return ErrAcceptanceStopped
	case <-alsoStop:
		return ErrAcceptanceStopped
	case <-ctx.Done():
		return invocationError(problem.CodeFunctionTimeout, "function execution deadline was exceeded while queued")
	default:
	}
	select {
	case l.queue <- struct{}{}:
		defer func() { <-l.queue }()
	case <-stop:
		return ErrAcceptanceStopped
	case <-alsoStop:
		return ErrAcceptanceStopped
	default:
		if ctx.Err() != nil {
			return invocationError(problem.CodeFunctionTimeout, "function execution deadline was exceeded while queued")
		}
		if acceptanceStopped(stop, alsoStop) {
			return ErrAcceptanceStopped
		}
		return invocationError(problem.CodeOverloaded, "function execution queue is full")
	}
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-stop:
		return ErrAcceptanceStopped
	case <-alsoStop:
		return ErrAcceptanceStopped
	case <-ctx.Done():
		return invocationError(problem.CodeFunctionTimeout, "function execution deadline was exceeded while queued")
	}
}

func (l *limiter) release() {
	<-l.slots
}

func acceptanceStopped(stop, alsoStop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	case <-alsoStop:
		return true
	default:
		return false
	}
}
