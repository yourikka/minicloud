package servingauth

import "time"

// MonotonicClock reports elapsed time within the current process boot.
type MonotonicClock func() time.Duration

func newProcessClock() MonotonicClock {
	started := time.Now()
	return func() time.Duration {
		return time.Since(started)
	}
}
