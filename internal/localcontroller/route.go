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
	if input.ExpectedActiveRouteRevision == math.MaxUint64 {
		return model.Route{}, model.Function{}, &problem.Error{
			Code:    problem.CodeConflict,
			Message: "function route revision space is exhausted",
		}
	}

	version, deployment, err := c.releases.Get(input.VersionID)
	if err != nil {
		return model.Route{}, model.Function{}, fmt.Errorf("loading route target version: %w", err)
	}
	if version.FunctionID != input.FunctionID || version.State != model.VersionReady || deployment == nil ||
		deployment.DesiredPhase != model.DeploymentActive {
		return model.Route{}, model.Function{}, &problem.Error{
			Code:    problem.CodeConflict,
			Message: "route target is not a ready active version for this function",
		}
	}

	routeID, err := c.newID("route")
	if err != nil {
		return model.Route{}, model.Function{}, err
	}
	saltID, err := c.newID("salt")
	if err != nil {
		return model.Route{}, model.Function{}, err
	}
	salt, err := c.newRouteSalt()
	if err != nil {
		return model.Route{}, model.Function{}, err
	}
	command, err := c.nextCommand()
	if err != nil {
		return model.Route{}, model.Function{}, err
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
	published, function, err := c.routes.Publish(controlplane.PublishRouteCommand{
		FunctionID:                  input.FunctionID,
		ExpectedActiveRouteRevision: input.ExpectedActiveRouteRevision,
		Route:                       route,
		UpdatedAt:                   command.At,
		AppliedIndex:                command.AppliedIndex,
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
