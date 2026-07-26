package gatewayhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/gatewaydiscovery"
	"github.com/yourikka/minicloud/internal/gatewayinvoke"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/wasmexec"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

func TestHandlerAuthenticatesAndBuildsBoundedEnvelope(t *testing.T) {
	t.Parallel()
	invoker := &recordingGateway{result: successfulResult()}
	handler := newTestHandler(t, discovery.AuthToken, "invocation-secret", invoker, Config{})
	request := httptest.NewRequest(
		http.MethodPost,
		"/invoke/function-a/nested%20path?tag=a&tag=b",
		stringsReader("payload"),
	)
	request.Header.Set("Authorization", "Bearer invocation-secret")
	request.Header.Set("Connection", "X-Remove")
	request.Header.Set("X-Remove", "connection-scoped")
	request.Header.Set("X-Forwarded-For", "198.51.100.7")
	request.Header.Set("X-Minicloud-Internal", "private")
	request.Header.Set("X-Request-ID", "req-client")
	request.Header.Set("X-Custom", "visible")
	request.Header.Set("Idempotency-Key", "stable-key")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("X-Request-ID") != "req-client" ||
		response.Header().Get("X-Minicloud-Invocation-ID") != "inv-fixed-1" ||
		response.Header().Get("X-Minicloud-Version-ID") != "version-a" ||
		response.Header().Get("X-Minicloud-Route-Revision") != "7" ||
		response.Header().Get("X-Minicloud-Spoof") != "" ||
		response.Header().Get("Content-Length") != "" ||
		response.Header().Get("Set-Cookie") != "session=guest; Secure; HttpOnly" {
		t.Fatalf("response headers = %+v", response.Header())
	}
	calls := invoker.Calls()
	if len(calls) != 1 {
		t.Fatalf("Gateway.Invoke() calls = %d, want 1", len(calls))
	}
	call := calls[0]
	invocation := call.request.Invocation
	if call.request.FunctionID != "function-a-id" || string(call.request.AffinityKey) != "stable-key" ||
		invocation.InvocationID != "inv-fixed-1" || invocation.Method != http.MethodPost ||
		invocation.Path != "/nested%20path" || string(invocation.Body) != "payload" ||
		invocation.Trigger.ID != "trigger-a" || invocation.Trigger.ResourceRevision != 4 ||
		!slices.Equal(invocation.Query["tag"], []string{"a", "b"}) {
		t.Fatalf("Gateway.Invoke() request = %+v", call.request)
	}
	for _, hidden := range []string{
		"authorization", "connection", "x-remove", "x-forwarded-for", "x-minicloud-internal", "x-request-id",
	} {
		if _, exists := invocation.Headers[hidden]; exists {
			t.Fatalf("guest headers contain %q: %+v", hidden, invocation.Headers)
		}
	}
	if !slices.Equal(invocation.Headers["x-custom"], []string{"visible"}) {
		t.Fatalf("guest headers = %+v", invocation.Headers)
	}
	if response.Body.String() != "guest-response" {
		t.Fatalf("response body = %q", response.Body.String())
	}
}

