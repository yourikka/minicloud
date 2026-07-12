// Package engine performs actual WebAssembly compilation inside the disposable
// validator process. It must not be imported by Controller or Management code.
package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/validator/protocol"
)

const wasmPageBytes = uint32(64 << 10)

// Validate compiles the untrusted module using the locked runtime profile and
// returns only stable, non-sensitive diagnostics.
func Validate(
	ctx context.Context,
	request protocol.Request,
	wasm []byte,
) (report protocol.Report, err error) {
	report = baseReport(request)
	if len(request.RequestedCapabilities) != 0 {
		return reject(
			report,
			problem.CodeCapabilityDenied,
			"requested_capability_not_supported",
			"requested capability is not available in the v1 host profile",
		), nil
	}

	runtimeConfig := wazero.NewRuntimeConfigCompiler()
	if request.RuntimeEngine == protocol.EngineInterpreter {
		runtimeConfig = wazero.NewRuntimeConfigInterpreter()
	}
	memoryLimitPages := request.MemoryLimitMiB * (1 << 20) / wasmPageBytes
	runtimeConfig = runtimeConfig.
		WithCoreFeatures(api.CoreFeaturesV2).
		WithMemoryLimitPages(memoryLimitPages).
		WithCloseOnContextDone(true).
		WithCustomSections(false).
		WithDebugInfoEnabled(false)

	runtime := wazero.NewRuntimeWithConfig(ctx, runtimeConfig)
	cleanupContext := context.WithoutCancel(ctx)
	defer func() {
		err = errors.Join(err, runtime.Close(cleanupContext))
	}()

	wasiModule, compileWASIErr := wasi_snapshot_preview1.NewBuilder(runtime).Compile(ctx)
	if compileWASIErr != nil {
		return protocol.Report{}, fmt.Errorf("compiling locked wasi host profile: %w", compileWASIErr)
	}
	defer func() {
		err = errors.Join(err, wasiModule.Close(cleanupContext))
	}()

	compileStarted := time.Now()
	compiled, compileErr := runtime.CompileModule(ctx, wasm)
	report.Timing.CompileNanoseconds = time.Since(compileStarted).Nanoseconds()
	if compileErr != nil {
		return reject(
			report,
			problem.CodeInvalidModule,
			"compile_failed",
			"module failed WebAssembly validation or compilation",
		), nil
	}
	defer func() {
		err = errors.Join(err, compiled.Close(cleanupContext))
	}()
	declaredImportCount, countErr := countDeclaredImports(wasm)
	if countErr != nil {
		return protocol.Report{}, fmt.Errorf("counting imports in compiled module: %w", countErr)
	}
	visibleImportCount := len(compiled.ImportedFunctions()) + len(compiled.ImportedMemories())
	if uint64(declaredImportCount) != uint64(visibleImportCount) {
		return reject(
			report,
			problem.CodeInvalidModule,
			"unknown_or_incompatible_import",
			"module imports are outside the locked WASI profile",
		), nil
	}

	imports, importErr := validateImports(compiled, wasiModule)
	report.Imports = imports
	if importErr != nil {
		return reject(
			report,
			problem.CodeInvalidModule,
			"unknown_or_incompatible_import",
			"module imports are outside the locked WASI profile",
		), nil
	}

	exportedFunctions := compiled.ExportedFunctions()
	start, exists := exportedFunctions["_start"]
	if !exists {
		return reject(
			report,
			problem.CodeInvalidModule,
			"missing_start",
			"module does not export _start",
		), nil
	}
	_, _, importedStart := start.Import()
	hasCommandSignature := len(start.ParamTypes()) == 0 && len(start.ResultTypes()) == 0
	if importedStart || !hasCommandSignature {
		return reject(
			report,
			problem.CodeInvalidModule,
			"invalid_start",
			"module _start export must be a defined function with no parameters or results",
		), nil
	}

	report.Exports = make([]string, 0, len(exportedFunctions))
	for name := range exportedFunctions {
		report.Exports = append(report.Exports, name)
	}
	slices.Sort(report.Exports)
	report.Memory = memoryReport(compiled, memoryLimitPages)
	report.Valid = true
	report.Code = protocol.CodeOK
	report.Reason = "accepted"
	report.Message = "module is compatible with the locked v1 runtime profile"
	return report, nil
}

