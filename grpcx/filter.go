package grpcx

import (
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc/filters"
)

// telemetryFilter excludes infrastructure RPCs from tracing and metrics on
// both stats handlers: gRPC health checks (kubelet probes, client keepalive
// pings with PermitWithoutStream) and server reflection (grpcurl debugging).
// Instrumenting them adds a steady stream of worthless spans and
// rpc_*_call_duration series; business RPCs are never matched.
//
// Tradeoff (accepted): reflection calls become invisible in traces/metrics,
// so in-cluster API enumeration leaves no telemetry footprint. Sampled traces
// were never an audit trail — NetworkPolicy fences :9090 and production runs
// GRPC_REFLECTION=false.
var telemetryFilter otelgrpc.Filter = filters.None(
	filters.HealthCheck(),
	filters.ServicePrefix("grpc.reflection."),
)
