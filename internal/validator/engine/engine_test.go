package engine

import (
	"context"
	"testing"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/validator/protocol"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

func TestValidateFeatureProfile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		wasm       []byte
		valid      bool
		reason     string
		memoryMiB  uint32
		capability []model.CapabilityRequest
	}{
		{name: "minimal command", wasm: commandModule(nil, nil), valid: true, reason: "accepted"},
		{name: "missing start", wasm: commandModule(nil, omitStartExport()), reason: "missing_start"},
		{name: "invalid start signature", wasm: invalidStartSignatureModule(), reason: "invalid_start"},
		{name: "imported start", wasm: importedStartModule(), reason: "invalid_start"},
		{name: "unknown import", wasm: commandModule(unknownFunctionImport(), nil), reason: "unknown_or_incompatible_import"},
		{name: "imported global", wasm: nonFunctionImportModule(0x03, []byte{0x7f, 0x00}), reason: "unknown_or_incompatible_import"},
		{name: "imported table", wasm: nonFunctionImportModule(0x01, []byte{0x70, 0x00, 0x01}), reason: "unknown_or_incompatible_import"},
		{name: "function and global imports", wasm: mixedImportModule(), reason: "unknown_or_incompatible_import"},
		{name: "shared memory", wasm: commandModule(sharedMemory(), nil), reason: "compile_failed"},
		{name: "memory64", wasm: commandModule(memory64(), nil), reason: "compile_failed"},
		{name: "threads opcode", wasm: atomicFenceModule(), reason: "compile_failed"},
		{name: "binary start section", wasm: startSectionModule(), reason: "unsupported_start_section"},
		{name: "component model", wasm: []byte{0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01, 0x00}, reason: "compile_failed"},
		{name: "memory tier exceeded", wasm: commandModule(memoryWithMinimum(1025), nil), reason: "compile_failed", memoryMiB: 64},
		{
			name:       "optional capability denied",
			wasm:       commandModule(nil, nil),
			reason:     "requested_capability_not_supported",
			capability: []model.CapabilityRequest{{Name: "network", Version: "v1"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			memoryMiB := test.memoryMiB
			if memoryMiB == 0 {
				memoryMiB = 128
			}
			request := validRequest(test.wasm, memoryMiB)
			request.RequestedCapabilities = test.capability

			report, err := Validate(context.Background(), request, test.wasm)
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if report.Valid != test.valid {
				t.Fatalf("Valid = %t, want %t; report = %+v", report.Valid, test.valid, report)
			}
			if report.Reason != test.reason {
				t.Fatalf("Reason = %q, want %q", report.Reason, test.reason)
			}
			if test.valid && report.Code != protocol.CodeOK {
				t.Fatalf("Code = %q, want %q", report.Code, protocol.CodeOK)
			}
			if !test.valid && report.Code != string(problem.CodeInvalidModule) &&
				report.Code != string(problem.CodeCapabilityDenied) {
				t.Fatalf("Code = %q, want stable admission rejection", report.Code)
			}
		})
	}
}

func TestCountDeclaredImportsAfterCustomSection(t *testing.T) {
	t.Parallel()
	module := commandModule(unknownFunctionImport(), nil)
	withCustom := append(wasmHeader(), section(0, name("fixture"))...)
	withCustom = append(withCustom, module[len(wasmHeader()):]...)

	metadata, err := wasmprofile.InspectBinary(withCustom)
	if err != nil {
		t.Fatalf("InspectBinary() error = %v", err)
	}
	if metadata.DeclaredImports != 1 {
		t.Fatalf("DeclaredImports = %d, want 1", metadata.DeclaredImports)
	}
}

