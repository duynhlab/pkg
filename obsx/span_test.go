package obsx_test

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

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
