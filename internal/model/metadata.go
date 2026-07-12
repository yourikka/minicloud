// Package model defines versioned control-plane values and their deterministic
// validation rules. It contains no I/O, wall-clock reads, or random generation.
package model

import (
	"regexp"
	"time"

	"github.com/yourikka/minicloud/internal/problem"
)

const DefaultNamespace = "default"

var idPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Metadata is embedded by every persistent control-plane object.
type Metadata struct {
	ID               string    `json:"id"`
	Namespace        string    `json:"namespace"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	CreatedRaftIndex uint64    `json:"created_raft_index"`
	ResourceRevision uint64    `json:"resource_revision"`
}

// Validate checks the common persisted-object contract.
func (m Metadata) Validate() error {
	if !idPattern.MatchString(m.ID) {
		return problem.Invalid("id", "must contain 1 to 128 allowed characters")
	}
	if m.Namespace != DefaultNamespace {
		return problem.Invalid("namespace", "only default is supported in v1")
	}
	if m.CreatedAt.IsZero() {
		return problem.Invalid("created_at", "is required")
	}
	if !isUTC(m.CreatedAt) {
		return problem.Invalid("created_at", "must use UTC")
	}
	if m.UpdatedAt.IsZero() {
		return problem.Invalid("updated_at", "is required")
	}
	if !isUTC(m.UpdatedAt) {
		return problem.Invalid("updated_at", "must use UTC")
	}
	if m.UpdatedAt.Before(m.CreatedAt) {
		return problem.Invalid("updated_at", "must not precede created_at")
	}
	if m.CreatedRaftIndex == 0 {
		return problem.Invalid("created_raft_index", "must be greater than zero")
	}
	if m.ResourceRevision == 0 {
		return problem.Invalid("resource_revision", "must be greater than zero")
	}
	return nil
}

func isUTC(value time.Time) bool {
	_, offset := value.Zone()
	return offset == 0
}
