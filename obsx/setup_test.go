package obsx

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

func TestConfigFromEnv_Defaults(t *testing.T) {
	for _, k := range []string{"OTEL_SERVICE_NAME", "SERVICE_NAME", "SERVICE_VERSION",
		"OTEL_COLLECTOR_ENDPOINT", "TRACING_ENABLED", "OTEL_SAMPLE_RATE",
		"OTEL_METRICS_ENABLED", "OTEL_LOGS_ENABLED", "OTEL_METRIC_EXPORT_INTERVAL_SECONDS",
		"PROFILING_ENABLED"} {
		t.Setenv(k, "")
	}
	cfg := ConfigFromEnv()

	if !cfg.ProfilingEnabled {
		t.Error("ProfilingEnabled should default to true (fleet config default)")
	}

	if cfg.ServiceName != "unknown-service" {
		t.Errorf("ServiceName = %q, want unknown-service", cfg.ServiceName)
	}
	if !cfg.TracesEnabled {
		t.Error("TracesEnabled should default to true")
	}
	if !cfg.MetricsEnabled {
		t.Error("metrics must default to ENABLED (OTLP is the only pipeline since the P3 cutover)")
	}
	if cfg.LogsEnabled {
		t.Error("logs must default to DISABLED (P4 rollout flag)")
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
	t.Setenv("OTEL_METRICS_ENABLED", "false") // explicit kill switch
	t.Setenv("OTEL_LOGS_ENABLED", "true")
	t.Setenv("TRACING_ENABLED", "false")
	t.Setenv("OTEL_SAMPLE_RATE", "not-a-number") // invalid → default
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL_SECONDS", "bogus")
	t.Setenv("PROFILING_ENABLED", "false")

	cfg := ConfigFromEnv()
	if cfg.ProfilingEnabled {
		t.Error("PROFILING_ENABLED=false must disable the profiling wrap")
	}
	if cfg.MetricsInterval != 15*time.Second {
		t.Errorf("invalid interval must fall back to 15s, got %v", cfg.MetricsInterval)
	}
	if cfg.ServiceName != "order" {
		t.Errorf("ServiceName = %q, want order", cfg.ServiceName)
	}
	if cfg.MetricsEnabled || !cfg.LogsEnabled || cfg.TracesEnabled {
		t.Errorf("flag parsing wrong (metrics kill switch / logs on / traces off): %+v", cfg)
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
	if obs.Enabled() != (Signals{}) {
		t.Errorf("Enabled() = %+v, want all false when every signal is disabled", obs.Enabled())
	}
	// The API accessors must return TRUE nils, not typed nils in interfaces —
	// a main() that checks `obs.MeterProvider() != nil` must get the right
	// answer.
	if obs.TracerProvider() != nil || obs.MeterProvider() != nil || obs.LoggerProvider() != nil {
		t.Error("accessors must return nil when every signal is disabled")
	}
	if err := obs.ForceFlush(context.Background()); err != nil {
		t.Errorf("ForceFlush with no signals must be a no-op: %v", err)
	}
	var nilObs *Observability
	if err := nilObs.ForceFlush(context.Background()); err != nil {
		t.Errorf("ForceFlush on a nil Observability must be a no-op: %v", err)
	}
	if err := obs.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown of empty Observability: %v", err)
	}
}

func TestObservability_NilReceiverIsSafe(t *testing.T) {
	// main() keeps a nil *Observability when SetupObservability failed and
	// still asks it questions; every accessor must answer "nothing".
	var obs *Observability
	if obs.Enabled() != (Signals{}) {
		t.Errorf("Enabled() on nil = %+v, want zero", obs.Enabled())
	}
	if obs.TracerProvider() != nil || obs.MeterProvider() != nil || obs.LoggerProvider() != nil {
		t.Error("accessors on a nil receiver must return nil")
	}
	if err := obs.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on nil: %v", err)
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

	if otel.GetMeterProvider() != obs.MeterProvider() {
		t.Error("global MeterProvider must be installed (otelgrpc/Temporal ride on it)")
	}
	if !obs.Enabled().Metrics || obs.Enabled().Traces || obs.Enabled().Logs {
		t.Errorf("Enabled() = %+v, want metrics only", obs.Enabled())
	}

	meter := obs.MeterProvider().Meter("test")

	dur, err := meter.Float64Histogram("http.server.request.duration")
	if err != nil {
		t.Fatal(err)
	}
	// otelgin falls back to the client-supplied Host header for server.port
	// when the configured service name is portless — the View must drop both
	// keys or any caller can mint arbitrary series (cardinality DoS).
	dur.Record(ctx, 0.25, metric.WithAttributes(
		attribute.String("server.address", "evil-host"),
		attribute.Int("server.port", 31337),
		attribute.String("http.request.method", "GET")))

	rpc, err := meter.Float64Histogram("rpc.client.call.duration")
	if err != nil {
		t.Fatal(err)
	}
	// server.address is a per-pod IP under headless DNS — the View must drop it.
	rpc.Record(ctx, 0.05, metric.WithAttributes(
		attribute.String("server.address", "10.1.2.3"),
		attribute.Int("server.port", 9090),
		attribute.String("rpc.method", "CreateShipment")))

	// otelpgx creates this instrument via the semconv dbconv helper with no
	// bucket hint, so without the View the SDK's ms-shaped default (0,5,…,10000)
	// applies to a seconds-unit histogram and every sub-5s query collapses into
	// the first bucket. Record a typical 2ms query to prove the DB-scale
	// boundaries are in effect.
	db, err := meter.Float64Histogram("db.client.operation.duration")
	if err != nil {
		t.Fatal(err)
	}
	db.Record(ctx, 0.002, metric.WithAttributes(
		attribute.String("pgx.operation.type", "query")))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	var sawDuration, sawRPC, sawDB, sawRuntime bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "http.server.request.duration":
				sawDuration = true
				h, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("duration data type %T", m.Data)
				}
				attrs := h.DataPoints[0].Attributes
				if _, found := attrs.Value("server.address"); found {
					t.Error("server.address must be dropped from http.server.request.duration (Host-header cardinality)")
				}
				if _, found := attrs.Value("server.port"); found {
					t.Error("server.port must be dropped from http.server.request.duration")
				}
				if _, found := attrs.Value("http.request.method"); !found {
					t.Error("http.request.method must survive the attribute filter")
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
			case "db.client.operation.duration":
				sawDB = true
				h, ok := m.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("db duration data type %T", m.Data)
				}
				got := h.DataPoints[0].Bounds
				if len(got) != len(DBDurationBuckets) {
					t.Fatalf("db duration bounds = %v, want the DB-scale set %v", got, DBDurationBuckets)
				}
				for i := range got {
					if got[i] != DBDurationBuckets[i] {
						t.Fatalf("db duration bounds = %v, want %v", got, DBDurationBuckets)
					}
				}
				if _, found := h.DataPoints[0].Attributes.Value("pgx.operation.type"); !found {
					t.Error("pgx.operation.type must survive on db.client.operation.duration (bounded op label)")
				}
			case "go.goroutine.count":
				sawRuntime = true
			}
		}
	}
	if !sawDuration || !sawRPC || !sawDB {
		t.Fatalf("missing instruments: duration=%v rpc=%v db=%v", sawDuration, sawRPC, sawDB)
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

	meter := obs.MeterProvider().Meter("test")
	for _, name := range []string{"http.server.request.body.size", "http.server.response.body.size"} {
		h, err := meter.Int64Histogram(name)
		if err != nil {
			t.Fatal(err)
		}
		h.Record(ctx, 512, metric.WithAttributes(attribute.String("server.address", "evil-host")))
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
			if _, found := h.DataPoints[0].Attributes.Value("server.address"); found {
				t.Errorf("%s: server.address must be dropped by the View", m.Name)
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

	if obs.tracerProvider == nil {
		t.Fatal("stock SDK provider nil with TracesEnabled")
	}
	if otel.GetTracerProvider() != obs.TracerProvider() {
		t.Error("TracerProvider() must be what was installed as the global")
	}
	if !obs.Enabled().Traces {
		t.Error("Enabled().Traces must be true")
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

	_, span := obs.TracerProvider().Tracer("t").Start(ctx, "op")
	span.End()
	if err := obs.tracerProvider.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(spans))
	}
}

func TestSetupObservability_ProfilingWrapsGlobalTracer(t *testing.T) {
	ctx := context.Background()
	obs, err := SetupObservability(ctx,
		Config{ServiceName: "t", TracesEnabled: true, SampleRate: 1, ProfilingEnabled: true},
		withSpanExporter(tracetest.NewInMemoryExporter()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Shutdown(ctx) })

	// The global must be the Pyroscope wrapper (trace→profile correlation),
	// NOT the raw SDK provider — while Observability keeps the raw provider
	// for Shutdown. TestSetupObservability_TracesAndGlobals covers the
	// unwrapped branch (ProfilingEnabled=false → global == raw provider).
	global := otel.GetTracerProvider()
	if global == nil {
		t.Fatal("no global TracerProvider installed")
	}
	if global == any(obs.tracerProvider) {
		t.Error("ProfilingEnabled must install the profiling wrapper as the global, got the raw SDK provider")
	}
	if obs.tracerProvider == nil {
		t.Error("the raw SDK provider must be kept for Shutdown")
	}
	if obs.TracerProvider() != global {
		t.Error("TracerProvider() must return the installed wrapper, not the raw provider")
	}
	// Spans must still flow through the wrapper.
	_, span := global.Tracer("t").Start(ctx, "op")
	span.End()
}

func TestExportInterval_Clamps(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"in-window value passes through", 30 * time.Second, 30 * time.Second},
		{"sub-second tight loop falls back", 100 * time.Millisecond, 15 * time.Second},
		{"zero falls back", 0, 15 * time.Second},
		{"negative (float overflow wrap) falls back", -time.Hour, 15 * time.Second},
		{"multi-year (export effectively off) falls back", 24 * 365 * time.Hour, 15 * time.Second},
		{"window edges are valid", time.Second, time.Second},
		{"upper edge is valid", 5 * time.Minute, 5 * time.Minute},
	}
	for _, c := range cases {
		if got := exportInterval(c.in); got != c.want {
			t.Errorf("%s: exportInterval(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
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

	// The provider is installed as the OTel global, which is where the
	// logger/slogx facade finds it — obsx no longer hands out a zap bridge.
	if obs.LoggerProvider() == nil {
		t.Fatal("LoggerProvider must be non-nil when logs are enabled")
	}
	var rec otellog.Record
	rec.SetBody(attribute.StringValue("hello otlp"))
	global.GetLoggerProvider().Logger("t").Emit(ctx, rec)

	// ForceFlush exports what the batch processor holds WITHOUT stopping the
	// provider: this is the FATAL path, where nothing runs after the call.
	if err := obs.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if got := exp.count(); got != 1 {
		t.Fatalf("after ForceFlush exported %d records, want 1", got)
	}
	global.GetLoggerProvider().Logger("t").Emit(ctx, rec)
	if err := obs.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := exp.count(); got != 2 {
		t.Fatalf("the provider must still export after ForceFlush; got %d records, want 2", got)
	}
}

// ForceFlush reaches every built provider, including a factory-built tracer
// provider that implements it, and joins their errors.
func TestObservability_ForceFlushJoinsEveryProvider(t *testing.T) {
	calls := 0
	boom := errors.New("collector refused")
	obs := &Observability{flushes: []func(context.Context) error{
		func(context.Context) error { calls++; return nil },
		func(context.Context) error { calls++; return boom },
		func(context.Context) error { calls++; return nil },
	}}
	if err := obs.ForceFlush(context.Background()); !errors.Is(err, boom) {
		t.Errorf("ForceFlush must report a provider's error: %v", err)
	}
	if calls != 3 {
		t.Errorf("a failing provider must not starve the rest: %d of 3 ran", calls)
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
	if obs.Enabled() != (Signals{Traces: true, Metrics: true, Logs: true}) {
		t.Fatalf("Enabled() = %+v, want all true when all signals are enabled", obs.Enabled())
	}
	if obs.TracerProvider() == nil || obs.MeterProvider() == nil || obs.LoggerProvider() == nil {
		t.Fatal("all accessors must be non-nil when all signals are enabled")
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
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "") // hermetic: buildResource reads it since Task 1.2
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

// fakeFactoryProvider is a minimal ShutdownTracerProvider with a distinct
// concrete type, standing in for temporalx's ReplaySafeTracerProvider.
type fakeFactoryProvider struct {
	*sdktrace.TracerProvider
}

func TestSetupObservability_TracerProviderFactory(t *testing.T) {
	ctx := context.Background()
	exp := tracetest.NewInMemoryExporter()
	var got []sdktrace.TracerProviderOption
	obs, err := SetupObservability(ctx,
		Config{ServiceName: "t", TracesEnabled: true, SampleRate: 1, ProfilingEnabled: true},
		withSpanExporter(exp),
		WithTracerProviderFactory(func(c TracerProviderConfig) ShutdownTracerProvider {
			got = c.SDKOptions()
			return &fakeFactoryProvider{sdktrace.NewTracerProvider(c.SDKOptions()...)}
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Shutdown(ctx) })

	if len(got) != 3 {
		t.Fatalf("factory received %d options, want 3 (resource, sampler, batcher)", len(got))
	}
	if obs.tracerProvider != nil {
		t.Error("the stock provider must not be built under a factory")
	}
	global := otel.GetTracerProvider()
	if _, ok := global.(*fakeFactoryProvider); !ok {
		// ProfilingEnabled=true on purpose: the wrapper must be SKIPPED so the
		// factory's concrete type survives as the global (the OTel v2 plugin
		// type-asserts it).
		t.Fatalf("global must be the factory's concrete type, got %T", global)
	}
	if obs.TracerProvider() != global {
		t.Error("TracerProvider() must return the factory's provider — what was installed")
	}
	if !obs.Enabled().Traces {
		t.Error("Enabled().Traces must be true under a factory")
	}

	_, span := global.Tracer("t").Start(ctx, "op")
	span.End()
	if err := global.(*fakeFactoryProvider).ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(exp.GetSpans()); n != 1 {
		t.Fatalf("exported %d spans through the factory provider, want 1", n)
	}
	if err := obs.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown must close the factory provider: %v", err)
	}
}

func TestSetupObservability_PropagatorInstalledWithoutTraces(t *testing.T) {
	// RFC-0031 Task 1.2: a process that exports no spans must still forward
	// W3C trace context, or one TRACING_ENABLED=false hop breaks every trace
	// downstream of it. Before this test the propagator was only installed
	// inside the traces branch.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator()) // reset to an empty one
	ctx := context.Background()
	obs, err := SetupObservability(ctx, Config{ServiceName: "t"}) // every signal off
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Shutdown(ctx) })

	fields := otel.GetTextMapPropagator().Fields()
	var hasTraceparent, hasBaggage bool
	for _, f := range fields {
		hasTraceparent = hasTraceparent || f == "traceparent"
		hasBaggage = hasBaggage || f == "baggage"
	}
	if !hasTraceparent || !hasBaggage {
		t.Fatalf("propagator fields = %v, want traceparent and baggage with traces disabled", fields)
	}

	// The real hop: transport instrumentation extracts the incoming context,
	// starts a span on the GLOBAL provider — a noop provider here, since
	// traces are off — and injects from the span's context on the way out.
	// The noop tracer keeps a remote parent, so the same trace id must leave.
	carrier := propagation.MapCarrier{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
	extracted := otel.GetTextMapPropagator().Extract(ctx, carrier)
	spanCtx, span := otel.Tracer("t").Start(extracted, "op")
	defer span.End()
	out := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(spanCtx, out)
	if !strings.HasPrefix(out["traceparent"], "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
		t.Fatalf("trace id not forwarded through a noop span without traces: got %q", out["traceparent"])
	}
}

func TestResourceAttributes_Precedence(t *testing.T) {
	// OTEL_RESOURCE_ATTRIBUTES is the lowest source: the manifest-level
	// Downward-API variables and Config override the same keys, and the
	// standard list's own duplicates resolve last-wins.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", " service.version=from-env , service.namespace=env-ns,service.instance.id=pod-1, ,bogus,=nokey,empty=,service.instance.id=pod-2,host.name=node%20a,cloud.region=bad%zz")
	t.Setenv("K8S_NAMESPACE_NAME", "order")
	t.Setenv("K8S_POD_NAME", "order-abc123")
	t.Setenv("DEPLOYMENT_ENVIRONMENT", "local")

	got := map[attribute.Key]string{}
	for _, kv := range resourceAttributes(Config{ServiceName: "order", ServiceVersion: "1.2.3"}) {
		if _, dup := got[kv.Key]; dup {
			t.Errorf("attribute %s emitted twice", kv.Key)
		}
		got[kv.Key] = kv.Value.AsString()
	}
	want := map[attribute.Key]string{
		semconv.ServiceNameKey:               "order",
		semconv.ServiceVersionKey:            "1.2.3", // Config beats OTEL_RESOURCE_ATTRIBUTES
		semconv.ServiceNamespaceKey:          "order", // K8S_NAMESPACE_NAME beats OTEL_RESOURCE_ATTRIBUTES
		semconv.K8SNamespaceNameKey:          "order",
		semconv.K8SPodNameKey:                "order-abc123",
		semconv.DeploymentEnvironmentNameKey: "local",
		semconv.ServiceInstanceIDKey:         "pod-2",  // last wins inside the env list
		attribute.Key("host.name"):           "node a", // percent-decoded like the SDK detector
		attribute.Key("cloud.region"):        "bad%zz", // undecodable → raw value kept, as the SDK does
	}
	// "=nokey" (empty key) and "empty=" (empty value) must produce nothing.
	if len(got) != len(want) {
		t.Errorf("attribute set = %v, want exactly %d keys (the contract lists them)", got, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("attr[%s] = %q, want %q", k, got[k], v)
		}
	}
}

func TestResourceAttributes_EnvFillsWhatConfigLeavesEmpty(t *testing.T) {
	// The worker manifests put service.version into OTEL_RESOURCE_ATTRIBUTES
	// (from the build-id label) and leave SERVICE_VERSION unset; that value
	// must survive, and unset Downward-API variables must add nothing.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.version=build-7")
	t.Setenv("K8S_NAMESPACE_NAME", "")
	t.Setenv("K8S_POD_NAME", "")
	t.Setenv("DEPLOYMENT_ENVIRONMENT", "")

	got := map[attribute.Key]string{}
	for _, kv := range resourceAttributes(Config{ServiceName: "order-worker"}) {
		got[kv.Key] = kv.Value.AsString()
	}
	if got[semconv.ServiceVersionKey] != "build-7" {
		t.Errorf("service.version = %q, want build-7 from OTEL_RESOURCE_ATTRIBUTES", got[semconv.ServiceVersionKey])
	}
	if len(got) != 2 {
		t.Errorf("attribute set = %v, want service.name and service.version only", got)
	}
}

func TestPlatformView_SecondsHistogramsGetFleetBuckets(t *testing.T) {
	// RFC-0031 Task 1.3 / ADR-073: a seconds histogram that matches no named
	// View used to fall back to the SDK's millisecond-shaped defaults and read
	// ~0 at every quantile. Declaring WithUnit("s") is now enough; the View
	// supplies DurationBuckets. Anything not in seconds keeps the SDK default,
	// and no instrument may end up on two streams.
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	obs, err := SetupObservability(ctx, Config{ServiceName: "t", MetricsEnabled: true}, withMetricReader(reader))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Shutdown(ctx) })
	meter := obs.MeterProvider().Meter("test")

	lag, _ := meter.Float64Histogram("order.inventory.commit_lag", metric.WithUnit("s"))
	rpcServer, _ := meter.Float64Histogram("rpc.server.call.duration", metric.WithUnit("s"))
	cents, _ := meter.Int64Histogram("payment.amount", metric.WithUnit("{cent}"))
	unitless, _ := meter.Float64Histogram("legacy.no_unit")
	httpDur, _ := meter.Float64Histogram("http.server.request.duration", metric.WithUnit("s"))
	counter, _ := meter.Int64Counter("order.saga.outcome.total")
	lag.Record(ctx, 0.3)
	rpcServer.Record(ctx, 0.02)
	cents.Record(ctx, 1999)
	unitless.Record(ctx, 0.3)
	httpDur.Record(ctx, 0.1)
	counter.Add(ctx, 1)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	bounds := map[string][]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen[m.Name]++
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok && len(h.DataPoints) > 0 {
				bounds[m.Name] = h.DataPoints[0].Bounds
			}
			if h, ok := m.Data.(metricdata.Histogram[int64]); ok && len(h.DataPoints) > 0 {
				bounds[m.Name] = h.DataPoints[0].Bounds
			}
		}
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("%s produced %d streams, want exactly 1 (a wildcard View beside the named ones would double it)", name, n)
		}
	}
	for _, name := range []string{"order.inventory.commit_lag", "rpc.server.call.duration", "http.server.request.duration"} {
		if got := bounds[name]; !equalFloats(got, DurationBuckets) {
			t.Errorf("%s bounds = %v, want the fleet set %v", name, got, DurationBuckets)
		}
	}
	for _, name := range []string{"payment.amount", "legacy.no_unit"} {
		if got := bounds[name]; equalFloats(got, DurationBuckets) {
			t.Errorf("%s is not a seconds histogram and must keep the SDK default, got the fleet set", name)
		}
		if len(bounds[name]) == 0 {
			t.Errorf("%s produced no histogram data", name)
		}
	}
}

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
