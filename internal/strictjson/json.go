// Package strictjson validates bounded JSON before typed decoding.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

var (
	ErrTooLarge     = errors.New("json input exceeds size limit")
	ErrTooDeep      = errors.New("json input exceeds depth limit")
	ErrDuplicateKey = errors.New("json object contains duplicate key")
)

// Read reads at most maxBytes+1 bytes so oversized input is rejected without
// consuming or allocating the remainder.
func Read(source io.Reader, maxBytes int64) ([]byte, error) {
	if source == nil {
		return nil, errors.New("json source is required")
	}
	if maxBytes < 1 {
		return nil, errors.New("json size limit must be positive")
	}
	data, err := io.ReadAll(io.LimitReader(source, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading json: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

// Validate rejects invalid UTF-8, excessive nesting, duplicate object keys,
// multiple root values, and malformed JSON.
func Validate(data []byte, maxDepth int) error {
	if maxDepth < 1 {
		return errors.New("json depth limit must be positive")
	}
	if !utf8.Valid(data) {
		return errors.New("json input is not valid utf-8")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := validateValue(decoder, 0, maxDepth); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("json input contains multiple values")
		}
		return fmt.Errorf("reading json trailing data: %w", err)
	}
	return nil
}

func validateValue(decoder *json.Decoder, depth, maxDepth int) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("reading json token: %w", err)
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}

	depth++
	if depth > maxDepth {
		return ErrTooDeep
	}
	switch delimiter {
	case '{':
		return validateObject(decoder, depth, maxDepth)
	case '[':
		return validateArray(decoder, depth, maxDepth)
	default:
		return errors.New("json input contains unexpected closing delimiter")
	}
}

func validateObject(decoder *json.Decoder, depth, maxDepth int) error {
	keys := map[string]struct{}{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("reading json object key: %w", err)
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("json object key is not a string")
		}
		if _, exists := keys[key]; exists {
			return fmt.Errorf("%w: %q", ErrDuplicateKey, key)
		}
		keys[key] = struct{}{}
		if err := validateValue(decoder, depth, maxDepth); err != nil {
			return err
		}
	}
	return consumeClosing(decoder, '}')
}

func validateArray(decoder *json.Decoder, depth, maxDepth int) error {
	for decoder.More() {
		if err := validateValue(decoder, depth, maxDepth); err != nil {
			return err
		}
	}
	return consumeClosing(decoder, ']')
}

func consumeClosing(decoder *json.Decoder, expected json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("reading json closing delimiter: %w", err)
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != expected {
		return errors.New("json input contains mismatched delimiter")
	}
	return nil
}
