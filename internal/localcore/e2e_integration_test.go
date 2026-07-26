//go:build integration

package localcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/managementhttp"
	"github.com/yourikka/minicloud/internal/problem"
)

const e2eManagementToken = "0123456789abcdef0123456789abcdef"

// TestLocalCoreCompletesMVPUserFlowOverHTTP proves the bounded MVP user flow
// against one fresh process: create a Function, upload a real Go artifact,
// admit a Version through the isolated validator, publish the Route, and
// perform one authenticated synchronous invocation. It also proves the
// rejection paths: an invalid module becomes a queryable Failed Version, a
// stale Route write is refused, and a replayed create returns its original
// resource without a second state transition.
func TestLocalCoreCompletesMVPUserFlowOverHTTP(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repository root: %v", err)
	}
	binDirectory := t.TempDir()
	validatorPath := filepath.Join(binDirectory, e2eExecutableName("minicloud-validator"))
	e2eBuild(t, root, nil, validatorPath, "./cmd/minicloud-validator")
	wasmPath := filepath.Join(binDirectory, "runtime.wasm")
	e2eBuild(t, root, []string{"GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0"}, wasmPath, "./test/fixtures/wasm/runtime")
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("reading built fixture: %v", err)
	}

	core, err := New(t.Context(), Config{
		DataRoot:         t.TempDir(),
		ValidatorCommand: validatorPath,
		SyncInterval:     25 * time.Millisecond,
		HTTP:             serverConfig("127.0.0.1:0"),
		Management: ManagementConfig{
			HTTP:  serverConfig("127.0.0.1:0"),
			Token: e2eManagementToken,
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	serveContext, cancel := context.WithCancel(t.Context())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- core.Serve(serveContext, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Serve() did not stop after cancellation")
		}
		closeContext, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = core.Close(closeContext)
	})
	management := "http://" + managementAddressEventually(t, core)
	invocation := "http://" + listener.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}

	// Step 1: create the Function with its token-authenticated default Trigger.
	created := e2eDo(t, client, e2eRequest{
		method: http.MethodPost, url: management + "/v1/functions",
		operationID: "op-e2e-function", ifNoneMatch: true,
		body: `{"name":"echo","auth_policy":"token"}`,
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create function = (%d, %s), want 201", created.status, created.body)
	}
	var function struct {
		Function struct {
			ID string `json:"id"`
		} `json:"function"`
		InvocationToken string `json:"invocation_token"`
	}
	e2eDecode(t, created.body, &function)
	if function.Function.ID == "" || function.InvocationToken == "" {
		t.Fatalf("create function response = %s, want id and one-time token", created.body)
	}

	// Step 2: upload the built artifact into content-addressed storage.
	artifactDigest := digest.Sum(wasm)
	upload := e2eDo(t, client, e2eRequest{
		method: http.MethodPut, url: management + "/v1/artifacts/" + artifactDigest.String(),
		raw: wasm,
	})
	if upload.status != http.StatusCreated {
		t.Fatalf("upload artifact = (%d, %s), want 201", upload.status, upload.body)
	}

	// Step 3: create the immutable Version and observe isolated admission.
	versionRequest := e2eVersionBody(artifactDigest)
	versionCreated := e2eDo(t, client, e2eRequest{
		method: http.MethodPost, url: management + "/v1/functions/" + function.Function.ID + "/versions",
		operationID: "op-e2e-version", ifNoneMatch: true, body: versionRequest,
	})
	if versionCreated.status != http.StatusCreated {
		t.Fatalf("create version = (%d, %s), want 201", versionCreated.status, versionCreated.body)
	}
	var version struct {
		Version struct {
			VersionID string `json:"version_id"`
			State     string `json:"state"`
		} `json:"version"`
	}
	e2eDecode(t, versionCreated.body, &version)
	versionURL := management + "/v1/functions/" + function.Function.ID + "/versions/" + version.Version.VersionID
	e2eAwaitVersionState(t, client, versionURL, "Ready")

	// A replayed create with the lost-response Operation ID returns the same
	// Version without a second admission.
	replayed := e2eDo(t, client, e2eRequest{
		method: http.MethodPost, url: management + "/v1/functions/" + function.Function.ID + "/versions",
		operationID: "op-e2e-version", ifNoneMatch: true, body: versionRequest,
	})
	var replayedVersion struct {
		Version struct {
			VersionID string `json:"version_id"`
		} `json:"version"`
		Operation struct {
			Disposition string `json:"disposition"`
		} `json:"operation"`
	}
	e2eDecode(t, replayed.body, &replayedVersion)
	if replayed.status != http.StatusOK || replayedVersion.Version.VersionID != version.Version.VersionID ||
		replayedVersion.Operation.Disposition != "replay" {
		t.Fatalf("version replay = (%d, %s), want the original version", replayed.status, replayed.body)
	}

	// Step 4: publish the single-target Route under the exact route CAS.
	route := e2eDo(t, client, e2eRequest{
		method: http.MethodPut, url: management + "/v1/functions/" + function.Function.ID + "/route",
		operationID: "op-e2e-route", ifMatch: `"0"`,
		body: fmt.Sprintf(`{"version_id":%q}`, version.Version.VersionID),
	})
	if route.status != http.StatusOK {
		t.Fatalf("publish route = (%d, %s), want 200", route.status, route.body)
	}
	stale := e2eDo(t, client, e2eRequest{
		method: http.MethodPut, url: management + "/v1/functions/" + function.Function.ID + "/route",
		operationID: "op-e2e-route-stale", ifMatch: `"0"`,
		body: fmt.Sprintf(`{"version_id":%q}`, version.Version.VersionID),
	})
	e2eAssertProblem(t, stale, http.StatusConflict, problem.CodeRevisionConflict)

	// Step 5: the convergence loop places, prepares, and serves the replica.
	payload := "e2e-user-flow"
	response := e2eAwaitInvocation(t, client, invocation+"/invoke/echo/hello", function.InvocationToken, payload)
	fields := strings.Split(response, "|")
	if len(fields) != 6 || fields[0] != "1" || fields[5] != payload {
		t.Fatalf("invocation response = %q, want the fixture echo contract", response)
	}
	unauthenticated := e2eDo(t, client, e2eRequest{
		method: http.MethodPost, url: invocation + "/invoke/echo/hello", raw: []byte(payload),
		skipAuthorization: true,
	})
	e2eAssertProblem(t, unauthenticated, http.StatusUnauthorized, problem.CodeUnauthenticated)

	// Rejection path: an invalid module becomes a queryable Failed Version.
	invalid := []byte("not-a-wasm-module")
	invalidDigest := digest.Sum(invalid)
	invalidUpload := e2eDo(t, client, e2eRequest{
		method: http.MethodPut, url: management + "/v1/artifacts/" + invalidDigest.String(), raw: invalid,
	})
	if invalidUpload.status != http.StatusCreated {
		t.Fatalf("upload invalid artifact = (%d, %s), want 201", invalidUpload.status, invalidUpload.body)
	}
	invalidCreated := e2eDo(t, client, e2eRequest{
		method: http.MethodPost, url: management + "/v1/functions/" + function.Function.ID + "/versions",
		operationID: "op-e2e-invalid", ifNoneMatch: true, body: e2eVersionBody(invalidDigest),
	})
	if invalidCreated.status != http.StatusCreated {
		t.Fatalf("create invalid version = (%d, %s), want 201", invalidCreated.status, invalidCreated.body)
	}
	var invalidVersion struct {
		Version struct {
			VersionID string `json:"version_id"`
		} `json:"version"`
	}
	e2eDecode(t, invalidCreated.body, &invalidVersion)
	failed := e2eAwaitVersionState(
		t, client,
		management+"/v1/functions/"+function.Function.ID+"/versions/"+invalidVersion.Version.VersionID,
		"Failed",
	)
	if failed.Version.ValidationError == nil || failed.Version.ValidationError.Code != string(problem.CodeInvalidModule) {
		t.Fatalf("failed version = %+v, want a stable invalid_module validation error", failed.Version)
	}
}

