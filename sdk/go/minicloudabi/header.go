package minicloudabi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var headerNamePattern = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

// RequestHeaders contains lowercase field names. Decoding merges differently
// cased names while preserving the received value order.
type RequestHeaders map[string][]string

// ResponseHeaders contains lowercase field names. Decoding rejects differently
// cased names that normalize to the same field.
type ResponseHeaders map[string][]string

// Query preserves value order for each case-sensitive query key.
type Query map[string][]string

func (h *RequestHeaders) UnmarshalJSON(data []byte) error {
	if h == nil {
		return errors.New("minicloudabi: cannot unmarshal request headers into nil receiver")
	}
	fields, err := decodeHeaders(data, true)
	if err != nil {
		return err
	}
	*h = RequestHeaders(fields)
	return nil
}

func (h *ResponseHeaders) UnmarshalJSON(data []byte) error {
	if h == nil {
		return errors.New("minicloudabi: cannot unmarshal response headers into nil receiver")
	}
	fields, err := decodeHeaders(data, false)
	if err != nil {
		return err
	}
	*h = ResponseHeaders(fields)
	return nil
}

func (q *Query) UnmarshalJSON(data []byte) error {
	if q == nil {
		return errors.New("minicloudabi: cannot unmarshal query into nil receiver")
	}
	fields, err := decodeStringMap(data, "query")
	if err != nil {
		return err
	}
	*q = Query(fields)
	return nil
}

func decodeHeaders(data []byte, mergeCase bool) (map[string][]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("minicloudabi: decoding headers: %w", err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("minicloudabi: headers must be an object")
	}

	fields := map[string][]string{}
	exact := map[string]struct{}{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("minicloudabi: decoding header name: %w", err)
		}
		name, ok := token.(string)
		if !ok || !headerNamePattern.MatchString(name) {
			return nil, errors.New("minicloudabi: invalid header name")
		}
		if _, exists := exact[name]; exists {
			return nil, errors.New("minicloudabi: duplicate header name")
		}
		exact[name] = struct{}{}

		values, err := decodeStringArray(decoder, "header values")
		if err != nil {
			return nil, err
		}
		normalized := strings.ToLower(name)
		if _, exists := fields[normalized]; exists && !mergeCase {
			return nil, errors.New("minicloudabi: response contains case-colliding header names")
		}
		fields[normalized] = append(fields[normalized], values...)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("minicloudabi: decoding headers closing delimiter: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("minicloudabi: headers contain trailing data")
	}
	return fields, nil
}

func decodeStringMap(data []byte, field string) (map[string][]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("minicloudabi: decoding %s: %w", field, err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, fmt.Errorf("minicloudabi: %s must be an object", field)
	}
	valuesByName := map[string][]string{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("minicloudabi: decoding %s key: %w", field, err)
		}
		name, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("minicloudabi: %s key must be a string", field)
		}
		values, err := decodeStringArray(decoder, field+" values")
		if err != nil {
			return nil, err
		}
		valuesByName[name] = values
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("minicloudabi: decoding %s closing delimiter: %w", field, err)
	}
	return valuesByName, nil
}

func decodeStringArray(decoder *json.Decoder, field string) ([]string, error) {
	var rawValues []json.RawMessage
	if err := decoder.Decode(&rawValues); err != nil {
		return nil, fmt.Errorf("minicloudabi: decoding %s: %w", field, err)
	}
	if rawValues == nil {
		return nil, fmt.Errorf("minicloudabi: %s must be an array", field)
	}
	values := make([]string, 0, len(rawValues))
	for _, raw := range rawValues {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '"' {
			return nil, fmt.Errorf("minicloudabi: %s must contain only strings", field)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("minicloudabi: decoding %s string: %w", field, err)
		}
		values = append(values, value)
	}
	return values, nil
}
