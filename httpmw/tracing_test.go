package httpmw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/duynhlab/pkg/httpmw"
)

// harness installs isolated trace and metric providers as the globals httpmw
// reads, and returns a router with a probe route and a parameterised one.
func harness(t *testing.T) (*gin.Engine, *tracetest.SpanRecorder, *sdkmetric.ManualReader) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	prevTP, prevMP, prevProp := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
	})

	r := gin.New()
	r.Use(httpmw.Tracing("test-service"))
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/products/:id", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r, rec, reader
}

func do(t *testing.T, r *gin.Engine, path string, header map[string]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	r.ServeHTTP(httptest.NewRecorder(), req)
}

// serverDurationCount totals the data points recorded for the HTTP server
// duration histogram, whatever semconv spelling the current otelgin uses.
func serverDurationCount(t *testing.T, reader *sdkmetric.ManualReader) int {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	total := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.duration" && m.Name != "http.server.duration" {
				continue
			}
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok {
				for _, dp := range h.DataPoints {
					total += int(dp.Count)
				}
			}
		}
	}
	return total
}

// The platform contract for probe routes is "no span, no metric" — both halves
// matter, and they come from the same otelgin instrumentation. The per-service
// middleware this replaced got that for free by returning before otelgin ran;
// moving the skip into WithGinFilter only preserves it if the filter is checked
// before the metric is recorded, which is what this pins.
func TestTracing_SkippedRouteEmitsNoSpanAndNoMetric(t *testing.T) {
	r, rec, reader := harness(t)

	do(t, r, "/health", nil)

	if got := len(rec.Ended()); got != 0 {
		t.Errorf("spans = %d, want 0 for a skipped route", got)
	}
	if got := serverDurationCount(t, reader); got != 0 {
		t.Errorf("http.server duration data points = %d, want 0 — the filter let a metric through", got)
	}
}

// A real route is traced and measured, and carries http.route so dashboards can
// group by route pattern rather than by raw path.
func TestTracing_RealRouteIsTracedAndMeasured(t *testing.T) {
	r, rec, reader := harness(t)

	do(t, r, "/products/42", nil)

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	var route string
	for _, a := range spans[0].Attributes() {
		if a.Key == attribute.Key("http.route") {
			route = a.Value.AsString()
		}
	}
	if route != "/products/:id" {
		t.Errorf("http.route = %q, want the route pattern %q", route, "/products/:id")
	}
	if got := serverDurationCount(t, reader); got != 1 {
		t.Errorf("http.server duration data points = %d, want 1", got)
	}
}

// A route this service never registered has an empty FullPath, so it is NOT
// skipped. That is deliberate: a probe aimed at a path that does not exist is a
// misconfiguration, and it should be visible rather than silently swallowed.
func TestTracing_UnregisteredProbePathIsTraced(t *testing.T) {
	r, rec, _ := harness(t)

	do(t, r, "/metrics", nil)

	if got := len(rec.Ended()); got != 1 {
		t.Errorf("spans = %d, want 1 — an unregistered path must not be skipped", got)
	}
}

// The edge is the root sampling authority: Envoy starts the trace and sends
// traceparent upstream, so the service span must join that trace rather than
// start a new one.
func TestTracing_JoinsInboundTraceparent(t *testing.T) {
	r, rec, _ := harness(t)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

	do(t, r, "/products/42", map[string]string{
		"traceparent": "00-" + traceID + "-00f067aa0ba902b7-01",
	})

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if got := spans[0].SpanContext().TraceID().String(); got != traceID {
		t.Errorf("trace id = %s, want %s — the edge trace was not joined", got, traceID)
	}
	if !spans[0].Parent().IsValid() {
		t.Error("span has no parent — it started a new trace instead of joining the edge")
	}
}