func TestHandlerAuthenticationPolicies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		policy        discovery.AuthPolicy
		token         string
		authorization []string
		wantStatus    int
		wantCalls     int
	}{
		{
			name: "token accepted", policy: discovery.AuthToken, token: "secret",
			authorization: []string{"Bearer secret"}, wantStatus: 201, wantCalls: 1,
		},
		{name: "token missing", policy: discovery.AuthToken, token: "secret", wantStatus: 401},
		{
			name: "token wrong", policy: discovery.AuthToken, token: "secret",
			authorization: []string{"Bearer wrong"}, wantStatus: 401,
		},
		{
			name: "scheme malformed", policy: discovery.AuthToken, token: "secret",
			authorization: []string{"Basic secret"}, wantStatus: 401,
		},
		{
			name: "multiple credentials", policy: discovery.AuthToken, token: "secret",
			authorization: []string{"Bearer secret", "Bearer secret"}, wantStatus: 401,
		},
		{name: "public anonymous", policy: discovery.AuthPublic, wantStatus: 201, wantCalls: 1},
		{
			name: "public strips credential", policy: discovery.AuthPublic,
			authorization: []string{"Bearer unrelated"}, wantStatus: 201, wantCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			invoker := &recordingGateway{result: successfulResult()}
			handler := newTestHandler(t, test.policy, test.token, invoker, Config{})
			request := httptest.NewRequest(http.MethodGet, "/invoke/function-a/", nil)
			for _, value := range test.authorization {
				request.Header.Add("Authorization", value)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || len(invoker.Calls()) != test.wantCalls {
				t.Fatalf(
					"status = %d, calls = %d, want status %d calls %d",
					response.Code,
					len(invoker.Calls()),
					test.wantStatus,
					test.wantCalls,
				)
			}
			if response.Code == http.StatusUnauthorized && response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q", response.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

func TestHandlerRejectsInvalidAndOversizedRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		target     string
		body       string
		requestID  string
		limits     abi.Limits
		wantStatus int
	}{
		{name: "wrong route", target: "/v1/functions/function-a", wantStatus: 400},
		{name: "invalid function name", target: "/invoke/INVALID/", wantStatus: 400},
		{name: "invalid query encoding", target: "/invoke/function-a/?value=%zz", wantStatus: 400},
		{name: "invalid request id", target: "/invoke/function-a/", requestID: "contains space", wantStatus: 400},
		{name: "body limit", target: "/invoke/function-a/", body: "large", limits: abi.Limits{BodyBytes: 4}, wantStatus: 400},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			invoker := &recordingGateway{result: successfulResult()}
			handler := newTestHandler(t, discovery.AuthPublic, "", invoker, Config{Limits: test.limits})
			request := httptest.NewRequest(http.MethodPost, test.target, stringsReader(test.body))
			if test.requestID != "" {
				request.Header.Set("X-Request-ID", test.requestID)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || len(invoker.Calls()) != 0 {
				t.Fatalf("status = %d, calls = %d, body = %s", response.Code, len(invoker.Calls()), response.Body)
			}
			var envelope problem.Envelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil ||
				envelope.Error.Code != problem.CodeInvalidArgument || envelope.Error.RequestID == "" {
				t.Fatalf("error envelope = %+v, decode error = %v", envelope, err)
			}
		})
	}
}

func TestHandlerEnforcesLocalRateAndConcurrencyLimits(t *testing.T) {
	t.Run("rate", func(t *testing.T) {
		invoker := &recordingGateway{result: successfulResult()}
		handler := newTestHandler(t, discovery.AuthPublic, "", invoker, Config{
			RatePerSecond: rate.Limit(0.001), RateBurst: 1,
		})
		first := httptest.NewRecorder()
		handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/invoke/function-a/", nil))
		second := httptest.NewRecorder()
		handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/invoke/function-a/", nil))
		if first.Code != http.StatusCreated || second.Code != http.StatusTooManyRequests || len(invoker.Calls()) != 1 {
			t.Fatalf("rate statuses = %d, %d; calls = %d", first.Code, second.Code, len(invoker.Calls()))
		}
	})
	t.Run("concurrency", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		invoker := &recordingGateway{result: successfulResult(), started: started, release: release}
		handler := newTestHandler(t, discovery.AuthPublic, "", invoker, Config{MaxConcurrent: 1})
		firstDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/invoke/function-a/", nil))
			firstDone <- response
		}()
		<-started
		second := httptest.NewRecorder()
		handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/invoke/function-a/", nil))
		close(release)
		first := <-firstDone
		if first.Code != http.StatusCreated || second.Code != http.StatusTooManyRequests {
			t.Fatalf("concurrency statuses = %d, %d", first.Code, second.Code)
		}
	})
}

func TestHandlerMapsClassifiedGatewayErrors(t *testing.T) {
	t.Parallel()
	invoker := &recordingGateway{err: &problem.Error{
		Code: problem.CodeNoReadyReplica, Message: "no ready endpoint",
	}}
	handler := newTestHandler(t, discovery.AuthPublic, "", invoker, Config{})
	request := httptest.NewRequest(http.MethodGet, "/invoke/function-a/", nil)
	request.Header.Set("X-Request-ID", "req-known")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var envelope problem.Envelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	if response.Code != http.StatusServiceUnavailable || envelope.Error.Code != problem.CodeNoReadyReplica ||
		envelope.Error.RequestID != "req-known" {
		t.Fatalf("status = %d, envelope = %+v", response.Code, envelope)
	}
}

