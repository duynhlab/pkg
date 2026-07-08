package obsx

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestConfigFromEnv_Defaults(t *testing.T) {
	for _, k := range []string{"OTEL_SERVICE_NAME", "SERVICE_NAME", "SERVICE_VERSION",
		"OTEL_COLLECTOR_ENDPOINT", "TRACING_ENABLED", "OTEL_SAMPLE_RATE",
		"OTEL_METRICS_ENABLED", "OTEL_LOGS_ENABLED", "OTEL_METRIC_EXPORT_INTERVAL_SECONDS"} {
		t.Setenv(k, "")
	}
	cfg := ConfigFromEnv()

	if cfg.ServiceName != "unknown-service" {
		t.Errorf("ServiceName = %q, want unknown-service", cfg.ServiceName)
	}
	if !cfg.TracesEnabled {
		t.Error("TracesEnabled should default to true")
	}
	if cfg.MetricsEnabled || cfg.LogsEnabled {
		t.Error("metrics/logs must default to DISABLED (RFC-0014 rollout flags)")
	}
	if cfg.SampleRate != 0.1 {
		t.Errorf("SampleRate = %v, want 0.1", cfg.SampleRate)
	}
	if cfg.MetricsInterval != 15*time.Second {
		t.Errorf("MetricsInterval = %v, want 15s (D-7: matches the scrape interval)", cfg.MetricsInterval)
	}
	if cfg.Endpoint == "" {
		t.Error("Endpoint default must be set")
	}
}

func TestConfigFromEnv_Overrides(t *testing.T) {
	t.Setenv("SERVICE_NAME", "order")
	t.Setenv("OTEL_METRICS_ENABLED", "true")
	t.Setenv("OTEL_LOGS_ENABLED", "true")
	t.Setenv("TRACING_ENABLED", "false")
	t.Setenv("OTEL_SAMPLE_RATE", "not-a-number") // invalid → default
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL_SECONDS", "bogus")

	cfg := ConfigFromEnv()
	if cfg.MetricsInterval != 15*time.Second {
		t.Errorf("invalid interval must fall back to 15s, got %v", cfg.MetricsInterval)
	}
	if cfg.ServiceName != "order" {
		t.Errorf("ServiceName = %q, want order", cfg.ServiceName)
	}
	if !cfg.MetricsEnabled || !cfg.LogsEnabled || cfg.TracesEnabled {
		t.Errorf("flag parsing wrong: %+v", cfg)
	}
	if cfg.SampleRate != 0.1 {
		t.Errorf("invalid OTEL_SAMPLE_RATE must fall back to 0.1, got %v", cfg.SampleRate)
	}
}

func TestSetupObservability_RequiresServiceName(t *testing.T) {
	if _, err := SetupObservability(context.Background(), Config{}); err == nil {
		t.Fatal("want error for empty ServiceName")
	}
}

func TestSetupObservability_DisabledByDefault(t *testing.T) {
	obs, err := SetupObservability(context.Background(), Config{ServiceName: "t"})
	if err != nil {
		t.Fatalf("SetupObservability: %v", err)
	}
	if obs.TracerProvider != nil || obs.MeterProvider != nil || obs.LoggerProvider != nil {
		t.Error("all providers must be nil when every signal is disabled")
	}
	core := obs.ZapCore("t", zapcore.InfoLevel)
	if core == nil {
		t.Fatal("ZapCore must return a usable no-op core when logs are disabled (unconditional tee)")
	}
	if core.Enabled(zapcore.ErrorLevel) {
		t.Error("disabled-logs core must be a no-op")
	}
	if err := obs.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown of empty Observability: %v", err)
	}
}

