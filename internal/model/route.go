package model

import (
	"regexp"
	"strconv"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/problem"
)

const (
	TotalRouteWeightBasisPoints = uint32(10_000)
	MaxRouteTargets             = 32
	HashVersionSHA256BPSV1      = "sha256-bps-v1"
)

var httpTokenPattern = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)

// AffinitySource selects the stable input used by Route hashing.
type AffinitySource string

const (
	AffinityRequestID      AffinitySource = "request_id"
	AffinityIdempotencyKey AffinitySource = "idempotency_key"
	AffinityHeader         AffinitySource = "header"
)

// RouteTarget binds routing to one admitted execution-policy generation.
type RouteTarget struct {
	VersionID             string        `json:"version_id"`
	AdmissionEpoch        uint64        `json:"admission_epoch"`
	DeploymentGeneration  uint64        `json:"deployment_generation"`
	EffectivePolicyDigest digest.SHA256 `json:"effective_policy_digest"`
	WeightBasisPoints     uint32        `json:"weight_basis_points"`
}

// Route is an immutable complete routing snapshot for a Function revision.
type Route struct {
	Metadata
	FunctionID     string         `json:"function_id"`
	RouteRevision  uint64         `json:"route_revision"`
	Targets        []RouteTarget  `json:"targets"`
	Affinity       AffinitySource `json:"affinity_source"`
	AffinityHeader string         `json:"affinity_header,omitempty"`
	HashVersion    string         `json:"hash_version"`
	SaltID         string         `json:"salt_id"`
	Salt           []byte         `json:"salt"`
	Enabled        bool           `json:"enabled"`
}

// Validate checks the full v1 Route representation. Multi-target routes are
// structurally valid here even though Core mode applies an additional gate.
func (r Route) Validate() error {
	if err := r.Metadata.Validate(); err != nil {
		return err
	}
	if !idPattern.MatchString(r.FunctionID) {
		return problem.Invalid("function_id", "must be a valid identifier")
	}
	if r.RouteRevision == 0 {
		return problem.Invalid("route_revision", "must be greater than zero")
	}
	if len(r.Targets) > MaxRouteTargets {
		return problem.Invalid("targets", "must not contain more than 32 entries")
	}
	if !r.Enabled && len(r.Targets) != 0 {
		return problem.Invalid("targets", "must be empty when the route is disabled")
	}
	if r.Enabled && len(r.Targets) == 0 {
		return problem.Invalid("targets", "must not be empty when the route is enabled")
	}
	if err := validateRouteTargets(r.Targets); err != nil {
		return err
	}
	if err := r.validateAffinity(); err != nil {
		return err
	}
	if r.HashVersion != HashVersionSHA256BPSV1 {
		return problem.Invalid("hash_version", "is not supported")
	}
	if !idPattern.MatchString(r.SaltID) {
		return problem.Invalid("salt_id", "must be a valid identifier")
	}
	if len(r.Salt) != 16 {
		return problem.Invalid("salt", "must contain exactly 128 bits")
	}
	return nil
}

// ValidateCore enforces the P0 single-target feature gate.
func (r Route) ValidateCore() error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Enabled && len(r.Targets) != 1 {
		return problem.Invalid("targets", "core mode requires exactly one target")
	}
	return nil
}

func validateRouteTargets(targets []RouteTarget) error {
	var total uint32
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if !idPattern.MatchString(target.VersionID) {
			return problem.Invalid("targets", "contains an invalid version_id")
		}
		if target.AdmissionEpoch == 0 || target.DeploymentGeneration == 0 {
			return problem.Invalid("targets", "contains a zero epoch or generation")
		}
		if _, err := digest.ParseSHA256(target.EffectivePolicyDigest.String()); err != nil {
			return problem.Invalid("targets", "contains an invalid policy digest")
		}
		if target.WeightBasisPoints == 0 || target.WeightBasisPoints > TotalRouteWeightBasisPoints {
			return problem.Invalid("targets", "contains an invalid weight")
		}
		key := target.VersionID + "\x00" + strconv.FormatUint(target.DeploymentGeneration, 10)
		if _, exists := seen[key]; exists {
			return problem.Invalid("targets", "contains a duplicate target")
		}
		seen[key] = struct{}{}
		total += target.WeightBasisPoints
	}
	if len(targets) > 0 && total != TotalRouteWeightBasisPoints {
		return problem.Invalid("targets", "weights must total 10000 basis points")
	}
	return nil
}

func (r Route) validateAffinity() error {
	switch r.Affinity {
	case AffinityRequestID, AffinityIdempotencyKey:
		if r.AffinityHeader != "" {
			return problem.Invalid("affinity_header", "is only valid for header affinity")
		}
	case AffinityHeader:
		if !httpTokenPattern.MatchString(r.AffinityHeader) {
			return problem.Invalid("affinity_header", "must be a valid HTTP field name")
		}
	default:
		return problem.Invalid("affinity_source", "is not supported")
	}
	return nil
}
