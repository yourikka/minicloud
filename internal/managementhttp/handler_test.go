package managementhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yourikka/minicloud/internal/artifact"
	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/localcontroller"
	"github.com/yourikka/minicloud/internal/problem"
	validatorprotocol "github.com/yourikka/minicloud/internal/validator/protocol"
)

const testToken = "0123456789abcdef0123456789abcdef"

func TestHandlerRequiresManagementToken(t *testing.T) {
	t.Parallel()
	handler := newHandlerHarness(t)

	tests := []struct {
		name          string
		authorization string
	}{
		{name: "missing token"},
		{name: "wrong token", authorization: "Bearer wrong-token-wrong-token-wrong-token"},
		{name: "wrong scheme", authorization: "Basic " + testToken},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := httptest.NewRequest(http.MethodGet, "/v1/functions", nil)
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			assertProblemResponse(t, recorder, http.StatusUnauthorized, problem.CodeUnauthenticated)
			if recorder.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q, want Bearer", recorder.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

func TestHandlerCreateFunctionAppliesOnceAndReplays(t *testing.T) {
	t.Parallel()
	handler := newHandlerHarness(t)

	first := doJSON(t, handler, createRequest{
		method: http.MethodPost, path: "/v1/functions", operationID: "op-create",
		ifNoneMatch: true, body: `{"name":"echo","auth_policy":"public"}`,
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s, want 201", first.Code, first.Body.String())
	}
	created := decodeFunctionResponse(t, first)
	if created.Function.Name != "echo" || created.Function.Lifecycle != "Active" {
		t.Fatalf("create function = %+v, want an active echo function", created.Function)
	}
	if created.Operation == nil || created.Operation.Disposition != "applied" {
		t.Fatalf("create operation = %+v, want an applied disposition", created.Operation)
	}
	if first.Header().Get("Location") != "/v1/functions/"+created.Function.ID {
		t.Fatalf("Location = %q, want the created function path", first.Header().Get("Location"))
	}
	if first.Header().Get("ETag") != `"1"` {
		t.Fatalf("ETag = %q, want the initial resource revision", first.Header().Get("ETag"))
	}

	replay := doJSON(t, handler, createRequest{
		method: http.MethodPost, path: "/v1/functions", operationID: "op-create",
		ifNoneMatch: true, body: `{"name":"echo","auth_policy":"public"}`,
	})
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body = %s, want 200", replay.Code, replay.Body.String())
	}
	replayed := decodeFunctionResponse(t, replay)
	if replayed.Operation == nil || replayed.Operation.Disposition != "replay" {
		t.Fatalf("replay operation = %+v, want a replay disposition", replayed.Operation)
	}
	if replayed.Function.ID != created.Function.ID {
		t.Fatalf("replay function id = %q, want %q", replayed.Function.ID, created.Function.ID)
	}

	conflict := doJSON(t, handler, createRequest{
		method: http.MethodPost, path: "/v1/functions", operationID: "op-create",
		ifNoneMatch: true, body: `{"name":"other","auth_policy":"public"}`,
	})
	assertProblemResponse(t, conflict, http.StatusConflict, problem.CodeConflict)
}

func TestHandlerCreateFunctionRequiresWritePreconditions(t *testing.T) {
	t.Parallel()
	handler := newHandlerHarness(t)

	missingOperation := doJSON(t, handler, createRequest{
		method: http.MethodPost, path: "/v1/functions",
		ifNoneMatch: true, body: `{"name":"echo","auth_policy":"public"}`,
	})
	assertProblemResponse(t, missingOperation, http.StatusBadRequest, problem.CodeInvalidArgument)

	missingPrecondition := doJSON(t, handler, createRequest{
		method: http.MethodPost, path: "/v1/functions", operationID: "op-create",
		body: `{"name":"echo","auth_policy":"public"}`,
	})
	assertProblemResponse(t, missingPrecondition, http.StatusBadRequest, problem.CodeInvalidArgument)

	unknownField := doJSON(t, handler, createRequest{
		method: http.MethodPost, path: "/v1/functions", operationID: "op-create",
		ifNoneMatch: true, body: `{"name":"echo","auth_policy":"public","extra":true}`,
	})
	assertProblemResponse(t, unknownField, http.StatusBadRequest, problem.CodeInvalidArgument)
}

func TestHandlerNeverSerializesVerifierAndIssuesCredentialOnce(t *testing.T) {
	t.Parallel()
	handler := newHandlerHarness(t)

	first := doJSON(t, handler, createRequest{
		method: http.MethodPost, path: "/v1/functions", operationID: "op-token",
		ifNoneMatch: true, body: `{"name":"secure","auth_policy":"token"}`,
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s, want 201", first.Code, first.Body.String())
	}
	created := decodeFunctionResponse(t, first)
	if created.InvocationToken == "" {
		t.Fatalf("create response = %s, want a one-time invocation token", first.Body.String())
	}
	if strings.Contains(first.Body.String(), "token_verifier_digest") {
		t.Fatalf("create response leaked a verifier digest: %s", first.Body.String())
	}

	replay := doJSON(t, handler, createRequest{
		method: http.MethodPost, path: "/v1/functions", operationID: "op-token",
		ifNoneMatch: true, body: `{"name":"secure","auth_policy":"token"}`,
	})
	assertProblemResponse(t, replay, http.StatusConflict, problem.CodeCredentialNotReplayable)
	var envelope problem.Envelope
	if err := json.Unmarshal(replay.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("Unmarshal(replay envelope) error = %v", err)
	}
	if envelope.Error.Details["http_trigger_id"] == "" || envelope.Error.Details["resource_revision"] == nil {
		t.Fatalf("replay details = %+v, want resource identity and current revision", envelope.Error.Details)
	}
	if strings.Contains(replay.Body.String(), created.InvocationToken) {
		t.Fatalf("replay response leaked the issued credential")
	}

	rotate := doJSON(t, handler, createRequest{
		method: http.MethodPost, path: "/v1/functions/" + created.Function.ID + "/invocation-token:rotate",
		operationID: "op-rotate", ifMatch: `"1"`,
	})
	if rotate.Code != http.StatusOK {
		t.Fatalf("rotate status = %d, body = %s, want 200", rotate.Code, rotate.Body.String())
	}
	rotated := decodeFunctionResponse(t, rotate)
	if rotated.InvocationToken == "" || rotated.InvocationToken == created.InvocationToken {
		t.Fatalf("rotate token = %q, want a fresh credential", rotated.InvocationToken)
	}
	if rotate.Header().Get("ETag") != `"2"` {
		t.Fatalf("rotate ETag = %q, want the advanced trigger revision", rotate.Header().Get("ETag"))
	}
}

func TestHandlerCompletesPublishFlowWithCASHeaders(t *testing.T) {
	t.Parallel()
	handler := newHandlerHarness(t)
	created := createTestFunction(t, handler, "flow")

	artifactBytes := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	artifactDigest := digest.Sum(artifactBytes)
	upload := doRaw(t, handler, http.MethodPut, "/v1/artifacts/"+artifactDigest.String(), artifactBytes)
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s, want 201", upload.Code, upload.Body.String())
	}
	repeat := doRaw(t, handler, http.MethodPut, "/v1/artifacts/"+artifactDigest.String(), artifactBytes)
	if repeat.Code != http.StatusOK {
		t.Fatalf("repeat upload status = %d, want 200 for the existing artifact", repeat.Code)
	}

	versionBody := fmt.Sprintf(
		`{"artifact_digest":%q,"manifest_digest":%q,`+
			`"toolchain_metadata":{"name":"go","version":"1.26","provenance":"unverified"},`+
			`"resource_request":{"timeout_ms":1000,"memory_mib":64,"max_concurrency":1,`+
			`"max_input_bytes":1024,"max_output_bytes":1024},"requested_capabilities":[]}`,
		artifactDigest.String(),
		digest.Sum([]byte("canonical-manifest")).String(),
	)
	versionResult := doJSON(t, handler, createRequest{
		method: http.MethodPost, path: "/v1/functions/" + created.Function.ID + "/versions",
		operationID: "op-version", ifNoneMatch: true, body: versionBody,
	})
	if versionResult.Code != http.StatusCreated {
		t.Fatalf("version status = %d, body = %s, want 201", versionResult.Code, versionResult.Body.String())
	}
	var version versionResponseBody
	if err := json.Unmarshal(versionResult.Body.Bytes(), &version); err != nil {
		t.Fatalf("Unmarshal(version) error = %v", err)
	}
	if version.Version.State != "Ready" || version.Deployment == nil || version.Deployment.Generation != 1 {
		t.Fatalf("version = %+v, want a ready version with generation one", version)
	}

	route := doJSON(t, handler, createRequest{
		method: http.MethodPut, path: "/v1/functions/" + created.Function.ID + "/route",
		operationID: "op-route", ifMatch: `"0"`,
		body: fmt.Sprintf(`{"version_id":%q}`, version.Version.VersionID),
	})
	if route.Code != http.StatusOK {
		t.Fatalf("route status = %d, body = %s, want 200", route.Code, route.Body.String())
	}
	var published routeResponseBody
	if err := json.Unmarshal(route.Body.Bytes(), &published); err != nil {
		t.Fatalf("Unmarshal(route) error = %v", err)
	}
	if published.Route.RouteRevision != 1 || !published.Route.Enabled {
		t.Fatalf("route = %+v, want an enabled first revision", published.Route)
	}
	if published.Function == nil || published.Function.ActiveRouteRevision != 1 {
		t.Fatalf("route function = %+v, want an advanced active pointer", published.Function)
	}
	if strings.Contains(route.Body.String(), `"salt":`) {
		t.Fatalf("route response leaked the hashing salt: %s", route.Body.String())
	}

	stale := doJSON(t, handler, createRequest{
		method: http.MethodPut, path: "/v1/functions/" + created.Function.ID + "/route",
		operationID: "op-route-stale", ifMatch: `"0"`,
		body: fmt.Sprintf(`{"version_id":%q}`, version.Version.VersionID),
	})
	assertProblemResponse(t, stale, http.StatusConflict, problem.CodeRevisionConflict)

	get := doRaw(t, handler, http.MethodGet, "/v1/functions/"+created.Function.ID+"/route", nil)
	if get.Code != http.StatusOK || get.Header().Get("ETag") != `"1"` {
		t.Fatalf("get route = (%d, %q), want 200 with the route revision ETag",
			get.Code, get.Header().Get("ETag"))
	}
}

func TestHandlerLifecyclePatchUsesExactRevisionCAS(t *testing.T) {
	t.Parallel()
	handler := newHandlerHarness(t)
	created := createTestFunction(t, handler, "patchable")

	disable := doJSON(t, handler, createRequest{
		method: http.MethodPatch, path: "/v1/functions/" + created.Function.ID,
		operationID: "op-disable", ifMatch: `"1"`, body: `{"lifecycle":"Disabled"}`,
	})
	if disable.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body = %s, want 200", disable.Code, disable.Body.String())
	}
	disabled := decodeFunctionResponse(t, disable)
	if disabled.Function.Lifecycle != "Disabled" || disabled.Function.ResourceRevision != 2 {
		t.Fatalf("disable function = %+v, want a disabled revision two", disabled.Function)
	}

	stale := doJSON(t, handler, createRequest{
		method: http.MethodPatch, path: "/v1/functions/" + created.Function.ID,
		operationID: "op-stale", ifMatch: `"1"`, body: `{"lifecycle":"Active"}`,
	})
	assertProblemResponse(t, stale, http.StatusConflict, problem.CodeRevisionConflict)

	invalid := doJSON(t, handler, createRequest{
		method: http.MethodPatch, path: "/v1/functions/" + created.Function.ID,
		operationID: "op-invalid", ifMatch: `"2"`, body: `{"lifecycle":"Deleting"}`,
	})
	assertProblemResponse(t, invalid, http.StatusBadRequest, problem.CodeInvalidArgument)
}

func TestHandlerReturnsRetainedOperationRecord(t *testing.T) {
	t.Parallel()
	handler := newHandlerHarness(t)
	created := createTestFunction(t, handler, "records")

	record := doRaw(t, handler, http.MethodGet, "/v1/operations/op-func-records", nil)
	if record.Code != http.StatusOK {
		t.Fatalf("operation status = %d, body = %s, want 200", record.Code, record.Body.String())
	}
	var body struct {
		OperationID string `json:"operation_id"`
		Principal   string `json:"principal"`
		Status      string `json:"status"`
		Affected    []struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"affected_resources"`
	}
	if err := json.Unmarshal(record.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal(operation) error = %v", err)
	}
	if body.OperationID != "op-func-records" || body.Principal != "admin" || body.Status != "succeeded" {
		t.Fatalf("operation record = %+v, want the retained create success", body)
	}
	if len(body.Affected) != 2 || body.Affected[0].ID != created.Function.ID {
		t.Fatalf("operation affected = %+v, want the created function first", body.Affected)
	}

	missing := doRaw(t, handler, http.MethodGet, "/v1/operations/op-missing", nil)
	assertProblemResponse(t, missing, http.StatusNotFound, problem.CodeNotFound)
}

func TestHandlerArtifactUploadRejectsDigestMismatch(t *testing.T) {
	t.Parallel()
	handler := newHandlerHarness(t)
	expected := digest.Sum([]byte("expected-bytes"))
	response := doRaw(t, handler, http.MethodPut, "/v1/artifacts/"+expected.String(), []byte("different-bytes"))
	assertProblemResponse(t, response, http.StatusBadRequest, problem.CodeInvalidArgument)

	invalidDigest := doRaw(t, handler, http.MethodPut, "/v1/artifacts/not-a-digest", []byte("payload"))
	assertProblemResponse(t, invalidDigest, http.StatusBadRequest, problem.CodeInvalidArgument)
}

func TestHandlerProfileStatesLocalCoreLimits(t *testing.T) {
	t.Parallel()
	handler := newHandlerHarness(t)
	response := doRaw(t, handler, http.MethodGet, "/v1/profile", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("profile status = %d, want 200", response.Code)
	}
	var profile profileResponse
	if err := json.Unmarshal(response.Body.Bytes(), &profile); err != nil {
		t.Fatalf("Unmarshal(profile) error = %v", err)
	}
	if profile.Profile != "local-core" || profile.Replicated || profile.Durable {
		t.Fatalf("profile = %+v, want an explicit non-replicated local profile", profile)
	}
}

type createRequest struct {
	method      string
	path        string
	operationID string
	ifNoneMatch bool
	ifMatch     string
	body        string
}

type functionResponseBody struct {
	Function struct {
		ID                  string `json:"id"`
		Name                string `json:"name"`
		Lifecycle           string `json:"lifecycle"`
		ResourceRevision    uint64 `json:"resource_revision"`
		ActiveRouteRevision uint64 `json:"active_route_revision"`
	} `json:"function"`
	HTTPTrigger *struct {
		ID               string `json:"id"`
		AuthPolicy       string `json:"auth_policy"`
		ResourceRevision uint64 `json:"resource_revision"`
	} `json:"http_trigger"`
	InvocationToken string `json:"invocation_token"`
	Operation       *struct {
		ID               string `json:"id"`
		Disposition      string `json:"disposition"`
		RaftAppliedIndex uint64 `json:"raft_applied_index"`
	} `json:"operation"`
}

type versionResponseBody struct {
	Version struct {
		VersionID        string `json:"version_id"`
		State            string `json:"state"`
		ResourceRevision uint64 `json:"resource_revision"`
	} `json:"version"`
	Deployment *struct {
		Generation uint64 `json:"generation"`
	} `json:"deployment"`
}

type routeResponseBody struct {
	Route struct {
		RouteRevision uint64 `json:"route_revision"`
		Enabled       bool   `json:"enabled"`
	} `json:"route"`
	Function *struct {
		ActiveRouteRevision uint64 `json:"active_route_revision"`
	} `json:"function"`
}

func newHandlerHarness(t *testing.T) *Handler {
	t.Helper()
	store, err := artifact.Open(artifact.Config{Root: t.TempDir(), MaxArtifactBytes: 1 << 20})
	if err != nil {
		t.Fatalf("artifact.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	controller, err := localcontroller.New(localcontroller.Config{
		Artifacts: store,
		Validator: acceptAllValidator{},
	})
	if err != nil {
		t.Fatalf("localcontroller.New() error = %v", err)
	}
	handler, err := New(Config{Controller: controller, Token: testToken})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return handler
}

func createTestFunction(t *testing.T, handler *Handler, name string) functionResponseBody {
	t.Helper()
	response := doJSON(t, handler, createRequest{
		method: http.MethodPost, path: "/v1/functions", operationID: "op-func-" + name,
		ifNoneMatch: true, body: fmt.Sprintf(`{"name":%q,"auth_policy":"public"}`, name),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s, want 201", response.Code, response.Body.String())
	}
	return decodeFunctionResponse(t, response)
}

func doJSON(t *testing.T, handler *Handler, input createRequest) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if input.body != "" {
		body = strings.NewReader(input.body)
	}
	request := httptest.NewRequest(input.method, input.path, body)
	request.Header.Set("Authorization", "Bearer "+testToken)
	if input.body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if input.operationID != "" {
		request.Header.Set(OperationIDHeader, input.operationID)
	}
	if input.ifNoneMatch {
		request.Header.Set("If-None-Match", "*")
	}
	if input.ifMatch != "" {
		request.Header.Set("If-Match", input.ifMatch)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func doRaw(
	t *testing.T,
	handler *Handler,
	method string,
	path string,
	payload []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Authorization", "Bearer "+testToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeFunctionResponse(t *testing.T, recorder *httptest.ResponseRecorder) functionResponseBody {
	t.Helper()
	var body functionResponseBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal(function response) error = %v: %s", err, recorder.Body.String())
	}
	return body
}

func assertProblemResponse(
	t *testing.T,
	recorder *httptest.ResponseRecorder,
	wantStatus int,
	wantCode problem.Code,
) {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status = %d, body = %s, want %d", recorder.Code, recorder.Body.String(), wantStatus)
	}
	var envelope problem.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("Unmarshal(problem envelope) error = %v: %s", err, recorder.Body.String())
	}
	if envelope.Error.Code != wantCode {
		t.Fatalf("problem code = %q, body = %s, want %q", envelope.Error.Code, recorder.Body.String(), wantCode)
	}
	if envelope.Error.RequestID == "" {
		t.Fatalf("problem request id is empty: %s", recorder.Body.String())
	}
}

type acceptAllValidator struct{}

func (acceptAllValidator) Validate(
	ctx context.Context,
	request validatorprotocol.Request,
	artifactFile io.ReadCloser,
) (validatorprotocol.Report, error) {
	if ctx == nil {
		return validatorprotocol.Report{}, errors.New("test validator context is required")
	}
	if err := ctx.Err(); err != nil {
		return validatorprotocol.Report{}, err
	}
	_, readErr := io.ReadAll(artifactFile)
	closeErr := artifactFile.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return validatorprotocol.Report{}, err
	}
	return validatorprotocol.Report{
		SchemaVersion:         validatorprotocol.SchemaVersion,
		ValidationID:          request.ValidationID,
		Valid:                 true,
		Code:                  validatorprotocol.CodeOK,
		Reason:                "accepted",
		Message:               "module is compatible with the locked v1 runtime profile",
		ArtifactDigest:        request.ArtifactDigest,
		ArtifactSize:          request.ArtifactSize,
		RuntimeName:           validatorprotocol.RuntimeName,
		RuntimeVersion:        validatorprotocol.RuntimeVersion,
		RuntimeFeatureProfile: validatorprotocol.FeatureProfile,
		RuntimeEngine:         request.RuntimeEngine,
		Imports:               []validatorprotocol.Import{},
		Exports:               []string{"_start"},
	}, nil
}
