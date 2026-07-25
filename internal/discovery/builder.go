package discovery

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
)

const maxEndpointAddressBytes = 512

var (
	identifierPattern   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	functionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	hostnamePattern     = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}$`)
	httpTokenPattern    = regexp.MustCompile(`^[!#$%&'*+.^_` + "`" + `|~0-9A-Za-z-]+$`)
)

// Builder creates complete snapshots without retaining caller-owned memory.
type Builder struct {
	maxEndpoints int
}

// New validates the configured endpoint bound.
func New(config Config) (*Builder, error) {
	if config.MaxEndpoints == 0 {
		config.MaxEndpoints = DefaultMaxEndpoints
	}
	if config.MaxEndpoints < 1 || config.MaxEndpoints > HardMaxEndpoints {
		return nil, errors.New("discovery endpoint limit is outside v1 bounds")
	}
	return &Builder{maxEndpoints: config.MaxEndpoints}, nil
}

// Build validates one complete serving view, filters Endpoint candidates, and
// computes its domain-separated canonical checksum.
func (b *Builder) Build(input Input) (Result, error) {
	if b == nil {
		return Result{}, errors.New("discovery builder is nil")
	}
	if len(input.Candidates) > HardMaxEndpoints {
		return Result{}, &problem.Error{Code: problem.CodeOverloaded, Message: "endpoint candidate limit exceeded"}
	}
	snapshot := Snapshot{
		SchemaVersion:   ServingSchemaVersion,
		FunctionID:      input.Function.ID,
		DiscoveryEpoch:  input.DiscoveryEpoch,
		ServingSequence: input.ServingSequence,
		GeneratedAt:     input.GeneratedAt.Round(0),
		Function:        cloneFunction(input.Function),
		Trigger:         cloneHTTPTrigger(input.Trigger),
		Route:           cloneRoute(input.Route),
		Endpoints:       make([]Endpoint, 0),
	}
	if err := validateSnapshotHeader(snapshot); err != nil {
		return Result{}, err
	}
	if err := validateFunction(snapshot.Function); err != nil {
		return Result{}, err
	}
	if err := validateHTTPTrigger(snapshot.Trigger, snapshot.FunctionID); err != nil {
		return Result{}, err
	}
	if err := validateRoute(snapshot.Route, snapshot.FunctionID); err != nil {
		return Result{}, err
	}
	slices.SortFunc(snapshot.Route.Targets, compareRouteTargets)

	candidates := slices.Clone(input.Candidates)
	slices.SortFunc(candidates, compareCandidates)
	decisions := make([]CandidateDecision, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		assignmentID := candidate.Assignment.AssignmentID
		if !identifierPattern.MatchString(assignmentID) {
			return Result{}, problem.Invalid("candidates.assignment_id", "must be a valid identifier")
		}
		if _, exists := seen[assignmentID]; exists {
			return Result{}, problem.Invalid("candidates.assignment_id", "contains a duplicate Assignment ID")
		}
		seen[assignmentID] = struct{}{}
		reasons := exclusionReasons(snapshot, candidate)
		included := len(reasons) == 0
		decisions = append(decisions, CandidateDecision{
			AssignmentID: assignmentID,
			Included:     included,
			Reasons:      slices.Clone(reasons),
		})
		if included {
			if len(snapshot.Endpoints) >= b.maxEndpoints {
				return Result{}, &problem.Error{Code: problem.CodeOverloaded, Message: "serving endpoint limit exceeded"}
			}
			snapshot.Endpoints = append(snapshot.Endpoints, Endpoint{
				Assignment: candidate.Assignment,
				Address:    candidate.Address,
				State:      EndpointReady,
			})
		}
	}
	slices.SortFunc(snapshot.Endpoints, compareEndpoints)
	checksum, err := calculateChecksum(snapshot)
	if err != nil {
		return Result{}, err
	}
	snapshot.Checksum = checksum
	if err := snapshot.Validate(); err != nil {
		return Result{}, fmt.Errorf("validating built serving snapshot: %w", err)
	}
	return Result{Snapshot: snapshot.Clone(), Candidates: cloneDecisions(decisions)}, nil
}