type e2eRequest struct {
	method            string
	url               string
	operationID       string
	ifNoneMatch       bool
	ifMatch           string
	body              string
	raw               []byte
	skipAuthorization bool
}

type e2eResponse struct {
	status int
	body   []byte
}

type e2eVersionState struct {
	Version struct {
		VersionID       string `json:"version_id"`
		State           string `json:"state"`
		ValidationError *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"validation_error"`
	} `json:"version"`
}

func e2eDo(t *testing.T, client *http.Client, input e2eRequest) e2eResponse {
	t.Helper()
	var body io.Reader
	if input.body != "" {
		body = strings.NewReader(input.body)
	} else if input.raw != nil {
		body = bytes.NewReader(input.raw)
	}
	request, err := http.NewRequest(input.method, input.url, body)
	if err != nil {
		t.Fatalf("NewRequest(%s %s) error = %v", input.method, input.url, err)
	}
	if !input.skipAuthorization {
		request.Header.Set("Authorization", "Bearer "+e2eManagementToken)
	}
	if input.operationID != "" {
		request.Header.Set(managementhttp.OperationIDHeader, input.operationID)
	}
	if input.ifNoneMatch {
		request.Header.Set("If-None-Match", "*")
	}
	if input.ifMatch != "" {
		request.Header.Set("If-Match", input.ifMatch)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do(%s %s) error = %v", input.method, input.url, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	return e2eResponse{status: response.StatusCode, body: data}
}

func e2eDecode(t *testing.T, data []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", data, err)
	}
}

func e2eAssertProblem(t *testing.T, response e2eResponse, wantStatus int, wantCode problem.Code) {
	t.Helper()
	if response.status != wantStatus {
		t.Fatalf("status = (%d, %s), want %d", response.status, response.body, wantStatus)
	}
	var envelope problem.Envelope
	e2eDecode(t, response.body, &envelope)
	if envelope.Error.Code != wantCode {
		t.Fatalf("problem code = %q, want %q", envelope.Error.Code, wantCode)
	}
}

func e2eAwaitVersionState(
	t *testing.T,
	client *http.Client,
	url string,
	wantState string,
) e2eVersionState {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		response := e2eDo(t, client, e2eRequest{method: http.MethodGet, url: url})
		if response.status == http.StatusOK {
			var state e2eVersionState
			e2eDecode(t, response.body, &state)
			if state.Version.State == wantState {
				return state
			}
			if state.Version.State == "Failed" && wantState != "Failed" {
				t.Fatalf("version failed while waiting for %s: %s", wantState, response.body)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("version did not reach %s: (%d, %s)", wantState, response.status, response.body)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func e2eAwaitInvocation(
	t *testing.T,
	client *http.Client,
	url string,
	token string,
	payload string,
) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(payload))
		if err != nil {
			t.Fatalf("NewRequest(%s) error = %v", url, err)
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("reading invocation response: read=%v close=%v", readErr, closeErr)
			}
			if response.StatusCode == http.StatusOK {
				return string(body)
			}
			if time.Now().After(deadline) {
				t.Fatalf("invocation did not become ready: (%d, %s)", response.StatusCode, body)
			}
		} else if time.Now().After(deadline) {
			t.Fatalf("invocation did not become ready: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func e2eVersionBody(artifactDigest digest.SHA256) string {
	return fmt.Sprintf(
		`{"artifact_digest":%q,"manifest_digest":%q,`+
			`"toolchain_metadata":{"name":"go","version":"1.26","provenance":"unverified"},`+
			`"resource_request":{"timeout_ms":2000,"memory_mib":128,"max_concurrency":1,`+
			`"max_input_bytes":1048576,"max_output_bytes":1048576},"requested_capabilities":[]}`,
		artifactDigest.String(),
		digest.Sum([]byte("e2e-manifest")).String(),
	)
}

func e2eBuild(t *testing.T, root string, env []string, output string, packagePath string) {
	t.Helper()
	command := exec.Command("go", "build", "-trimpath", "-o", output, packagePath)
	command.Dir = root
	command.Env = append(os.Environ(), env...)
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", packagePath, err, data)
	}
}

func e2eExecutableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
