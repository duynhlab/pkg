package obsx

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Span helpers for the logic and core layers: open a business span under the
// transport span, and enrich the active one.
//
// They live in obsx rather than httpmw because they touch only the OTel API — a
// gRPC-only service uses them without taking a dependency on a web framework.
// The transport spans themselves come from httpmw (HTTP) or grpcx (gRPC); these
// never create a root.

// Tracer returns the tracer for an instrumentation scope. The OTel SDK is wired
// once in main() by SetupObservability; this only reads the global provider it
// installed.
//
// scope names the CODE that creates the span, and OpenTelemetry asks for a
// package path rather than a free-form label:
//
//	github.com/duynhlab/order-service/internal/logic/v1   // yes
//	order                                                 // no
//
// The deployment identity is a different axis and already travels as
// service.name on the Resource, stamped on every span. Naming the scope after
// the service duplicates that and loses the only thing a scope is for: telling
// two instrumented packages inside one service apart.
func Tracer(scope string) trace.Tracer {
	return otel.Tracer(scope)
}

// StartSpan opens a child span under whatever is already on the context. The
// caller owns the returned span and must End it. See Tracer for what scope is.
//
//	const tracerScope = "github.com/duynhlab/checkout-service/internal/logic/v1"
//
//	ctx, span := obsx.StartSpan(ctx, tracerScope, "checkout.confirm")
//	defer span.End()
func StartSpan(ctx context.Context, scope, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	//nolint:spancheck // the span is returned; ending it is the caller's job
	return Tracer(scope).Start(ctx, name, opts...)
}

// AddSpanAttributes attaches attributes to the active span.
//
// Every helper below is a no-op when the span is not recording, so callers do
// not guard at each site and an unsampled request costs nothing.
func AddSpanAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(attrs...)
	}
}

// AddSpanEvent records a point-in-time event on the active span.
func AddSpanEvent(ctx context.Context, name string, attrs ...attribute.KeyValue) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.AddEvent(name, trace.WithAttributes(attrs...))
	}
}

// RecordError records err on the active span and marks the span failed.
//
// Marking the status is the half that is easy to forget: an error recorded
// without it leaves the span green, and the trace looks healthy while carrying
// an exception event nobody queries for.
func RecordError(ctx context.Context, err error) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
}

// SetSpanStatus sets the status of the active span.
func SetSpanStatus(ctx context.Context, code codes.Code, description string) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetStatus(code, description)
	}
}
