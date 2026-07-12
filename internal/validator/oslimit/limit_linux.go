//go:build linux

package oslimit

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// Apply installs Linux rlimits. Memory and aggregate temp-disk quotas require
// the external cgroup/tmpfs supervisor and are intentionally not claimed here.
func Apply(config Config) (Result, error) {
	if config.CPUSeconds == 0 || config.MaxFileBytes == 0 || config.MaxOpenFiles == 0 {
		return Result{}, errors.New("validator os limits must be positive")
	}

	result := Result{}
	cpuLimit := unix.Rlimit{Cur: config.CPUSeconds, Max: config.CPUSeconds + 1}
	if err := unix.Setrlimit(unix.RLIMIT_CPU, &cpuLimit); err != nil {
		return Result{}, fmt.Errorf("setting validator cpu limit: %w", err)
	}
	result.CPULimit = true

	fileLimit := unix.Rlimit{Cur: config.MaxFileBytes, Max: config.MaxFileBytes}
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &fileLimit); err != nil {
		return Result{}, fmt.Errorf("setting validator file size limit: %w", err)
	}
	result.FileSizeLimit = true

	openFileLimit := unix.Rlimit{Cur: config.MaxOpenFiles, Max: config.MaxOpenFiles}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &openFileLimit); err != nil {
		return Result{}, fmt.Errorf("setting validator open file limit: %w", err)
	}
	result.OpenFileLimit = true
	return result, nil
}
