// Package localworker adapts in-process Worker invocation boundaries without
// bypassing the same Endpoint address and invocation fence used over RPC.
package localworker

import (
	"context"
	"errors"

	"github.com/yourikka/minicloud/internal/gatewayinvoke"
	"github.com/yourikka/minicloud/internal/problem"
)

// Resolver binds one Local Core internal Endpoint address to its Worker
// invoker. The invoker remains responsible for checking the exact fence.
type Resolver struct {
	address string
	invoker gatewayinvoke.EndpointInvoker
}

// NewResolver creates an immutable single-Worker resolver.
func NewResolver(address string, invoker gatewayinvoke.EndpointInvoker) (*Resolver, error) {
	if address == "" {
		return nil, errors.New("local worker endpoint address is required")
	}
	if invoker == nil {
		return nil, errors.New("local worker endpoint invoker is required")
	}
	return &Resolver{address: address, invoker: invoker}, nil
}

// Resolve returns the Worker only for its exact published Endpoint address.
func (r *Resolver) Resolve(ctx context.Context, address string) (gatewayinvoke.EndpointInvoker, error) {
	if ctx == nil {
		return nil, errors.New("local worker resolve context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || r.invoker == nil || r.address == "" {
		return nil, errors.New("local worker resolver dependencies are required")
	}
	if address != r.address {
		return nil, &problem.Error{
			Code:    problem.CodeNoReadyReplica,
			Message: "published endpoint is not registered in the local worker resolver",
		}
	}
	return r.invoker, nil
}
