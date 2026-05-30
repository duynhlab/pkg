package grpcx

import (
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

// Dial creates a lazy gRPC client connection to target, e.g.
// "dns:///shipping-grpc.shipping.svc.cluster.local:9090".
//
// It enables OpenTelemetry tracing and client-side round-robin load balancing.
// Transport is currently insecure (plaintext) for in-cluster east-west traffic;
// mTLS is a later phase. Additional DialOptions are appended after the defaults
// (and may override them). The connection is created lazily and does not block
// on connect — the first RPC triggers name resolution and connection.
func Dial(target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	base := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithDefaultServiceConfig(roundRobinConfig),
	}

	return grpc.NewClient(target, append(base, opts...)...)
}
