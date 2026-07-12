// Package oslimit installs child-process resource limits before untrusted bytes
// are read. Platform-specific implementations report exactly what was enforced.
package oslimit

// Config defines limits that are safe to apply from inside the child process.
type Config struct {
	CPUSeconds   uint64
	MaxFileBytes uint64
	MaxOpenFiles uint64
}

// Result identifies limits enforced by this operating system.
type Result struct {
	CPULimit      bool
	FileSizeLimit bool
	OpenFileLimit bool
}