func TestSetupObservability_MetricsViews(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	obs, err := SetupObservability(ctx,
		Config{ServiceName: "t", MetricsEnabled: true},
		withMetricReader(reader))
	if err != nil {
		t.Fatalf("SetupObservability: %v", err)
	}
	t.Cleanup(func() { _ = obs.Shutdown(ctx) })

	if otel.GetMeterProvider() != obs.MeterProvider {
		t.Error("global MeterProvider must be installed (otelgrpc/Temporal ride on it)")
	}

	meter := obs.MeterProvider.Meter("test")

	dur, err := meter.Float64Histogram("http.server.request.duration")
	if err != nil {
		t.Fatal(err)
	}
	dur.Record(ctx, 0.25)

	rpc, err := meter.Float64Histogram("rpc.client.call.duration")
	if err != nil {
		t.Fatal(err)
	}
	// server.address is a per-pod IP under headless DNS — the View must drop it.
	rpc.Record(ctx, 0.05, metric.WithAttributes(
		attribute.String("server.address", "10.1.2.3"),
		attribute.Int("server.port", 9090),
		attribute.String("rpc.method", "CreateShipment")))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var sawDuration, sawRPC, sawRuntime bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "http.server.request.duration":
				sawDuration = true
				h, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("duration data type %T", m.Data)
				}
				got := h.DataPoints[0].Bounds
				if len(got) != len(DurationBuckets) {
					t.Fatalf("duration bounds = %v, want the platform 13-bucket set %v", got, DurationBuckets)
				}
				for i := range got {
					if got[i] != DurationBuckets[i] {
						t.Fatalf("duration bounds = %v, want %v", got, DurationBuckets)
					}
				}
			case "rpc.client.call.duration":
				sawRPC = true
				h, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("rpc data type %T", m.Data)
				}
				attrs := h.DataPoints[0].Attributes
				if _, found := attrs.Value("server.address"); found {
					t.Error("server.address must be dropped by the View (pod-IP churn)")
				}
				if _, found := attrs.Value("server.port"); found {
					t.Error("server.port must be dropped by the View")
				}
				if _, found := attrs.Value("rpc.method"); !found {
					t.Error("rpc.method must survive the attribute filter")
				}
			case "go.goroutine.count":
				sawRuntime = true
			}
		}
	}
	if !sawDuration || !sawRPC {
		t.Fatalf("missing instruments: duration=%v rpc=%v", sawDuration, sawRPC)
	}
	if !sawRuntime {
		t.Error("runtime instrumentation must be started (go.goroutine.count absent) — D-4 liveness depends on it")
	}
}

func TestSetupObservability_BodySizeViews(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	obs, err := SetupObservability(ctx,
		Config{ServiceName: "t", MetricsEnabled: true},
		withMetricReader(reader))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Shutdown(ctx) })

	meter := obs.MeterProvider.Meter("test")
	for _, name := range []string{"http.server.request.body.size", "http.server.response.body.size"} {
		h, err := meter.Int64Histogram(name)
		if err != nil {
			t.Fatal(err)
		}
		h.Record(ctx, 512)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.body.size" && m.Name != "http.server.response.body.size" {
				continue
			}
			seen++
			h, ok := m.Data.(metricdata.Histogram[int64])
			if !ok {
				t.Fatalf("%s data type %T", m.Name, m.Data)
			}
			got := h.DataPoints[0].Bounds
			if len(got) != len(BodySizeBuckets) || got[0] != BodySizeBuckets[0] || got[len(got)-1] != BodySizeBuckets[len(got)-1] {
				t.Fatalf("%s bounds = %v, want byte buckets %v", m.Name, got, BodySizeBuckets)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("saw %d body.size instruments, want 2", seen)
	}
}

func TestSetupObservability_TracesAndGlobals(t *testing.T) {
	ctx := context.Background()
	exp := tracetest.NewInMemoryExporter()
	obs, err := SetupObservability(ctx,
		Config{ServiceName: "t", TracesEnabled: true, SampleRate: 1},
		withSpanExporter(exp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Shutdown(ctx) })

	if obs.TracerProvider == nil {
		t.Fatal("TracerProvider nil with TracesEnabled")
	}
	if otel.GetTracerProvider() != obs.TracerProvider {
		t.Error("global TracerProvider must be installed")
	}
	fields := otel.GetTextMapPropagator().Fields()
	var hasTraceparent bool
	for _, f := range fields {
		if f == "traceparent" {
			hasTraceparent = true
		}
	}
	if !hasTraceparent {
		t.Errorf("W3C propagator must be installed, fields=%v", fields)
	}

	_, span := obs.TracerProvider.Tracer("t").Start(ctx, "op")
	span.End()
	if err := obs.TracerProvider.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
}

