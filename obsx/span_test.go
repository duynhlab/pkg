package obsx_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/duynhlab/pkg/obsx"
)

func recorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

// A business span nests under the transport span rather than starting a new
// trace — that is what keeps one request one trace from the edge inward.
func TestStartSpan_NestsUnderTheActiveSpan(t *testing.T) {
	rec := recorder(t)

	parentCtx, parent := obsx.StartSpan(context.Background(), "svc", "transport")
	childCtx, child := obsx.StartSpan(parentCtx, "svc", "logic")
	child.End()
	parent.End()
	_ = childCtx

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2", len(spans))
	}
	var childSpan, parentSpan = spans[0], spans[1]
	if childSpan.Parent().SpanID() != parentSpan.SpanContext().SpanID() {
		t.Error("child span is not parented to the transport span")
	}
	if childSpan.SpanContext().TraceID() != parentSpan.SpanContext().TraceID() {
		t.Error("child span started a new trace")
	}
}

// RecordError must also set the status. An error recorded without it leaves the
// span green, so the trace looks healthy while carrying an exception nobody
// queries for.
func TestRecordError_AlsoMarksTheSpanFailed(t *testing.T) {
	rec := recorder(t)

	ctx, span := obsx.StartSpan(context.Background(), "svc", "work")
	obsx.RecordError(ctx, errors.New("boom"))
	span.End()

	got := rec.Ended()[0]
	if got.Status().Code != codes.Error {
		t.Errorf("status = %v, want Error", got.Status().Code)
	}
	if len(got.Events()) == 0 {
		t.Error("no exception event recorded")
	}
}

// Every helper is a no-op without a recording span, so callers need no guard and
// an unsampled request costs nothing.
func TestHelpers_AreNoOpsWithoutARecordingSpan(t *testing.T) {
	ctx := context.Background() // no span at all

	obsx.AddSpanAttributes(ctx, attribute.String("k", "v"))
	obsx.AddSpanEvent(ctx, "event")
	obsx.RecordError(ctx, errors.New("boom"))
	obsx.SetSpanStatus(ctx, codes.Ok, "fine")
	// Reaching here without a panic is the assertion.
}

func TestAddSpanAttributesAndEvent_LandOnTheActiveSpan(t *testing.T) {
	rec := recorder(t)

	ctx, span := obsx.StartSpan(context.Background(), "svc", "work")
	obsx.AddSpanAttributes(ctx, attribute.String("order.id", "42"))
	obsx.AddSpanEvent(ctx, "cache.hit", attribute.String("key", "k"))
	span.End()

	got := rec.Ended()[0]
	var found bool
	for _, a := range got.Attributes() {
		if a.Key == "order.id" && a.Value.AsString() == "42" {
			found = true
		}
	}
	if !found {
		t.Error("attribute did not land on the span")
	}
	if len(got.Events()) != 1 || got.Events()[0].Name != "cache.hit" {
		t.Errorf("events = %v, want one cache.hit", got.Events())
	}
}

// A manual span is INTERNAL and its scope is the package path the caller gave —
// the two facts the RFC-0031 tracing contract makes reviewable.
func TestStartSpan_IsInternalWithCallerScope(t *testing.T) {
	rec := recorder(t)
	const scope = "github.com/duynhlab/order-service/internal/logic/v1"

	_, span := obsx.StartSpan(context.Background(), scope, "order.confirm")
	span.End()

	got := rec.Ended()[0]
	if got.SpanKind() != trace.SpanKindInternal {
		t.Errorf("kind = %v, want INTERNAL for a manual logic span", got.SpanKind())
	}
	if got.InstrumentationScope().Name != scope {
		t.Errorf("scope = %q, want the package path %q", got.InstrumentationScope().Name, scope)
	}
}

type pgLikeError struct{ code string }

func (e *pgLikeError) Error() string {
	return "SQLSTATE " + e.code + ": " + strings.Repeat("secret-payload ", 40)
}

