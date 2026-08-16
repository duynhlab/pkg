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

// Tracer returns the tracer for a service scope. The OTel SDK is wired once in
// main() by SetupObservability; this only reads the global provider it installed.
//
// serviceName is a parameter rather than package state on purpose: the
// per-service middleware this replaced kept it in a package-level variable
// written by a setter at startup and read from the request path, which was safe
// only by convention.
func Tracer(serviceName string) trace.Tracer {
	return otel.Tracer(serviceName)
}

// StartSpan opens a child span under whatever is already on the context. The
// caller owns the returned span and must End it.
//
//	ctx, span := obsx.StartSpan(ctx, "checkout.confirm")
//	defer span.End()
func StartSpan(ctx context.Context, serviceName, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	//nolint:spancheck // the span is returned; ending it is the caller's job
	return Tracer(serviceName).Start(ctx, name, opts...)
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
