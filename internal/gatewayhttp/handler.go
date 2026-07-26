// Package gatewayhttp adapts the public default HTTP Trigger to the bounded
// invocation ABI without exposing platform credentials to guest code.
package gatewayhttp

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/yourikka/minicloud/internal/digest"
	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/gatewaydiscovery"
	"github.com/yourikka/minicloud/internal/gatewayinvoke"
	"github.com/yourikka/minicloud/internal/problem"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

const (
	DefaultTimeout       = 10 * time.Second
	HardMaxTimeout       = 10 * time.Second
	DefaultMaxConcurrent = 256
	HardMaxConcurrent    = 4096
	DefaultRatePerSecond = 100
	DefaultRateBurst     = 200
	HardMaxRatePerSecond = 10_000
	HardMaxRateBurst     = 20_000
	maxTokenBytes        = 4096
)

var (
	functionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	requestIDPattern    = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

// DiscoverySource returns one atomic, serving-safe view of every Function.
type DiscoverySource interface {
	LookupAll() ([]gatewaydiscovery.View, error)
}

// InvocationGateway is the already-fenced Gateway coordinator.
type InvocationGateway interface {
	Invoke(context.Context, gatewayinvoke.Request) (gatewayinvoke.Result, error)
}

// IDSource creates opaque request and invocation identifiers.
type IDSource interface {
	NewID(string) (string, error)
}

// Config bounds the public synchronous invocation boundary.
type Config struct {
	Discovery     DiscoverySource
	Gateway       InvocationGateway
	IDs           IDSource
	Limits        abi.Limits
	Timeout       time.Duration
	MaxConcurrent int
	RatePerSecond rate.Limit
	RateBurst     int
}

// Handler is safe for concurrent use by one Gateway process.
type Handler struct {
	discovery DiscoverySource
	gateway   InvocationGateway
	ids       IDSource
	limits    abi.Limits
	timeout   time.Duration
	slots     chan struct{}
	rate      *rate.Limiter
}

// New validates all public request bounds and creates an invocation Handler.
func New(config Config) (*Handler, error) {
	if config.Discovery == nil || config.Gateway == nil {
		return nil, errors.New("gateway HTTP discovery and invocation dependencies are required")
	}
	limits, err := config.Limits.Effective()
	if err != nil {
		return nil, fmt.Errorf("normalizing gateway HTTP ABI limits: %w", err)
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = DefaultMaxConcurrent
	}
	if config.RatePerSecond == 0 {
		config.RatePerSecond = DefaultRatePerSecond
	}
	if config.RateBurst == 0 {
		config.RateBurst = DefaultRateBurst
	}
	if config.Timeout < time.Millisecond || config.Timeout > HardMaxTimeout {
		return nil, errors.New("gateway HTTP timeout is outside v1 bounds")
	}
	if config.MaxConcurrent < 1 || config.MaxConcurrent > HardMaxConcurrent {
		return nil, errors.New("gateway HTTP concurrency is outside v1 bounds")
	}
	if config.RatePerSecond <= 0 || config.RatePerSecond > HardMaxRatePerSecond ||
		config.RateBurst < 1 || config.RateBurst > HardMaxRateBurst {
		return nil, errors.New("gateway HTTP rate limit is outside v1 bounds")
	}
	if config.IDs == nil {
		config.IDs = randomIDSource{}
	}
	return &Handler{
		discovery: config.Discovery,
		gateway:   config.Gateway,
		ids:       config.IDs,
		limits:    limits,
		timeout:   config.Timeout,
		slots:     make(chan struct{}, config.MaxConcurrent),
		rate:      rate.NewLimiter(config.RatePerSecond, config.RateBurst),
	}, nil
}

// ServeHTTP authenticates, bounds, and invokes one default HTTP Trigger.
func (h *Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	requestID, err := h.requestID(request)
	if err != nil {
		h.writeError(response, requestID, problem.Invalid("x-request-id", "must be a valid request identifier"))
		return
	}
	h.setPlatformHeaders(response, requestID)
	if h == nil || h.discovery == nil || h.gateway == nil || h.ids == nil || h.rate == nil {
		h.writeError(response, requestID, errors.New("gateway HTTP dependencies are required"))
		return
	}
	if !h.rate.Allow() {
		h.writeError(response, requestID, classified(problem.CodeOverloaded, "gateway request rate limit exceeded"))
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		h.writeError(response, requestID, classified(problem.CodeOverloaded, "gateway concurrency limit exceeded"))
		return
	}

	functionName, guestPath, err := invocationPath(request.URL.EscapedPath())
	if err != nil {
		h.writeError(response, requestID, err)
		return
	}
	view, err := h.lookupFunction(functionName)
	if err != nil {
		h.writeError(response, requestID, err)
		return
	}
	if err := authenticate(request.Header.Values("Authorization"), view.Snapshot.Trigger); err != nil {
		response.Header().Set("WWW-Authenticate", "Bearer")
		h.writeError(response, requestID, err)
		return
	}

	invocationID, err := h.ids.NewID("inv")
	if err != nil {
		h.writeError(response, requestID, fmt.Errorf("generating invocation id: %w", err))
		return
	}
	invocation, err := h.buildInvocation(response, request, view.Snapshot, invocationID, guestPath)
	if err != nil {
		h.writeError(response, requestID, err)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), h.timeout)
	defer cancel()
	result, err := h.gateway.Invoke(ctx, gatewayinvoke.Request{
		FunctionID:  view.Snapshot.FunctionID,
		AffinityKey: affinityKey(request, requestID),
		Invocation:  invocation,
		Timeout:     h.timeout,
	})
	if err != nil {
		h.writeError(response, requestID, err)
		return
	}
	h.writeResult(response, request.Method, requestID, result)
}

