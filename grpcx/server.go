// Package grpcx provides shared gRPC server and client helpers for internal
// (east-west) service communication: OpenTelemetry instrumentation, panic
// recovery, the gRPC health checking protocol, server reflection, keepalive,
// and client-side round-robin load balancing over headless Services.
//
// It is the foundation for the gRPC migration described in
// homelab/docs/api/grpc-internal-comms.md.
package grpcx

import (
	"context"
	"log"
	"os"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

// recoveryUnary turns a panicking unary handler into a codes.Internal error
// instead of letting the panic crash the process. The gRPC server shares its
// process with the HTTP server, so an unrecovered handler panic would take the
// whole pod down — this keeps a single bad request from doing that.
func recoveryUnary(
	ctx context.Context,
	req any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("grpcx: recovered panic in %s: %v\n%s", info.FullMethod, r, debug.Stack())
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return handler(ctx, req)
}

// recoveryStream is the streaming counterpart of recoveryUnary.
func recoveryStream(
	srv any,
	ss grpc.ServerStream,
	info *grpc.StreamServerInfo,
	handler grpc.StreamHandler,
) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("grpcx: recovered panic in %s: %v\n%s", info.FullMethod, r, debug.Stack())
			err = status.Error(codes.Internal, "internal error")
		}
	}()
	return handler(srv, ss)
}

// NewServer returns a *grpc.Server preconfigured for internal services:
//   - OpenTelemetry tracing/metrics via the otelgrpc stats handler,
//   - panic recovery (a handler panic becomes codes.Internal, not a crash),
//   - keepalive (MaxConnectionAge forces periodic reconnect so clients
//     re-resolve and rebalance after a scale/rolling deploy) and an enforcement
//     policy compatible with the client keepalive in Dial,
//   - bounded MaxConcurrentStreams and receive message size,
//   - the standard gRPC health service (reporting SERVING by default), and
//   - server reflection unless GRPC_REFLECTION=false (gate it off in prod).
//
// Additional ServerOptions are appended after the defaults. The returned
// *health.Server lets callers flip serving status during startup/shutdown,
// e.g. hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING).
func NewServer(opts ...grpc.ServerOption) (*grpc.Server, *health.Server) {
	base := []grpc.ServerOption{
		// Health-check and reflection RPCs are pure plumbing: without a
		// filter every probe/keepalive mints spans and duration series
		// (steady telemetry noise). Real RPCs are unaffected.
		grpc.StatsHandler(otelgrpc.NewServerHandler(otelgrpc.WithFilter(telemetryFilter))),
		grpc.ChainUnaryInterceptor(recoveryUnary),
		grpc.ChainStreamInterceptor(recoveryStream),
		grpc.MaxConcurrentStreams(1000),
		grpc.MaxRecvMsgSize(4 << 20), // 4 MiB
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      30 * time.Minute,
			MaxConnectionAgeGrace: 5 * time.Minute,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	}
	srv := grpc.NewServer(append(base, opts...)...)

	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(srv, hs)

	// Reflection eases grpcurl debugging but discloses the full API surface;
	// disable it in production (GRPC_REFLECTION=false).
	if os.Getenv("GRPC_REFLECTION") != "false" {
		reflection.Register(srv)
	}

	return srv, hs
}
