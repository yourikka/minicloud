package wasmprofile

import (
	"errors"
	"fmt"
)

// BinaryMetadata contains the small amount of section metadata that wazero's
// public CompiledModule API does not expose.
type BinaryMetadata struct {
	DeclaredImports uint32
	HasStartSection bool
}

// InspectBinary reads section framing only. Callers first use CompileModule as
// the authoritative parser and validator, then use this to fill public API
// metadata gaps without decoding the module a second time.
func InspectBinary(wasm []byte) (BinaryMetadata, error) {
	const wasmHeaderBytes = 8
	if len(wasm) < wasmHeaderBytes {
		return BinaryMetadata{}, errors.New("module is shorter than its header")
	}
	metadata := BinaryMetadata{}
	for offset := wasmHeaderBytes; offset < len(wasm); {
		sectionID := wasm[offset]
		offset++
		sectionSize, bytesRead, err := readU32(wasm[offset:])
		if err != nil {
			return BinaryMetadata{}, fmt.Errorf("reading section size: %w", err)
		}
		offset += bytesRead
		sectionEnd := uint64(offset) + uint64(sectionSize)
		if sectionEnd > uint64(len(wasm)) {
			return BinaryMetadata{}, errors.New("section exceeds module size")
		}
		switch sectionID {
		case 2:
			count, _, err := readU32(wasm[offset:int(sectionEnd)])
			if err != nil {
				return BinaryMetadata{}, fmt.Errorf("reading import count: %w", err)
			}
			metadata.DeclaredImports = count
		case 8:
			metadata.HasStartSection = true
		}
		offset = int(sectionEnd)
	}
	return metadata, nil
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
