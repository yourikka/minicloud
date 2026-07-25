package controlplane

import (
	"errors"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
)

// PublishRouteCommand atomically replaces one Function's complete current
// Route. Zero ExpectedActiveRouteRevision is the explicit first-publish CAS.
type PublishRouteCommand struct {
	FunctionID                  string      `json:"function_id"`
	ExpectedActiveRouteRevision uint64      `json:"expected_active_route_revision"`
	Route                       model.Route `json:"route"`
	UpdatedAt                   time.Time   `json:"updated_at"`
	AppliedIndex                uint64      `json:"applied_index"`
}

// RouteStore retains only the current Route in Local Core. Route history,
// pinning, rollback eligibility, and GC belong to a later feature slice.
type RouteStore struct {
	catalog  *Catalog
	releases *ReleaseStore

	mu       sync.Mutex
	routes   map[string]model.Route
	routeIDs map[string]struct{}
}

// NewRouteStore binds the current Route state to its Function and Version
// authorities. All cross-store writes use Catalog -> Release -> Route locking.
func NewRouteStore(catalog *Catalog, releases *ReleaseStore) *RouteStore {
	return &RouteStore{
		catalog:  catalog,
		releases: releases,
		routes:   make(map[string]model.Route),
		routeIDs: make(map[string]struct{}),
	}
}

// Publish validates the complete Route and atomically advances both the Route
// revision and Function active-route pointer.
func (s *RouteStore) Publish(command PublishRouteCommand) (model.Route, model.Function, error) {
	if s == nil || s.catalog == nil || s.releases == nil {
		return model.Route{}, model.Function{}, errors.New("control-plane route store dependencies are required")
	}
	if err := validatePublishRoute(command); err != nil {
		return model.Route{}, model.Function{}, err
	}

	// This ordering is the Local Core aggregate transaction. Do not call public
	// Catalog or ReleaseStore methods here because they would split the check and
	// write into observable intermediate states.
	s.catalog.mu.Lock()
	s.releases.mu.Lock()
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.releases.mu.Unlock()
	defer s.catalog.mu.Unlock()

	function, exists := s.catalog.functionsByID[command.FunctionID]
	if !exists {
		return model.Route{}, model.Function{}, classified(problem.CodeNotFound, "function was not found")
	}
	if command.ExpectedActiveRouteRevision != function.ActiveRouteRevision {
		return model.Route{}, model.Function{}, revisionConflict(
			"active_route_revision",
			command.ExpectedActiveRouteRevision,
			function.ActiveRouteRevision,
		)
	}
	if function.ActiveRouteRevision == math.MaxUint64 || function.ResourceRevision == math.MaxUint64 {
		return model.Route{}, model.Function{}, classified(problem.CodeConflict, "function revision space is exhausted")
	}
	if command.Route.RouteRevision != function.ActiveRouteRevision+1 {
		return model.Route{}, model.Function{}, problem.Invalid("route.route_revision", "must advance the active route revision by one")
	}
	if command.UpdatedAt.Before(function.UpdatedAt) {
		return model.Route{}, model.Function{}, problem.Invalid("updated_at", "must not precede the current function update time")
	}
	if _, exists := s.routeIDs[command.Route.ID]; exists {
		return model.Route{}, model.Function{}, classified(problem.CodeConflict, "route id was already used")
	}
	if command.Route.Enabled {
		if function.Lifecycle != model.FunctionActive {
			return model.Route{}, model.Function{}, classified(problem.CodeConflict, "only an active function may publish an enabled route")
		}
		if err := s.validateReadyTargetLocked(command.Route, function); err != nil {
			return model.Route{}, model.Function{}, err
		}
	}

	route := cloneControlRoute(command.Route)
	function.ActiveRouteRevision = route.RouteRevision
	function.UpdatedAt = command.UpdatedAt.Round(0)
	function.ResourceRevision++
	if err := function.Validate(); err != nil {
		return model.Route{}, model.Function{}, err
	}
	s.routes[function.ID] = route
	s.routeIDs[route.ID] = struct{}{}
	s.catalog.functionsByID[function.ID] = function
	return cloneControlRoute(route), cloneFunction(function), nil
}

// Get returns the current complete Route for one Function.
func (s *RouteStore) Get(functionID string) (model.Route, error) {
	if s == nil {
		return model.Route{}, errors.New("control-plane route store is nil")
	}
	if !identifierPattern.MatchString(functionID) {
		return model.Route{}, problem.Invalid("function_id", "must be a valid identifier")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	route, exists := s.routes[functionID]
	if !exists {
		return model.Route{}, classified(problem.CodeNotFound, "route was not found")
	}
	return cloneControlRoute(route), nil
}

// Snapshot returns all current Routes ordered by Function ID.
func (s *RouteStore) Snapshot() []model.Route {
	if s == nil {
		return []model.Route{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	routes := make([]model.Route, 0, len(s.routes))
	for _, route := range s.routes {
		routes = append(routes, cloneControlRoute(route))
	}
	slices.SortFunc(routes, func(left, right model.Route) int {
		if left.FunctionID < right.FunctionID {
			return -1
		}
		if left.FunctionID > right.FunctionID {
			return 1
		}
		if left.RouteRevision < right.RouteRevision {
			return -1
		}
		if left.RouteRevision > right.RouteRevision {
			return 1
		}
		return 0
	})
	return routes
}

func cloneControlRoute(route model.Route) model.Route {
	route.Targets = slices.Clone(route.Targets)
	route.Salt = slices.Clone(route.Salt)
	return route
}

func validatePublishRoute(command PublishRouteCommand) error {
	if !identifierPattern.MatchString(command.FunctionID) || command.Route.FunctionID != command.FunctionID {
		return problem.Invalid("function_id", "must match a valid route function id")
	}
	if command.AppliedIndex == 0 {
		return problem.Invalid("applied_index", "must be greater than zero")
	}
	if err := validateUTC("updated_at", command.UpdatedAt); err != nil {
		return err
	}
	if err := command.Route.ValidateCore(); err != nil {
		return err
	}
	if command.Route.CreatedRaftIndex != command.AppliedIndex || command.Route.ResourceRevision != 1 ||
		!command.Route.CreatedAt.Equal(command.UpdatedAt) || !command.Route.UpdatedAt.Equal(command.UpdatedAt) {
		return problem.Invalid("route", "must be a new immutable snapshot from this command")
	}
	return nil
}

func (s *RouteStore) validateReadyTargetLocked(route model.Route, function model.Function) error {
	target := route.Targets[0]
	version, exists := s.releases.versions[target.VersionID]
	if !exists || version.FunctionID != function.ID || version.State != model.VersionReady ||
		version.AdmissionEpoch != target.AdmissionEpoch {
		return classified(problem.CodeConflict, "route target is not a ready version for this function")
	}
	deployment, exists := s.releases.deployments[target.VersionID]
	if !exists || deployment.Generation != target.DeploymentGeneration || deployment.DesiredPhase != model.DeploymentActive ||
		deployment.EffectivePolicyDigest != target.EffectivePolicyDigest {
		return classified(problem.CodeConflict, "route target does not match an active deployment policy")
	}
	return nil
}