func (h *Handler) requestID(request *http.Request) (string, error) {
	if h == nil || h.ids == nil {
		return "unknown", errors.New("gateway HTTP id source is required")
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

func (h *Handler) lookupFunction(name string) (gatewaydiscovery.View, error) {
	views, err := h.discovery.LookupAll()
	if err != nil {
		return gatewaydiscovery.View{}, fmt.Errorf("looking up HTTP serving views: %w", err)
	}
	var match gatewaydiscovery.View
	found := false
	for _, view := range views {
		if view.Snapshot.Function.Name != name {
			continue
		}
		if found {
			return gatewaydiscovery.View{}, classified(
				problem.CodeControlPlaneStale,
				"serving view contains a duplicate function name",
			)
		}
		match = view
		found = true
	}
	if !found {
		return gatewaydiscovery.View{}, classified(problem.CodeNotFound, "function was not found")
	}
	return match, nil
}

func (h *Handler) buildInvocation(
	response http.ResponseWriter,
	request *http.Request,
	snapshot discovery.Snapshot,
	invocationID string,
	guestPath string,
) (abi.Request, error) {
	request.Body = http.MaxBytesReader(response, request.Body, int64(h.limits.BodyBytes))
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return abi.Request{}, problem.Invalid("body", "exceeds the gateway request limit")
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return abi.Request{}, problem.Invalid("query", "must use valid URL encoding")
	}
	deadline := time.Now().Add(h.timeout)
	if parent, exists := request.Context().Deadline(); exists && parent.Before(deadline) {
		deadline = parent
	}
	invocation := abi.Request{
		SpecVersion:    abi.Version,
		InvocationID:   invocationID,
		Method:         request.Method,
		Path:           guestPath,
		Query:          abi.Query(query),
		Headers:        guestHeaders(request.Header),
		Body:           body,
		DeadlineUnixMS: deadline.UnixMilli(),
		Trigger: abi.Trigger{
			Type: "http", ID: snapshot.Trigger.ID,
			ResourceRevision: snapshot.Trigger.ResourceRevision,
		},
	}
	if err := abi.EncodeRequest(io.Discard, invocation, h.limits); err != nil {
		return abi.Request{}, problem.Invalid("request", "exceeds the invocation ABI limits")
	}
	return invocation, nil
}

func (h *Handler) writeResult(
	response http.ResponseWriter,
	method string,
	requestID string,
	result gatewayinvoke.Result,
) {
	h.setPlatformHeaders(response, requestID)
	for name, values := range result.Execution.Response.Headers {
		if reservedResponseHeader(name) {
			continue
		}
		for _, value := range values {
			response.Header().Add(name, value)
		}
	}
	response.Header().Set("X-Minicloud-Invocation-ID", result.InvocationID)
	response.Header().Set("X-Minicloud-Version-ID", result.VersionID)
	response.Header().Set("X-Minicloud-Route-Revision", fmt.Sprintf("%d", result.RouteRevision))
	response.WriteHeader(result.Execution.Response.Status)
	if method != http.MethodHead {
		_, _ = response.Write(result.Execution.Response.Body)
	}
}

func (h *Handler) writeError(response http.ResponseWriter, requestID string, err error) {
	h.setPlatformHeaders(response, requestID)
	classifiedError := &problem.Error{
		Code: problem.CodeWorkerLost, Message: "invocation failed",
	}
	var public *problem.Error
	if errors.As(err, &public) && problem.Known(public.Code) {
		classifiedError = public
	}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(problem.HTTPStatus(classifiedError.Code))
	_ = json.NewEncoder(response).Encode(problem.NewEnvelope(classifiedError, requestID, nil))
}

func (h *Handler) setPlatformHeaders(response http.ResponseWriter, requestID string) {
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.Header().Set("X-Request-ID", requestID)
}

func invocationPath(escapedPath string) (string, string, error) {
	remainder, found := strings.CutPrefix(escapedPath, "/invoke/")
	if !found || remainder == "" {
		return "", "", problem.Invalid("path", "must match /invoke/{name}/{path...}")
	}
	name, suffix, hasSuffix := strings.Cut(remainder, "/")
	if !functionNamePattern.MatchString(name) {
		return "", "", problem.Invalid("function_name", "must be a valid function name")
	}
	guestPath := "/"
	if hasSuffix {
		guestPath += suffix
	}
	return name, guestPath, nil
}

func authenticate(values []string, trigger discovery.HTTPTrigger) error {
	if trigger.AuthPolicy == discovery.AuthPublic {
		return nil
	}
	if trigger.AuthPolicy != discovery.AuthToken || trigger.TokenVerifierDigest == nil || len(values) != 1 {
		return classified(problem.CodeUnauthenticated, "invocation token is required")
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || len(token) > maxTokenBytes ||
		strings.ContainsAny(token, " \t\r\n") {
		return classified(problem.CodeUnauthenticated, "invocation token is invalid")
	}
	actual := digest.Sum([]byte(token))
	if subtle.ConstantTimeCompare([]byte(actual), []byte(*trigger.TokenVerifierDigest)) != 1 {
		return classified(problem.CodeUnauthenticated, "invocation token is invalid")
	}
	return nil
}

func guestHeaders(source http.Header) abi.RequestHeaders {
	connectionHeaders := make(map[string]struct{})
	for _, value := range source.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			connectionHeaders[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
		}
	}
	result := make(abi.RequestHeaders)
	for name, values := range source {
		lower := strings.ToLower(name)
		if reservedRequestHeader(lower) {
			continue
		}
		if _, blocked := connectionHeaders[lower]; blocked {
			continue
		}
		result[lower] = append([]string{}, values...)
	}
	return result
}

func reservedRequestHeader(name string) bool {
	switch name {
	case "authorization", "connection", "content-length", "forwarded", "keep-alive",
		"proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding",
		"upgrade", "x-request-id":
		return true
	default:
		return strings.HasPrefix(name, "x-forwarded-") || strings.HasPrefix(name, "x-minicloud-")
	}
}

func reservedResponseHeader(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "connection", "content-length", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade", "x-request-id":
		return true
	default:
		return strings.HasPrefix(lower, "x-minicloud-")
	}
}

func affinityKey(request *http.Request, requestID string) []byte {
	if value := request.Header.Get("Idempotency-Key"); value != "" {
		return []byte(value)
	}
	return []byte(requestID)
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
