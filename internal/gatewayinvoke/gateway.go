// Package gatewayinvoke invokes one Worker only through an atomically applied
// serving view. HTTP authentication and ABI request construction belong to an
// outer Gateway boundary; this package never reads Controller state directly.
package gatewayinvoke

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/yourikka/minicloud/internal/discovery"
	"github.com/yourikka/minicloud/internal/gatewaydiscovery"
	"github.com/yourikka/minicloud/internal/gatewayroute"
	"github.com/yourikka/minicloud/internal/model"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/routing"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmexec"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

// EndpointInvoker is the Worker invocation RPC surface required by a Gateway.
// Implementations must recheck the supplied fence immediately before guest
// code is instantiated.
type EndpointInvoker interface {
	Invoke(context.Context, servingauth.InvocationFence, abi.Request, time.Duration) (wasmexec.Result, error)
}

// EndpointResolver resolves a complete internal Endpoint address from the
// authoritative serving view. A Local Core adapter can resolve addresses
// in-process; network Gateways can use the same contract for RPC clients.
type EndpointResolver interface {
	Resolve(context.Context, string) (EndpointInvoker, error)
}

// Config provides the trusted, independently-owned Gateway components.
type Config struct {
	Discovery *gatewaydiscovery.Store
	Balancer  *gatewayroute.Balancer
	Resolver  EndpointResolver
}

// Gateway is safe for concurrent invocation. The supplied Discovery Store owns
// snapshot freshness and the supplied Balancer owns local endpoint occupancy.
type Gateway struct {
	discovery *gatewaydiscovery.Store
	balancer  *gatewayroute.Balancer
	resolver  EndpointResolver
}

// Request contains the already-authenticated ABI request and the raw affinity
// key derived by an outer Gateway boundary from the complete serving view.
type Request struct {
	FunctionID  string
	AffinityKey []byte
	Invocation  abi.Request
	Timeout     time.Duration
}

// Result records the immutable execution identity selected for one call. The
// Endpoint is an internal control-plane value and is not a public HTTP result.
type Result struct {
	InvocationID   string
	VersionID      string
	RouteRevision  uint64
	DiscoveryEpoch uint64
	Endpoint       discovery.Endpoint
	Execution      wasmexec.Result
}

// New validates dependencies and creates a Gateway invocation coordinator. A
// nil Balancer uses a fresh local least-inflight/round-robin balancer.
func New(config Config) (*Gateway, error) {
	if config.Discovery == nil {
		return nil, errors.New("gateway invocation discovery store is required")
	}
	if config.Resolver == nil {
		return nil, errors.New("gateway invocation endpoint resolver is required")
	}
	if config.Balancer == nil {
		config.Balancer = gatewayroute.New()
	}
	return &Gateway{
		discovery: config.Discovery,
		balancer:  config.Balancer,
		resolver:  config.Resolver,
	}, nil
}

// Invoke selects exactly one Route Target and one Endpoint from the current
// complete serving view, then forwards the exact Discovery and Assignment
// fence to the resolved Worker. It never retries or falls back after selection.
func (g *Gateway) Invoke(ctx context.Context, request Request) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("gateway invocation context is required")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if g == nil || g.discovery == nil || g.balancer == nil || g.resolver == nil {
		return Result{}, errors.New("gateway invocation dependencies are required")
	}

	view, err := g.discovery.Lookup(request.FunctionID)
	if err != nil {
		return Result{}, fmt.Errorf("looking up serving view: %w", err)
	}
	if view.Snapshot.Function.Lifecycle != model.FunctionActive ||
		!view.Snapshot.Trigger.Enabled ||
		!view.Snapshot.Route.Present {
		return Result{}, &problem.Error{
			Code:    problem.CodeFunctionDisabled,
			Message: "function does not have an enabled serving view",
		}
	}
	decision, err := routing.SelectServing(servingRoute(view.Snapshot.Route), request.AffinityKey)
	if err != nil {
		return Result{}, fmt.Errorf("selecting route target: %w", err)
	}
	lease, err := g.balancer.Acquire(request.FunctionID, decision.Target, view.Endpoints)
	if err != nil {
		return Result{}, fmt.Errorf("acquiring endpoint: %w", err)
	}
	defer lease.Release()

	result := Result{
		InvocationID:   request.Invocation.InvocationID,
		VersionID:      decision.Target.VersionID,
		RouteRevision:  view.Snapshot.Route.RouteRevision,
		DiscoveryEpoch: view.Snapshot.DiscoveryEpoch,
		Endpoint:       lease.Endpoint,
	}
	invoker, err := g.resolver.Resolve(ctx, lease.Endpoint.Address)
	if err != nil {
		return result, fmt.Errorf("resolving endpoint: %w", err)
	}
	if invoker == nil {
		return result, errors.New("resolving endpoint: resolver returned no worker invoker")
	}
	result.Execution, err = invoker.Invoke(ctx, servingauth.InvocationFence{
		Assignment:     lease.Endpoint.Assignment,
		DiscoveryEpoch: view.Snapshot.DiscoveryEpoch,
	}, request.Invocation, request.Timeout)
	if err != nil {
		return result, fmt.Errorf("invoking endpoint: %w", err)
	}
	return result, nil
}

func servingRoute(route discovery.Route) model.ServingRoute {
	return model.ServingRoute{
		FunctionID:    route.FunctionID,
		RouteRevision: route.RouteRevision,
		Targets:       slices.Clone(route.Targets),
		HashVersion:   route.HashVersion,
		Salt:          slices.Clone(route.Salt),
		Enabled:       route.Enabled,
	}
}
