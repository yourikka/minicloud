package localcore

import (
	"context"
	"errors"
	"fmt"

	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/localcontroller"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/scheduler"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmprofile"
)

// localCoreBarrier is the trivial single-process leadership stand-in. One
// Local Core process is always its own planner authority; this is not a Raft
// leadership claim and is replaced by the committed M1 Leader Barrier.
var localCoreBarrier = scheduler.LeaderBarrier{Term: 1, AppliedIndex: 1, Ready: true}

// placeLocalAssignments commits Assignment intent for every Function whose
// enabled serving target has no live placement on the single local Worker,
// and cancels intent whose Route target has been superseded. The replacement
// placement lands on the next convergence pass, after the Worker released the
// superseded replica's resources.
func (r *Runtime) placeLocalAssignments(ctx context.Context) error {
	if r == nil || r.controller == nil || r.planner == nil || r.ids == nil {
		return errors.New("local core placement dependencies are required")
	}
	states, err := r.controller.ListServingStates(ctx)
	if err != nil {
		return fmt.Errorf("listing serving states for placement: %w", err)
	}
	assignments, err := r.controller.ListAssignments(ctx)
	if err != nil {
		return fmt.Errorf("listing assignments for placement: %w", err)
	}
	worker, err := r.workers.WorkerSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("loading worker snapshot for placement: %w", err)
	}

	live := make(map[string]controlplane.AssignmentRecord, len(assignments))
	for _, record := range assignments {
		if record.DesiredState == controlplane.AssignmentAssigned {
			live[record.FunctionID] = record
		}
	}
	var failures []error
	for _, state := range states {
		target, servable := servableTarget(state)
		if !servable {
			continue
		}
		if record, exists := live[state.Function.ID]; exists {
			if placementMatchesTarget(record.Placement, target, worker.Session) {
				continue
			}
			if _, err := r.controller.CancelAssignment(ctx, localcontroller.CancelAssignmentInput{
				AssignmentID:             record.Placement.AssignmentID,
				ExpectedResourceRevision: record.ResourceRevision,
			}); err != nil {
				failures = append(failures, fmt.Errorf("cancelling superseded assignment: %w", err))
			}
			continue
		}
		if err := r.placeTarget(ctx, state, target, worker); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (r *Runtime) placeTarget(
	ctx context.Context,
	state controlplane.ServingState,
	target model.RouteTarget,
	worker scheduler.WorkerSnapshot,
) error {
	commandID, err := r.ids.NewID("placement")
	if err != nil {
		return fmt.Errorf("generating placement command id: %w", err)
	}
	assignmentID, err := r.ids.NewID("assignment")
	if err != nil {
		return fmt.Errorf("generating placement assignment id: %w", err)
	}
	decision, err := r.planner.Plan(scheduler.PlacementRequest{
		CommandID:      commandID,
		AssignmentID:   assignmentID,
		VersionID:      state.Version.VersionID,
		ArtifactDigest: state.Version.ArtifactDigest,
		ArtifactSize:   state.Version.ArtifactSize,
		ABI:            state.Version.ABI,
		HostAPI:        state.Version.HostAPIProfile,
		FeatureProfile: state.Version.RuntimeFeatureProfile,
		RuntimeName:    wasmprofile.RuntimeName,
		RuntimeVersion: wasmprofile.RuntimeVersion,
		RuntimeEngine:  worker.Runtime.Engine,
		GOOS:           worker.Runtime.GOOS,
		GOARCH:         worker.Runtime.GOARCH,
		MemoryMiB:      state.Deployment.ResourceLimits.MemoryMiB,
		// Local Core places one single-replica slot; concurrency-aware slot
		// budgeting belongs to the M1 Scheduler.
		RequiredSlots:        1,
		AdmissionEpoch:       target.AdmissionEpoch,
		DeploymentGeneration: target.DeploymentGeneration,
		PolicyDigest:         target.EffectivePolicyDigest,
	}, []scheduler.WorkerSnapshot{worker})
	if err != nil {
		return fmt.Errorf("planning local placement: %w", err)
	}

	_, commitErr := r.controller.CommitAssignment(ctx, localcontroller.CommitAssignmentInput{
		FunctionID:              state.Function.ID,
		Placement:               decision.Assignment,
		ExpectedScalingRevision: state.Deployment.ScalingRevision,
	})
	// The planner result is acknowledged in both directions: a committed
	// intent is now authoritative controller state, and a rejected commit must
	// free the Assignment ID for the next convergence attempt.
	if ackErr := r.planner.Acknowledge(decision.Assignment.CommandID); ackErr != nil {
		commitErr = errors.Join(commitErr, ackErr)
	}
	if commitErr != nil {
		return fmt.Errorf("committing local placement: %w", commitErr)
	}
	return nil
}

// servableTarget returns the single Route target that should hold a live local
// placement. Disabled Functions keep their existing warm placement but do not
// receive a new one.
func servableTarget(state controlplane.ServingState) (model.RouteTarget, bool) {
	if state.Route == nil || !state.Route.Enabled || len(state.Route.Targets) != 1 ||
		state.Version == nil || state.Deployment == nil ||
		state.Function.Lifecycle != model.FunctionActive {
		return model.RouteTarget{}, false
	}
	target := state.Route.Targets[0]
	if state.Version.VersionID != target.VersionID || state.Version.State != model.VersionReady ||
		state.Deployment.DesiredPhase != model.DeploymentActive ||
		state.Deployment.Generation != target.DeploymentGeneration {
		return model.RouteTarget{}, false
	}
	return target, true
}

func placementMatchesTarget(
	placement scheduler.Assignment,
	target model.RouteTarget,
	session servingauth.WorkerSession,
) bool {
	return placement.Worker == session &&
		placement.VersionID == target.VersionID &&
		placement.AdmissionEpoch == target.AdmissionEpoch &&
		placement.DeploymentGeneration == target.DeploymentGeneration &&
		placement.PolicyDigest == target.EffectivePolicyDigest
}
