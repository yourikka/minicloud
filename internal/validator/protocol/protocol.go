// Package protocol defines the bounded parent-to-validator subprocess protocol.
package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/strictjson"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

const (
	SchemaVersion        = "validator-v1"
	RuntimeName          = wasmprofile.RuntimeName
	RuntimeVersion       = wasmprofile.RuntimeVersion
	FeatureProfile       = wasmprofile.FeatureProfile
	EngineCompiler       = wasmprofile.EngineCompiler
	EngineInterpreter    = wasmprofile.EngineInterpreter
	MaxHeaderBytes       = 64 << 10
	MaxReportBytes       = 256 << 10
	DefaultArtifactBytes = int64(32 << 20)
	HardArtifactBytes    = int64(256 << 20)
	CodeOK               = "ok"
)

var (
	frameMagic          = [8]byte{'M', 'C', 'V', 'A', 'L', '0', '0', '1'}
	validationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// Request is trusted policy metadata followed by untrusted Artifact bytes.
type Request struct {
	SchemaVersion         string                    `json:"schema_version"`
	ValidationID          string                    `json:"validation_id"`
	ArtifactDigest        digest.SHA256             `json:"artifact_digest"`
	ArtifactSize          int64                     `json:"artifact_size"`
	ABI                   string                    `json:"abi"`
	HostAPIProfile        string                    `json:"host_api_profile"`
	RuntimeFeatureProfile string                    `json:"runtime_feature_profile"`
	RuntimeEngine         string                    `json:"runtime_engine"`
	MemoryLimitMiB        uint32                    `json:"memory_limit_mib"`
	RequestedCapabilities []model.CapabilityRequest `json:"requested_capabilities"`
}

// Import is a deterministic description of an observed function or memory import.
type Import struct {
	Module string `json:"module"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
}

// Memory describes the module's exported linear memory after applying the tier.
type Memory struct {
	Exported bool   `json:"exported"`
	MinPages uint32 `json:"min_pages"`
	MaxPages uint32 `json:"max_pages"`
}

// Timing separates binary validation from native compilation time.
type Timing struct {
	CompileNanoseconds int64 `json:"compile_nanoseconds"`
}

// Isolation reports which child-process boundaries were actually enforced.
type Isolation struct {
	ProcessBoundary bool `json:"process_boundary"`
	Deadline        bool `json:"deadline"`
	CPULimit        bool `json:"cpu_limit"`
	FileSizeLimit   bool `json:"file_size_limit"`
	MemoryLimit     bool `json:"memory_limit"`
	TempDiskLimit   bool `json:"temp_disk_limit"`
}

// Report is the validator's only stdout value. Message is always safe to expose.
type Report struct {
	SchemaVersion         string        `json:"schema_version"`
	ValidationID          string        `json:"validation_id"`
	Valid                 bool          `json:"valid"`
	Code                  string        `json:"code"`
	Reason                string        `json:"reason"`
	Message               string        `json:"message"`
	ArtifactDigest        digest.SHA256 `json:"artifact_digest"`
	ArtifactSize          int64         `json:"artifact_size"`
	RuntimeName           string        `json:"runtime_name"`
	RuntimeVersion        string        `json:"runtime_version"`
	RuntimeFeatureProfile string        `json:"runtime_feature_profile"`
	RuntimeEngine         string        `json:"runtime_engine"`
	Imports               []Import      `json:"imports"`
	Exports               []string      `json:"exports"`
	Memory                Memory        `json:"memory"`
	Timing                Timing        `json:"timing"`
	Isolation             Isolation     `json:"isolation"`
}

// NewRequestReader creates a streaming frame without buffering the Artifact.
func NewRequestReader(request Request, artifact io.Reader) (io.Reader, error) {
	if artifact == nil {
		return nil, errors.New("validator protocol: artifact reader is required")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	header, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("validator protocol: encoding request header: %w", err)
	}
	if len(header) > MaxHeaderBytes {
		return nil, errors.New("validator protocol: request header exceeds limit")
	}

	prefix := make([]byte, len(frameMagic)+4)
	copy(prefix, frameMagic[:])
	binary.BigEndian.PutUint32(prefix[len(frameMagic):], uint32(len(header)))
	return io.MultiReader(
		bytes.NewReader(prefix),
		bytes.NewReader(header),
		&exactReader{source: artifact, remaining: request.ArtifactSize},
	), nil
}

// ReadRequest reads one complete frame, bounds allocation, and verifies bytes.
func ReadRequest(source io.Reader, maxArtifactBytes int64) (Request, []byte, error) {
	if source == nil {
		return Request{}, nil, errors.New("validator protocol: source is required")
	}
	if maxArtifactBytes == 0 {
		maxArtifactBytes = DefaultArtifactBytes
	}
	if maxArtifactBytes < 1 || maxArtifactBytes > HardArtifactBytes {
		return Request{}, nil, errors.New("validator protocol: artifact limit is outside v1 bounds")
	}

	prefix := make([]byte, len(frameMagic)+4)
	if _, err := io.ReadFull(source, prefix); err != nil {
		return Request{}, nil, fmt.Errorf("validator protocol: reading frame prefix: %w", err)
	}
	if !bytes.Equal(prefix[:len(frameMagic)], frameMagic[:]) {
		return Request{}, nil, errors.New("validator protocol: unsupported frame magic")
	}
	headerSize := binary.BigEndian.Uint32(prefix[len(frameMagic):])
	if headerSize == 0 || headerSize > MaxHeaderBytes {
		return Request{}, nil, errors.New("validator protocol: invalid request header size")
	}
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(source, header); err != nil {
		return Request{}, nil, fmt.Errorf("validator protocol: reading request header: %w", err)
	}
	if err := strictjson.Validate(header, 16); err != nil {
		return Request{}, nil, fmt.Errorf("validator protocol: validating request header: %w", err)
	}

	var request Request
	decoder := json.NewDecoder(bytes.NewReader(header))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return Request{}, nil, fmt.Errorf("validator protocol: decoding request header: %w", err)
	}
	if err := request.Validate(); err != nil {
		return Request{}, nil, err
	}
	if request.ArtifactSize > maxArtifactBytes {
		return Request{}, nil, errors.New("validator protocol: artifact exceeds configured limit")
	}

	artifact := make([]byte, request.ArtifactSize)
	if _, err := io.ReadFull(source, artifact); err != nil {
		return Request{}, nil, fmt.Errorf("validator protocol: reading artifact: %w", err)
	}
	var extra [1]byte
	if count, err := source.Read(extra[:]); count != 0 || !errors.Is(err, io.EOF) {
		return Request{}, nil, errors.New("validator protocol: frame contains trailing artifact bytes")
	}
	if digest.Sum(artifact) != request.ArtifactDigest {
		return Request{}, nil, errors.New("validator protocol: artifact digest mismatch")
	}
	return request, artifact, nil
}

// EncodeReport returns one bounded JSON report.
func EncodeReport(report Report) ([]byte, error) {
	if err := report.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("validator protocol: encoding report: %w", err)
	}
	if len(encoded) > MaxReportBytes {
		return nil, errors.New("validator protocol: report exceeds limit")
	}
	return encoded, nil
}

// DecodeReport strictly reads one bounded JSON report.
func DecodeReport(source io.Reader) (Report, error) {
	data, err := strictjson.Read(source, MaxReportBytes)
	if err != nil {
		return Report{}, fmt.Errorf("validator protocol: reading report: %w", err)
	}
	if err := strictjson.Validate(data, 16); err != nil {
		return Report{}, fmt.Errorf("validator protocol: validating report: %w", err)
	}
	var report Report
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return Report{}, fmt.Errorf("validator protocol: decoding report: %w", err)
	}
	if err := report.Validate(); err != nil {
		return Report{}, err
	}
	return report, nil
}

// Validate checks trusted request policy before a subprocess is started.
func (r Request) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return errors.New("validator protocol: unsupported schema version")
	}
	if !validationIDPattern.MatchString(r.ValidationID) {
		return errors.New("validator protocol: invalid validation id")
	}
	if _, err := digest.ParseSHA256(r.ArtifactDigest.String()); err != nil {
		return errors.New("validator protocol: invalid artifact digest")
	}
	if r.ArtifactSize < 1 || r.ArtifactSize > HardArtifactBytes {
		return errors.New("validator protocol: invalid artifact size")
	}
	if r.ABI != model.ABIWASICommandV1 {
		return errors.New("validator protocol: unsupported abi")
	}
	if r.HostAPIProfile != model.HostAPIProfileNone {
		return errors.New("validator protocol: unsupported host api profile")
	}
	if r.RuntimeFeatureProfile != FeatureProfile {
		return errors.New("validator protocol: unsupported runtime feature profile")
	}
	if r.RuntimeEngine != EngineCompiler && r.RuntimeEngine != EngineInterpreter {
		return errors.New("validator protocol: unsupported runtime engine")
	}
	if !wasmprofile.ValidMemoryTier(r.MemoryLimitMiB) {
		return errors.New("validator protocol: unsupported memory tier")
	}
	if r.RequestedCapabilities == nil {
		return errors.New("validator protocol: requested capabilities must be an array")
	}
	return nil
}

// Validate checks the response shape without trusting child output.
func (r Report) Validate() error {
	if r.SchemaVersion != SchemaVersion || !validationIDPattern.MatchString(r.ValidationID) {
		return errors.New("validator protocol: invalid report identity")
	}
	if r.Message == "" || r.Reason == "" {
		return errors.New("validator protocol: invalid report classification")
	}
	if _, err := digest.ParseSHA256(r.ArtifactDigest.String()); err != nil {
		return errors.New("validator protocol: invalid report artifact digest")
	}
	if r.ArtifactSize < 1 || r.RuntimeName != RuntimeName || r.RuntimeVersion != RuntimeVersion {
		return errors.New("validator protocol: invalid report runtime metadata")
	}
	if r.RuntimeFeatureProfile != FeatureProfile || r.Imports == nil || r.Exports == nil {
		return errors.New("validator protocol: incomplete report metadata")
	}
	if r.RuntimeEngine != EngineCompiler && r.RuntimeEngine != EngineInterpreter {
		return errors.New("validator protocol: invalid report runtime engine")
	}
	if r.Timing.CompileNanoseconds < 0 {
		return errors.New("validator protocol: invalid report timing")
	}
	if r.Valid && r.Code != CodeOK {
		return errors.New("validator protocol: valid report must use ok code")
	}
	if !r.Valid && !problem.Known(problem.Code(r.Code)) {
		return errors.New("validator protocol: rejected report must use a stable error code")
	}
	return nil
}

type exactReader struct {
	source    io.Reader
	remaining int64
}

func (r *exactReader) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	read, err := r.source.Read(buffer)
	r.remaining -= int64(read)
	if errors.Is(err, io.EOF) && r.remaining > 0 {
		return read, io.ErrUnexpectedEOF
	}
	return read, err
}
