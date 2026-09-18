package obsx

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// maxErrorMessageLen bounds what an error string may put on a span. Driver
// and provider errors routinely embed the statement, the payload or the
// response body that failed; the span keeps enough to recognise the failure
// class and the log record (redacted by the logging facade) keeps the rest.
const maxErrorMessageLen = 256

// OutcomeKey is the attribute a business rejection carries instead of an
// Error status (RFC-0031 tracing contract): the operation did what it was
// asked, the answer was "no", and the span stays green so error-rate SLOs
// count failures rather than customers being told no.
const OutcomeKey = attribute.Key("outcome")

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
// Every span carries exactly one kind and the kind follows the layer: manual
// spans opened here are INTERNAL by default — the SDK's default when no
// trace.WithSpanKind is given — and that is the right kind for logic/v1 and
// core code. SERVER and CLIENT belong to the transport instrumentation in
// httpmw, grpcx and the instrumented adapters; PRODUCER/CONSUMER to the
// Temporal integration. Passing a kind here is the exception that needs a
// reason in review.
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

// RecordError records err on the active span and marks the span failed:
// status Error, the semconv error.type attribute, and a standard exception
// event whose message is bounded to maxErrorMessageLen (RFC-0031 tracing
// contract — no raw payloads or provider responses on a span).
//
// Marking the status is the half that is easy to forget: an error recorded
// without it leaves the span green, and the trace looks healthy while carrying
// an exception event nobody queries for.
//
// This is for UNEXPECTED failure. An expected business rejection — not found,
// price changed, stock unavailable, payment declined, invalid transition — is
// not an error: use RecordOutcome and leave the status unset.
func RecordError(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)
	if err == nil || !span.IsRecording() {
		return
	}
	msg := boundedMessage(err.Error())
	typ := ErrorType(err)
	span.SetAttributes(semconv.ErrorTypeKey.String(typ))
	span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(
		semconv.ExceptionTypeKey.String(typ),
		semconv.ExceptionMessageKey.String(msg),
	))
	span.SetStatus(codes.Error, msg)
}

// RecordOutcome stamps a bounded business outcome on the active span and
// leaves its status untouched. The value is an operation class such as
// "not_found", "price_changed", "stock_unavailable" or "payment_declined" —
// never an identifier, a message or free text.
func RecordOutcome(ctx context.Context, outcome string) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(OutcomeKey.String(outcome))
	}
}

// ErrorType returns the low-cardinality class of err for the semconv
// error.type attribute: the concrete Go type of the first error in the chain
// that is not a standard-library wrapper, without pointer marker.
//
// The fleet records errors after wrapping them — fmt.Errorf("charge: %w",
// pgErr) is the common shape — so classifying the outermost type would make
// almost every error.type read "fmt.wrapError". Walking past fmt.Errorf and
// errors.Join wrappers yields the cause's type ("pgconn.PgError") or, when a
// domain error wraps a cause, the domain type — which is what a dashboard
// groups on. An unwrapped fmt.Errorf still reads "errors.errorString".
//
// The value is the SHORT package path ("v1.NotFoundError", not the full
// import path) with no "*": the SDK's own exception.type used the pointer
// form for pointer errors and the full import path for value errors, so this
// is a deliberate normalisation to one stable shape, not a copy of it.
func ErrorType(err error) string {
	if err == nil {
		return ""
	}
	// A join with several causes has no single Unwrap and keeps its own type.
	for next := errors.Unwrap(err); isStdlibWrapper(err) && next != nil; next = errors.Unwrap(err) {
		err = next
	}
	t := reflect.TypeOf(err)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.PkgPath() == "" || t.Name() == "" {
		return strings.TrimPrefix(fmt.Sprintf("%T", err), "*")
	}
	return t.String()
}

// isStdlibWrapper reports whether err is one of the anonymous wrapper types
// fmt.Errorf and errors.Join produce — a shape, not a class worth reporting.
func isStdlibWrapper(err error) bool {
	switch fmt.Sprintf("%T", err) {
	case "*fmt.wrapError", "*fmt.wrapErrors", "*errors.joinError":
		return true
	}
	return false
}

// truncatedMarker is appended when boundedMessage cuts a value.
const truncatedMarker = "…(truncated)"

// boundedMessage caps s at maxErrorMessageLen bytes without ever producing
// invalid UTF-8. That is not cosmetic: an attribute string with a half rune
// fails proto.Marshal on export, and the batch span processor then drops the
// WHOLE batch — up to 512 spans, healthy traces included — with one logged
// error. Invalid input is repaired first; the cut lands on a rune boundary.
func boundedMessage(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= maxErrorMessageLen {
		return s
	}
	cut := maxErrorMessageLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMarker
}

// SetSpanStatus sets the status of the active span.
func SetSpanStatus(ctx context.Context, code codes.Code, description string) {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetStatus(code, description)
	}
}
