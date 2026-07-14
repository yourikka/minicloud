package wasmexec

import (
	"errors"
	"time"

	"github.com/yourikka/minicloud/internal/wasmprofile"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

func normalizeConfig(config Config) (Config, error) {
	if config.Engine == "" {
		config.Engine = wasmprofile.EngineCompiler
	}
	if config.MemoryLimitMiB == 0 {
		config.MemoryLimitMiB = DefaultMemoryLimitMiB
	}
	if config.DefaultTimeout == 0 {
		config.DefaultTimeout = DefaultTimeout
	}
	if config.MaxTimeout == 0 {
		config.MaxTimeout = DefaultMaxTimeout
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = DefaultMaxConcurrent
	}
	if config.MaxQueue == 0 {
		config.MaxQueue = DefaultMaxQueue
	}
	if config.MaxConcurrentPerProgram == 0 {
		config.MaxConcurrentPerProgram = min(DefaultMaxConcurrentPerProgram, config.MaxConcurrent)
	}
	if config.MaxQueuePerProgram == 0 {
		config.MaxQueuePerProgram = min(DefaultMaxQueuePerProgram, config.MaxQueue)
	}
	if config.MaxLogBytes == 0 {
		config.MaxLogBytes = DefaultMaxLogBytes
	}
	if config.MaxLogLineBytes == 0 {
		config.MaxLogLineBytes = DefaultMaxLogLineBytes
	}
	if _, err := wasmprofile.RuntimeConfig(config.Engine, config.MemoryLimitMiB); err != nil {
		return Config{}, err
	}
	invalidTimeouts := config.DefaultTimeout < time.Millisecond ||
		config.MaxTimeout > DefaultMaxTimeout || config.DefaultTimeout > config.MaxTimeout
	if invalidTimeouts {
		return Config{}, errors.New("wasmexec timeout configuration is outside v1 bounds")
	}
	invalidConcurrency := config.MaxConcurrent < 1 || config.MaxConcurrent > 64 ||
		config.MaxConcurrentPerProgram < 1 || config.MaxConcurrentPerProgram > config.MaxConcurrent
	if invalidConcurrency {
		return Config{}, errors.New("wasmexec concurrency is outside v1 bounds")
	}
	if config.MaxQueue < 1 || config.MaxQueue > 4096 ||
		config.MaxQueuePerProgram < 1 || config.MaxQueuePerProgram > config.MaxQueue {
		return Config{}, errors.New("wasmexec queue limit is outside v1 bounds")
	}
	if config.MaxLogBytes < 1 || config.MaxLogBytes > DefaultMaxLogBytes ||
		config.MaxLogLineBytes < 1 || config.MaxLogLineBytes > DefaultMaxLogLineBytes {
		return Config{}, errors.New("wasmexec log limit is outside v1 bounds")
	}
	if err := config.ABILimits.Validate(); err != nil {
		return Config{}, errors.New("wasmexec abi limits are outside v1 bounds")
	}
	return config, nil
}

func rawEnvelopeLimit(limits abi.Limits) int {
	if limits.RawEnvelopeBytes == 0 {
		return int(abi.DefaultRawEnvelopeBytes)
	}
	return int(limits.RawEnvelopeBytes)
}
