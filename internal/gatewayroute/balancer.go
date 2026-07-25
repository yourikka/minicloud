// Package gatewayroute selects one Endpoint only after the Route target is
// fixed. It never falls back across weighted Route targets.
package gatewayroute

import (
	"errors"
	"regexp"
	"slices"
	"sync"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/servingauth"
)

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Balancer is safe for concurrent invocation selection and completion.
type Balancer struct {
	mu         sync.Mutex
	inflight   map[servingauth.AssignmentIdentity]uint64
	roundRobin map[targetKey]uint64
}

type targetKey struct {
	functionID string
	versionID  string
	generation uint64
}

type releaseState struct {
	once     sync.Once
	balancer *Balancer
	identity servingauth.AssignmentIdentity
}

// Lease identifies one selected Endpoint and releases its local inflight count
// at most once, even when the Lease value is copied.
type Lease struct {
	Endpoint discovery.Endpoint
	state    *releaseState
}

// New creates an empty local load balancer.
func New() *Balancer {
	return &Balancer{
		inflight:   make(map[servingauth.AssignmentIdentity]uint64),
		roundRobin: make(map[targetKey]uint64),
	}
}

// Acquire chooses the least-inflight Endpoint for exactly target. Ties use a
// stable Round-robin cursor over canonical Assignment order.
func (b *Balancer) Acquire(
	functionID string,
	target model.RouteTarget,
	endpoints []discovery.Endpoint,
) (Lease, error) {
	if b == nil {
		return Lease{}, errors.New("gateway route balancer is nil")
	}
	if !identifierPattern.MatchString(functionID) {
		return Lease{}, problem.Invalid("function_id", "must be a valid identifier")
	}
	if err := validateTarget(target); err != nil {
		return Lease{}, err
	}
	eligible := make([]discovery.Endpoint, 0, len(endpoints))
	seen := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		identity := endpoint.Assignment
		if err := endpoint.Validate(); err != nil {
			return Lease{}, err
		}
		if endpoint.State != discovery.EndpointReady || identity.Mode != servingauth.ModeNormal ||
			identity.VersionID != target.VersionID ||
			identity.AdmissionEpoch != target.AdmissionEpoch ||
			identity.DeploymentGeneration != target.DeploymentGeneration ||
			identity.PolicyDigest != target.EffectivePolicyDigest {
			continue
		}
		if _, exists := seen[identity.AssignmentID]; exists {
			return Lease{}, problem.Invalid("endpoints", "contains a duplicate Assignment ID")
		}
		seen[identity.AssignmentID] = struct{}{}
		eligible = append(eligible, endpoint)
	}
	if len(eligible) == 0 {
		return Lease{}, classified(problem.CodeNoReadyReplica, "selected Route target has no ready Endpoint")
	}
	slices.SortFunc(eligible, compareEndpoints)
	key := targetKey{functionID: functionID, versionID: target.VersionID, generation: target.DeploymentGeneration}
	b.mu.Lock()
	if b.inflight == nil {
		b.inflight = make(map[servingauth.AssignmentIdentity]uint64)
	}
	if b.roundRobin == nil {
		b.roundRobin = make(map[targetKey]uint64)
	}
	minimum := b.inflight[eligible[0].Assignment]
	for _, endpoint := range eligible[1:] {
		minimum = min(minimum, b.inflight[endpoint.Assignment])
	}
	tied := make([]discovery.Endpoint, 0, len(eligible))
	for _, endpoint := range eligible {
		if b.inflight[endpoint.Assignment] == minimum {
			tied = append(tied, endpoint)
		}
	}
	cursor := b.roundRobin[key]
	chosen := tied[cursor%uint64(len(tied))]
	b.roundRobin[key] = cursor + 1
	b.inflight[chosen.Assignment]++
	b.mu.Unlock()
	return Lease{
		Endpoint: chosen,
		state:    &releaseState{balancer: b, identity: chosen.Assignment},
	}, nil
}

// Release decrements local inflight occupancy at most once.
func (l Lease) Release() {
	if l.state == nil {
		return
	}
	l.state.once.Do(func() {
		b := l.state.balancer
		b.mu.Lock()
		count := b.inflight[l.state.identity]
		if count <= 1 {
			delete(b.inflight, l.state.identity)
		} else {
			b.inflight[l.state.identity] = count - 1
		}
		b.mu.Unlock()
	})
}

// Inflight reports local occupancy for diagnostics and tests.
func (b *Balancer) Inflight(identity servingauth.AssignmentIdentity) uint64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inflight[identity]
}

func compareEndpoints(left, right discovery.Endpoint) int {
	if left.Assignment.AssignmentID < right.Assignment.AssignmentID {
		return -1
	}
	if left.Assignment.AssignmentID > right.Assignment.AssignmentID {
		return 1
	}
	return 0
}

func validateTarget(target model.RouteTarget) error {
	if !identifierPattern.MatchString(target.VersionID) {
		return problem.Invalid("route_target.version_id", "must be a valid identifier")
	}
	if target.AdmissionEpoch == 0 || target.DeploymentGeneration == 0 ||
		target.WeightBasisPoints == 0 || target.WeightBasisPoints > model.TotalRouteWeightBasisPoints {
		return problem.Invalid("route_target", "contains an invalid epoch, generation, or weight")
	}
	if _, err := digest.ParseSHA256(target.EffectivePolicyDigest.String()); err != nil {
		return problem.Invalid("route_target.effective_policy_digest", "must be a lowercase SHA-256 digest")
	}
	return nil
}

func classified(code problem.Code, message string) error {
	return &problem.Error{Code: code, Message: message}
}
