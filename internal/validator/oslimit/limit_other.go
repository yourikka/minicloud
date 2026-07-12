//go:build !linux

package oslimit

// Apply leaves OS quotas unenforced on non-Linux development platforms. The
// parent process boundary and watchdog remain mandatory and are reported apart.
func Apply(Config) (Result, error) {
	return Result{}, nil
}
