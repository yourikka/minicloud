package wasmexec

import (
	"context"
	"sync"
)

type lifecycle struct {
	mu          sync.Mutex
	active      int
	closing     bool
	closed      bool
	releasing   bool
	drained     chan struct{}
	releaseDone chan struct{}
	closeErr    error
}

func newLifecycle() *lifecycle {
	drained := make(chan struct{})
	close(drained)
	return &lifecycle{drained: drained}
}

func (l *lifecycle) begin() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing || l.closed {
		return false
	}
	if l.active == 0 {
		l.drained = make(chan struct{})
	}
	l.active++
	return true
}

func (l *lifecycle) end() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	if l.active == 0 {
		close(l.drained)
	}
}

func (l *lifecycle) close(ctx context.Context, release func(context.Context) error) error {
	for {
		l.mu.Lock()
		if l.closed {
			err := l.closeErr
			l.mu.Unlock()
			return err
		}
		l.closing = true
		if l.releasing {
			releaseDone := l.releaseDone
			l.mu.Unlock()
			select {
			case <-releaseDone:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		drained := l.drained
		l.mu.Unlock()
		select {
		case <-drained:
		case <-ctx.Done():
			return ctx.Err()
		}

		l.mu.Lock()
		if l.releasing {
			l.mu.Unlock()
			continue
		}
		l.releasing = true
		l.releaseDone = make(chan struct{})
		releaseDone := l.releaseDone
		l.mu.Unlock()

		err := release(ctx)
		l.mu.Lock()
		l.releasing = false
		l.closeErr = err
		if err == nil {
			l.closed = true
		}
		close(releaseDone)
		l.mu.Unlock()
		return err
	}
}
