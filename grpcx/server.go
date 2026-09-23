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
	"fmt"
	"log/slog"
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

// maxPanicStack bounds the stack a recovered panic writes. A panic is the one
// record where a stack is worth its bytes, but a deep one runs to tens of
// kilobytes, and the record may be exported to a store kept for months.
const maxPanicStack = 4096

// recovery returns the interceptor pair that turns a panicking handler into a
// codes.Internal error instead of letting the panic crash the process. The gRPC
// server shares its process with the HTTP server, so an unrecovered handler
// panic would take the whole pod down — this keeps a single bad request from
// doing that.
//
// The panic is written through the service logger as one structured record —
// never to stderr as free text, which would bypass the redaction the facade
// applies and break the one-JSON-record-per-line contract. A nil logger (access
// log disabled) still reports it, through slog.Default: a panic is never
// swallowed.
func recovery(logger *slog.Logger) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	if logger == nil {
		logger = slog.Default()
	}
	report := func(ctx context.Context, fullMethod string, r any) error {
		stack := debug.Stack()
		if len(stack) > maxPanicStack {
			stack = stack[:maxPanicStack]
		}
		logger.LogAttrs(ctx, slog.LevelError, "gRPC handler panicked",
			slog.String("rpc.method", trimSlash(fullMethod)),
			slog.String("error.type", "panic"),
			slog.String("exception.message", fmt.Sprint(r)),
			slog.String("exception.stacktrace", string(stack)),
		)
		return status.Error(codes.Internal, "internal error")
	}
	unary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = report(ctx, info.FullMethod, r)
			}
		}()
		return handler(ctx, req)
	}
	stream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = report(ss.Context(), info.FullMethod, r)
			}
		}()
		return handler(srv, ss)
	}
	return unary, stream
}

func trimSlash(fullMethod string) string {
	if len(fullMethod) > 0 && fullMethod[0] == '/' {
		return fullMethod[1:]
	}
	return fullMethod
}

// NewServer returns a *grpc.Server preconfigured for internal services:
//   - OpenTelemetry tracing/metrics via the otelgrpc stats handler,
//   - a per-RPC access log (the gRPC counterpart of the HTTP request log),
//   - panic recovery (a handler panic becomes codes.Internal, not a crash),
//   - keepalive (MaxConnectionAge forces periodic reconnect so clients
//     re-resolve and rebalance after a scale/rolling deploy) and an enforcement
//     policy compatible with the client keepalive in Dial,
//   - bounded MaxConcurrentStreams and receive message size,
//   - the standard gRPC health service (reporting SERVING by default), and
//   - server reflection unless GRPC_REFLECTION=false (gate it off in prod).
//
// logger is the service's structured logger — the platform facade's Slog(), so
// access records are redacted and reach both stdout and OTLP. A nil logger
// disables the access log; recovered panics are still reported. Additional
// ServerOptions are appended after the defaults. The returned *health.Server
// lets callers flip serving status during startup/shutdown, e.g.
// hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING).
func NewServer(logger *slog.Logger, opts ...grpc.ServerOption) (*grpc.Server, *health.Server) {
	recoverUnary, recoverStream := recovery(logger)
	base := []grpc.ServerOption{
		// Health-check and reflection RPCs are pure plumbing: without a
		// filter every probe/keepalive mints spans and duration series
		// (steady telemetry noise). Real RPCs are unaffected.
		grpc.StatsHandler(otelgrpc.NewServerHandler(otelgrpc.WithFilter(telemetryFilter))),
		// Access log OUTERMOST, recovery inner: grpc-go runs chained
		// interceptors first-to-last as outer-to-inner, so a handler panic is
		// caught by the recovery interceptor (inner), turned into codes.Internal,
		// and RETURNED up to accessLogUnary — which then records that Internal
		// result. Reversing the order would unwind the panic straight past a
		// not-yet-logged access interceptor, so panics would never be logged.
		grpc.ChainUnaryInterceptor(accessLogUnary(logger), recoverUnary),
		grpc.ChainStreamInterceptor(accessLogStream(logger), recoverStream),
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
