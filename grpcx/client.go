package grpcx

import (
	"context"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// defaultServiceConfig configures every connection from Dial:
//   - round-robin load balancing (combined with a dns:/// target at a headless
//     Service, this fans RPCs across all backend pods instead of pinning to one —
//     the HTTP/2 single-connection pitfall a naive ClusterIP + gRPC client hits);
//   - a transparent retry on UNAVAILABLE. UNAVAILABLE is a pre-processing
//     transport failure (gRPC only retries when no response was committed), so it
//     is safe to retry across a rolling deploy / pod restart even for the
//     non-idempotent RPCs.
const defaultServiceConfig = `{
  "loadBalancingConfig": [{"round_robin": {}}],
  "methodConfig": [{
    "name": [{}],
    "retryPolicy": {
      "MaxAttempts": 3,
      "InitialBackoff": "0.1s",
      "MaxBackoff": "1s",
      "BackoffMultiplier": 2.0,
      "RetryableStatusCodes": ["UNAVAILABLE"]
    }
  }]
}`

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
// It enables OpenTelemetry tracing, client-side round-robin load balancing, a
// transparent retry on UNAVAILABLE, HTTP/2 keepalive (so a dead peer is detected
// in seconds rather than minutes, and the dns resolver rebalances after a pod
// restart/scale), and a default per-RPC deadline (DefaultCallTimeout) for calls
// that don't set one. Transport is currently insecure (plaintext) for in-cluster
// east-west traffic; mTLS is a later phase. Additional DialOptions are appended
// after the defaults (and may override them). The connection is created lazily
// and does not block on connect — the first RPC triggers resolution/connection.
func Dial(target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	base := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithDefaultServiceConfig(defaultServiceConfig),
		grpc.WithChainUnaryInterceptor(deadlineInterceptor),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	}

	return grpc.NewClient(target, append(base, opts...)...)
}