func TestHandlerOmitsGuestBodyForHEAD(t *testing.T) {
	t.Parallel()
	invoker := &recordingGateway{result: successfulResult()}
	handler := newTestHandler(t, discovery.AuthPublic, "", invoker, Config{})
	request := httptest.NewRequest(http.MethodHead, "/invoke/function-a/resource", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Body.Len() != 0 {
		t.Fatalf("HEAD status = %d, body = %q", response.Code, response.Body.String())
	}
}

func TestNewRejectsInvalidBoundsAndDependencies(t *testing.T) {
	t.Parallel()
	valid := Config{Discovery: staticDiscovery{}, Gateway: &recordingGateway{}}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "missing discovery", mutate: func(config *Config) { config.Discovery = nil }},
		{name: "missing gateway", mutate: func(config *Config) { config.Gateway = nil }},
		{name: "timeout above bound", mutate: func(config *Config) { config.Timeout = HardMaxTimeout + time.Nanosecond }},
		{name: "concurrency above bound", mutate: func(config *Config) { config.MaxConcurrent = HardMaxConcurrent + 1 }},
		{name: "rate above bound", mutate: func(config *Config) { config.RatePerSecond = HardMaxRatePerSecond + 1 }},
		{name: "burst above bound", mutate: func(config *Config) { config.RateBurst = HardMaxRateBurst + 1 }},
		{name: "ABI limit above bound", mutate: func(config *Config) { config.Limits.BodyBytes = abi.DefaultBodyBytes + 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config := valid
			test.mutate(&config)
			if _, err := New(config); err == nil {
				t.Fatal("New() accepted invalid configuration")
			}
		})
	}
}

type gatewayCall struct {
	request gatewayinvoke.Request
}

type recordingGateway struct {
	mu      sync.Mutex
	calls   []gatewayCall
	result  gatewayinvoke.Result
	err     error
	started chan struct{}
	release chan struct{}
}

func (g *recordingGateway) Invoke(
	_ context.Context,
	request gatewayinvoke.Request,
) (gatewayinvoke.Result, error) {
	g.mu.Lock()
	g.calls = append(g.calls, gatewayCall{request: request})
	started := g.started
	release := g.release
	result := g.result
	err := g.err
	g.mu.Unlock()
	if started != nil {
		close(started)
	}
	if release != nil {
		<-release
	}
	result.InvocationID = request.Invocation.InvocationID
	return result, err
}

func (g *recordingGateway) Calls() []gatewayCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

type staticDiscovery struct {
	views []gatewaydiscovery.View
	err   error
}

func (d staticDiscovery) LookupAll() ([]gatewaydiscovery.View, error) {
	return slices.Clone(d.views), d.err
}

type sequentialIDs struct {
	mu   sync.Mutex
	next int
}

func (s *sequentialIDs) NewID(prefix string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return prefix + "-fixed-" + strconv.Itoa(s.next), nil
}

func newTestHandler(
	t *testing.T,
	policy discovery.AuthPolicy,
	token string,
	invoker InvocationGateway,
	override Config,
) *Handler {
	t.Helper()
	view := testView(policy, token)
	override.Discovery = staticDiscovery{views: []gatewaydiscovery.View{view}}
	override.Gateway = invoker
	override.IDs = &sequentialIDs{}
	handler, err := New(override)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return handler
}

func testView(policy discovery.AuthPolicy, token string) gatewaydiscovery.View {
	var verifier *digest.SHA256
	if policy == discovery.AuthToken {
		value := digest.Sum([]byte(token))
		verifier = &value
	}
	return gatewaydiscovery.View{Snapshot: discovery.Snapshot{
		FunctionID: "function-a-id",
		Function:   discovery.Function{ID: "function-a-id", Name: "function-a"},
		Trigger: discovery.HTTPTrigger{
			ID: "trigger-a", FunctionID: "function-a-id", ResourceRevision: 4,
			Enabled: true, AuthPolicy: policy, TokenVerifierDigest: verifier,
		},
	}}
}

func successfulResult() gatewayinvoke.Result {
	return gatewayinvoke.Result{
		VersionID: "version-a", RouteRevision: 7,
		Execution: wasmexec.Result{Response: abi.Response{
			SpecVersion: abi.Version, Status: http.StatusCreated,
			Headers: abi.ResponseHeaders{
				"content-length":    []string{"999"},
				"content-type":      []string{"text/plain"},
				"set-cookie":        []string{"session=guest; Secure; HttpOnly"},
				"x-minicloud-spoof": []string{"unsafe"},
			},
			Body: []byte("guest-response"),
		}},
	}
}

func stringsReader(value string) *strings.Reader {
	return strings.NewReader(value)
}
