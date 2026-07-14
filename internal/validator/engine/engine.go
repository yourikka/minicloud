// Package engine performs actual WebAssembly compilation inside the disposable
// validator process. It must not be imported by Controller or Management code.
package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/validator/protocol"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

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

	runtimeConfig, configErr := wasmprofile.RuntimeConfig(request.RuntimeEngine, request.MemoryLimitMiB)
	if configErr != nil {
		return protocol.Report{}, fmt.Errorf("configuring locked runtime profile: %w", configErr)
	}
	memoryLimitPages := wasmprofile.MemoryLimitPages(request.MemoryLimitMiB)

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
	binaryMetadata, inspectErr := wasmprofile.InspectBinary(wasm)
	if inspectErr != nil {
		return protocol.Report{}, fmt.Errorf("inspecting compiled module: %w", inspectErr)
	}
	profileImports, compatibilityErr := wasmprofile.ValidateCommand(compiled, wasiModule, binaryMetadata)
	report.Imports = make([]protocol.Import, 0, len(profileImports))
	for _, item := range profileImports {
		report.Imports = append(report.Imports, protocol.Import{
			Module: item.Module,
			Name:   item.Name,
			Kind:   item.Kind,
		})
	}
	if compatibilityErr != nil {
		var profileErr *wasmprofile.CompatibilityError
		if !errors.As(compatibilityErr, &profileErr) {
			return protocol.Report{}, fmt.Errorf("validating compiled module profile: %w", compatibilityErr)
		}
		return reject(
			report,
			problem.CodeInvalidModule,
			profileErr.Reason,
			compatibilityMessage(profileErr.Reason),
		), nil
	}

	exportedFunctions := compiled.ExportedFunctions()
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

func compatibilityMessage(reason string) string {
	switch reason {
	case wasmprofile.ReasonUnsupportedStartSection:
		return "module must use only the wasi command _start entrypoint"
	case wasmprofile.ReasonMissingStart:
		return "module does not export _start"
	case wasmprofile.ReasonInvalidStart:
		return "module _start export must be a defined function with no parameters or results"
	default:
		return "module imports are outside the locked WASI profile"
	}
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