// countDeclaredImports fills the metadata gap in wazero's public API, which
// currently exposes imported functions and memories but not every import kind.
// CompileModule has already validated the full binary before this is called.
func countDeclaredImports(wasm []byte) (uint32, error) {
	const wasmHeaderBytes = 8
	if len(wasm) < wasmHeaderBytes {
		return 0, errors.New("module is shorter than its header")
	}
	for offset := wasmHeaderBytes; offset < len(wasm); {
		sectionID := wasm[offset]
		offset++
		sectionSize, bytesRead, err := readU32(wasm[offset:])
		if err != nil {
			return 0, fmt.Errorf("reading section size: %w", err)
		}
		offset += bytesRead
		sectionEnd := uint64(offset) + uint64(sectionSize)
		if sectionEnd > uint64(len(wasm)) {
			return 0, errors.New("section exceeds module size")
		}
		if sectionID == 2 {
			count, _, err := readU32(wasm[offset:int(sectionEnd)])
			if err != nil {
				return 0, fmt.Errorf("reading import count: %w", err)
			}
			return count, nil
		}
		offset = int(sectionEnd)
	}
	return 0, nil
}

func readU32(data []byte) (uint32, int, error) {
	var value uint32
	for index := range min(len(data), 5) {
		current := data[index]
		if index == 4 && current > 0x0f {
			return 0, 0, errors.New("u32 value overflows")
		}
		value |= uint32(current&0x7f) << (7 * index)
		if current&0x80 == 0 {
			return value, index + 1, nil
		}
	}
	return 0, 0, errors.New("unterminated u32 value")
}

func validateImports(
	guest wazero.CompiledModule,
	wasi wazero.CompiledModule,
) ([]protocol.Import, error) {
	imports := make([]protocol.Import, 0, len(guest.ImportedFunctions())+len(guest.ImportedMemories()))
	wasiFunctions := wasi.ExportedFunctions()
	for _, definition := range guest.ImportedFunctions() {
		moduleName, name, isImport := definition.Import()
		if !isImport {
			return imports, errors.New("function definition is not an import")
		}
		imports = append(imports, protocol.Import{Module: moduleName, Name: name, Kind: "function"})
		hostDefinition, exists := wasiFunctions[name]
		isWASI := moduleName == wasi_snapshot_preview1.ModuleName
		if !exists || !isWASI {
			return sortedImports(imports), errors.New("unknown import")
		}
		paramsMatch := slices.Equal(definition.ParamTypes(), hostDefinition.ParamTypes())
		resultsMatch := slices.Equal(definition.ResultTypes(), hostDefinition.ResultTypes())
		if !paramsMatch || !resultsMatch {
			return sortedImports(imports), errors.New("incompatible import signature")
		}
	}
	for _, definition := range guest.ImportedMemories() {
		moduleName, name, _ := definition.Import()
		imports = append(imports, protocol.Import{Module: moduleName, Name: name, Kind: "memory"})
		return sortedImports(imports), errors.New("imported memory is not supported")
	}
	return sortedImports(imports), nil
}

func sortedImports(imports []protocol.Import) []protocol.Import {
	slices.SortFunc(imports, func(left, right protocol.Import) int {
		if byModule := cmp.Compare(left.Module, right.Module); byModule != 0 {
			return byModule
		}
		if byName := cmp.Compare(left.Name, right.Name); byName != 0 {
			return byName
		}
		return cmp.Compare(left.Kind, right.Kind)
	})
	return imports
}

func memoryReport(compiled wazero.CompiledModule, tierPages uint32) protocol.Memory {
	definition, exists := compiled.ExportedMemories()["memory"]
	if !exists {
		return protocol.Memory{}
	}
	maxPages, hasMax := definition.Max()
	if !hasMax {
		maxPages = tierPages
	}
	return protocol.Memory{
		Exported: true,
		MinPages: definition.Min(),
		MaxPages: min(maxPages, tierPages),
	}
}

func baseReport(request protocol.Request) protocol.Report {
	return protocol.Report{
		SchemaVersion:         protocol.SchemaVersion,
		ValidationID:          request.ValidationID,
		Code:                  string(problem.CodeInvalidModule),
		Reason:                "validation_not_completed",
		Message:               "module validation did not complete",
		ArtifactDigest:        request.ArtifactDigest,
		ArtifactSize:          request.ArtifactSize,
		RuntimeName:           protocol.RuntimeName,
		RuntimeVersion:        protocol.RuntimeVersion,
		RuntimeFeatureProfile: protocol.FeatureProfile,
		RuntimeEngine:         request.RuntimeEngine,
		Imports:               []protocol.Import{},
		Exports:               []string{},
	}
}

func reject(
	report protocol.Report,
	code problem.Code,
	reason string,
	message string,
) protocol.Report {
	report.Valid = false
	report.Code = string(code)
	report.Reason = reason
	report.Message = message
	return report
}
