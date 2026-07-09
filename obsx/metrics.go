// Package obsx wires shared observability plumbing for services.
//
// SetupObservability (setup.go) is the single OTel SDK wiring point since
// RFC-0014; the scrape-era Prometheus bridge (SetupMetrics) was removed with
// the P3 metrics cutover (ADR-016 in homelab).
//
// TraceIDFromContext returns the active span's trace ID for log↔trace
// correlation.
package obsx

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

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
