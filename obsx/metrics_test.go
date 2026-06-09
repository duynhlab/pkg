package obsx

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// TestSetupMetricsIdempotent verifies repeated calls return usable shutdown
// funcs without re-registering the exporter (a second registration on the
// default Prometheus registry would panic).
func TestSetupMetricsIdempotent(t *testing.T) {
	shutdown1, err := SetupMetrics()
	if err != nil {
		t.Fatalf("first SetupMetrics: %v", err)
	}
	shutdown2, err := SetupMetrics()
	if err != nil {
		t.Fatalf("second SetupMetrics: %v", err)
	}
	if shutdown1 == nil || shutdown2 == nil {
		t.Fatal("SetupMetrics returned a nil shutdown func")
	}
	if err := shutdown1(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

// TestTraceIDFromContextEmpty verifies that a context with no span yields "".
func TestTraceIDFromContextEmpty(t *testing.T) {
	if got := TraceIDFromContext(context.Background()); got != "" {
		t.Fatalf("TraceIDFromContext(empty) = %q, want \"\"", got)
	}
}

// TestTraceIDFromContextValid verifies the trace ID is returned when ctx carries
// a span with a valid trace ID.
func TestTraceIDFromContextValid(t *testing.T) {
	tid := trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid,
		SpanID:  trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	if got, want := TraceIDFromContext(ctx), tid.String(); got != want {
		t.Fatalf("TraceIDFromContext = %q, want %q", got, want)
	}
}
