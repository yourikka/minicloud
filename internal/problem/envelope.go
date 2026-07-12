package problem

// Envelope is the versioned shape of every non-streaming public API error.
type Envelope struct {
	Error ResponseError `json:"error"`
}

// ResponseError contains only stable and safe public error information.
type ResponseError struct {
	Code      Code           `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Details   map[string]any `json:"details,omitempty"`
}

// NewEnvelope translates a classified error into the public API contract.
func NewEnvelope(classified *Error, requestID string, details map[string]any) Envelope {
	publicDetails := make(map[string]any, len(details)+1)
	for key, value := range details {
		publicDetails[key] = value
	}
	if classified.Field != "" {
		publicDetails["field"] = classified.Field
	}

	return Envelope{
		Error: ResponseError{
			Code:      classified.Code,
			Message:   classified.Message,
			RequestID: requestID,
			Details:   publicDetails,
		},
	}
}
