// Package managementhttp adapts the Local Core controller to the authenticated
// management API boundary. Every write requires a persistent Operation ID and
// an explicit concurrency precondition; responses never contain verifier
// digests, route salts, or any credential beyond its single issuing response.
package managementhttp

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/controlplane"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/localcontroller"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/strictjson"
)

const (
	// OperationIDHeader carries the client-supplied persistent Operation ID
	// required by every management write.
	OperationIDHeader = "X-Minicloud-Operation-Id"

	DefaultTimeout       = 10 * time.Second
	DefaultMaxBodyBytes  = 1 << 20
	DefaultMaxConcurrent = 64
	HardMaxConcurrent    = 1024

	minTokenBytes = 32
	maxTokenBytes = 4096
	maxJSONDepth  = 32
)

var (
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	subjectPattern   = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// Controller is the Local Core write and query coordinator behind this API.
type Controller = *localcontroller.Controller

// IDSource creates opaque request identifiers.
type IDSource interface {
	NewID(prefix string) (string, error)
}

// Config bounds the authenticated management boundary.
type Config struct {
	Controller Controller
	// Token is the static management token. Its digest is compared in
	// constant time; the token itself is never persisted or logged.
	Token string
	// Subject is the stable authenticated principal recorded by Operations.
	// It must not be derived from the token. Defaults to "admin".
	Subject          string
	IDs              IDSource
	Timeout          time.Duration
	MaxBodyBytes     int64
	MaxArtifactBytes int64
	MaxConcurrent    int
}

// Handler serves the versioned management API for one Local Core process.
type Handler struct {
	controller  Controller
	tokenDigest digest.SHA256
	subject     string
	ids         IDSource
	timeout     time.Duration
	maxBody     int64
	maxArtifact int64
	slots       chan struct{}
	mux         *http.ServeMux
}

// New validates the management boundary configuration and registers the v1
// route table.
func New(config Config) (*Handler, error) {
	if config.Controller == nil {
		return nil, errors.New("management HTTP controller is required")
	}
	if len(config.Token) < minTokenBytes || len(config.Token) > maxTokenBytes {
		return nil, errors.New("management HTTP token must contain 32 to 4096 bytes")
	}
	if config.Subject == "" {
		config.Subject = "admin"
	}
	if !subjectPattern.MatchString(config.Subject) {
		return nil, errors.New("management HTTP subject must be a valid identifier")
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	if config.Timeout < time.Millisecond || config.Timeout > time.Minute {
		return nil, errors.New("management HTTP timeout is outside v1 bounds")
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if config.MaxBodyBytes < 1 || config.MaxBodyBytes > DefaultMaxBodyBytes {
		return nil, errors.New("management HTTP body limit is outside v1 bounds")
	}
	if config.MaxArtifactBytes == 0 {
		config.MaxArtifactBytes = model.MaxArtifactBytes
	}
	if config.MaxArtifactBytes < 1 || config.MaxArtifactBytes > model.MaxArtifactBytes {
		return nil, errors.New("management HTTP artifact limit is outside v1 bounds")
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = DefaultMaxConcurrent
	}
	if config.MaxConcurrent < 1 || config.MaxConcurrent > HardMaxConcurrent {
		return nil, errors.New("management HTTP concurrency is outside v1 bounds")
	}
	if config.IDs == nil {
		config.IDs = randomIDSource{}
	}

	handler := &Handler{
		controller:  config.Controller,
		tokenDigest: digest.Sum([]byte(config.Token)),
		subject:     config.Subject,
		ids:         config.IDs,
		timeout:     config.Timeout,
		maxBody:     config.MaxBodyBytes,
		maxArtifact: config.MaxArtifactBytes,
		slots:       make(chan struct{}, config.MaxConcurrent),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/profile", handler.handleProfile)
	mux.HandleFunc("POST /v1/functions", handler.handleCreateFunction)
	mux.HandleFunc("GET /v1/functions", handler.handleListFunctions)
	mux.HandleFunc("GET /v1/functions/{id}", handler.handleGetFunction)
	mux.HandleFunc("PATCH /v1/functions/{id}", handler.handleSetLifecycle)
	mux.HandleFunc("POST /v1/functions/{id}/invocation-token:rotate", handler.handleRotateToken)
	mux.HandleFunc("POST /v1/functions/{id}/versions", handler.handleCreateVersion)
	mux.HandleFunc("GET /v1/functions/{id}/versions/{version}", handler.handleGetVersion)
	mux.HandleFunc("PUT /v1/functions/{id}/route", handler.handlePutRoute)
	mux.HandleFunc("GET /v1/functions/{id}/route", handler.handleGetRoute)
	mux.HandleFunc("PUT /v1/artifacts/{digest}", handler.handlePutArtifact)
	mux.HandleFunc("GET /v1/operations/{id}", handler.handleGetOperation)
	handler.mux = mux
	return handler, nil
}

type contextKey string

const requestIDKey contextKey = "management-request-id"

// ServeHTTP authenticates and bounds one management request before routing it.
func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	requestID, err := h.requestID(request)
	if err != nil {
		writeError(response, requestID, problem.Invalid("x-request-id", "must be a valid request identifier"), nil)
		return
	}
	setPlatformHeaders(response, requestID)
	if h == nil || h.controller == nil || h.mux == nil {
		writeError(response, requestID, errors.New("management HTTP dependencies are required"), nil)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		writeError(response, requestID, classified(problem.CodeOverloaded, "management concurrency limit exceeded"), nil)
		return
	}
	if err := h.authenticate(request.Header.Values("Authorization")); err != nil {
		response.Header().Set("WWW-Authenticate", "Bearer")
		writeError(response, requestID, err, nil)
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), h.timeout)
	defer cancel()
	ctx = context.WithValue(ctx, requestIDKey, requestID)
	h.mux.ServeHTTP(response, request.WithContext(ctx))
}

func (h *Handler) handleProfile(response http.ResponseWriter, request *http.Request) {
	writeJSON(response, http.StatusOK, profileResponse{
		Profile:    "local-core",
		Replicated: false,
		Durable:    false,
		Message: "single-process development profile: control state does not survive " +
			"process restart and no quorum or failover guarantee applies",
	})
}

func (h *Handler) handleCreateFunction(response http.ResponseWriter, request *http.Request) {
	requestID := requestIDFrom(request.Context())
	key, err := h.operationKey(request)
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	if err := requireCreatePrecondition(request); err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	var body createFunctionRequest
	if err := h.decodeBody(response, request, &body); err != nil {
		writeError(response, requestID, err, nil)
		return
	}

	result, err := h.controller.CreateFunctionOperation(request.Context(), localcontroller.CreateFunctionOperationInput{
		Operation: key,
		Function: localcontroller.CreateFunctionInput{
			Name:       body.Name,
			Labels:     body.Labels,
			AuthPolicy: controlplane.AuthPolicy(body.AuthPolicy),
		},
	})
	if err != nil {
		writeError(response, requestID, writeErrorForOperation(err), credentialDetails(err, result.View))
		return
	}
	status := http.StatusOK
	if result.Disposition == controlplane.CompletionApplied {
		status = http.StatusCreated
		response.Header().Set("Location", "/v1/functions/"+result.View.Function.ID)
	}
	setRevisionHeader(response, result.View.Function.ResourceRevision)
	writeJSON(response, status, functionOperationResponse(key, result))
}

func (h *Handler) handleListFunctions(response http.ResponseWriter, request *http.Request) {
	requestID := requestIDFrom(request.Context())
	views, err := h.controller.ListFunctions(request.Context())
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	list := functionListResponse{Functions: make([]functionResponse, 0, len(views))}
	for _, view := range views {
		list.Functions = append(list.Functions, functionResponse{
			Function:    functionView(view.Function),
			HTTPTrigger: triggerView(view.Trigger),
		})
	}
	writeJSON(response, http.StatusOK, list)
}

func (h *Handler) handleGetFunction(response http.ResponseWriter, request *http.Request) {
	requestID := requestIDFrom(request.Context())
	view, err := h.controller.GetFunction(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	setRevisionHeader(response, view.Function.ResourceRevision)
	writeJSON(response, http.StatusOK, functionResponse{
		Function:    functionView(view.Function),
		HTTPTrigger: triggerView(view.Trigger),
	})
}

func (h *Handler) handleSetLifecycle(response http.ResponseWriter, request *http.Request) {
	requestID := requestIDFrom(request.Context())
	key, err := h.operationKey(request)
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	expected, err := expectedRevision(request)
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	var body setLifecycleRequest
	if err := h.decodeBody(response, request, &body); err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	lifecycle := model.FunctionLifecycle(body.Lifecycle)
	if lifecycle != model.FunctionActive && lifecycle != model.FunctionDisabled {
		writeError(response, requestID, problem.Invalid("lifecycle", "must be Active or Disabled"), nil)
		return
	}

	result, err := h.controller.SetFunctionLifecycleOperation(request.Context(), localcontroller.SetFunctionLifecycleOperationInput{
		Operation: key,
		Lifecycle: localcontroller.SetFunctionLifecycleInput{
			FunctionID:               request.PathValue("id"),
			ExpectedResourceRevision: expected,
			Lifecycle:                lifecycle,
		},
	})
	if err != nil {
		writeError(response, requestID, writeErrorForOperation(err), nil)
		return
	}
	setRevisionHeader(response, result.View.Function.ResourceRevision)
	writeJSON(response, http.StatusOK, functionOperationResponse(key, result))
}

func (h *Handler) handleRotateToken(response http.ResponseWriter, request *http.Request) {
	requestID := requestIDFrom(request.Context())
	key, err := h.operationKey(request)
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	expected, err := expectedRevision(request)
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}

	result, err := h.controller.RotateInvocationTokenOperation(request.Context(), localcontroller.RotateInvocationTokenOperationInput{
		Operation: key,
		Rotation: localcontroller.RotateInvocationTokenInput{
			FunctionID:               request.PathValue("id"),
			ExpectedResourceRevision: expected,
		},
	})
	if err != nil {
		writeError(response, requestID, writeErrorForOperation(err), credentialDetails(err, result.View))
		return
	}
	setRevisionHeader(response, result.View.Trigger.ResourceRevision)
	writeJSON(response, http.StatusOK, functionOperationResponse(key, result))
}

func (h *Handler) handlePutArtifact(response http.ResponseWriter, request *http.Request) {
	requestID := requestIDFrom(request.Context())
	expected, err := digest.ParseSHA256(request.PathValue("digest"))
	if err != nil {
		writeError(response, requestID, problem.Invalid("digest", "must be a lowercase sha-256 digest"), nil)
		return
	}
	request.Body = http.MaxBytesReader(response, request.Body, h.maxArtifact)
	info, err := h.controller.PutArtifact(request.Context(), expected, request.Body)
	if err != nil {
		writeError(response, requestID, artifactError(err), nil)
		return
	}
	status := http.StatusOK
	if info.Created {
		status = http.StatusCreated
	}
	writeJSON(response, status, artifactView(info))
}

func (h *Handler) handleCreateVersion(response http.ResponseWriter, request *http.Request) {
	requestID := requestIDFrom(request.Context())
	key, err := h.operationKey(request)
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	if err := requireCreatePrecondition(request); err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	var body createVersionRequest
	if err := h.decodeBody(response, request, &body); err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	input, err := versionInput(request.PathValue("id"), body)
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}

	result, err := h.controller.CreateVersionOperation(request.Context(), localcontroller.CreateVersionOperationInput{
		Operation: key,
		Version:   input,
	})
	if err != nil && result.Version.VersionID == "" {
		writeError(response, requestID, writeErrorForOperation(err), nil)
		return
	}
	// A committed Version whose first admission attempt hit a validator
	// infrastructure failure stays Validating; the convergence loop retries it
	// and the client polls the Version state instead of retrying the write.
	status := http.StatusOK
	if result.Disposition == controlplane.CompletionApplied {
		status = http.StatusCreated
		response.Header().Set(
			"Location",
			"/v1/functions/"+input.FunctionID+"/versions/"+result.Version.VersionID,
		)
	}
	setRevisionHeader(response, result.Version.ResourceRevision)
	writeJSON(response, status, versionResponse{
		Version:    versionView(result.Version),
		Deployment: deploymentView(result.Deployment),
		Operation:  operationView(key, result.Disposition, result.Record),
	})
}

func (h *Handler) handleGetVersion(response http.ResponseWriter, request *http.Request) {
	requestID := requestIDFrom(request.Context())
	version, deployment, err := h.controller.GetVersion(request.Context(), request.PathValue("version"))
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	if version.FunctionID != request.PathValue("id") {
		writeError(response, requestID, classified(problem.CodeNotFound, "version was not found"), nil)
		return
	}
	setRevisionHeader(response, version.ResourceRevision)
	writeJSON(response, http.StatusOK, versionResponse{
		Version:    versionView(version),
		Deployment: deploymentView(deployment),
	})
}

func (h *Handler) handlePutRoute(response http.ResponseWriter, request *http.Request) {
	requestID := requestIDFrom(request.Context())
	key, err := h.operationKey(request)
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	expected, err := expectedRouteRevision(request)
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	var body putRouteRequest
	if err := h.decodeBody(response, request, &body); err != nil {
		writeError(response, requestID, err, nil)
		return
	}

	result, err := h.controller.PublishRouteOperation(request.Context(), localcontroller.PublishRouteOperationInput{
		Operation: key,
		Route: localcontroller.PublishRouteInput{
			FunctionID:                  request.PathValue("id"),
			VersionID:                   body.VersionID,
			ExpectedActiveRouteRevision: expected,
		},
	})
	if err != nil {
		writeError(response, requestID, writeErrorForOperation(err), nil)
		return
	}
	function := functionView(result.Function)
	setRevisionHeader(response, result.Route.RouteRevision)
	writeJSON(response, http.StatusOK, routeResponse{
		Route:     routeView(result.Route),
		Function:  &function,
		Operation: operationView(key, result.Disposition, result.Record),
	})
}

func (h *Handler) handleGetRoute(response http.ResponseWriter, request *http.Request) {
	requestID := requestIDFrom(request.Context())
	route, err := h.controller.GetRoute(request.Context(), request.PathValue("id"))
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	setRevisionHeader(response, route.RouteRevision)
	writeJSON(response, http.StatusOK, routeResponse{Route: routeView(route)})
}

func (h *Handler) handleGetOperation(response http.ResponseWriter, request *http.Request) {
	requestID := requestIDFrom(request.Context())
	record, err := h.controller.GetOperation(request.Context(), controlplane.OperationKey{
		Principal:   h.subject,
		Namespace:   controlplane.DefaultNamespace,
		OperationID: request.PathValue("id"),
	})
	if err != nil {
		writeError(response, requestID, err, nil)
		return
	}
	writeJSON(response, http.StatusOK, operationRecordView(record))
}

func (h *Handler) authenticate(values []string) error {
	if len(values) != 1 {
		return classified(problem.CodeUnauthenticated, "management token is required")
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || len(token) > maxTokenBytes ||
		strings.ContainsAny(token, " \t\r\n") {
		return classified(problem.CodeUnauthenticated, "management token is invalid")
	}
	actual := digest.Sum([]byte(token))
	if subtle.ConstantTimeCompare([]byte(actual), []byte(h.tokenDigest)) != 1 {
		return classified(problem.CodeUnauthenticated, "management token is invalid")
	}
	return nil
}

func (h *Handler) requestID(request *http.Request) (string, error) {
	if h == nil || h.ids == nil {
		return "unknown", errors.New("management HTTP id source is required")
	}
	value := request.Header.Get("X-Request-ID")
	if value != "" {
		if !requestIDPattern.MatchString(value) {
			fallback, _ := h.ids.NewID("req")
			return fallback, errors.New("invalid request id")
		}
		return value, nil
	}
	value, err := h.ids.NewID("req")
	if err != nil {
		return "unknown", fmt.Errorf("generating request id: %w", err)
	}
	if !requestIDPattern.MatchString(value) {
		return "unknown", errors.New("id source returned an invalid request id")
	}
	return value, nil
}

func (h *Handler) operationKey(request *http.Request) (controlplane.OperationKey, error) {
	value := request.Header.Get(OperationIDHeader)
	if value == "" {
		return controlplane.OperationKey{}, problem.Invalid(
			strings.ToLower(OperationIDHeader),
			"is required for every management write",
		)
	}
	key := controlplane.OperationKey{
		Principal:   h.subject,
		Namespace:   controlplane.DefaultNamespace,
		OperationID: value,
	}
	if err := key.Validate(); err != nil {
		return controlplane.OperationKey{}, err
	}
	return key, nil
}

func (h *Handler) decodeBody(
	response http.ResponseWriter,
	request *http.Request,
	target any,
) error {
	request.Body = http.MaxBytesReader(response, request.Body, h.maxBody)
	data, err := strictjson.Read(request.Body, h.maxBody)
	if err != nil {
		return problem.Invalid("body", "must be JSON within the management body limit")
	}
	if err := strictjson.Validate(data, maxJSONDepth); err != nil {
		return problem.Invalid("body", "must be bounded strict JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return problem.Invalid("body", "does not match the request schema")
	}
	return nil
}

func versionInput(functionID string, body createVersionRequest) (localcontroller.CreateVersionInput, error) {
	artifactDigest, err := digest.ParseSHA256(body.ArtifactDigest)
	if err != nil {
		return localcontroller.CreateVersionInput{}, problem.Invalid("artifact_digest", "must be a lowercase sha-256 digest")
	}
	manifestDigest, err := digest.ParseSHA256(body.ManifestDigest)
	if err != nil {
		return localcontroller.CreateVersionInput{}, problem.Invalid("manifest_digest", "must be a lowercase sha-256 digest")
	}
	if body.ResourceRequest.TimeoutMS < 1 {
		return localcontroller.CreateVersionInput{}, problem.Invalid("resource_request.timeout_ms", "must be greater than zero")
	}
	capabilities := body.RequestedCapabilities
	if capabilities == nil {
		capabilities = []model.CapabilityRequest{}
	}
	return localcontroller.CreateVersionInput{
		FunctionID:     functionID,
		ArtifactDigest: artifactDigest,
		ManifestDigest: manifestDigest,
		Toolchain:      body.Toolchain,
		ResourceRequest: model.ResourceRequest{
			Timeout:        time.Duration(body.ResourceRequest.TimeoutMS) * time.Millisecond,
			MemoryMiB:      body.ResourceRequest.MemoryMiB,
			MaxConcurrency: body.ResourceRequest.MaxConcurrency,
			MaxInputBytes:  body.ResourceRequest.MaxInputBytes,
			MaxOutputBytes: body.ResourceRequest.MaxOutputBytes,
		},
		RequestedCapabilities: capabilities,
	}, nil
}

// requireCreatePrecondition enforces the explicit `If-None-Match: *` create
// precondition so that a create cannot be confused with a revision CAS write.
func requireCreatePrecondition(request *http.Request) error {
	if request.Header.Get("If-None-Match") != "*" {
		return problem.Invalid("if-none-match", "must be * for a create request")
	}
	if request.Header.Get("If-Match") != "" {
		return problem.Invalid("if-match", "is not allowed for a create request")
	}
	return nil
}

// expectedRevision parses the strong quoted `If-Match: "N"` resource-revision
// precondition required by conditional management writes.
func expectedRevision(request *http.Request) (uint64, error) {
	return parseIfMatch(request, "must be the expected resource revision as a quoted number")
}

// expectedRouteRevision parses `If-Match: "N"` as the expected active Route
// revision. Zero is valid: it publishes a Function's first Route.
func expectedRouteRevision(request *http.Request) (uint64, error) {
	value := request.Header.Get("If-Match")
	if value == "" {
		return 0, problem.Invalid("if-match", "must be the expected active route revision as a quoted number")
	}
	revision, ok := parseQuotedRevision(value)
	if !ok {
		return 0, problem.Invalid("if-match", "must be the expected active route revision as a quoted number")
	}
	return revision, nil
}

func parseIfMatch(request *http.Request, message string) (uint64, error) {
	value := request.Header.Get("If-Match")
	if value == "" {
		return 0, problem.Invalid("if-match", message)
	}
	revision, ok := parseQuotedRevision(value)
	if !ok || revision == 0 {
		return 0, problem.Invalid("if-match", message)
	}
	return revision, nil
}

func parseQuotedRevision(value string) (uint64, bool) {
	if len(value) < 3 || value[0] != '"' || value[len(value)-1] != '"' {
		return 0, false
	}
	revision, err := strconv.ParseUint(value[1:len(value)-1], 10, 64)
	if err != nil {
		return 0, false
	}
	return revision, true
}

// writeErrorForOperation keeps classified problems and converts everything
// else into the documented result-unknown contract: the client must query or
// retry its persistent Operation ID.
func writeErrorForOperation(err error) error {
	var classifiedError *problem.Error
	if errors.As(err, &classifiedError) && problem.Known(classifiedError.Code) {
		return err
	}
	return classified(problem.CodeOperationUnknown, "management write result is unknown; query the operation id")
}

// credentialDetails builds the credential_not_replayable details contract:
// resource identifiers and their current resource revisions, never plaintext.
func credentialDetails(err error, view localcontroller.FunctionView) map[string]any {
	if problemCodeOf(err) != problem.CodeCredentialNotReplayable {
		return nil
	}
	details := map[string]any{}
	if view.Function.ID != "" {
		details["function_id"] = view.Function.ID
		details["function_resource_revision"] = view.Function.ResourceRevision
	}
	if view.Trigger.ID != "" {
		details["http_trigger_id"] = view.Trigger.ID
		details["resource_revision"] = view.Trigger.ResourceRevision
	}
	if len(details) == 0 {
		return nil
	}
	return details
}

func artifactError(err error) error {
	switch {
	case errors.Is(err, artifact.ErrTooLarge):
		return problem.Invalid("artifact", "exceeds the artifact size limit")
	case errors.Is(err, artifact.ErrDigestMismatch):
		return problem.Invalid("artifact", "does not match the requested digest")
	default:
		return err
	}
}

func problemCodeOf(err error) problem.Code {
	var classifiedError *problem.Error
	if errors.As(err, &classifiedError) && problem.Known(classifiedError.Code) {
		return classifiedError.Code
	}
	return ""
}

func requestIDFrom(ctx context.Context) string {
	if value, ok := ctx.Value(requestIDKey).(string); ok && value != "" {
		return value
	}
	return "unknown"
}

func setPlatformHeaders(response http.ResponseWriter, requestID string) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.Header().Set("X-Request-ID", requestID)
}

func setRevisionHeader(response http.ResponseWriter, revision uint64) {
	response.Header().Set("ETag", `"`+strconv.FormatUint(revision, 10)+`"`)
}

func writeJSON(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

func writeError(response http.ResponseWriter, requestID string, err error, details map[string]any) {
	classifiedError := &problem.Error{
		Code:    problem.CodeOperationUnknown,
		Message: "management request could not be completed",
	}
	var public *problem.Error
	if errors.As(err, &public) && problem.Known(public.Code) {
		classifiedError = public
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(problem.HTTPStatus(classifiedError.Code))
	_ = json.NewEncoder(response).Encode(problem.NewEnvelope(classifiedError, requestID, details))
}

func classified(code problem.Code, message string) error {
	return &problem.Error{Code: code, Message: message}
}

type randomIDSource struct{}

func (randomIDSource) NewID(prefix string) (string, error) {
	var random [16]byte
	if _, err := cryptorand.Read(random[:]); err != nil {
		return "", fmt.Errorf("reading secure randomness: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(random[:]), nil
}
