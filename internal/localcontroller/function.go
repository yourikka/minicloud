package localcontroller

import (
	"context"
	"fmt"
	"io"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
)

// PutArtifact publishes one verified immutable Artifact to the CAS.
func (c *Controller) PutArtifact(
	ctx context.Context,
	expected digest.SHA256,
	source io.Reader,
) (artifact.Info, error) {
	if err := checkContext(ctx); err != nil {
		return artifact.Info{}, err
	}
	if c == nil || c.artifacts == nil {
		return artifact.Info{}, fmt.Errorf("putting artifact: local controller artifact store is required")
	}
	info, err := c.artifacts.Put(ctx, expected, source)
	if err != nil {
		return artifact.Info{}, fmt.Errorf("putting artifact: %w", err)
	}
	if info.Digest != expected {
		return artifact.Info{}, fmt.Errorf("putting artifact: store returned a mismatched digest")
	}
	return info, nil
}

// CreateFunction creates an Active Function and its enabled default HTTP
// Trigger in one deterministic catalog command.
func (c *Controller) CreateFunction(ctx context.Context, input CreateFunctionInput) (FunctionView, error) {
	if err := checkContext(ctx); err != nil {
		return FunctionView{}, err
	}
	if c == nil || c.catalog == nil {
		return FunctionView{}, fmt.Errorf("creating function: local controller catalog is required")
	}
	functionID, err := c.newID("function")
	if err != nil {
		return FunctionView{}, err
	}
	triggerID, err := c.newID("trigger")
	if err != nil {
		return FunctionView{}, err
	}
	command, err := c.nextCommand()
	if err != nil {
		return FunctionView{}, err
	}

	function := model.Function{
		Metadata: model.Metadata{
			ID:               functionID,
			Namespace:        model.DefaultNamespace,
			CreatedAt:        command.At,
			UpdatedAt:        command.At,
			CreatedRaftIndex: command.AppliedIndex,
			ResourceRevision: 1,
		},
		Name:      input.Name,
		Labels:    cloneLabels(input.Labels),
		Lifecycle: model.FunctionActive,
	}
	trigger := controlplane.HTTPTrigger{
		Metadata: model.Metadata{
			ID:               triggerID,
			Namespace:        model.DefaultNamespace,
			CreatedAt:        command.At,
			UpdatedAt:        command.At,
			CreatedRaftIndex: command.AppliedIndex,
			ResourceRevision: 1,
		},
		FunctionID:          functionID,
		Enabled:             true,
		AuthPolicy:          input.AuthPolicy,
		TokenVerifierDigest: cloneDigest(input.TokenVerifierDigest),
	}
	if _, err := c.catalog.CreateFunction(controlplane.CreateFunctionCommand{
		IfNoneMatch:  true,
		AppliedIndex: command.AppliedIndex,
		Function:     function,
		Trigger:      trigger,
	}); err != nil {
		return FunctionView{}, fmt.Errorf("creating function: %w", err)
	}
	function, trigger, err = c.catalog.GetFunction(functionID)
	if err != nil {
		return FunctionView{}, fmt.Errorf("loading created function: %w", err)
	}
	return FunctionView{Function: function, Trigger: trigger}, nil
}

// GetFunction returns one Function and its default HTTP Trigger.
func (c *Controller) GetFunction(ctx context.Context, functionID string) (FunctionView, error) {
	if err := checkContext(ctx); err != nil {
		return FunctionView{}, err
	}
	if c == nil || c.catalog == nil {
		return FunctionView{}, fmt.Errorf("getting function: local controller catalog is required")
	}
	function, trigger, err := c.catalog.GetFunction(functionID)
	if err != nil {
		return FunctionView{}, fmt.Errorf("getting function: %w", err)
	}
	return FunctionView{Function: function, Trigger: trigger}, nil
}

// ListFunctions returns ordered Function views with their default Triggers.
func (c *Controller) ListFunctions(ctx context.Context) ([]FunctionView, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if c == nil || c.catalog == nil {
		return nil, fmt.Errorf("listing functions: local controller catalog is required")
	}
	snapshot := c.catalog.Snapshot()
	triggers := make(map[string]controlplane.HTTPTrigger, len(snapshot.Triggers))
	for _, trigger := range snapshot.Triggers {
		triggers[trigger.FunctionID] = trigger
	}
	functions := make([]FunctionView, 0, len(snapshot.Functions))
	for _, function := range snapshot.Functions {
		trigger, exists := triggers[function.ID]
		if !exists {
			return nil, fmt.Errorf("listing functions: catalog invariant has no default trigger")
		}
		functions = append(functions, FunctionView{Function: function, Trigger: trigger})
	}
	return functions, nil
}

// SetFunctionLifecycle conditionally enables or disables one Function.
func (c *Controller) SetFunctionLifecycle(
	ctx context.Context,
	input SetFunctionLifecycleInput,
) (model.Function, error) {
	if err := checkContext(ctx); err != nil {
		return model.Function{}, err
	}
	if c == nil || c.catalog == nil {
		return model.Function{}, fmt.Errorf("setting function lifecycle: local controller catalog is required")
	}
	command, err := c.nextCommand()
	if err != nil {
		return model.Function{}, err
	}
	function, err := c.catalog.SetFunctionLifecycle(controlplane.SetFunctionLifecycleCommand{
		FunctionID:               input.FunctionID,
		ExpectedResourceRevision: input.ExpectedResourceRevision,
		Lifecycle:                input.Lifecycle,
		UpdatedAt:                command.At,
		AppliedIndex:             command.AppliedIndex,
	})
	if err != nil {
		return model.Function{}, fmt.Errorf("setting function lifecycle: %w", err)
	}
	return function, nil
}

func cloneLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func cloneDigest(value *digest.SHA256) *digest.SHA256 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
