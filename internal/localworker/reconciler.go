package localworker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/workeragent"
	"github.com/yourikka/minicloud/internal/workercache"
)

// AssignmentSource returns committed desired Assignment intent.
type AssignmentSource interface {
	ListAssignments(context.Context) ([]controlplane.AssignmentRecord, error)
}

// ServingStateSource returns consistent Function execution inputs.
type ServingStateSource interface {
	ListServingStates(context.Context) ([]controlplane.ServingState, error)
}

// WorkerSource returns the current registry-derived Worker snapshot.
type WorkerSource interface {
	WorkerSnapshot(context.Context) (scheduler.WorkerSnapshot, error)
}

// Agent is the Worker control surface consumed by the local reconciler.
type Agent interface {
	AcceptControl(servingauth.ControlConnection) error
	Prepare(context.Context, workeragent.PrepareRequest) (workeragent.Observation, error)
	InstallAuthorization(servingauth.ControlConnection, servingauth.Authorization) error
	Cancel(workeragent.CancelRequest) error
	AcknowledgeTerminal(servingauth.ControlConnection, string) error
	Inventory() workeragent.Inventory
}

// ReconcilerConfig binds one committed Worker session to local desired state.
type ReconcilerConfig struct {
	Assignments AssignmentSource
	States      ServingStateSource
	Workers     WorkerSource
	Agent       Agent
	Connection  servingauth.ControlConnection
	Address     string
}

// Reconciler converges one local Worker Agent and exposes a read-only serving
// candidate view. Reconcile is serialized while candidate reads stay concurrent.
type Reconciler struct {
	assignments AssignmentSource
	states      ServingStateSource
	workers     WorkerSource
	agent       Agent
	connection  servingauth.ControlConnection
	address     string

	reconcileMu sync.Mutex
	candidateMu sync.RWMutex
	candidates  map[string]installedCandidate
}

type installedCandidate struct {
	functionID    string
	authorization servingauth.Authorization
}

// NewReconciler validates local dependencies without performing Worker I/O.
func NewReconciler(config ReconcilerConfig) (*Reconciler, error) {
	if config.Assignments == nil || config.States == nil || config.Workers == nil || config.Agent == nil {
		return nil, errors.New("local worker reconciler dependencies are required")
	}
	if config.Address == "" {
		return nil, errors.New("local worker reconciler endpoint address is required")
	}
	return &Reconciler{
		assignments: config.Assignments,
		states:      config.States,
		workers:     config.Workers,
		agent:       config.Agent,
		connection:  config.Connection,
		address:     config.Address,
		candidates:  make(map[string]installedCandidate),
	}, nil
}

// Reconcile prepares Assigned intent, installs live-only authorization, and
// drains Cancelled intent. It never creates or changes control-plane intent.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	if ctx == nil {
		return errors.New("local worker reconcile context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil || r.assignments == nil || r.states == nil || r.workers == nil || r.agent == nil {
		return errors.New("local worker reconciler dependencies are required")
	}

	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	assignments, err := r.assignments.ListAssignments(ctx)
	if err != nil {
		return fmt.Errorf("listing assignment intent: %w", err)
	}
	states, err := r.states.ListServingStates(ctx)
	if err != nil {
		return fmt.Errorf("listing worker serving state: %w", err)
	}
	worker, err := r.workers.WorkerSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("loading worker snapshot: %w", err)
	}
	if err := validateWorkerControl(worker, r.connection); err != nil {
		return err
	}
	if err := r.agent.AcceptControl(r.connection); err != nil {
		return fmt.Errorf("accepting local worker control: %w", err)
	}

	statesByFunction, err := indexServingStates(states)
	if err != nil {
		return err
	}
	r.removeUnpublishableCandidates(assignments, worker.Session)
	observations := indexObservations(r.agent.Inventory())
	reconcileErrors := []error{}
	for _, assignment := range assignments {
		if err := ctx.Err(); err != nil {
			return errors.Join(errors.Join(reconcileErrors...), err)
		}
		if assignment.Placement.Worker != worker.Session {
			continue
		}
		switch assignment.DesiredState {
		case controlplane.AssignmentAssigned:
			state, exists := statesByFunction[assignment.FunctionID]
			if !exists {
				r.removeCandidate(assignment.Placement.AssignmentID)
				reconcileErrors = append(reconcileErrors, errors.New("assigned function has no serving state"))
				continue
			}
			observation := observations[assignment.Placement.AssignmentID]
			if err := r.reconcileAssigned(ctx, assignment, state, observation); err != nil {
				r.removeCandidate(assignment.Placement.AssignmentID)
				reconcileErrors = append(reconcileErrors, err)
			}
		case controlplane.AssignmentCancelled:
			r.removeCandidate(assignment.Placement.AssignmentID)
			if err := r.reconcileCancelled(assignment); err != nil {
				reconcileErrors = append(reconcileErrors, err)
			}
		default:
			r.removeCandidate(assignment.Placement.AssignmentID)
			reconcileErrors = append(reconcileErrors, errors.New("assignment has unsupported desired state"))
		}
	}
	return errors.Join(reconcileErrors...)
}

