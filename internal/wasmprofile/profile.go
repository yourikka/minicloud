// Package wasmprofile defines the runtime configuration shared by admission
// validation and Worker execution.
package wasmprofile

import (
	"errors"
	"slices"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

const (
	RuntimeName       = "wazero"
	RuntimeVersion    = "v1.12.0"
	FeatureProfile    = "wazero-core-v2-v1"
	EngineCompiler    = "compiler"
	EngineInterpreter = "interpreter"
	WasmPageBytes     = uint32(64 << 10)
)

var memoryTiersMiB = []uint32{64, 128, 256, 512}

// Profile identifies the immutable runtime choices that affect compilation.
type Profile struct {
	Engine         string
	MemoryLimitMiB uint32
}

// New validates and returns a locked runtime profile.
func New(engine string, memoryLimitMiB uint32) (Profile, error) {
	if engine != EngineCompiler && engine != EngineInterpreter {
		return Profile{}, errors.New("unsupported wasm runtime engine")
	}
	if !ValidMemoryTier(memoryLimitMiB) {
		return Profile{}, errors.New("unsupported wasm memory tier")
	}
	return Profile{Engine: engine, MemoryLimitMiB: memoryLimitMiB}, nil
}

// RuntimeConfig returns the locked v1 runtime configuration.
func RuntimeConfig(engine string, memoryLimitMiB uint32) (wazero.RuntimeConfig, error) {
	profile, err := New(engine, memoryLimitMiB)
	if err != nil {
		return nil, err
	}
	config := wazero.NewRuntimeConfigCompiler()
	if profile.Engine == EngineInterpreter {
		config = wazero.NewRuntimeConfigInterpreter()
	}
	return config.
		WithCoreFeatures(api.CoreFeaturesV2).
		WithMemoryLimitPages(MemoryLimitPages(profile.MemoryLimitMiB)).
		WithCloseOnContextDone(true).
		WithCustomSections(false).
		WithDebugInfoEnabled(false), nil
}

// ValidMemoryTier reports whether memoryMiB is one of the fixed v1 tiers.
func ValidMemoryTier(memoryMiB uint32) bool {
	return slices.Contains(memoryTiersMiB, memoryMiB)
}

// MemoryLimitPages converts a validated MiB tier to 64 KiB Wasm pages.
func MemoryLimitPages(memoryMiB uint32) uint32 {
	return memoryMiB * (1 << 20) / WasmPageBytes
}
