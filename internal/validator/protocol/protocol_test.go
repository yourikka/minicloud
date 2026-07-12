package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
)

func TestRequestRoundTrip(t *testing.T) {
	t.Parallel()

	artifact := []byte("\x00asm validator protocol fixture")
	want := validRequest(artifact)
	framed, err := NewRequestReader(want, bytes.NewReader(artifact))
	if err != nil {
		t.Fatalf("NewRequestReader() error = %v", err)
	}

	got, gotArtifact, err := ReadRequest(framed, int64(len(artifact)))
	if err != nil {
		t.Fatalf("ReadRequest() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadRequest() request = %#v, want %#v", got, want)
	}
	if !bytes.Equal(gotArtifact, artifact) {
		t.Fatalf("ReadRequest() artifact = %q, want %q", gotArtifact, artifact)
	}
}

func TestReadRequestRejectsInvalidFrames(t *testing.T) {
	t.Parallel()

	artifact := []byte("validator protocol artifact")
	request := validRequest(artifact)
	header := marshalRequest(t, request)

	truncatedRequest := request
	truncatedRequest.ArtifactSize++

	mismatchedRequest := request
	mismatchedRequest.ArtifactDigest = digest.Sum([]byte("different artifact"))

	tests := []struct {
		name    string
		frame   []byte
		wantErr string
	}{
		{
			name:    "unknown header field",
			frame:   makeFrame(addJSONField(t, header, `"unknown":true`), artifact),
			wantErr: "unknown field",
		},
		{
			name:    "duplicate header field",
			frame:   makeFrame(addJSONField(t, header, `"validation_id":"val_other"`), artifact),
			wantErr: "duplicate key",
		},
		{
			name:    "oversized header",
			frame:   framePrefix(frameMagic, MaxHeaderBytes+1),
			wantErr: "invalid request header size",
		},
		{
			name: "wrong magic",
			frame: framePrefix(
				[8]byte{'N', 'O', 'T', 'V', 'A', 'L', '0', '1'},
				uint32(len(header)),
			),
			wantErr: "unsupported frame magic",
		},
		{
			name:    "truncated artifact",
			frame:   makeFrame(marshalRequest(t, truncatedRequest), artifact),
			wantErr: "reading artifact",
		},
		{
			name:    "trailing artifact byte",
			frame:   append(makeFrame(header, artifact), 0xff),
			wantErr: "trailing artifact bytes",
		},
		{
			name:    "artifact digest mismatch",
			frame:   makeFrame(marshalRequest(t, mismatchedRequest), artifact),
			wantErr: "artifact digest mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := ReadRequest(bytes.NewReader(tt.frame), HardArtifactBytes)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ReadRequest() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestReportRoundTrip(t *testing.T) {
	t.Parallel()

	want := validReport()
	encoded, err := EncodeReport(want)
	if err != nil {
		t.Fatalf("EncodeReport() error = %v", err)
	}
	got, err := DecodeReport(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("DecodeReport() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DecodeReport() = %#v, want %#v", got, want)
	}
}

func TestDecodeReportRejectsUnknownAndDuplicateFields(t *testing.T) {
	t.Parallel()

	encoded, err := EncodeReport(validReport())
	if err != nil {
		t.Fatalf("EncodeReport() error = %v", err)
	}
	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{
			name:    "unknown field",
			data:    addJSONField(t, encoded, `"unknown":true`),
			wantErr: "unknown field",
		},
		{
			name:    "duplicate field",
			data:    addJSONField(t, encoded, `"validation_id":"val_other"`),
			wantErr: "duplicate key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeReport(bytes.NewReader(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("DecodeReport() error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestReportSizeLimit(t *testing.T) {
	t.Parallel()

	t.Run("encode", func(t *testing.T) {
		t.Parallel()

		report := validReport()
		report.Message = strings.Repeat("x", MaxReportBytes)
		_, err := EncodeReport(report)
		if err == nil || !strings.Contains(err.Error(), "report exceeds limit") {
			t.Fatalf("EncodeReport() error = %v, want report size error", err)
		}
	})

	t.Run("decode", func(t *testing.T) {
		t.Parallel()

		_, err := DecodeReport(io.LimitReader(strings.NewReader(strings.Repeat("x", MaxReportBytes+1)), MaxReportBytes+1))
		if err == nil || !strings.Contains(err.Error(), "exceeds size limit") {
			t.Fatalf("DecodeReport() error = %v, want report size error", err)
		}
	})
}

func TestReportValidateRejectsInvalidIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Report)
	}{
		{
			name: "unsupported schema",
			mutate: func(report *Report) {
				report.SchemaVersion = "validator-v2"
			},
		},
		{
			name: "invalid validation id",
			mutate: func(report *Report) {
				report.ValidationID = "contains whitespace"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			report := validReport()
			tt.mutate(&report)
			if err := report.Validate(); err == nil || !strings.Contains(err.Error(), "invalid report identity") {
				t.Fatalf("Report.Validate() error = %v, want invalid identity error", err)
			}
		})
	}
}

func validRequest(artifact []byte) Request {
	return Request{
		SchemaVersion:         SchemaVersion,
		ValidationID:          "val_protocol-test:1",
		ArtifactDigest:        digest.Sum(artifact),
		ArtifactSize:          int64(len(artifact)),
		ABI:                   "wasi-command-v1",
		HostAPIProfile:        "none",
		RuntimeFeatureProfile: FeatureProfile,
		RuntimeEngine:         EngineCompiler,
		MemoryLimitMiB:        128,
		RequestedCapabilities: []model.CapabilityRequest{},
	}
}

func validReport() Report {
	artifact := []byte("validator protocol artifact")
	return Report{
		SchemaVersion:         SchemaVersion,
		ValidationID:          "val_protocol-test:1",
		Valid:                 true,
		Code:                  CodeOK,
		Reason:                "accepted",
		Message:               "module is compatible with the locked v1 runtime profile",
		ArtifactDigest:        digest.Sum(artifact),
		ArtifactSize:          int64(len(artifact)),
		RuntimeName:           RuntimeName,
		RuntimeVersion:        RuntimeVersion,
		RuntimeFeatureProfile: FeatureProfile,
		RuntimeEngine:         EngineCompiler,
		Imports:               []Import{},
		Exports:               []string{"_start"},
		Memory: Memory{
			Exported: true,
			MinPages: 1,
			MaxPages: 2,
		},
		Timing: Timing{CompileNanoseconds: 1},
		Isolation: Isolation{
			ProcessBoundary: true,
			Deadline:        true,
			CPULimit:        true,
			FileSizeLimit:   true,
		},
	}
}

func marshalRequest(t *testing.T, request Request) []byte {
	t.Helper()

	header, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return header
}

func addJSONField(t *testing.T, object []byte, field string) []byte {
	t.Helper()

	if len(object) == 0 || object[len(object)-1] != '}' {
		t.Fatalf("test object is not a JSON object: %q", object)
	}
	result := bytes.Clone(object[:len(object)-1])
	result = append(result, ',')
	result = append(result, field...)
	result = append(result, '}')
	return result
}

func makeFrame(header, artifact []byte) []byte {
	frame := framePrefix(frameMagic, uint32(len(header)))
	frame = append(frame, header...)
	frame = append(frame, artifact...)
	return frame
}

func framePrefix(magic [8]byte, headerSize uint32) []byte {
	prefix := make([]byte, len(magic)+4)
	copy(prefix, magic[:])
	binary.BigEndian.PutUint32(prefix[len(magic):], headerSize)
	return prefix
}
