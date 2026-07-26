package localcontroller

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
)

// PublishRoute creates and atomically publishes the next complete Local Core
// Route snapshot. It allows exactly one Ready Version Target at 10000 basis
// points; multi-target rollout remains a later feature slice.
func (c *Controller) PublishRoute(
	ctx context.Context,
	input PublishRouteInput,
) (model.Route, model.Function, error) {
	if err := checkContext(ctx); err != nil {
		return model.Route{}, model.Function{}, err
	}
	if c == nil || c.routes == nil || c.releases == nil {
		return model.Route{}, model.Function{}, errors.New("publishing route: local controller route dependencies are required")
	}
	prepared, err := c.preparePublishRoute(input)
	if err != nil {
		return model.Route{}, model.Function{}, err
	}
	return c.applyPublishRoute(prepared)
}

// preparedRoute is one complete immutable Route snapshot and the exact CAS its
// publication applies. Every fallible identifier and salt is already reserved.
type preparedRoute struct {
	command CommandMeta
	input   PublishRouteInput
	route   model.Route
}

func (c *Controller) preparePublishRoute(input PublishRouteInput) (preparedRoute, error) {
	if input.ExpectedActiveRouteRevision == math.MaxUint64 {
		return preparedRoute{}, &problem.Error{
			Code:    problem.CodeConflict,
			Message: "function route revision space is exhausted",
		}
	}

	version, deployment, err := c.releases.Get(input.VersionID)
	if err != nil {
		return preparedRoute{}, fmt.Errorf("loading route target version: %w", err)
	}
	if version.FunctionID != input.FunctionID || version.State != model.VersionReady || deployment == nil ||
		deployment.DesiredPhase != model.DeploymentActive {
		return preparedRoute{}, &problem.Error{
			Code:    problem.CodeConflict,
			Message: "route target is not a ready active version for this function",
		}
	}

	routeID, err := c.newID("route")
	if err != nil {
		return preparedRoute{}, err
	}
	saltID, err := c.newID("salt")
	if err != nil {
		return preparedRoute{}, err
	}
	salt, err := c.newRouteSalt()
	if err != nil {
		return preparedRoute{}, err
	}
	command, err := c.nextCommand()
	if err != nil {
		return preparedRoute{}, err
	}

	route := model.Route{
		Metadata: model.Metadata{
			ID:               routeID,
			Namespace:        version.Namespace,
			CreatedAt:        command.At,
			UpdatedAt:        command.At,
			CreatedRaftIndex: command.AppliedIndex,
			ResourceRevision: 1,
		},
		FunctionID:    input.FunctionID,
		RouteRevision: input.ExpectedActiveRouteRevision + 1,
		Targets: []model.RouteTarget{{
			VersionID:             version.VersionID,
			AdmissionEpoch:        version.AdmissionEpoch,
			DeploymentGeneration:  deployment.Generation,
			EffectivePolicyDigest: deployment.EffectivePolicyDigest,
			WeightBasisPoints:     model.TotalRouteWeightBasisPoints,
		}},
		Affinity:    model.AffinityRequestID,
		HashVersion: model.HashVersionSHA256BPSV1,
		SaltID:      saltID,
		Salt:        salt,
		Enabled:     true,
	}
	return preparedRoute{command: command, input: input, route: route}, nil
}

func (c *Controller) applyPublishRoute(prepared preparedRoute) (model.Route, model.Function, error) {
	published, function, err := c.routes.Publish(controlplane.PublishRouteCommand{
		FunctionID:                  prepared.input.FunctionID,
		ExpectedActiveRouteRevision: prepared.input.ExpectedActiveRouteRevision,
		Route:                       prepared.route,
		UpdatedAt:                   prepared.command.At,
		AppliedIndex:                prepared.command.AppliedIndex,
	})
	if err != nil {
		return model.Route{}, model.Function{}, fmt.Errorf("publishing route: %w", err)
	}
	return published, function, nil
}

// GetRoute returns the current complete Route snapshot for one Function.
func (c *Controller) GetRoute(ctx context.Context, functionID string) (model.Route, error) {
	if err := checkContext(ctx); err != nil {
		return model.Route{}, err
	}
	if c == nil || c.routes == nil {
		return model.Route{}, errors.New("getting route: local controller route store is required")
	}
	route, err := c.routes.Get(functionID)
	if err != nil {
		return model.Route{}, fmt.Errorf("getting route: %w", err)
	}
	return route, nil
}

// ListServingStates returns consistent, defensive copies of every Function's
// serving inputs. It is intended for the trusted Local Core serving projector,
// not a public management response because token verifier digests are present.
func (c *Controller) ListServingStates(ctx context.Context) ([]controlplane.ServingState, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if c == nil || c.routes == nil {
		return nil, errors.New("listing serving states: local controller route store is required")
	}
	states, err := c.routes.ServingStates()
	if err != nil {
		return nil, fmt.Errorf("listing serving states: %w", err)
	}
	return states, nil
}
