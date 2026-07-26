package localcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
)

const (
	functionsPath      = "/v1/functions"
	versionsSuffix     = "/versions"
	routeSuffix        = "/route"
	tokenRotateSuffix  = "/invocation-token:rotate"
	functionKind       = "function"
	httpTriggerKind    = "http_trigger"
	versionKind        = "version"
	routeKind          = "route"
	maxOperationBodies = controlplane.MaxOperationBodyBytes
)

// preparedOperation is one management write whose fallible inputs are already
// reserved and whose terminal outcome is known before any state changes. Its
// apply function must be a deterministic state transition without I/O.
type preparedOperation struct {
	command CommandMeta
	outcome controlplane.Outcome
	apply   func() error
}

// CreateFunctionOperation atomically commits one Function/default Trigger and
// its terminal idempotency record in the Local Controller observation domain.
func (c *Controller) CreateFunctionOperation(
	ctx context.Context,
	input CreateFunctionOperationInput,
) (FunctionOperationResult, error) {
	if err := checkContext(ctx); err != nil {
		return FunctionOperationResult{}, err
	}
	if c == nil || c.catalog == nil || c.ledger == nil {
		return FunctionOperationResult{}, errors.New("creating function operation: local controller dependencies are required")
	}
	request, err := createFunctionRequest(input.Function)
	if err != nil {
		return FunctionOperationResult{}, err
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()

	var view FunctionView
	result, err := c.applyOperation(input.Operation, request, func() (preparedOperation, error) {
		prepared, err := c.prepareCreateFunction(input.Function)
		if err != nil {
			return preparedOperation{}, err
		}
		functionRevision := prepared.view.Function.ResourceRevision
		triggerRevision := prepared.view.Trigger.ResourceRevision
		return preparedOperation{
			command: prepared.command,
			outcome: controlplane.Outcome{
				Status: controlplane.OutcomeSucceeded,
				AffectedResources: []controlplane.AffectedResource{
					{Kind: functionKind, ID: prepared.view.Function.ID, ResourceRevision: &functionRevision},
					{Kind: httpTriggerKind, ID: prepared.view.Trigger.ID, ResourceRevision: &triggerRevision},
				},
				CredentialIssued: prepared.view.InvocationToken != "",
			},
			apply: func() error {
				applied, applyErr := c.applyCreateFunction(prepared)
				view = applied
				return applyErr
			},
		}, nil
	})
	if err != nil {
		return FunctionOperationResult{Disposition: result.Disposition, Record: result.Record}, err
	}
	return c.functionOperationResult(result, view, "")
}

// SetFunctionLifecycleOperation applies one idempotent Function enable/disable
// command under an exact resource-revision CAS.
func (c *Controller) SetFunctionLifecycleOperation(
	ctx context.Context,
	input SetFunctionLifecycleOperationInput,
) (FunctionOperationResult, error) {
	if err := checkContext(ctx); err != nil {
		return FunctionOperationResult{}, err
	}
	if c == nil || c.catalog == nil || c.ledger == nil {
		return FunctionOperationResult{}, errors.New("setting function lifecycle operation: local controller dependencies are required")
	}
	request, err := setFunctionLifecycleRequest(input.Lifecycle)
	if err != nil {
		return FunctionOperationResult{}, err
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()

	result, err := c.applyOperation(input.Operation, request, func() (preparedOperation, error) {
		command, err := c.nextCommand()
		if err != nil {
			return preparedOperation{}, err
		}
		revision, err := c.nextLifecycleRevision(input.Lifecycle)
		if err != nil {
			return preparedOperation{}, err
		}
		return preparedOperation{
			command: command,
			outcome: controlplane.Outcome{
				Status: controlplane.OutcomeSucceeded,
				AffectedResources: []controlplane.AffectedResource{
					{Kind: functionKind, ID: input.Lifecycle.FunctionID, ResourceRevision: &revision},
				},
			},
			apply: func() error {
				_, applyErr := c.applyFunctionLifecycle(input.Lifecycle, command)
				return applyErr
			},
		}, nil
	})
	if err != nil {
		return FunctionOperationResult{Disposition: result.Disposition, Record: result.Record}, err
	}
	return c.functionOperationResult(result, FunctionView{}, input.Lifecycle.FunctionID)
}

// RotateInvocationTokenOperation replaces the single Trigger verifier once per
// Operation ID. The plaintext exists only in the first applied response.
func (c *Controller) RotateInvocationTokenOperation(
	ctx context.Context,
	input RotateInvocationTokenOperationInput,
) (FunctionOperationResult, error) {
	if err := checkContext(ctx); err != nil {
		return FunctionOperationResult{}, err
	}
	if c == nil || c.catalog == nil || c.ledger == nil || c.tokens == nil {
		return FunctionOperationResult{}, errors.New("rotating invocation token operation: local controller dependencies are required")
	}
	request, err := rotateInvocationTokenRequest(input.Rotation)
	if err != nil {
		return FunctionOperationResult{}, err
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()

	var view FunctionView
	result, err := c.applyOperation(input.Operation, request, func() (preparedOperation, error) {
		prepared, err := c.prepareRotateInvocationToken(input.Rotation)
		if err != nil {
			return preparedOperation{}, err
		}
		revision := input.Rotation.ExpectedResourceRevision + 1
		return preparedOperation{
			command: prepared.command,
			outcome: controlplane.Outcome{
				Status: controlplane.OutcomeSucceeded,
				AffectedResources: []controlplane.AffectedResource{
					{Kind: httpTriggerKind, ID: prepared.triggerID, ResourceRevision: &revision},
				},
				CredentialIssued: true,
			},
			apply: func() error {
				applied, applyErr := c.applyRotateInvocationToken(prepared)
				view = applied
				return applyErr
			},
		}, nil
	})
	if err != nil {
		return FunctionOperationResult{Disposition: result.Disposition, Record: result.Record}, err
	}
	return c.functionOperationResult(result, view, input.Rotation.FunctionID)
}

// CreateVersionOperation commits one immutable Uploaded Version and its
// terminal Operation record, then drives the first admission attempt.
//
// The committed operation covers Version creation only. A validator
// infrastructure failure returns the current persisted Version together with
// that error while the Operation record and validation fence remain valid, so
// an exact retry replays the original Version instead of creating a second one.
func (c *Controller) CreateVersionOperation(
	ctx context.Context,
	input CreateVersionOperationInput,
) (VersionOperationResult, error) {
	if err := checkContext(ctx); err != nil {
		return VersionOperationResult{}, err
	}
	if c == nil || c.releases == nil || c.ledger == nil {
		return VersionOperationResult{}, errors.New("creating version operation: local controller dependencies are required")
	}
	request, err := createVersionRequest(input.Version)
	if err != nil {
		return VersionOperationResult{}, err
	}

	var prepared preparedVersion
	var created model.Version
	result, err := func() (controlplane.CompletionResult, error) {
		c.operationMu.Lock()
		defer c.operationMu.Unlock()
		return c.applyOperation(input.Operation, request, func() (preparedOperation, error) {
			candidate, err := c.prepareCreateVersion(ctx, input.Version)
			if err != nil {
				return preparedOperation{}, err
			}
			prepared = candidate
			revision := candidate.version.ResourceRevision
			return preparedOperation{
				command: candidate.createCommand,
				outcome: controlplane.Outcome{
					Status: controlplane.OutcomeSucceeded,
					AffectedResources: []controlplane.AffectedResource{
						{Kind: versionKind, ID: candidate.version.VersionID, ResourceRevision: &revision},
					},
				},
				apply: func() error {
					applied, applyErr := c.applyCreateVersion(candidate)
					created = applied
					return applyErr
				},
			}, nil
		})
	}()
	if err != nil {
		return VersionOperationResult{Disposition: result.Disposition, Record: result.Record}, err
	}
	if result.Disposition != controlplane.CompletionApplied {
		return c.replayedVersionResult(result)
	}

	admission, admissionErr := c.driveAdmission(ctx, created, prepared)
	return VersionOperationResult{
		Disposition: result.Disposition,
		Record:      result.Record,
		Version:     admission.Version,
		Deployment:  admission.Deployment,
	}, admissionErr
}

// PublishRouteOperation atomically advances one Function's active Route pointer
// and records its terminal Operation result.
func (c *Controller) PublishRouteOperation(
	ctx context.Context,
	input PublishRouteOperationInput,
) (RouteOperationResult, error) {
	if err := checkContext(ctx); err != nil {
		return RouteOperationResult{}, err
	}
	if c == nil || c.routes == nil || c.releases == nil || c.ledger == nil {
		return RouteOperationResult{}, errors.New("publishing route operation: local controller dependencies are required")
	}
	request, err := publishRouteRequest(input.Route)
	if err != nil {
		return RouteOperationResult{}, err
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()

	var route model.Route
	var function model.Function
	result, err := c.applyOperation(input.Operation, request, func() (preparedOperation, error) {
		prepared, err := c.preparePublishRoute(input.Route)
		if err != nil {
			return preparedOperation{}, err
		}
		current, _, err := c.catalog.GetFunction(input.Route.FunctionID)
		if err != nil {
			return preparedOperation{}, fmt.Errorf("loading route function: %w", err)
		}
		routeRevision := prepared.route.RouteRevision
		functionRevision := current.ResourceRevision + 1
		return preparedOperation{
			command: prepared.command,
			outcome: controlplane.Outcome{
				Status: controlplane.OutcomeSucceeded,
				AffectedResources: []controlplane.AffectedResource{
					{Kind: routeKind, ID: prepared.route.ID, RouteRevision: &routeRevision},
					{Kind: functionKind, ID: current.ID, ResourceRevision: &functionRevision},
				},
			},
			apply: func() error {
				published, updated, applyErr := c.applyPublishRoute(prepared)
				route, function = published, updated
				return applyErr
			},
		}, nil
	})
	if err != nil {
		return RouteOperationResult{Disposition: result.Disposition, Record: result.Record}, err
	}
	if result.Disposition != controlplane.CompletionApplied {
		return c.replayedRouteResult(result)
	}
	return RouteOperationResult{
		Disposition: result.Disposition, Record: result.Record, Route: route, Function: function,
	}, nil
}

// GetOperation returns one retained safe management Operation result.
func (c *Controller) GetOperation(
	ctx context.Context,
	key controlplane.OperationKey,
) (controlplane.Record, error) {
	if err := checkContext(ctx); err != nil {
		return controlplane.Record{}, err
	}
	if c == nil || c.ledger == nil {
		return controlplane.Record{}, errors.New("getting operation: local controller ledger is required")
	}
	c.operationMu.RLock()
	defer c.operationMu.RUnlock()
	record, err := c.ledger.Lookup(key)
	if err != nil {
		return controlplane.Record{}, fmt.Errorf("getting operation: %w", err)
	}
	return record, nil
}

// applyOperation serializes replay detection, request-digest comparison,
// capacity reservation, and one deterministic resource mutation. Callers must
// already hold the operation lock. A replayed operation never calls prepare.
func (c *Controller) applyOperation(
	key controlplane.OperationKey,
	request controlplane.Request,
	prepare func() (preparedOperation, error),
) (controlplane.CompletionResult, error) {
	record, found, err := c.lookupOperation(key, request)
	if err != nil {
		return controlplane.CompletionResult{}, err
	}
	if found {
		result := controlplane.CompletionResult{Disposition: replayDisposition(record), Record: record}
		if record.Outcome.Status == controlplane.OutcomeFailed {
			return result, failureError(record.Outcome.Failure)
		}
		return result, nil
	}

	prepared, err := prepare()
	if err != nil {
		return controlplane.CompletionResult{}, err
	}
	completion := controlplane.Completion{
		Key:          key,
		Request:      request,
		Outcome:      prepared.outcome,
		CompletedAt:  prepared.command.At,
		AppliedIndex: prepared.command.AppliedIndex,
	}
	result, err := c.ledger.CompleteAfter(completion, prepared.apply)
	if err != nil {
		return c.recordOperationFailure(completion, err)
	}
	return result, nil
}

func (c *Controller) lookupOperation(
	key controlplane.OperationKey,
	request controlplane.Request,
) (controlplane.Record, bool, error) {
	record, err := c.ledger.Lookup(key)
	if err != nil {
		if problemCode(err) == problem.CodeNotFound {
			return controlplane.Record{}, false, nil
		}
		return controlplane.Record{}, false, err
	}
	digestValue, err := request.Digest()
	if err != nil {
		return controlplane.Record{}, false, err
	}
	if record.Digest != digestValue {
		return controlplane.Record{}, false, &problem.Error{
			Code: problem.CodeConflict, Message: "operation id was already used with a different request",
		}
	}
	return record, true, nil
}

func (c *Controller) recordOperationFailure(
	completion controlplane.Completion,
	applyErr error,
) (controlplane.CompletionResult, error) {
	code := problemCode(applyErr)
	if !terminalOperationFailure(code) {
		return controlplane.CompletionResult{}, applyErr
	}
	completion.Outcome = controlplane.Outcome{
		Status:            controlplane.OutcomeFailed,
		Failure:           &controlplane.Failure{Code: code, Message: safeProblemMessage(applyErr)},
		AffectedResources: []controlplane.AffectedResource{},
	}
	result, err := c.ledger.Complete(completion)
	if err != nil {
		return controlplane.CompletionResult{}, errors.Join(applyErr, err)
	}
	return result, applyErr
}

func (c *Controller) functionOperationResult(
	result controlplane.CompletionResult,
	view FunctionView,
	fallbackFunctionID string,
) (FunctionOperationResult, error) {
	if result.Disposition != controlplane.CompletionApplied || view.Function.ID == "" {
		resolved, err := c.resolveFunctionView(result.Record, fallbackFunctionID)
		if err != nil {
			return FunctionOperationResult{}, err
		}
		view = resolved
	}
	operation := FunctionOperationResult{
		Disposition: result.Disposition, Record: result.Record, View: view,
	}
	if result.Disposition == controlplane.CompletionCredentialNotReplayable {
		return operation, &problem.Error{
			Code: problem.CodeCredentialNotReplayable, Message: "operation credential cannot be replayed",
		}
	}
	return operation, nil
}

// resolveFunctionView loads the current Function view named by a retained
// Operation record. A rotation record identifies only its Trigger, so the
// caller supplies the request's Function ID as the fallback identity.
func (c *Controller) resolveFunctionView(
	record controlplane.Record,
	fallbackFunctionID string,
) (FunctionView, error) {
	functionID, err := affectedResourceID(record.Outcome, functionKind)
	if err != nil {
		if fallbackFunctionID == "" {
			return FunctionView{}, err
		}
		functionID = fallbackFunctionID
	}
	function, trigger, err := c.catalog.GetFunction(functionID)
	if err != nil {
		return FunctionView{}, fmt.Errorf("resolving function operation result: %w", err)
	}
	return FunctionView{Function: function, Trigger: trigger}, nil
}

func (c *Controller) replayedVersionResult(
	result controlplane.CompletionResult,
) (VersionOperationResult, error) {
	versionID, err := affectedResourceID(result.Record.Outcome, versionKind)
	if err != nil {
		return VersionOperationResult{}, err
	}
	version, deployment, err := c.releases.Get(versionID)
	if err != nil {
		return VersionOperationResult{}, fmt.Errorf("resolving version operation result: %w", err)
	}
	return VersionOperationResult{
		Disposition: result.Disposition, Record: result.Record,
		Version: version, Deployment: deployment,
	}, nil
}

func (c *Controller) replayedRouteResult(
	result controlplane.CompletionResult,
) (RouteOperationResult, error) {
	functionID, err := affectedResourceID(result.Record.Outcome, functionKind)
	if err != nil {
		return RouteOperationResult{}, err
	}
	function, _, err := c.catalog.GetFunction(functionID)
	if err != nil {
		return RouteOperationResult{}, fmt.Errorf("resolving route operation function: %w", err)
	}
	route, err := c.routes.Get(functionID)
	if err != nil {
		return RouteOperationResult{}, fmt.Errorf("resolving route operation result: %w", err)
	}
	return RouteOperationResult{
		Disposition: result.Disposition, Record: result.Record, Route: route, Function: function,
	}, nil
}

// nextLifecycleRevision derives the Function resource revision the lifecycle
// command will produce. An unchanged lifecycle is an accepted no-op that does
// not advance the revision.
func (c *Controller) nextLifecycleRevision(input SetFunctionLifecycleInput) (uint64, error) {
	function, _, err := c.catalog.GetFunction(input.FunctionID)
	if err != nil {
		return 0, fmt.Errorf("loading function lifecycle: %w", err)
	}
	if function.Lifecycle == input.Lifecycle {
		return function.ResourceRevision, nil
	}
	return input.ExpectedResourceRevision + 1, nil
}

func (c *Controller) applyFunctionLifecycle(
	input SetFunctionLifecycleInput,
	command CommandMeta,
) (model.Function, error) {
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

func createFunctionRequest(input CreateFunctionInput) (controlplane.Request, error) {
	body, err := operationBody(input)
	if err != nil {
		return controlplane.Request{}, err
	}
	return validOperationRequest(controlplane.Request{
		Method:        http.MethodPost,
		Path:          functionsPath,
		Preconditions: controlplane.Preconditions{IfNoneMatch: true},
		BodyPresent:   true,
		Body:          body,
	})
}

func setFunctionLifecycleRequest(input SetFunctionLifecycleInput) (controlplane.Request, error) {
	path, err := functionPath(input.FunctionID, "")
	if err != nil {
		return controlplane.Request{}, err
	}
	body, err := operationBody(struct {
		Lifecycle model.FunctionLifecycle `json:"lifecycle"`
	}{Lifecycle: input.Lifecycle})
	if err != nil {
		return controlplane.Request{}, err
	}
	expected := input.ExpectedResourceRevision
	return validOperationRequest(controlplane.Request{
		Method:        http.MethodPatch,
		Path:          path,
		Preconditions: controlplane.Preconditions{ExpectedResourceRevision: &expected},
		BodyPresent:   true,
		Body:          body,
	})
}

func rotateInvocationTokenRequest(input RotateInvocationTokenInput) (controlplane.Request, error) {
	path, err := functionPath(input.FunctionID, tokenRotateSuffix)
	if err != nil {
		return controlplane.Request{}, err
	}
	expected := input.ExpectedResourceRevision
	return validOperationRequest(controlplane.Request{
		Method:        http.MethodPost,
		Path:          path,
		Preconditions: controlplane.Preconditions{ExpectedResourceRevision: &expected},
	})
}

func createVersionRequest(input CreateVersionInput) (controlplane.Request, error) {
	path, err := functionPath(input.FunctionID, versionsSuffix)
	if err != nil {
		return controlplane.Request{}, err
	}
	body, err := operationBody(input)
	if err != nil {
		return controlplane.Request{}, err
	}
	artifactDigest := input.ArtifactDigest
	return validOperationRequest(controlplane.Request{
		Method:         http.MethodPost,
		Path:           path,
		Preconditions:  controlplane.Preconditions{IfNoneMatch: true},
		BodyPresent:    true,
		Body:           body,
		ArtifactDigest: &artifactDigest,
	})
}

func publishRouteRequest(input PublishRouteInput) (controlplane.Request, error) {
	path, err := functionPath(input.FunctionID, routeSuffix)
	if err != nil {
		return controlplane.Request{}, err
	}
	body, err := operationBody(struct {
		VersionID string `json:"version_id"`
	}{VersionID: input.VersionID})
	if err != nil {
		return controlplane.Request{}, err
	}
	expected := input.ExpectedActiveRouteRevision
	return validOperationRequest(controlplane.Request{
		Method:        http.MethodPut,
		Path:          path,
		Preconditions: controlplane.Preconditions{ExpectedActiveRouteRevision: &expected},
		BodyPresent:   true,
		Body:          body,
	})
}

// functionPath builds the canonical operation path of one Function-scoped
// command. It uses the non-reusable Function ID so that a recreated Function
// name cannot collide with a retained Operation digest.
func functionPath(functionID string, suffix string) (string, error) {
	if !identifierPattern.MatchString(functionID) {
		return "", problem.Invalid("function_id", "must be a valid identifier")
	}
	return functionsPath + "/" + functionID + suffix, nil
}

func operationBody(value any) (json.RawMessage, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encoding management operation body: %w", err)
	}
	if len(body) > maxOperationBodies {
		return nil, problem.Invalid("body", "exceeds the management operation body limit")
	}
	return body, nil
}

func validOperationRequest(request controlplane.Request) (controlplane.Request, error) {
	if _, err := request.Digest(); err != nil {
		return controlplane.Request{}, fmt.Errorf("validating management operation request: %w", err)
	}
	return request, nil
}

func replayDisposition(record controlplane.Record) controlplane.CompletionDisposition {
	if record.Outcome.CredentialIssued {
		return controlplane.CompletionCredentialNotReplayable
	}
	return controlplane.CompletionReplay
}

func affectedResourceID(outcome controlplane.Outcome, kind string) (string, error) {
	for _, resource := range outcome.AffectedResources {
		if resource.Kind == kind {
			return resource.ID, nil
		}
	}
	return "", fmt.Errorf("operation outcome does not identify its %s", kind)
}

func failureError(failure *controlplane.Failure) error {
	if failure == nil {
		return errors.New("operation failure record has no safe failure")
	}
	return &problem.Error{Code: failure.Code, Message: failure.Message}
}

func problemCode(err error) problem.Code {
	var classified *problem.Error
	if errors.As(err, &classified) && problem.Known(classified.Code) {
		return classified.Code
	}
	return ""
}

func safeProblemMessage(err error) string {
	var classified *problem.Error
	if errors.As(err, &classified) {
		return classified.Message
	}
	return "operation failed"
}

func terminalOperationFailure(code problem.Code) bool {
	switch code {
	case problem.CodeInvalidArgument, problem.CodeNotFound, problem.CodeConflict,
		problem.CodeRevisionConflict, problem.CodeStaleGeneration:
		return true
	default:
		return false
	}
}
