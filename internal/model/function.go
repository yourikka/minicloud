package model

import (
	"regexp"
	"unicode/utf8"

	"github.com/yourikka/minicloud/internal/problem"
)

const (
	maxDescriptionBytes = 512
	maxLabels           = 32
	maxLabelValueBytes  = 256
)

var (
	functionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	labelKeyPattern     = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
)

// FunctionLifecycle is the persisted desired lifecycle of a Function.
type FunctionLifecycle string

const (
	FunctionActive     FunctionLifecycle = "Active"
	FunctionDisabled   FunctionLifecycle = "Disabled"
	FunctionDeleting   FunctionLifecycle = "Deleting"
	FunctionTombstoned FunctionLifecycle = "Tombstoned"
)

// Function is the stable logical identity addressed by invocation routes.
type Function struct {
	Metadata
	Name                string            `json:"name"`
	Description         string            `json:"description,omitempty"`
	ActiveRouteRevision uint64            `json:"active_route_revision"`
	Labels              map[string]string `json:"labels"`
	Lifecycle           FunctionLifecycle `json:"lifecycle"`
}

// Validate checks Function invariants at command and restore boundaries.
func (f Function) Validate() error {
	if err := f.Metadata.Validate(); err != nil {
		return err
	}
	if !functionNamePattern.MatchString(f.Name) {
		return problem.Invalid("name", "must match the function name syntax")
	}
	if !utf8.ValidString(f.Description) {
		return problem.Invalid("description", "must be valid UTF-8")
	}
	if len(f.Description) > maxDescriptionBytes {
		return problem.Invalid("description", "must not exceed 512 bytes")
	}
	if len(f.Labels) > maxLabels {
		return problem.Invalid("labels", "must not contain more than 32 entries")
	}
	for key, value := range f.Labels {
		if !labelKeyPattern.MatchString(key) {
			return problem.Invalid("labels", "contains an invalid key")
		}
		if !utf8.ValidString(value) {
			return problem.Invalid("labels", "contains a value that is not valid UTF-8")
		}
		if len(value) > maxLabelValueBytes {
			return problem.Invalid("labels", "contains a value longer than 256 bytes")
		}
	}
	if !f.Lifecycle.valid() {
		return problem.Invalid("lifecycle", "is not a supported function lifecycle")
	}
	return nil
}

func (l FunctionLifecycle) valid() bool {
	switch l {
	case FunctionActive, FunctionDisabled, FunctionDeleting, FunctionTombstoned:
		return true
	default:
		return false
	}
}