func (r *Reconciler) reconcileAssigned(
	ctx context.Context,
	record controlplane.AssignmentRecord,
	state controlplane.ServingState,
	observation workeragent.Observation,
) error {
	request, err := prepareRequest(record, state, r.connection)
	if err != nil {
		return err
	}
	if observation.Identity.AssignmentID != "" && observation.Identity != request.Fence.Assignment {
		return errors.New("local assignment replica has a conflicting identity")
	}
	if terminalReplica(observation.State) {
		return errors.New("assigned local replica is terminal and requires replacement intent")
	}
	if observation.State != workeragent.ReplicaReady {
		observation, err = r.agent.Prepare(ctx, request)
		if err != nil {
			cancelErr := r.agent.Cancel(workeragent.CancelRequest{
				Connection: r.connection,
				Fence:      request.Fence,
			})
			return errors.Join(fmt.Errorf("preparing local assignment: %w", err), cancelErr)
		}
	}
	if observation.Identity != request.Fence.Assignment || observation.State != workeragent.ReplicaReady {
		return errors.New("local assignment replica is not ready with the committed identity")
	}
	authorization := servingauth.Authorization{
		Fence:    request.Fence,
		Lifetime: servingauth.LifetimeLiveOnly,
	}
	if err := r.agent.InstallAuthorization(r.connection, authorization); err != nil {
		cancelErr := r.agent.Cancel(workeragent.CancelRequest{
			Connection: r.connection,
			Fence:      request.Fence,
		})
		return errors.Join(fmt.Errorf("installing local serving authorization: %w", err), cancelErr)
	}
	r.candidateMu.Lock()
	r.candidates[record.Placement.AssignmentID] = installedCandidate{
		functionID:    record.FunctionID,
		authorization: authorization,
	}
	r.candidateMu.Unlock()
	return nil
}

func (r *Reconciler) reconcileCancelled(record controlplane.AssignmentRecord) error {
	fence := servingauth.InvocationFence{
		Assignment:     record.Identity(),
		DiscoveryEpoch: r.connection.DiscoveryEpoch,
	}
	if err := r.agent.Cancel(workeragent.CancelRequest{Connection: r.connection, Fence: fence}); err != nil {
		return fmt.Errorf("cancelling local assignment: %w", err)
	}
	observation, exists := observationFor(r.agent.Inventory(), record.Placement.AssignmentID)
	if !exists || !terminalReplica(observation.State) {
		return nil
	}
	if err := r.agent.AcknowledgeTerminal(r.connection, record.Placement.AssignmentID); err != nil {
		return fmt.Errorf("acknowledging terminal local assignment: %w", err)
	}
	return nil
}

// Candidates is a side-effect-free serving observation. It returns only
// currently Ready exact identities whose authorization was installed by Reconcile.
func (r *Reconciler) Candidates(
	ctx context.Context,
	state controlplane.ServingState,
	epoch uint64,
) ([]discovery.EndpointCandidate, error) {
	if ctx == nil {
		return nil, errors.New("local worker candidate context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.workers == nil || r.agent == nil {
		return nil, errors.New("local worker candidate dependencies are required")
	}
	if epoch != r.connection.DiscoveryEpoch {
		return []discovery.EndpointCandidate{}, nil
	}
	worker, err := r.workers.WorkerSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading candidate worker snapshot: %w", err)
	}
	if err := validateWorkerControl(worker, r.connection); err != nil {
		return nil, err
	}
	observations := indexObservations(r.agent.Inventory())
	r.candidateMu.RLock()
	installed := make([]installedCandidate, 0, len(r.candidates))
	for _, candidate := range r.candidates {
		if candidate.functionID == state.Function.ID {
			installed = append(installed, candidate)
		}
	}
	r.candidateMu.RUnlock()
	result := make([]discovery.EndpointCandidate, 0, len(installed))
	for _, candidate := range installed {
		if candidate.authorization.Fence.Assignment.Worker != worker.Session {
			continue
		}
		observation := observations[candidate.authorization.Fence.Assignment.AssignmentID]
		if observation.State != workeragent.ReplicaReady ||
			observation.Identity != candidate.authorization.Fence.Assignment {
			continue
		}
		authorization := candidate.authorization
		result = append(result, discovery.EndpointCandidate{
			Assignment:    authorization.Fence.Assignment,
			DesiredState:  discovery.AssignmentAssigned,
			Worker:        cloneWorkerSnapshot(worker),
			ReplicaReady:  true,
			Authorization: &authorization,
			Address:       r.address,
		})
	}
	slices.SortFunc(result, func(left, right discovery.EndpointCandidate) int {
		return strings.Compare(left.Assignment.AssignmentID, right.Assignment.AssignmentID)
	})
	return result, nil
}