func TestValidateInterpreter(t *testing.T) {
	t.Parallel()
	wasm := commandModule(nil, nil)
	request := validRequest(wasm, 128)
	request.RuntimeEngine = protocol.EngineInterpreter

	report, err := Validate(context.Background(), request, wasm)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !report.Valid || report.RuntimeEngine != protocol.EngineInterpreter {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func validRequest(wasm []byte, memoryMiB uint32) protocol.Request {
	return protocol.Request{
		SchemaVersion:         protocol.SchemaVersion,
		ValidationID:          "engine-test",
		ArtifactDigest:        digest.Sum(wasm),
		ArtifactSize:          int64(len(wasm)),
		ABI:                   model.ABIWASICommandV1,
		HostAPIProfile:        model.HostAPIProfileNone,
		RuntimeFeatureProfile: protocol.FeatureProfile,
		RuntimeEngine:         protocol.EngineCompiler,
		MemoryLimitMiB:        memoryMiB,
		RequestedCapabilities: []model.CapabilityRequest{},
	}
}

type moduleOption func(*moduleParts)

type moduleParts struct {
	imports      []byte
	memory       []byte
	exportStart  bool
	functionType []byte
	body         []byte
}

func commandModule(importOption, exportOption moduleOption) []byte {
	parts := moduleParts{
		exportStart:  true,
		functionType: functionType(nil, nil),
		body:         []byte{0x00, 0x0b},
	}
	if importOption != nil {
		importOption(&parts)
	}
	if exportOption != nil {
		exportOption(&parts)
	}

	module := wasmHeader()
	module = append(module, section(1, vector(parts.functionType))...)
	if len(parts.imports) != 0 {
		module = append(module, section(2, vector(parts.imports))...)
	}
	module = append(module, section(3, vector(u32(0)))...)
	if len(parts.memory) != 0 {
		module = append(module, section(5, vector(parts.memory))...)
	}
	if parts.exportStart {
		functionIndex := uint32(0)
		if len(parts.imports) != 0 {
			functionIndex = 1
		}
		export := append(name("_start"), 0x00)
		export = append(export, u32(functionIndex)...)
		module = append(module, section(7, vector(export))...)
	}
	module = append(module, section(10, vector(sized(parts.body)))...)
	return module
}

func omitStartExport() moduleOption {
	return func(parts *moduleParts) { parts.exportStart = false }
}

func unknownFunctionImport() moduleOption {
	return func(parts *moduleParts) {
		entry := append(name("evil"), name("pwn")...)
		parts.imports = append(entry, 0x00, 0x00)
	}
}

func sharedMemory() moduleOption {
	return func(parts *moduleParts) { parts.memory = []byte{0x03, 0x01, 0x01} }
}

func memory64() moduleOption {
	return func(parts *moduleParts) { parts.memory = []byte{0x04, 0x01} }
}

func memoryWithMinimum(pages uint32) moduleOption {
	return func(parts *moduleParts) {
		parts.memory = append([]byte{0x00}, u32(pages)...)
	}
}

func invalidStartSignatureModule() []byte {
	parts := moduleParts{
		exportStart:  true,
		functionType: functionType([]byte{0x7f}, nil),
		body:         []byte{0x00, 0x0b},
	}
	module := wasmHeader()
	module = append(module, section(1, vector(parts.functionType))...)
	module = append(module, section(3, vector(u32(0)))...)
	export := append(name("_start"), 0x00)
	export = append(export, u32(0)...)
	module = append(module, section(7, vector(export))...)
	module = append(module, section(10, vector(sized(parts.body)))...)
	return module
}

func importedStartModule() []byte {
	module := wasmHeader()
	module = append(module, section(1, vector(functionType([]byte{0x7f}, nil)))...)
	entry := append(name("wasi_snapshot_preview1"), name("proc_exit")...)
	entry = append(entry, 0x00, 0x00)
	module = append(module, section(2, vector(entry))...)
	export := append(name("_start"), 0x00)
	export = append(export, u32(0)...)
	module = append(module, section(7, vector(export))...)
	return module
}

func nonFunctionImportModule(kind byte, descriptor []byte) []byte {
	module := wasmHeader()
	module = append(module, section(1, vector(functionType(nil, nil)))...)
	entry := append(name("evil"), name("resource")...)
	entry = append(entry, kind)
	entry = append(entry, descriptor...)
	module = append(module, section(2, vector(entry))...)
	module = append(module, section(3, vector(u32(0)))...)
	export := append(name("_start"), 0x00)
	export = append(export, u32(0)...)
	module = append(module, section(7, vector(export))...)
	body := []byte{0x00, 0x0b}
	module = append(module, section(10, vector(sized(body)))...)
	return module
}

func mixedImportModule() []byte {
	module := wasmHeader()
	module = append(module, section(1, vector(functionType(nil, nil)))...)
	functionEntry := append(name("evil"), name("function")...)
	functionEntry = append(functionEntry, 0x00, 0x00)
	globalEntry := append(name("evil"), name("global")...)
	globalEntry = append(globalEntry, 0x03, 0x7f, 0x00)
	imports := append(u32(2), functionEntry...)
	imports = append(imports, globalEntry...)
	module = append(module, section(2, imports)...)
	module = append(module, section(3, vector(u32(0)))...)
	export := append(name("_start"), 0x00)
	export = append(export, u32(1)...)
	module = append(module, section(7, vector(export))...)
	body := []byte{0x00, 0x0b}
	module = append(module, section(10, vector(sized(body)))...)
	return module
}

func atomicFenceModule() []byte {
	module := wasmHeader()
	module = append(module, section(1, vector(functionType(nil, nil)))...)
	module = append(module, section(3, vector(u32(0)))...)
	export := append(name("_start"), 0x00)
	export = append(export, u32(0)...)
	module = append(module, section(7, vector(export))...)
	body := []byte{0x00, 0xfe, 0x03, 0x00, 0x0b}
	module = append(module, section(10, vector(sized(body)))...)
	return module
}

func startSectionModule() []byte {
	module := wasmHeader()
	module = append(module, section(1, vector(functionType(nil, nil)))...)
	module = append(module, section(3, vector(u32(0)))...)
	export := append(name("_start"), 0x00)
	export = append(export, u32(0)...)
	module = append(module, section(7, vector(export))...)
	module = append(module, section(8, u32(0))...)
	body := []byte{0x00, 0x0b}
	module = append(module, section(10, vector(sized(body)))...)
	return module
}

func functionType(params, results []byte) []byte {
	encoded := []byte{0x60}
	encoded = append(encoded, vector(params)...)
	encoded = append(encoded, vector(results)...)
	return encoded
}

func wasmHeader() []byte {
	return []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
}

func section(id byte, payload []byte) []byte {
	encoded := append([]byte{id}, u32(uint32(len(payload)))...)
	return append(encoded, payload...)
}

func vector(payload []byte) []byte {
	return append(u32(vectorLength(payload)), payload...)
}

func sized(payload []byte) []byte {
	return append(u32(uint32(len(payload))), payload...)
}

func vectorLength(payload []byte) uint32 {
	if len(payload) == 0 {
		return 0
	}
	return 1
}

func name(value string) []byte {
	return append(u32(uint32(len(value))), value...)
}

func u32(value uint32) []byte {
	encoded := make([]byte, 0, 5)
	for {
		current := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			current |= 0x80
		}
		encoded = append(encoded, current)
		if value == 0 {
			return encoded
		}
	}
}