func exclusionReasons(snapshot Snapshot, candidate EndpointCandidate) []ExclusionReason {
	reasons := make([]ExclusionReason, 0, 8)
	if snapshot.Function.Lifecycle != model.FunctionActive {
		reasons = append(reasons, ReasonFunctionNotActive)
	}
	if !snapshot.Trigger.Enabled {
		reasons = append(reasons, ReasonTriggerDisabled)
	}
	if !snapshot.Route.Present || !snapshot.Route.Enabled {
		reasons = append(reasons, ReasonRouteDisabled)
	}
	if err := candidate.Assignment.Validate(); err != nil {
		reasons = append(reasons, ReasonInvalidAssignment)
	}
	if candidate.DesiredState != AssignmentAssigned {
		reasons = append(reasons, ReasonAssignmentNotAssigned)
	}
	if candidate.Authorization == nil {
		reasons = append(reasons, ReasonAuthorizationNotInstalled)
	} else if err := candidate.Authorization.Validate(servingauth.HardMaxTTL); err != nil ||
		candidate.Authorization.Fence.Assignment != candidate.Assignment ||
		candidate.Authorization.Fence.DiscoveryEpoch != snapshot.DiscoveryEpoch {
		reasons = append(reasons, ReasonAuthorizationMismatch)
	}
	if candidate.Assignment.Mode != servingauth.ModeNormal {
		reasons = append(reasons, ReasonAssignmentModeNotNormal)
	}
	if err := candidate.Worker.Validate(); err != nil {
		reasons = append(reasons, ReasonInvalidWorkerObservation)
	}
	if candidate.Worker.Session != candidate.Assignment.Worker {
		reasons = append(reasons, ReasonWorkerSessionMismatch)
	}
	if candidate.Worker.Intent != scheduler.IntentSchedulable {
		reasons = append(reasons, ReasonWorkerNotSchedulable)
	}
	if candidate.Worker.State != scheduler.SessionReady {
		reasons = append(reasons, ReasonWorkerNotReady)
	}
	if candidate.Worker.Drain != scheduler.DrainNotDraining {
		reasons = append(reasons, ReasonWorkerDraining)
	}
	if !candidate.ReplicaReady {
		reasons = append(reasons, ReasonReplicaNotReady)
	}
	reasons = append(reasons, routeMismatchReasons(snapshot.Route.Targets, candidate.Assignment)...)
	if !validEndpointAddress(candidate.Address) {
		reasons = append(reasons, ReasonInvalidAddress)
	}
	return reasons
}

func routeMismatchReasons(targets []model.RouteTarget, assignment servingauth.AssignmentIdentity) []ExclusionReason {
	var versionFound, generationFound bool
	var target model.RouteTarget
	for _, candidate := range targets {
		if candidate.VersionID != assignment.VersionID {
			continue
		}
		versionFound = true
		if candidate.DeploymentGeneration == assignment.DeploymentGeneration {
			generationFound = true
			target = candidate
			break
		}
	}
	if !versionFound {
		return []ExclusionReason{ReasonRouteTargetMissing}
	}
	if !generationFound {
		return []ExclusionReason{ReasonDeploymentGenerationMismatch}
	}
	reasons := make([]ExclusionReason, 0, 2)
	if target.AdmissionEpoch != assignment.AdmissionEpoch {
		reasons = append(reasons, ReasonAdmissionEpochMismatch)
	}
	if target.EffectivePolicyDigest != assignment.PolicyDigest {
		reasons = append(reasons, ReasonPolicyDigestMismatch)
	}
	return reasons
}

func validEndpointAddress(address string) bool {
	if address == "" || len(address) > maxEndpointAddressBytes || !utf8.ValidString(address) || strings.TrimSpace(address) != address {
		return false
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return false
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if !hostnamePattern.MatchString(host) {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}

func compareCandidates(left, right EndpointCandidate) int {
	return compareEndpointIdentity(left.Assignment, right.Assignment)
}