func prepareRequest(
	record controlplane.AssignmentRecord,
	state controlplane.ServingState,
	connection servingauth.ControlConnection,
) (workeragent.PrepareRequest, error) {
	if state.Version == nil || state.Deployment == nil {
		return workeragent.PrepareRequest{}, errors.New("assignment has no admitted serving policy")
	}
	version := *state.Version
	deployment := *state.Deployment
	policy := model.EffectivePolicy{
		VersionID:             version.VersionID,
		AdmissionEpoch:        version.AdmissionEpoch,
		DeploymentGeneration:  deployment.Generation,
		ArtifactDigest:        version.ArtifactDigest,
		ArtifactSize:          version.ArtifactSize,
		ABI:                   version.ABI,
		HostAPIProfile:        version.HostAPIProfile,
		RuntimeFeatureProfile: version.RuntimeFeatureProfile,
		ResourceLimits:        deployment.ResourceLimits,
		MaxConcurrency:        version.ResourceRequest.MaxConcurrency,
		GrantedCapabilities:   slices.Clone(deployment.GrantedCapabilities),
	}
	policyDigest, err := policy.Digest()
	if err != nil {
		return workeragent.PrepareRequest{}, fmt.Errorf("calculating assignment policy: %w", err)
	}
	identity := record.Identity()
	if policyDigest != deployment.EffectivePolicyDigest || policyDigest != identity.PolicyDigest ||
		identity.VersionID != version.VersionID || identity.AdmissionEpoch != version.AdmissionEpoch ||
		identity.DeploymentGeneration != deployment.Generation {
		return workeragent.PrepareRequest{}, errors.New("assignment identity does not match the admitted serving policy")
	}
	return workeragent.PrepareRequest{
		Connection: connection,
		Fence: servingauth.InvocationFence{
			Assignment:     identity,
			DiscoveryEpoch: connection.DiscoveryEpoch,
		},
		Module: workercache.ModuleSpec{
			ArtifactDigest:        version.ArtifactDigest,
			ArtifactSize:          version.ArtifactSize,
			ABI:                   version.ABI,
			HostAPIProfile:        version.HostAPIProfile,
			RuntimeFeatureProfile: version.RuntimeFeatureProfile,
		},
		Policy: policy,
	}, nil
}

func validateWorkerControl(worker scheduler.WorkerSnapshot, connection servingauth.ControlConnection) error {
	if err := worker.Validate(); err != nil {
		return fmt.Errorf("validating local worker snapshot: %w", err)
	}
	if worker.Session.SessionEpoch != connection.SessionEpoch {
		return errors.New("local worker session does not match the control connection")
	}
	if connection.DiscoveryEpoch == 0 {
		return errors.New("local worker control discovery epoch is required")
	}
	return nil
}

func indexServingStates(states []controlplane.ServingState) (map[string]controlplane.ServingState, error) {
	indexed := make(map[string]controlplane.ServingState, len(states))
	for _, state := range states {
		if _, exists := indexed[state.Function.ID]; exists {
			return nil, errors.New("local worker serving states contain a duplicate function")
		}
		indexed[state.Function.ID] = state
	}
	return indexed, nil
}

func indexObservations(inventory workeragent.Inventory) map[string]workeragent.Observation {
	indexed := make(map[string]workeragent.Observation, len(inventory.Replicas))
	for _, observation := range inventory.Replicas {
		indexed[observation.Identity.AssignmentID] = observation
	}
	return indexed
}

func observationFor(inventory workeragent.Inventory, assignmentID string) (workeragent.Observation, bool) {
	for _, observation := range inventory.Replicas {
		if observation.Identity.AssignmentID == assignmentID {
			return observation, true
		}
	}
	return workeragent.Observation{}, false
}

func (r *Reconciler) removeUnpublishableCandidates(
	assignments []controlplane.AssignmentRecord,
	session servingauth.WorkerSession,
) {
	publishable := make(map[string]struct{}, len(assignments))
	for _, assignment := range assignments {
		if assignment.DesiredState == controlplane.AssignmentAssigned && assignment.Placement.Worker == session {
			publishable[assignment.Placement.AssignmentID] = struct{}{}
		}
	}
	r.candidateMu.Lock()
	defer r.candidateMu.Unlock()
	for assignmentID := range r.candidates {
		if _, exists := publishable[assignmentID]; !exists {
			delete(r.candidates, assignmentID)
		}
	}
}

func (r *Reconciler) removeCandidate(assignmentID string) {
	r.candidateMu.Lock()
	delete(r.candidates, assignmentID)
	r.candidateMu.Unlock()
}

func terminalReplica(state workeragent.ReplicaState) bool {
	return state == workeragent.ReplicaStopped || state == workeragent.ReplicaFailed || state == workeragent.ReplicaLost
}

func cloneWorkerSnapshot(worker scheduler.WorkerSnapshot) scheduler.WorkerSnapshot {
	worker.Labels = maps.Clone(worker.Labels)
	worker.Cache.Artifacts = maps.Clone(worker.Cache.Artifacts)
	worker.Cache.Compiled = maps.Clone(worker.Cache.Compiled)
	return worker
}
