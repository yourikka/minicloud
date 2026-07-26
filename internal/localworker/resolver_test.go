package localworker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/yourikka/minicloud/internal/gatewayinvoke"
	"github.com/yourikka/minicloud/internal/problem"
	"github.com/yourikka/minicloud/internal/servingauth"
	"github.com/yourikka/minicloud/internal/wasmexec"
	abi "github.com/yourikka/minicloud/sdk/go/minicloudabi"
)

func TestResolverReturnsInvokerOnlyForExactAddress(t *testing.T) {
	t.Parallel()
	invoker := invokerFunc(func(
		context.Context,
		servingauth.InvocationFence,
		abi.Request,
		time.Duration,
	) (wasmexec.Result, error) {
		return wasmexec.Result{}, nil
	})
	resolver, err := NewResolver("worker-a.internal:7443", invoker)
	if err != nil {
		t.Fatalf("NewResolver() error = %v", err)
	}
	resolved, err := resolver.Resolve(t.Context(), "worker-a.internal:7443")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved == nil {
		t.Fatal("Resolve() returned a nil invoker")
	}
	_, err = resolver.Resolve(t.Context(), "worker-b.internal:7443")
	var classified *problem.Error
	if !errors.As(err, &classified) || classified.Code != problem.CodeNoReadyReplica {
		t.Fatalf("Resolve() unknown address error = %v", err)
	}
}

func TestResolverRejectsMissingDependenciesAndCanceledContext(t *testing.T) {
	t.Parallel()
	if _, err := NewResolver("", nil); err == nil {
		t.Fatal("NewResolver() accepted missing dependencies")
	}
	var resolver *Resolver
	if _, err := resolver.Resolve(t.Context(), "worker-a.internal:7443"); err == nil {
		t.Fatal("nil Resolver.Resolve() succeeded")
	}
	valid, err := NewResolver("worker-a.internal:7443", invokerFunc(func(
		context.Context,
		servingauth.InvocationFence,
		abi.Request,
		time.Duration,
	) (wasmexec.Result, error) {
		return wasmexec.Result{}, nil
	}))
	if err != nil {
		t.Fatalf("NewResolver() error = %v", err)
	}
	if _, err := valid.Resolve(nil, "worker-a.internal:7443"); err == nil {
		t.Fatal("Resolve() accepted a nil context")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := valid.Resolve(ctx, "worker-a.internal:7443"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resolve() error = %v, want context cancellation", err)
	}
}

var _ gatewayinvoke.EndpointResolver = (*Resolver)(nil)

type invokerFunc func(
	context.Context,
	servingauth.InvocationFence,
	abi.Request,
	time.Duration,
) (wasmexec.Result, error)

func (f invokerFunc) Invoke(
	ctx context.Context,
	fence servingauth.InvocationFence,
	request abi.Request,
	timeout time.Duration,
) (wasmexec.Result, error) {
	return f(ctx, fence, request, timeout)
}
