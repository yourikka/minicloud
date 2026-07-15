package minicloudabi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/yourikka/minicloud/internal/strictjson"
)

const (
	DefaultRawEnvelopeBytes = int64(1536 << 10)
	DefaultMetadataBytes    = 128 << 10
	DefaultBodyBytes        = 1 << 20
	DefaultHeaderCount      = 64
	DefaultHeaderBytes      = 32 << 10
	DefaultHeaderValueBytes = 8 << 10
	DefaultQueryPairs       = 128
	DefaultQueryBytes       = 32 << 10
	DefaultJSONDepth        = 32
	DefaultMethodBytes      = 16
	DefaultPathBytes        = 4096
)

// Limits is shared by Gateway, Worker, and SDK envelope validation. A zero
// field selects the specification default; positive values may only lower it.
type Limits struct {
	RawEnvelopeBytes int64
	MetadataBytes    int
	BodyBytes        int
	HeaderCount      int
	HeaderBytes      int
	HeaderValueBytes int
	QueryPairs       int
	QueryBytes       int
	JSONDepth        int
	MethodBytes      int
	PathBytes        int
}

// Validate checks that every non-zero override only lowers a v1 limit.
func (l Limits) Validate() error {
	_, err := normalizeLimits(l)
	return err
}

// Effective returns limits with every zero field replaced by its v1 default.
func (l Limits) Effective() (Limits, error) {
	return normalizeLimits(l)
}

// Tighten applies an additional set of limits without allowing any existing
// bound to increase. Zero override fields retain the current effective bound.
func (l Limits) Tighten(override Limits) (Limits, error) {
	base, err := normalizeLimits(l)
	if err != nil {
		return Limits{}, err
	}
	requested, err := normalizeLimits(override)
	if err != nil {
		return Limits{}, err
	}
	return Limits{
		RawEnvelopeBytes: min(base.RawEnvelopeBytes, requested.RawEnvelopeBytes),
		MetadataBytes:    min(base.MetadataBytes, requested.MetadataBytes),
		BodyBytes:        min(base.BodyBytes, requested.BodyBytes),
		HeaderCount:      min(base.HeaderCount, requested.HeaderCount),
		HeaderBytes:      min(base.HeaderBytes, requested.HeaderBytes),
		HeaderValueBytes: min(base.HeaderValueBytes, requested.HeaderValueBytes),
		QueryPairs:       min(base.QueryPairs, requested.QueryPairs),
		QueryBytes:       min(base.QueryBytes, requested.QueryBytes),
		JSONDepth:        min(base.JSONDepth, requested.JSONDepth),
		MethodBytes:      min(base.MethodBytes, requested.MethodBytes),
		PathBytes:        min(base.PathBytes, requested.PathBytes),
	}, nil
}

// DecodeRequest reads and validates exactly one Request JSON value.
func DecodeRequest(source io.Reader, limits Limits) (Request, error) {
	limits, err := normalizeLimits(limits)
	if err != nil {
		return Request{}, err
	}
	data, err := readEnvelope(source, limits)
	if err != nil {
		return Request{}, err
	}
	var request Request
	if err := decodeKnownFields(data, &request); err != nil {
		return Request{}, fmt.Errorf("minicloudabi: decoding request: %w", err)
	}
	request, err = normalizeRequest(request, limits)
	if err != nil {
		return Request{}, err
	}
	return request, nil
}

// EncodeRequest validates and writes exactly one Request JSON value.
func EncodeRequest(destination io.Writer, request Request, limits Limits) error {
	limits, err := normalizeLimits(limits)
	if err != nil {
		return err
	}
	request, err = normalizeRequest(request, limits)
	if err != nil {
		return err
	}
	return encodeEnvelope(destination, request, limits)
}

