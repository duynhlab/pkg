// Package grpcx provides shared gRPC server and client helpers for internal
// (east-west) service communication: OpenTelemetry instrumentation, the gRPC
// health checking protocol, server reflection, and client-side round-robin
// load balancing over headless Services.
//
// It is the Phase 0 foundation for the gRPC migration described in
// homelab/docs/api/grpc-internal-comms.md.
package grpcx

import (
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

// NewServer returns a *grpc.Server preconfigured for internal services:
//   - OpenTelemetry tracing/metrics via the otelgrpc stats handler,
//   - the standard gRPC health service (reporting SERVING by default), and
//   - server reflection (so grpcurl works without local protos).
//
// Additional ServerOptions are appended after the defaults. The returned
// *health.Server lets callers flip serving status during startup/shutdown,
// e.g. hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING).
func NewServer(opts ...grpc.ServerOption) (*grpc.Server, *health.Server) {
	base := []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
	}
	srv := grpc.NewServer(append(base, opts...)...)

	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, hs)

	reflection.Register(srv)

	return srv, hs
}
