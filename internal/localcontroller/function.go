package localcontroller

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
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
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	prepared, err := c.prepareCreateFunction(input)
	if err != nil {
		return FunctionView{}, err
	}
	return c.applyCreateFunction(prepared)
}

type preparedFunction struct {
	command CommandMeta
	view    FunctionView
}

func (c *Controller) prepareCreateFunction(input CreateFunctionInput) (preparedFunction, error) {
	invocationToken, verifier, err := c.newInvocationToken(input.AuthPolicy)
	if err != nil {
		return preparedFunction{}, err
	}
	functionID, err := c.newID("function")
	if err != nil {
		return preparedFunction{}, err
	}
	triggerID, err := c.newID("trigger")
	if err != nil {
		return preparedFunction{}, err
	}
	command, err := c.nextCommand()
	if err != nil {
		return preparedFunction{}, err
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
		TokenVerifierDigest: verifier,
	}
	return preparedFunction{
		command: command,
		view: FunctionView{
			Function: function, Trigger: trigger, InvocationToken: invocationToken,
		},
	}, nil
}

func (c *Controller) applyCreateFunction(prepared preparedFunction) (FunctionView, error) {
	if _, err := c.catalog.CreateFunction(controlplane.CreateFunctionCommand{
		IfNoneMatch:  true,
		AppliedIndex: prepared.command.AppliedIndex,
		Function:     prepared.view.Function,
		Trigger:      prepared.view.Trigger,
	}); err != nil {
		return FunctionView{}, fmt.Errorf("creating function: %w", err)
	}
	return cloneFunctionView(prepared.view), nil
}

// RotateInvocationToken replaces the single verifier and returns plaintext
// exactly once in this response value.
func (c *Controller) RotateInvocationToken(
	ctx context.Context,
	input RotateInvocationTokenInput,
) (FunctionView, error) {
	if err := checkContext(ctx); err != nil {
		return FunctionView{}, err
	}
	if c == nil || c.catalog == nil || c.tokens == nil {
		return FunctionView{}, fmt.Errorf("rotating invocation token: local controller dependencies are required")
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	prepared, err := c.prepareRotateInvocationToken(input)
	if err != nil {
		return FunctionView{}, err
	}
	return c.applyRotateInvocationToken(prepared)
}

// preparedRotation holds one reserved Invocation Token plaintext, its verifier,
// and the Trigger identity the rotation CAS will advance.
type preparedRotation struct {
	command   CommandMeta
	input     RotateInvocationTokenInput
	triggerID string
	token     string
	verifier  digest.SHA256
}

func (c *Controller) prepareRotateInvocationToken(
	input RotateInvocationTokenInput,
) (preparedRotation, error) {
	_, trigger, err := c.catalog.GetFunction(input.FunctionID)
	if err != nil {
		return preparedRotation{}, fmt.Errorf("loading invocation token trigger: %w", err)
	}
	token, err := c.tokens.NewToken()
	if err != nil {
		return preparedRotation{}, fmt.Errorf("generating invocation token: %w", err)
	}
	if token == "" {
		return preparedRotation{}, errors.New("generating invocation token: token source returned an empty token")
	}
	command, err := c.nextCommand()
	if err != nil {
		return preparedRotation{}, err
	}
	return preparedRotation{
		command:   command,
		input:     input,
		triggerID: trigger.ID,
		token:     token,
		verifier:  digest.Sum([]byte(token)),
	}, nil
}

func (c *Controller) applyRotateInvocationToken(prepared preparedRotation) (FunctionView, error) {
	trigger, err := c.catalog.RotateInvocationToken(controlplane.RotateInvocationTokenCommand{
		FunctionID:               prepared.input.FunctionID,
		ExpectedResourceRevision: prepared.input.ExpectedResourceRevision,
		TokenVerifierDigest:      prepared.verifier,
		UpdatedAt:                prepared.command.At,
		AppliedIndex:             prepared.command.AppliedIndex,
	})
	if err != nil {
		return FunctionView{}, fmt.Errorf("rotating invocation token: %w", err)
	}
	function, _, err := c.catalog.GetFunction(prepared.input.FunctionID)
	if err != nil {
		return FunctionView{}, fmt.Errorf("loading rotated invocation token: %w", err)
	}
	return FunctionView{Function: function, Trigger: trigger, InvocationToken: prepared.token}, nil
}

// GetFunction returns one Function and its default HTTP Trigger.
func (c *Controller) GetFunction(ctx context.Context, functionID string) (FunctionView, error) {
	if err := checkContext(ctx); err != nil {
		return FunctionView{}, err
	}
	if c == nil || c.catalog == nil {
		return FunctionView{}, fmt.Errorf("getting function: local controller catalog is required")
	}
	c.operationMu.RLock()
	defer c.operationMu.RUnlock()
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
	c.operationMu.RLock()
	defer c.operationMu.RUnlock()
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
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
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

func cloneFunctionView(view FunctionView) FunctionView {
	view.Function.Labels = cloneLabels(view.Function.Labels)
	if view.Trigger.TokenVerifierDigest != nil {
		verifier := *view.Trigger.TokenVerifierDigest
		view.Trigger.TokenVerifierDigest = &verifier
	}
	return view
}

func (c *Controller) newInvocationToken(
	policy controlplane.AuthPolicy,
) (string, *digest.SHA256, error) {
	switch policy {
	case controlplane.AuthPolicyPublic:
		return "", nil, nil
	case controlplane.AuthPolicyToken:
		if c.tokens == nil {
			return "", nil, errors.New("creating function: invocation token source is required")
		}
		token, err := c.tokens.NewToken()
		if err != nil {
			return "", nil, fmt.Errorf("generating invocation token: %w", err)
		}
		if token == "" {
			return "", nil, errors.New("generating invocation token: token source returned an empty token")
		}
		verifier := digest.Sum([]byte(token))
		return token, &verifier, nil
	default:
		return "", nil, problem.Invalid("auth_policy", "must be token or public")
	}
}
