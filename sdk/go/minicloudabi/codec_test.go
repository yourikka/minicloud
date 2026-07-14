package minicloudabi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

func TestRequestRoundTrip(t *testing.T) {
	t.Parallel()

	request := validRequest()
	request.Method = "post"
	var encoded bytes.Buffer
	if err := abi.EncodeRequest(&encoded, request, abi.Limits{}); err != nil {
		t.Fatalf("EncodeRequest() error = %v", err)
	}
	decoded, err := abi.DecodeRequest(&encoded, abi.Limits{})
	if err != nil {
		t.Fatalf("DecodeRequest() error = %v", err)
	}
	if decoded.Method != "POST" {
		t.Errorf("decoded method = %q, want POST", decoded.Method)
	}
	if !bytes.Equal(decoded.Body, request.Body) {
		t.Errorf("decoded body = %q, want %q", decoded.Body, request.Body)
	}
	if got := decoded.Query["name"]; len(got) != 2 || got[0] != "Ada" || got[1] != "Lin" {
		t.Errorf("decoded query values = %v", got)
	}
}

func TestDecodeRequestMergesHeaderCaseInWireOrder(t *testing.T) {
	t.Parallel()

	raw := `{"spec_version":"1.0","invocation_id":"inv_1","method":"GET","path":"/","query":{},"headers":{"X-Test":["first"],"x-test":["second"]},"body_base64":"","deadline_unix_ms":1,"trigger":{"type":"http","id":"trg_1"}}`
	request, err := abi.DecodeRequest(strings.NewReader(raw), abi.Limits{})
	if err != nil {
		t.Fatalf("DecodeRequest() error = %v", err)
	}
	values := request.Headers["x-test"]
	if len(values) != 2 || values[0] != "first" || values[1] != "second" {
		t.Fatalf("merged header values = %v, want wire order", values)
	}
}

