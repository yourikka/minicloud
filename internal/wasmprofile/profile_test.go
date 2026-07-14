package wasmprofile

import "testing"

func TestRuntimeConfigAcceptsOnlyLockedEnginesAndMemoryTiers(t *testing.T) {
	t.Parallel()
	for _, engine := range []string{EngineCompiler, EngineInterpreter} {
		for _, memoryMiB := range []uint32{64, 128, 256, 512} {
			if _, err := RuntimeConfig(engine, memoryMiB); err != nil {
				t.Errorf("RuntimeConfig(%q, %d) error = %v", engine, memoryMiB, err)
			}
		}
	}
	if _, err := RuntimeConfig("jit", 128); err == nil {
		t.Fatal("RuntimeConfig() accepted an unknown engine")
	}
	if _, err := RuntimeConfig(EngineCompiler, 1024); err == nil {
		t.Fatal("RuntimeConfig() accepted an unknown memory tier")
	}
}