// RecordError is for unexpected failure: Error status, error.type, one bounded
// exception event. RecordOutcome is for the expected "no": an outcome attribute
// and an UNSET status, so error-rate SLOs count faults, not customers told no.
func TestStatusContract_FailureVersusBusinessRejection(t *testing.T) {
	rec := recorder(t)

	ctx, failed := obsx.StartSpan(context.Background(), "svc", "charge")
	obsx.RecordError(ctx, &pgLikeError{code: "23505"})
	failed.End()

	ctx2, rejected := obsx.StartSpan(context.Background(), "svc", "reserve")
	obsx.RecordOutcome(ctx2, "stock_unavailable")
	rejected.End()

	spans := rec.Ended()
	f, r := spans[0], spans[1]

	if f.Status().Code != codes.Error {
		t.Errorf("failure status = %v, want Error", f.Status().Code)
	}
	const maxLen = 256 + len("…(truncated)")
	if len(f.Status().Description) > maxLen {
		t.Errorf("status description not bounded: %d chars", len(f.Status().Description))
	}
	attrs := map[attribute.Key]string{}
	for _, a := range f.Attributes() {
		attrs[a.Key] = a.Value.AsString()
	}
	if attrs["error.type"] != "obsx_test.pgLikeError" {
		t.Errorf("error.type = %q, want the concrete type without pointer marker", attrs["error.type"])
	}
	if n := len(f.Events()); n != 1 {
		t.Fatalf("exception events = %d, want exactly 1", n)
	}
	for _, a := range f.Events()[0].Attributes {
		if a.Key == "exception.message" && len(a.Value.AsString()) > maxLen {
			t.Errorf("exception.message not bounded: %d chars", len(a.Value.AsString()))
		}
	}

	if r.Status().Code != codes.Unset {
		t.Errorf("rejection status = %v, want Unset — a business 'no' is not a failure", r.Status().Code)
	}
	var outcome string
	for _, a := range r.Attributes() {
		if a.Key == obsx.OutcomeKey {
			outcome = a.Value.AsString()
		}
	}
	if outcome != "stock_unavailable" {
		t.Errorf("outcome attribute = %q, want stock_unavailable", outcome)
	}
	if len(r.Events()) != 0 {
		t.Error("a business rejection must not record an exception event")
	}
}

// The expected values name unexported standard-library types on purpose: a
// Go release renaming one would silently change production error.type values,
// and this table is the only thing that would say so. Do not loosen it.
func TestErrorType(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{errors.New("x"), "errors.errorString"},
		{&pgLikeError{}, "obsx_test.pgLikeError"},
		{context.DeadlineExceeded, "context.deadlineExceededError"},
		// The fleet wraps before recording: the CAUSE is the class, not the wrapper.
		{fmt.Errorf("charge: %w", &pgLikeError{}), "obsx_test.pgLikeError"},
		{fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", context.Canceled)), "errors.errorString"},
		// A join with several causes has no single class; the join is reported.
		{errors.Join(errors.New("a"), errors.New("b")), "errors.joinError"},
		// A plain fmt.Errorf with no %w is its own class.
		{fmt.Errorf("plain %d", 1), "errors.errorString"},
	}
	for _, tc := range cases {
		if got := obsx.ErrorType(tc.err); got != tc.want {
			t.Errorf("ErrorType(%T) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// A message cut inside a multibyte rune is invalid UTF-8, which fails
// proto.Marshal on export and drops the whole span batch. The cut must land on
// a rune boundary and already-invalid input must be repaired.
func TestRecordError_MessageStaysValidUTF8(t *testing.T) {
	rec := recorder(t)
	// 254 ASCII bytes, then a 3-byte rune straddling byte 256.
	msg := strings.Repeat("a", 254) + "—" + strings.Repeat("b", 50) + "\xff"
	ctx, span := obsx.StartSpan(context.Background(), "svc", "work")
	obsx.RecordError(ctx, errors.New(msg))
	span.End()

	got := rec.Ended()[0]
	if !utf8.ValidString(got.Status().Description) {
		t.Errorf("status description is not valid UTF-8: %q", got.Status().Description)
	}
	for _, a := range got.Events()[0].Attributes {
		if a.Key == "exception.message" {
			if !utf8.ValidString(a.Value.AsString()) {
				t.Errorf("exception.message is not valid UTF-8: %q", a.Value.AsString())
			}
			if !strings.HasSuffix(a.Value.AsString(), "…(truncated)") {
				t.Error("long message must carry the truncation marker")
			}
		}
	}
}

func TestRecordError_NilIsNoop(t *testing.T) {
	rec := recorder(t)
	ctx, span := obsx.StartSpan(context.Background(), "svc", "work")
	obsx.RecordError(ctx, nil)
	span.End()
	got := rec.Ended()[0]
	if got.Status().Code != codes.Unset || len(got.Events()) != 0 {
		t.Error("RecordError(nil) must leave the span untouched")
	}
}