// DecodeResponse reads and validates exactly one Response JSON value.
func DecodeResponse(source io.Reader, requestMethod string, limits Limits) (Response, error) {
	limits, err := normalizeLimits(limits)
	if err != nil {
		return Response{}, err
	}
	data, err := readEnvelope(source, limits)
	if err != nil {
		return Response{}, err
	}
	var response Response
	if err := decodeKnownFields(data, &response); err != nil {
		return Response{}, fmt.Errorf("minicloudabi: decoding response: %w", err)
	}
	if err := validateResponse(response, requestMethod, limits); err != nil {
		return Response{}, err
	}
	return response, nil
}

// EncodeResponse validates and writes exactly one Response JSON value.
func EncodeResponse(destination io.Writer, response Response, requestMethod string, limits Limits) error {
	limits, err := normalizeLimits(limits)
	if err != nil {
		return err
	}
	if err := validateResponse(response, requestMethod, limits); err != nil {
		return err
	}
	return encodeEnvelope(destination, response, limits)
}

func readEnvelope(source io.Reader, limits Limits) ([]byte, error) {
	data, err := strictjson.Read(source, limits.RawEnvelopeBytes)
	if err != nil {
		return nil, fmt.Errorf("minicloudabi: reading envelope: %w", err)
	}
	if err := strictjson.Validate(data, limits.JSONDepth); err != nil {
		return nil, fmt.Errorf("minicloudabi: validating envelope json: %w", err)
	}
	return data, nil
}

func encodeEnvelope(destination io.Writer, envelope any, limits Limits) error {
	if destination == nil {
		return errors.New("minicloudabi: destination is required")
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("minicloudabi: encoding envelope: %w", err)
	}
	if int64(len(data)) > limits.RawEnvelopeBytes {
		return errors.New("minicloudabi: encoded envelope exceeds raw size limit")
	}
	written, err := destination.Write(data)
	if err != nil {
		return fmt.Errorf("minicloudabi: writing envelope: %w", err)
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func decodeKnownFields(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("minicloudabi: json contains trailing data")
	}
	return nil
}

func normalizeLimits(limits Limits) (Limits, error) {
	defaults := Limits{
		RawEnvelopeBytes: DefaultRawEnvelopeBytes,
		MetadataBytes:    DefaultMetadataBytes,
		BodyBytes:        DefaultBodyBytes,
		HeaderCount:      DefaultHeaderCount,
		HeaderBytes:      DefaultHeaderBytes,
		HeaderValueBytes: DefaultHeaderValueBytes,
		QueryPairs:       DefaultQueryPairs,
		QueryBytes:       DefaultQueryBytes,
		JSONDepth:        DefaultJSONDepth,
		MethodBytes:      DefaultMethodBytes,
		PathBytes:        DefaultPathBytes,
	}
	values := []*int{
		&limits.MetadataBytes,
		&limits.BodyBytes,
		&limits.HeaderCount,
		&limits.HeaderBytes,
		&limits.HeaderValueBytes,
		&limits.QueryPairs,
		&limits.QueryBytes,
		&limits.JSONDepth,
		&limits.MethodBytes,
		&limits.PathBytes,
	}
	defaultValues := []int{
		defaults.MetadataBytes,
		defaults.BodyBytes,
		defaults.HeaderCount,
		defaults.HeaderBytes,
		defaults.HeaderValueBytes,
		defaults.QueryPairs,
		defaults.QueryBytes,
		defaults.JSONDepth,
		defaults.MethodBytes,
		defaults.PathBytes,
	}
	if limits.RawEnvelopeBytes == 0 {
		limits.RawEnvelopeBytes = defaults.RawEnvelopeBytes
	}
	if limits.RawEnvelopeBytes < 1 || limits.RawEnvelopeBytes > defaults.RawEnvelopeBytes {
		return Limits{}, errors.New("minicloudabi: raw envelope limit is outside v1 bounds")
	}
	for index, value := range values {
		if *value == 0 {
			*value = defaultValues[index]
		}
		if *value < 1 || *value > defaultValues[index] {
			return Limits{}, errors.New("minicloudabi: limit is outside v1 bounds")
		}
	}
	return limits, nil
}
