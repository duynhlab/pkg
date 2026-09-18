package httpmw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
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

// RFC-0031 tracing contract: the platform default is NO application baggage.
// The propagator obsx installs would forward any baggage member to every
// downstream hop, including third-party providers, so the middleware itself
// must never introduce one — an incoming request without baggage leaves the
// handler with an empty baggage set and an outbound injection with no
// `baggage` header.
func TestTracing_SetsNoBaggage(t *testing.T) {
	// otelgin captures the propagator when the middleware is built, so the
	// composite one obsx installs in production must be in place BEFORE the
	// router — the harness's TraceContext-only propagator would hide baggage.
	prevProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	t.Cleanup(func() { otel.SetTextMapPropagator(prevProp) })
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(httpmw.Tracing("test-service"))
	var members int
	var outbound propagation.MapCarrier
	r.GET("/orders/:id", func(c *gin.Context) {
		members = baggage.FromContext(c.Request.Context()).Len()
		outbound = propagation.MapCarrier{}
		otel.GetTextMapPropagator().Inject(c.Request.Context(), outbound)
		c.Status(http.StatusOK)
	})

	do(t, r, "/orders/1", map[string]string{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"})

	if members != 0 {
		t.Errorf("baggage members in handler = %d, want 0 (default-deny)", members)
	}
	if _, ok := outbound["baggage"]; ok {
		t.Errorf("outbound carrier has a baggage header %q; the middleware must add none", outbound["baggage"])
	}
	if outbound["traceparent"] == "" {
		t.Error("traceparent must still be forwarded")
	}

	// And the composite propagator IS in the path: inbound baggage crosses the
	// middleware unchanged — neither added to nor stripped — so the only way a
	// key exists downstream is that a caller put it there deliberately.
	do(t, r, "/orders/2", map[string]string{"baggage": "tenant=t1"})
	if members != 1 {
		t.Errorf("baggage members with an inbound header = %d, want 1 (forwarded, not stripped)", members)
	}
	if outbound["baggage"] != "tenant=t1" {
		t.Errorf("outbound baggage = %q, want the inbound member forwarded unchanged", outbound["baggage"])
	}
}

// The probe skip list is a contract, not a convenience: dashboards, the RED
// metrics and the access log all assume exactly these routes vanish. Pin the
// exact set so an addition is a reviewed change, not a drift.
func TestDefaultSkipRoutes_Golden(t *testing.T) {
	want := map[string]struct{}{
		"/health": {}, "/healthz": {}, "/ready": {}, "/readyz": {}, "/livez": {}, "/metrics": {}, "/favicon.ico": {},
	}
	if len(httpmw.DefaultSkipRoutes) != len(want) {
		t.Fatalf("DefaultSkipRoutes has %d entries, want %d: %v", len(httpmw.DefaultSkipRoutes), len(want), httpmw.DefaultSkipRoutes)
	}
	for route := range want {
		if _, ok := httpmw.DefaultSkipRoutes[route]; !ok {
			t.Errorf("%s missing from DefaultSkipRoutes", route)
		}
	}
}
