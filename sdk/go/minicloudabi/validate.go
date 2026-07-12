package minicloudabi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

var connectionHeaders = map[string]struct{}{
	"connection":          {},
	"content-length":      {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

func normalizeRequest(request Request, limits Limits) (Request, error) {
	if request.SpecVersion != Version {
		return Request{}, errors.New("minicloudabi: unsupported request spec_version")
	}
	if !identifierPattern.MatchString(request.InvocationID) {
		return Request{}, errors.New("minicloudabi: invalid invocation_id")
	}
	method := strings.ToUpper(request.Method)
	if len(method) == 0 || len(method) > limits.MethodBytes || !headerNamePattern.MatchString(method) {
		return Request{}, errors.New("minicloudabi: invalid method")
	}
	request.Method = method
	if err := validatePath(request.Path, limits.PathBytes); err != nil {
		return Request{}, err
	}
	if request.DeadlineUnixMS <= 0 {
		return Request{}, errors.New("minicloudabi: deadline_unix_ms must be positive")
	}
	if len(request.Body) > limits.BodyBytes {
		return Request{}, errors.New("minicloudabi: request body exceeds size limit")
	}
	if err := validateTrigger(request.Trigger); err != nil {
		return Request{}, err
	}
	if err := validateQuery(request.Query, limits); err != nil {
		return Request{}, err
	}
	headers, err := normalizeHeaders(map[string][]string(request.Headers), limits)
	if err != nil {
		return Request{}, err
	}
	request.Headers = RequestHeaders(headers)
	if err := validateMetadataSize(request, limits.MetadataBytes); err != nil {
		return Request{}, err
	}
	return request, nil
}

func validateResponse(response Response, requestMethod string, limits Limits) error {
	if response.SpecVersion != Version {
		return errors.New("minicloudabi: unsupported response spec_version")
	}
	if response.Status < 200 || response.Status > 599 {
		return errors.New("minicloudabi: response status must be between 200 and 599")
	}
	if len(response.Body) > limits.BodyBytes {
		return errors.New("minicloudabi: response body exceeds size limit")
	}
	bodyForbidden := strings.EqualFold(requestMethod, "HEAD") || response.Status == 204 || response.Status == 304
	if bodyForbidden && len(response.Body) != 0 {
		return errors.New("minicloudabi: response body is forbidden for method or status")
	}
	if _, err := normalizeHeaders(map[string][]string(response.Headers), limits); err != nil {
		return err
	}
	return validateMetadataSize(response, limits.MetadataBytes)
}

func normalizeHeaders(fields map[string][]string, limits Limits) (map[string][]string, error) {
	if fields == nil {
		return nil, errors.New("minicloudabi: headers must be an object")
	}
	normalized := make(map[string][]string, len(fields))
	totalBytes := 0
	for name, values := range fields {
		if !headerNamePattern.MatchString(name) {
			return nil, errors.New("minicloudabi: invalid header name")
		}
		lowerName := strings.ToLower(name)
		if name != lowerName {
			return nil, errors.New("minicloudabi: constructed header names must be lowercase")
		}
		if _, blocked := connectionHeaders[lowerName]; blocked {
			return nil, errors.New("minicloudabi: connection-level header is forbidden")
		}
		if values == nil {
			return nil, errors.New("minicloudabi: header values must be an array")
		}
		copied := make([]string, len(values))
		for index, value := range values {
			if !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
				return nil, errors.New("minicloudabi: invalid header value")
			}
			if len(value) > limits.HeaderValueBytes {
				return nil, errors.New("minicloudabi: header value exceeds size limit")
			}
			copied[index] = value
			totalBytes += len(value)
		}
		totalBytes += len(lowerName)
		normalized[lowerName] = copied
	}
	if len(normalized) > limits.HeaderCount || totalBytes > limits.HeaderBytes {
		return nil, errors.New("minicloudabi: headers exceed count or size limit")
	}
	return normalized, nil
}

func validateQuery(query Query, limits Limits) error {
	if query == nil {
		return errors.New("minicloudabi: query must be an object")
	}
	pairs := 0
	totalBytes := 0
	for key, values := range query {
		if !utf8.ValidString(key) || values == nil {
			return errors.New("minicloudabi: invalid query entry")
		}
		for _, value := range values {
			if !utf8.ValidString(value) {
				return errors.New("minicloudabi: invalid query value")
			}
			pairs++
			totalBytes += len(key) + len(value)
		}
	}
	if pairs > limits.QueryPairs || totalBytes > limits.QueryBytes {
		return errors.New("minicloudabi: query exceeds pair or size limit")
	}
	return nil
}

func validatePath(value string, maxBytes int) error {
	if value == "" || value[0] != '/' || len(value) > maxBytes || !utf8.ValidString(value) {
		return errors.New("minicloudabi: invalid path")
	}
	if strings.ContainsRune(value, '\x00') {
		return errors.New("minicloudabi: path contains nul")
	}
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return errors.New("minicloudabi: path contains invalid percent encoding")
	}
	if !utf8.ValidString(decoded) || strings.ContainsRune(decoded, '\x00') {
		return errors.New("minicloudabi: decoded path is not valid utf-8 or contains nul")
	}
	return nil
}

func validateTrigger(trigger Trigger) error {
	if !identifierPattern.MatchString(trigger.ID) {
		return errors.New("minicloudabi: invalid trigger id")
	}
	switch trigger.Type {
	case "http", "cron", "event":
		return nil
	default:
		return errors.New("minicloudabi: invalid trigger type")
	}
}

func validateMetadataSize(envelope any, maxBytes int) error {
	var metadata any
	switch value := envelope.(type) {
	case Request:
		value.Body = []byte{}
		metadata = value
	case Response:
		value.Body = []byte{}
		metadata = value
	default:
		return errors.New("minicloudabi: unsupported envelope metadata type")
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("minicloudabi: measuring metadata: %w", err)
	}
	if len(data) > maxBytes {
		return errors.New("minicloudabi: envelope metadata exceeds size limit")
	}
	return nil
}