func TestDecodeRejectsUnsafeJSONAndBase64(t *testing.T) {
	t.Parallel()

	valid := `{"spec_version":"1.0","invocation_id":"inv_1","method":"GET","path":"/","query":{},"headers":{},"body_base64":"","deadline_unix_ms":1,"trigger":{"type":"http","id":"trg_1"}}`
	tests := []struct {
		name  string
		input string
	}{
		{name: "duplicate key", input: strings.Replace(valid, `"method":"GET"`, `"method":"GET","method":"POST"`, 1)},
		{name: "unknown field", input: strings.Replace(valid, `"trigger":`, `"unknown":true,"trigger":`, 1)},
		{name: "noncanonical base64", input: strings.Replace(valid, `"body_base64":""`, `"body_base64":"Zh=="`, 1)},
		{name: "unpadded base64", input: strings.Replace(valid, `"body_base64":""`, `"body_base64":"Zg"`, 1)},
		{name: "null base64", input: strings.Replace(valid, `"body_base64":""`, `"body_base64":null`, 1)},
		{name: "null header value", input: strings.Replace(valid, `"headers":{}`, `"headers":{"x-test":[null]}`, 1)},
		{name: "null query value", input: strings.Replace(valid, `"query":{}`, `"query":{"key":[null]}`, 1)},
		{name: "extra json value", input: valid + ` {}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := abi.DecodeRequest(strings.NewReader(tt.input), abi.Limits{}); err == nil {
				t.Fatal("DecodeRequest() error = nil, want rejection")
			}
		})
	}
}

func TestDecodeResponseRejectsCaseCollidingHeaders(t *testing.T) {
	t.Parallel()

	raw := `{"spec_version":"1.0","status":200,"headers":{"X-Test":["first"],"x-test":["second"]},"body_base64":""}`
	if _, err := abi.DecodeResponse(strings.NewReader(raw), "GET", abi.Limits{}); err == nil {
		t.Fatal("DecodeResponse() accepted case-colliding headers")
	}
}

func TestResponseRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response abi.Response
		method   string
		wantErr  bool
	}{
		{name: "valid", response: validResponse(), method: "GET"},
		{name: "status below range", response: responseWithStatus(101), method: "GET", wantErr: true},
		{name: "status above range", response: responseWithStatus(600), method: "GET", wantErr: true},
		{name: "head body", response: validResponse(), method: "HEAD", wantErr: true},
		{name: "204 body", response: responseWithStatus(204), method: "GET", wantErr: true},
		{name: "304 body", response: responseWithStatus(304), method: "GET", wantErr: true},
		{name: "connection header", response: responseWithHeader("connection", "close"), method: "GET", wantErr: true},
		{name: "content length", response: responseWithHeader("content-length", "2"), method: "GET", wantErr: true},
		{name: "header newline", response: responseWithHeader("x-test", "ok\r\nbad"), method: "GET", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			err := abi.EncodeResponse(&output, tt.response, tt.method, abi.Limits{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("EncodeResponse() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEnvelopeLimits(t *testing.T) {
	t.Parallel()

	t.Run("body is excluded from metadata count", func(t *testing.T) {
		t.Parallel()
		request := validRequest()
		request.Body = bytes.Repeat([]byte("x"), 200<<10)
		var output bytes.Buffer
		if err := abi.EncodeRequest(&output, request, abi.Limits{}); err != nil {
			t.Fatalf("EncodeRequest() error = %v", err)
		}
	})

	t.Run("raw envelope", func(t *testing.T) {
		t.Parallel()
		_, err := abi.DecodeRequest(strings.NewReader(strings.Repeat("x", 65)), abi.Limits{RawEnvelopeBytes: 64})
		if err == nil {
			t.Fatal("DecodeRequest() accepted oversized raw input")
		}
	})

	t.Run("body", func(t *testing.T) {
		t.Parallel()
		request := validRequest()
		request.Body = []byte("too long")
		if err := abi.EncodeRequest(io.Discard, request, abi.Limits{BodyBytes: 4}); err == nil {
			t.Fatal("EncodeRequest() accepted oversized body")
		}
	})

	t.Run("query pairs", func(t *testing.T) {
		t.Parallel()
		request := validRequest()
		request.Query = abi.Query{"a": {"1", "2"}}
		if err := abi.EncodeRequest(io.Discard, request, abi.Limits{QueryPairs: 1}); err == nil {
			t.Fatal("EncodeRequest() accepted too many query pairs")
		}
	})

	t.Run("invalid raised limit", func(t *testing.T) {
		t.Parallel()
		limits := abi.Limits{JSONDepth: 33}
		if err := limits.Validate(); err == nil {
			t.Fatal("Limits.Validate() accepted a limit above the v1 maximum")
		}
		if err := abi.EncodeRequest(io.Discard, validRequest(), limits); err == nil {
			t.Fatal("EncodeRequest() accepted a limit above the v1 maximum")
		}
	})
}

func TestRequestPathAndVersionRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*abi.Request)
	}{
		{name: "unsupported version", mutate: func(r *abi.Request) { r.SpecVersion = "2.0" }},
		{name: "invalid percent escape", mutate: func(r *abi.Request) { r.Path = "/bad%zz" }},
		{name: "percent encoded nul", mutate: func(r *abi.Request) { r.Path = "/bad%00path" }},
		{name: "percent encoded invalid utf8", mutate: func(r *abi.Request) { r.Path = "/bad%ffpath" }},
		{name: "path without slash", mutate: func(r *abi.Request) { r.Path = "relative" }},
		{name: "nul in path", mutate: func(r *abi.Request) { r.Path = "/bad\x00path" }},
		{name: "invalid invocation id", mutate: func(r *abi.Request) { r.InvocationID = "bad id" }},
		{name: "unknown trigger", mutate: func(r *abi.Request) { r.Trigger.Type = "queue" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			request := validRequest()
			tt.mutate(&request)
			if err := abi.EncodeRequest(io.Discard, request, abi.Limits{}); err == nil {
				t.Fatal("EncodeRequest() error = nil, want rejection")
			}
		})
	}
}

func TestEncodeDetectsShortWrite(t *testing.T) {
	t.Parallel()

	err := abi.EncodeResponse(shortWriter{}, validResponse(), "GET", abi.Limits{})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("EncodeResponse() error = %v, want io.ErrShortWrite", err)
	}
}

func TestSchemasAreValidJSON(t *testing.T) {
	t.Parallel()

	paths := []string{
		"../../../api/abi/v1/request-envelope.schema.json",
		"../../../api/abi/v1/response-envelope.schema.json",
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading schema %q: %v", path, err)
		}
		var schema map[string]any
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("decoding schema %q: %v", path, err)
		}
		if schema["$schema"] == nil || schema["$id"] == nil {
			t.Fatalf("schema %q lacks version identifiers", path)
		}
	}
}

func FuzzDecodeRequest(f *testing.F) {
	f.Add([]byte(`{"spec_version":"1.0","invocation_id":"inv_1","method":"GET","path":"/","query":{},"headers":{},"body_base64":"","deadline_unix_ms":1,"trigger":{"type":"http","id":"trg_1"}}`))
	f.Add([]byte(`{"spec_version":"1.0","spec_version":"1.0"}`))
	f.Add([]byte{0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		if _, err := abi.DecodeRequest(bytes.NewReader(data), abi.Limits{}); err != nil {
			return
		}
	})
}

func validRequest() abi.Request {
	return abi.Request{
		SpecVersion:    abi.Version,
		InvocationID:   "inv_1",
		Method:         "POST",
		Path:           "/hello",
		Query:          abi.Query{"name": {"Ada", "Lin"}},
		Headers:        abi.RequestHeaders{"content-type": {"application/json"}},
		Body:           []byte(`{"x":1}`),
		DeadlineUnixMS: 1_783_771_200_000,
		Trigger:        abi.Trigger{Type: "http", ID: "trg_1"},
	}
}

func validResponse() abi.Response {
	return abi.Response{
		SpecVersion: abi.Version,
		Status:      200,
		Headers:     abi.ResponseHeaders{"content-type": {"text/plain"}},
		Body:        []byte("ok"),
	}
}

func responseWithStatus(status int) abi.Response {
	response := validResponse()
	response.Status = status
	return response
}

func responseWithHeader(name, value string) abi.Response {
	response := validResponse()
	response.Headers[name] = []string{value}
	return response
}

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) {
	return 0, nil
}
