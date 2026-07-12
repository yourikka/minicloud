// Package minicloudabi implements MiniCloud's wasi-command-v1 JSON protocol.
package minicloudabi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

const Version = "1.0"

// Trigger identifies the immutable trigger revision that created an invocation.
type Trigger struct {
	Type             string `json:"type"`
	ID               string `json:"id"`
	ResourceRevision uint64 `json:"resource_revision,omitempty"`
	FireID           string `json:"fire_id,omitempty"`
	ScheduledTimeUTC string `json:"scheduled_time_utc,omitempty"`
	ReceiptID        string `json:"receipt_id,omitempty"`
	EventID          string `json:"event_id,omitempty"`
	SourceID         string `json:"source_id,omitempty"`
	EventType        string `json:"event_type,omitempty"`
	OccurredAt       string `json:"occurred_at,omitempty"`
	ContentType      string `json:"content_type,omitempty"`
}

// Request is one invocation delivered to a fresh WASI Command instance.
type Request struct {
	SpecVersion    string         `json:"spec_version"`
	InvocationID   string         `json:"invocation_id"`
	Method         string         `json:"method"`
	Path           string         `json:"path"`
	Query          Query          `json:"query"`
	Headers        RequestHeaders `json:"headers"`
	Body           []byte         `json:"-"`
	DeadlineUnixMS int64          `json:"deadline_unix_ms"`
	Trigger        Trigger        `json:"trigger"`
}

// Response is the single value a WASI Command writes to stdout.
type Response struct {
	SpecVersion string          `json:"spec_version"`
	Status      int             `json:"status"`
	Headers     ResponseHeaders `json:"headers"`
	Body        []byte          `json:"-"`
}

type requestWire struct {
	SpecVersion    string         `json:"spec_version"`
	InvocationID   string         `json:"invocation_id"`
	Method         string         `json:"method"`
	Path           string         `json:"path"`
	Query          Query          `json:"query"`
	Headers        RequestHeaders `json:"headers"`
	BodyBase64     *string        `json:"body_base64"`
	DeadlineUnixMS int64          `json:"deadline_unix_ms"`
	Trigger        Trigger        `json:"trigger"`
}

type responseWire struct {
	SpecVersion string          `json:"spec_version"`
	Status      int             `json:"status"`
	Headers     ResponseHeaders `json:"headers"`
	BodyBase64  *string         `json:"body_base64"`
}

func (r Request) MarshalJSON() ([]byte, error) {
	encodedBody := base64.StdEncoding.EncodeToString(r.Body)
	wire := requestWire{
		SpecVersion:    r.SpecVersion,
		InvocationID:   r.InvocationID,
		Method:         r.Method,
		Path:           r.Path,
		Query:          r.Query,
		Headers:        r.Headers,
		BodyBase64:     &encodedBody,
		DeadlineUnixMS: r.DeadlineUnixMS,
		Trigger:        r.Trigger,
	}
	return json.Marshal(wire)
}

func (r *Request) UnmarshalJSON(data []byte) error {
	if r == nil {
		return errors.New("minicloudabi: cannot unmarshal request into nil receiver")
	}
	var wire requestWire
	if err := decodeKnownFields(data, &wire); err != nil {
		return err
	}
	if wire.BodyBase64 == nil {
		return errors.New("minicloudabi: body_base64 must be a string")
	}
	body, err := decodeCanonicalBase64(*wire.BodyBase64)
	if err != nil {
		return fmt.Errorf("minicloudabi: decoding request body: %w", err)
	}
	*r = Request{
		SpecVersion:    wire.SpecVersion,
		InvocationID:   wire.InvocationID,
		Method:         wire.Method,
		Path:           wire.Path,
		Query:          wire.Query,
		Headers:        wire.Headers,
		Body:           body,
		DeadlineUnixMS: wire.DeadlineUnixMS,
		Trigger:        wire.Trigger,
	}
	return nil
}

func (r Response) MarshalJSON() ([]byte, error) {
	encodedBody := base64.StdEncoding.EncodeToString(r.Body)
	wire := responseWire{
		SpecVersion: r.SpecVersion,
		Status:      r.Status,
		Headers:     r.Headers,
		BodyBase64:  &encodedBody,
	}
	return json.Marshal(wire)
}

func (r *Response) UnmarshalJSON(data []byte) error {
	if r == nil {
		return errors.New("minicloudabi: cannot unmarshal response into nil receiver")
	}
	var wire responseWire
	if err := decodeKnownFields(data, &wire); err != nil {
		return err
	}
	if wire.BodyBase64 == nil {
		return errors.New("minicloudabi: body_base64 must be a string")
	}
	body, err := decodeCanonicalBase64(*wire.BodyBase64)
	if err != nil {
		return fmt.Errorf("minicloudabi: decoding response body: %w", err)
	}
	*r = Response{
		SpecVersion: wire.SpecVersion,
		Status:      wire.Status,
		Headers:     wire.Headers,
		Body:        body,
	}
	return nil
}

func decodeCanonicalBase64(encoded string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("body_base64 is not standard padded base64")
	}
	if base64.StdEncoding.EncodeToString(decoded) != encoded {
		return nil, errors.New("body_base64 is not canonical")
	}
	return decoded, nil
}
