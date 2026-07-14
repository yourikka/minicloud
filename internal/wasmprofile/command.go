package wasmprofile

import (
	"cmp"
	"errors"
	"slices"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

const (
	ReasonUnknownImport           = "unknown_or_incompatible_import"
	ReasonUnsupportedStartSection = "unsupported_start_section"
	ReasonMissingStart            = "missing_start"
	ReasonInvalidStart            = "invalid_start"
)

// Import is a deterministic description of a command module import.
type Import struct {
	Module string
	Name   string
	Kind   string
}

// CompatibilityError is a stable admission rejection without guest details.
type CompatibilityError struct {
	Reason string
}

func (e *CompatibilityError) Error() string {
	return "module is incompatible with the locked wasi command profile"
}

// ValidateCommand verifies the imports and command entrypoint of an already
// compiled module against the exact WASI host build used for execution.
func ValidateCommand(
	guest wazero.CompiledModule,
	wasi wazero.CompiledModule,
	metadata BinaryMetadata,
) ([]Import, error) {
	if metadata.HasStartSection {
		return nil, &CompatibilityError{Reason: ReasonUnsupportedStartSection}
	}
	visibleImportCount := len(guest.ImportedFunctions()) + len(guest.ImportedMemories())
	if uint64(metadata.DeclaredImports) != uint64(visibleImportCount) {
		return nil, &CompatibilityError{Reason: ReasonUnknownImport}
	}

	imports := make([]Import, 0, visibleImportCount)
	wasiFunctions := wasi.ExportedFunctions()
	for _, definition := range guest.ImportedFunctions() {
		moduleName, name, isImport := definition.Import()
		if !isImport {
			return sortedImports(imports), errors.New("compiled function is not an import")
		}
		imports = append(imports, Import{Module: moduleName, Name: name, Kind: "function"})
		hostDefinition, exists := wasiFunctions[name]
		isWASI := moduleName == wasi_snapshot_preview1.ModuleName
		paramsMatch := exists && slices.Equal(definition.ParamTypes(), hostDefinition.ParamTypes())
		resultsMatch := exists && slices.Equal(definition.ResultTypes(), hostDefinition.ResultTypes())
		if !isWASI || !paramsMatch || !resultsMatch {
			return sortedImports(imports), &CompatibilityError{Reason: ReasonUnknownImport}
		}
	}
	for _, definition := range guest.ImportedMemories() {
		moduleName, name, _ := definition.Import()
		imports = append(imports, Import{Module: moduleName, Name: name, Kind: "memory"})
		return sortedImports(imports), &CompatibilityError{Reason: ReasonUnknownImport}
	}

	start, exists := guest.ExportedFunctions()["_start"]
	if !exists {
		return sortedImports(imports), &CompatibilityError{Reason: ReasonMissingStart}
	}
	_, _, importedStart := start.Import()
	if importedStart || len(start.ParamTypes()) != 0 || len(start.ResultTypes()) != 0 {
		return sortedImports(imports), &CompatibilityError{Reason: ReasonInvalidStart}
	}
	return sortedImports(imports), nil
}

func sortedImports(imports []Import) []Import {
	slices.SortFunc(imports, func(left, right Import) int {
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
