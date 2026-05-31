package grpcx

import (
	"context"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// roundRobinConfig enables client-side round-robin load balancing. Combined
// with a dns:/// target pointing at a headless Service, this fans RPCs across
// all backend pods instead of pinning to a single one — the HTTP/2
// single-connection load-balancing pitfall that a naive ClusterIP + gRPC
// client hits.
const roundRobinConfig = `{"loadBalancingConfig":[{"round_robin":{}}]}`

// DefaultCallTimeout bounds any unary RPC whose context carries no deadline of
// its own, so a hung server can't block a caller indefinitely.
const DefaultCallTimeout = 5 * time.Second

// deadlineInterceptor applies DefaultCallTimeout to outgoing unary RPCs that
// don't already have a deadline. A caller that sets its own deadline is left
// untouched.
func deadlineInterceptor(
	ctx context.Context,
	method string,
	req, reply any,
	cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption,
) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultCallTimeout)
		defer cancel()
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

// Dial creates a lazy gRPC client connection to target, e.g.
// "dns:///shipping-grpc.shipping.svc.cluster.local:9090".
//
// It enables OpenTelemetry tracing, client-side round-robin load balancing, and
// a default per-RPC deadline (DefaultCallTimeout) for calls that don't set one.
// Transport is currently insecure (plaintext) for in-cluster east-west traffic;
// mTLS is a later phase. Additional DialOptions are appended after the defaults
// (and may override them). The connection is created lazily and does not block
// on connect — the first RPC triggers name resolution and connection.
func Dial(target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	base := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithDefaultServiceConfig(roundRobinConfig),
		grpc.WithChainUnaryInterceptor(deadlineInterceptor),
	}

	return grpc.NewClient(target, append(base, opts...)...)
}
