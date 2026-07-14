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

func (l *limiter) acquire(ctx context.Context) error {
	if ctx.Err() != nil {
		return invocationError(problem.CodeFunctionTimeout, "function execution deadline was exceeded while queued")
	}
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return invocationError(problem.CodeFunctionTimeout, "function execution deadline was exceeded while queued")
	default:
	}
	select {
	case l.queue <- struct{}{}:
		defer func() { <-l.queue }()
	default:
		if ctx.Err() != nil {
			return invocationError(problem.CodeFunctionTimeout, "function execution deadline was exceeded while queued")
		}
		return invocationError(problem.CodeOverloaded, "function execution queue is full")
	}
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return invocationError(problem.CodeFunctionTimeout, "function execution deadline was exceeded while queued")
	}
}

func (l *limiter) release() {
	<-l.slots
}