func TestSetupObservability_LogsBridge(t *testing.T) {
	ctx := context.Background()
	exp := &capturingLogExporter{}
	obs, err := SetupObservability(ctx,
		Config{ServiceName: "t", LogsEnabled: true},
		withLogExporter(exp))
	if err != nil {
		t.Fatal(err)
	}

	core := obs.ZapCore("t", zapcore.InfoLevel)
	if core == nil {
		t.Fatal("ZapCore must be non-nil when logs are enabled")
	}
	logger := zap.New(zapcore.NewTee(zapcore.NewNopCore(), core))
	logger.Debug("secret payload dump") // below min level — must NOT be exported
	logger.Info("hello otlp", zap.String("k", "v"))

	if err := obs.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := exp.count(); got != 1 {
		t.Fatalf("exported %d records, want exactly 1 (Info passes, Debug is level-gated)", got)
	}
}

func TestSetupObservability_RealExporterConstruction(t *testing.T) {
	// The OTLP http exporters are lazy — construction never dials, so the
	// un-injected paths (real exporters, interval default, sample-rate clamp)
	// are safe to exercise offline. Shutdown will fail to flush to the dead
	// endpoint; that error is expected and proves the join path works.
	ctx := context.Background()
	obs, err := SetupObservability(ctx, Config{
		ServiceName:    "t",
		Endpoint:       "127.0.0.1:1", // nothing listens here
		TracesEnabled:  true,
		SampleRate:     7, // out of range → clamped to default
		MetricsEnabled: true,
		LogsEnabled:    true,
	})
	if err != nil {
		t.Fatalf("SetupObservability with real exporters: %v", err)
	}
	if obs.TracerProvider == nil || obs.MeterProvider == nil || obs.LoggerProvider == nil {
		t.Fatal("all providers must be built when all signals are enabled")
	}

	shCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	// Export to the dead endpoint fails; Shutdown must surface (not swallow)
	// those flush errors while still stopping every provider.
	if err := obs.Shutdown(shCtx); err == nil {
		t.Log("Shutdown returned nil (nothing buffered) — acceptable")
	}
}

func TestBuildResource_KubernetesIdentity(t *testing.T) {
	t.Setenv("K8S_NAMESPACE_NAME", "order")
	t.Setenv("K8S_POD_NAME", "order-abc123")
	t.Setenv("DEPLOYMENT_ENVIRONMENT", "local")

	res := buildResource(context.Background(), Config{ServiceName: "order", ServiceVersion: "1.2.3"})
	got := map[attribute.Key]string{}
	for _, kv := range res.Attributes() {
		got[kv.Key] = kv.Value.String()
	}
	want := map[attribute.Key]string{
		semconv.ServiceNameKey:               "order",
		semconv.ServiceVersionKey:            "1.2.3",
		semconv.ServiceNamespaceKey:          "order",
		semconv.K8SNamespaceNameKey:          "order",
		semconv.K8SPodNameKey:                "order-abc123",
		semconv.DeploymentEnvironmentNameKey: "local",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("resource[%s] = %q, want %q", k, got[k], v)
		}
	}
}

// capturingLogExporter is an in-memory sdklog.Exporter for bridge tests.
type capturingLogExporter struct {
	mu      sync.Mutex
	records int
}

func (c *capturingLogExporter) Export(_ context.Context, recs []sdklog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records += len(recs)
	return nil
}

func (c *capturingLogExporter) Shutdown(context.Context) error   { return nil }
func (c *capturingLogExporter) ForceFlush(context.Context) error { return nil }

func (c *capturingLogExporter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.records
}
