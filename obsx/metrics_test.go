package obsx

import (
	"context"
	"testing"
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
