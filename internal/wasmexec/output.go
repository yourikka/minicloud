package wasmexec

import (
	"bytes"
	"sync"
)

type boundedOutput struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	cancel   func()
}

func newBoundedOutput(limit int, cancel func()) *boundedOutput {
	return &boundedOutput{limit: limit, cancel: cancel}
}

func (w *boundedOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.exceeded {
		return 0, errOutputLimit
	}
	remaining := w.limit - w.buffer.Len()
	if len(data) > remaining {
		written, _ := w.buffer.Write(data[:max(remaining, 0)])
		w.exceeded = true
		w.cancel()
		return written, errOutputLimit
	}
	return w.buffer.Write(data)
}

func (w *boundedOutput) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buffer.Bytes())
}

func (w *boundedOutput) Exceeded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exceeded
}

type guestLog struct {
	mu           sync.Mutex
	buffer       bytes.Buffer
	maxBytes     int
	maxLineBytes int
	lineBytes    int
	dropped      int64
}

func newGuestLog(maxBytes, maxLineBytes int) *guestLog {
	return &guestLog{maxBytes: maxBytes, maxLineBytes: maxLineBytes}
}

func (w *guestLog) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, current := range data {
		lineAllowed := current == '\n' || w.lineBytes < w.maxLineBytes
		totalAllowed := w.buffer.Len() < w.maxBytes
		if lineAllowed && totalAllowed {
			_ = w.buffer.WriteByte(current)
		} else {
			w.dropped++
		}
		if current == '\n' {
			w.lineBytes = 0
		} else {
			w.lineBytes++
		}
	}
	return len(data), nil
}

func (w *guestLog) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buffer.Bytes())
}

func (w *guestLog) Dropped() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}
