// Package localserving projects consistent Local Controller state into the
// same complete serving snapshots consumed by a standalone Gateway.
package localserving

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/gatewaydiscovery"
)

// StateSource returns one consistent aggregate read of every Function's
// serving inputs. localcontroller.Controller satisfies this interface.
type StateSource interface {
	ListServingStates(context.Context) ([]controlplane.ServingState, error)
}

// CandidateSource returns current Worker candidates for one aggregate state.
// Implementations must bind any Authorization to the supplied Discovery Epoch.
type CandidateSource interface {
	Candidates(context.Context, controlplane.ServingState, uint64) ([]discovery.EndpointCandidate, error)
}

// Config supplies the trusted Local Core serving components.
type Config struct {
	States     StateSource
	Candidates CandidateSource
	Publisher  *discovery.Publisher
	Store      *gatewaydiscovery.Store
	Now        func() time.Time
}

// Synchronizer serializes complete serving publications so an older Full Sync
// cannot race a newer batch into the local Gateway Store.
type Synchronizer struct {
	states     StateSource
	candidates CandidateSource
	publisher  *discovery.Publisher
	store      *gatewaydiscovery.Store
	now        func() time.Time

	mu sync.Mutex
}

// New validates the serving dependencies. A nil CandidateSource publishes no
// Endpoints, which keeps invocation fail-closed while Worker state is absent.
func New(config Config) (*Synchronizer, error) {
	if config.States == nil {
		return nil, errors.New("local serving state source is required")
	}
	if config.Publisher == nil {
		return nil, errors.New("local serving publisher is required")
	}
	if config.Store == nil {
		return nil, errors.New("local serving gateway store is required")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Synchronizer{
		states:     config.States,
		candidates: config.Candidates,
		publisher:  config.Publisher,
		store:      config.Store,
		now:        config.Now,
	}, nil
}

// FullSync builds and atomically applies one complete batch. Every Function in
// the batch receives the same Epoch, Sequence, and generated-at timestamp.
func (s *Synchronizer) FullSync(ctx context.Context) error {
	if ctx == nil {
		return errors.New("local serving sync context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.states == nil || s.publisher == nil || s.store == nil || s.now == nil {
		return errors.New("local serving sync dependencies are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	states, err := s.states.ListServingStates(ctx)
	if err != nil {
		return fmt.Errorf("listing local serving states: %w", err)
	}
	epoch, _ := s.publisher.Position()
	generatedAt := s.now().Round(0)
	inputs := make([]discovery.Input, 0, len(states))
	for _, state := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		input := projectInput(state, generatedAt)
		candidates, err := s.endpointCandidates(ctx, state, epoch)
		if err != nil {
			return fmt.Errorf("listing candidates for function %q: %w", state.Function.ID, err)
		}
		input.Candidates = slices.Clone(candidates)
		inputs = append(inputs, input)
	}

	results, sequence, err := s.publisher.BuildFull(inputs)
	if err != nil {
		return fmt.Errorf("building local serving Full Sync: %w", err)
	}
	snapshots := make([]discovery.Snapshot, len(results))
	for index, result := range results {
		snapshots[index] = result.Snapshot
	}
	if err := s.store.Apply(gatewaydiscovery.Event{
		Full:            true,
		DiscoveryEpoch:  epoch,
		ServingSequence: sequence,
		Snapshots:       snapshots,
	}); err != nil {
		return fmt.Errorf("applying local serving Full Sync: %w", err)
	}
	return nil
}

func (s *Synchronizer) endpointCandidates(
	ctx context.Context,
	state controlplane.ServingState,
	epoch uint64,
) ([]discovery.EndpointCandidate, error) {
	if s.candidates == nil {
		return []discovery.EndpointCandidate{}, nil
	}
	candidates, err := s.candidates.Candidates(ctx, state, epoch)
	if err != nil {
		return nil, err
	}
	return slices.Clone(candidates), nil
}

func projectInput(
	state controlplane.ServingState,
	generatedAt time.Time,
) discovery.Input {
	verifier := state.Trigger.TokenVerifierDigest
	if verifier != nil {
		value := *verifier
		verifier = &value
	}
	return discovery.Input{
		GeneratedAt: generatedAt,
		Function: discovery.Function{
			ID:               state.Function.ID,
			Name:             state.Function.Name,
			ResourceRevision: state.Function.ResourceRevision,
			Lifecycle:        state.Function.Lifecycle,
		},
		Trigger: discovery.HTTPTrigger{
			ID:                  state.Trigger.ID,
			FunctionID:          state.Trigger.FunctionID,
			ResourceRevision:    state.Trigger.ResourceRevision,
			Enabled:             state.Trigger.Enabled,
			AuthPolicy:          discovery.AuthPolicy(state.Trigger.AuthPolicy),
			TokenVerifierDigest: verifier,
		},
		Route:      projectRoute(state),
		Candidates: []discovery.EndpointCandidate{},
	}
}

func projectRoute(state controlplane.ServingState) discovery.Route {
	if state.Route == nil {
		return discovery.Route{FunctionID: state.Function.ID}
	}
	route := state.Route
	return discovery.Route{
		Present:          true,
		FunctionID:       route.FunctionID,
		ResourceRevision: route.ResourceRevision,
		RouteRevision:    route.RouteRevision,
		Enabled:          route.Enabled,
		Targets:          slices.Clone(route.Targets),
		Affinity:         route.Affinity,
		AffinityHeader:   route.AffinityHeader,
		HashVersion:      route.HashVersion,
		SaltID:           route.SaltID,
		Salt:             slices.Clone(route.Salt),
	}
}
