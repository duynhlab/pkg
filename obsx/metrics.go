// Package obsx wires shared observability plumbing for services.
//
// SetupMetrics bridges OpenTelemetry metrics — emitted by the otelgrpc stats
// handlers in pkg/grpcx — into the default Prometheus registry, so east-west
// gRPC RED metrics surface on the service's existing promhttp /metrics endpoint
// and are scraped by the same ServiceMonitor as HTTP metrics (no extra port; a
// gRPC server already owns :9090, so it can't also serve HTTP there).
//
// TraceIDFromContext returns the active span's trace ID for log↔trace
// correlation.
package obsx

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	setupOnce sync.Once
	setupErr  error
	provider  *sdkmetric.MeterProvider
)

// SetupMetrics installs a global OpenTelemetry MeterProvider backed by a
// Prometheus exporter registered with the default Prometheus registry. After it
// runs, the otelgrpc stats handlers in pkg/grpcx (which use the global
// MeterProvider) record gRPC server/client RED metrics that appear on the
// process's existing promhttp /metrics handler.
//
// It is idempotent: repeated calls return the same shutdown func and never
// register the exporter twice (which would panic on the default registry). The
// returned func flushes and stops the provider; call it during graceful
// shutdown.
func SetupMetrics() (func(context.Context) error, error) {
	setupOnce.Do(func() {
		exporter, err := otelprom.New()
		if err != nil {
			setupErr = err
			return
		}
		provider = sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
		otel.SetMeterProvider(provider)
	})
	if setupErr != nil {
		return nil, setupErr
	}
	return shutdownMetrics, nil
}

// shutdownMetrics flushes and stops the MeterProvider installed by SetupMetrics.
func shutdownMetrics(ctx context.Context) error {
	if provider == nil {
		return nil
	}
	return provider.Shutdown(ctx)
}

// TraceIDFromContext returns the trace ID of the span in ctx, or "" if no valid
// span is present. Middleware should prefer this over a request header so log
// lines carry the same trace ID that is exported to the tracing backend; fall
// back to the header/a generated ID only when this returns "".
func TraceIDFromContext(ctx context.Context) string {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}
