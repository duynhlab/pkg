// SetupObservability (RFC-0014 P0) is the single wiring point for the
// OpenTelemetry SDK. One call in main() builds the shared
// Resource and the per-signal providers — traces (OTLP), metrics (OTLP,
// semconv-shaped via Views) and logs (OTLP via the otelzap bridge) — and
// returns one Shutdown for all of them.
//
// Since the RFC-0014 P3 cutover OTLP metrics are the only pipeline:
// MetricsEnabled defaults to TRUE (OTEL_METRICS_ENABLED=false remains an
// explicit kill switch); logs stay opt-in behind OTEL_LOGS_ENABLED for the
// P4 wave. The scrape-era Prometheus bridge (SetupMetrics) is gone.
package obsx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelzap"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap/zapcore"
)

// Canonical histogram bucket sets (RFC-0014 D-7 exit criteria).
//
// DurationBuckets is the platform's SLO-tuned set: it keeps the 0.2/0.3/0.75
// precision points around the 500 ms latency threshold and the le=2 Apdex
// boundary that the semconv-advised defaults lack. Applied to
// http.server.request.duration via a View — without it the SLO and Apdex
// math silently breaks.
var DurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 0.75, 1, 2, 5, 10}

// DBDurationBuckets shapes db.client.operation.duration (otelpgx, RFC-0017 W4).
// DB queries live an order of magnitude below HTTP requests (~1–5 ms typical),
// so the HTTP set above — whose smallest bucket is 5 ms — would be blind to the
// entire healthy range. This is exactly the semconv-advised set for that
// instrument (https://opentelemetry.io/docs/specs/semconv/database/database-metrics/
// — "metric.db.client.operation.duration"); we keep it verbatim for
// interoperability and accept its known gaps (no sub-1ms bucket, a 0.1→0.5
// jump) rather than invent a bespoke grid. otelpgx creates the histogram via
// the semconv dbconv helper with no bucket hint, so without this View the SDK
// default (0,5,…,10000 — ms-shaped) applies to a seconds-unit histogram and
// every sub-5s query collapses into the first bucket (quantiles become
// garbage).
var DBDurationBuckets = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10}

// BodySizeBuckets shapes http.server.{request,response}.body.size — semconv
// ships no bucket advice for byte histograms, and the SDK default
// (duration-shaped) boundaries are meaningless for sizes.
var BodySizeBuckets = []float64{256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304}

// Config drives SetupObservability. Build it with ConfigFromEnv (canonical
// env mapping) and override fields as needed.
type Config struct {
	// ServiceName is required (semconv service.name). ConfigFromEnv resolves
	// OTEL_SERVICE_NAME, then SERVICE_NAME, then "unknown-service".
	ServiceName string
	// ServiceVersion sets semconv service.version when non-empty.
	ServiceVersion string
	// Endpoint is the OTLP/HTTP collector host:port (no scheme, no path);
	// the in-cluster collector speaks plaintext, so the exporters use the
	// insecure option. Default: OTEL_COLLECTOR_ENDPOINT.
	Endpoint string
	// TracesEnabled builds a TracerProvider (OTLP http, ParentBased sampler)
	// and installs it plus the W3C propagator globally.
	TracesEnabled bool
	// SampleRate is the ParentBased(TraceIDRatioBased) ratio. Values outside
	// [0, 1] (including NaN) fall back to the 0.1 default. 0 is a VALID value
	// meaning "never start a root sample" — hand-built Configs that want the
	// default must set 0.1 explicitly (ConfigFromEnv does). Only used when
	// TracesEnabled.
	SampleRate float64
	// MetricsEnabled builds the OTLP MeterProvider (PeriodicReader +
	// semconv Views) and starts the Go runtime instrumentation. Default
	// true since the P3 cutover — OTLP is the only metrics pipeline.
	MetricsEnabled bool
	// MetricsInterval is the PeriodicReader export interval. The default
	// (15s) deliberately matches the platform's historical scrape interval
	// so dashboard granularity and burn-rate math don't change (D-7); the
	// SDK's own 60s default would be a silent 4x regression.
	MetricsInterval time.Duration
	// LogsEnabled builds the OTLP LoggerProvider; bridge it into zap with
	// Observability.ZapCore.
	LogsEnabled bool
	// ProfilingEnabled wraps the global TracerProvider with the Pyroscope
	// span-profile linker (TracerProviderWithProfiles) so trace→profile
	// correlation needs no extra wiring in main(). Only used when
	// TracesEnabled. Observability.TracerProvider stays the raw SDK provider
	// either way (Shutdown needs it).
	ProfilingEnabled bool
}

// ConfigFromEnv builds a Config from the platform's canonical env vars:
// OTEL_SERVICE_NAME/SERVICE_NAME, SERVICE_VERSION, OTEL_COLLECTOR_ENDPOINT,
// TRACING_ENABLED (default true), OTEL_SAMPLE_RATE (default 0.1),
// OTEL_METRICS_ENABLED (default true since the P3 cutover — set false only
// as a kill switch), OTEL_LOGS_ENABLED (default false — P4 rollout flag),
// OTEL_METRIC_EXPORT_INTERVAL_SECONDS (default 15),
// PROFILING_ENABLED (default true, matching the fleet's config default).
func ConfigFromEnv() Config {
	name := os.Getenv("OTEL_SERVICE_NAME")
	if name == "" {
		name = os.Getenv("SERVICE_NAME")
	}
	if name == "" {
		name = "unknown-service"
	}
	endpoint := os.Getenv("OTEL_COLLECTOR_ENDPOINT")
	if endpoint == "" {
		endpoint = "otel-collector-opentelemetry-collector.monitoring.svc.cluster.local:4318"
	}
	return Config{
		ServiceName:      name,
		ServiceVersion:   os.Getenv("SERVICE_VERSION"),
		Endpoint:         endpoint,
		TracesEnabled:    envBool("TRACING_ENABLED", true),
		SampleRate:       envFloat("OTEL_SAMPLE_RATE", 0.1),
		MetricsEnabled:   envBool("OTEL_METRICS_ENABLED", true),
		MetricsInterval:  time.Duration(envFloat("OTEL_METRIC_EXPORT_INTERVAL_SECONDS", 15)) * time.Second,
		LogsEnabled:      envBool("OTEL_LOGS_ENABLED", false),
		ProfilingEnabled: envBool("PROFILING_ENABLED", true),
	}
}

// Observability holds the providers built by SetupObservability. Providers
// for disabled signals are nil.
type Observability struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	LoggerProvider *sdklog.LoggerProvider
	Resource       *resource.Resource

	shutdowns []func(context.Context) error
}

// setupOption customizes SetupObservability internals; test-only for now
// (reader/exporter injection keeps unit tests off the network).
type setupOption func(*setupState)

type setupState struct {
	metricReader sdkmetric.Reader
	spanExporter sdktrace.SpanExporter
	logExporter  sdklog.Exporter
}

func withMetricReader(r sdkmetric.Reader) setupOption {
	return func(s *setupState) { s.metricReader = r }
}

func withSpanExporter(e sdktrace.SpanExporter) setupOption {
	return func(s *setupState) { s.spanExporter = e }
}

func withLogExporter(e sdklog.Exporter) setupOption {
	return func(s *setupState) { s.logExporter = e }
}

// SetupObservability wires the OpenTelemetry SDK for a service. Call it once
// in main() and defer Shutdown. Signals are built independently per Config;
// enabled providers are also installed as the OTel globals so contrib
// instrumentation (otelgin, otelgrpc, Temporal SDK, log bridges) picks them
// up without further wiring. opts are internal (test injection); external
// callers pass none.
func SetupObservability(ctx context.Context, cfg Config, opts ...setupOption) (*Observability, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("obsx: Config.ServiceName is required")
	}
	var st setupState
	for _, o := range opts {
		o(&st)
	}

	res := buildResource(ctx, cfg)
	obs := &Observability{Resource: res}

	if cfg.TracesEnabled {
		exp := st.spanExporter
		if exp == nil {
			var err error
			exp, err = otlptracehttp.New(ctx,
				otlptracehttp.WithEndpoint(cfg.Endpoint),
				otlptracehttp.WithInsecure(),
				otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
			)
			if err != nil {
				return nil, fmt.Errorf("obsx: build trace exporter: %w", err)
			}
		}
		rate := cfg.SampleRate
		// Inverted comparison so NaN (which fails every comparison) also
		// falls back to the default instead of reaching the sampler.
		if !(rate >= 0 && rate <= 1) {
			rate = 0.1
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(rate))),
			sdktrace.WithBatcher(exp),
		)
		obs.TracerProvider = tp
		obs.shutdowns = append(obs.shutdowns, tp.Shutdown)
	}

	if cfg.MetricsEnabled {
		reader := st.metricReader
		if reader == nil {
			exp, err := otlpmetrichttp.New(ctx,
				otlpmetrichttp.WithEndpoint(cfg.Endpoint),
				otlpmetrichttp.WithInsecure(),
				otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression),
			)
			if err != nil {
				return nil, errors.Join(fmt.Errorf("obsx: build metric exporter: %w", err), obs.Shutdown(ctx))
			}
			reader = sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(exportInterval(cfg.MetricsInterval)))
		}
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(reader),
			sdkmetric.WithView(metricViews()...),
		)
		if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
			return nil, errors.Join(fmt.Errorf("obsx: start runtime instrumentation: %w", err), mp.Shutdown(ctx), obs.Shutdown(ctx))
		}
		obs.MeterProvider = mp
		obs.shutdowns = append(obs.shutdowns, mp.Shutdown)
	}

	if cfg.LogsEnabled {
		exp := st.logExporter
		if exp == nil {
			var err error
			exp, err = otlploghttp.New(ctx,
				otlploghttp.WithEndpoint(cfg.Endpoint),
				otlploghttp.WithInsecure(),
				otlploghttp.WithCompression(otlploghttp.GzipCompression),
			)
			if err != nil {
				return nil, errors.Join(fmt.Errorf("obsx: build log exporter: %w", err), obs.Shutdown(ctx))
			}
		}
		lp := sdklog.NewLoggerProvider(
			sdklog.WithResource(res),
			sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
		)
		obs.LoggerProvider = lp
		obs.shutdowns = append(obs.shutdowns, lp.Shutdown)
	}

	// Every enabled signal built — only now touch process-wide state. A
	// partial failure above therefore never leaves an already-shut-down
	// provider installed as a global (which would silently drop every span
	// for the process lifetime while the service keeps serving).
	if obs.TracerProvider != nil {
		var tp trace.TracerProvider = obs.TracerProvider
		if cfg.ProfilingEnabled {
			tp = TracerProviderWithProfiles(tp)
		}
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{},
		))
	}
	if obs.MeterProvider != nil {
		otel.SetMeterProvider(obs.MeterProvider)
	}
	if obs.LoggerProvider != nil {
		global.SetLoggerProvider(obs.LoggerProvider)
	}

	return obs, nil
}

// exportInterval clamps the PeriodicReader interval to a sane operational
// window. Below 1s a misconfigured env turns the exporter into a tight loop;
// above 5m export is effectively disabled (and float-parsed garbage from
// OTEL_METRIC_EXPORT_INTERVAL_SECONDS can overflow to multi-year or negative
// durations). Outside the window the platform default (15s, D-7) applies.
func exportInterval(d time.Duration) time.Duration {
	if d < time.Second || d > 5*time.Minute {
		return 15 * time.Second
	}
	return d
}

// ZapCore returns a zapcore.Core that bridges zap records into the OTLP log
// pipeline (tee it next to the service's stdout core), or nil when logs are
// disabled. scopeName is the instrumentation scope, typically the service
// name.
//
// min gates the bridge to the service's configured level. This is not
// cosmetic: the raw otelzap core enables EVERY level (the SDK logger has no
// level concept), and under zapcore.NewTee each core gates independently — an
// ungated bridge would export Debug records over OTLP that never reach the
// Info-level stdout core, and debug statements are exactly where payload and
// token dumps hide.
func (o *Observability) ZapCore(scopeName string, min zapcore.Level) zapcore.Core {
	if o == nil || o.LoggerProvider == nil {
		// A no-op core (never nil) lets every caller tee unconditionally:
		// zapcore.NewTee(stdoutCore, obs.ZapCore(name, lvl)).
		return zapcore.NewNopCore()
	}
	core := otelzap.NewCore(scopeName, otelzap.WithLoggerProvider(o.LoggerProvider))
	leveled, err := zapcore.NewIncreaseLevelCore(core, min)
	if err != nil {
		// Unreachable with the all-levels otelzap core; keep the gated intent
		// by falling back to the raw core only if wrapping ever fails.
		return core
	}
	return leveled
}

// Shutdown flushes and stops every provider built by SetupObservability, in
// reverse construction order. Safe to call once regardless of which signals
// were enabled.
func (o *Observability) Shutdown(ctx context.Context) error {
	if o == nil {
		return nil
	}
	var errs []error
	for i := len(o.shutdowns) - 1; i >= 0; i-- {
		if err := o.shutdowns[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// buildResource assembles the semconv v1.41 Resource: service identity from
// Config, Kubernetes identity from the Downward API envs (K8S_NAMESPACE_NAME,
// K8S_POD_NAME) and deployment.environment.name from DEPLOYMENT_ENVIRONMENT.
// Partial failure (the classic semconv schema-URL conflict) is tolerated:
// whatever resource.New assembled is used — never fail the service over
// telemetry identity (RFC-0013 lesson from product-service).
func buildResource(ctx context.Context, cfg Config) *resource.Resource {
	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	if v := os.Getenv("K8S_NAMESPACE_NAME"); v != "" {
		attrs = append(attrs, semconv.K8SNamespaceName(v), semconv.ServiceNamespace(v))
	}
	if v := os.Getenv("K8S_POD_NAME"); v != "" {
		attrs = append(attrs, semconv.K8SPodName(v))
	}
	if v := os.Getenv("DEPLOYMENT_ENVIRONMENT"); v != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentNameKey.String(v))
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(attrs...),
	)
	if err != nil || res == nil {
		// Schema conflicts still return a usable partial resource; a nil
		// resource falls back to the attributes alone.
		if res == nil {
			res = resource.NewWithAttributes(semconv.SchemaURL, attrs...)
		}
	}
	return res
}

// metricViews returns the platform's mandatory metric Views (RFC-0014):
// SLO-preserving buckets for the semconv HTTP histograms and the
// server.address/server.port cardinality guard on BOTH metric families that
// carry them. On rpc.client they are per-pod IPs under headless DNS (full
// series churn every rollout); on the http.server instruments otelgin derives
// the port from the client-supplied Host header when the service name has no
// port, so any caller can mint arbitrary label values (cardinality DoS).
// semconv marks them opt-in for HTTP server metrics for exactly this reason.
func metricViews() []sdkmetric.View {
	denyServerAddr := attribute.NewDenyKeysFilter("server.address", "server.port")
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.request.duration"},
			sdkmetric.Stream{
				Aggregation:     sdkmetric.AggregationExplicitBucketHistogram{Boundaries: DurationBuckets},
				AttributeFilter: denyServerAddr,
			},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.request.body.size"},
			sdkmetric.Stream{
				Aggregation:     sdkmetric.AggregationExplicitBucketHistogram{Boundaries: BodySizeBuckets},
				AttributeFilter: denyServerAddr,
			},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.response.body.size"},
			sdkmetric.Stream{
				Aggregation:     sdkmetric.AggregationExplicitBucketHistogram{Boundaries: BodySizeBuckets},
				AttributeFilter: denyServerAddr,
			},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "rpc.client.call.duration"},
			sdkmetric.Stream{AttributeFilter: denyServerAddr},
		),
		// DB-scale buckets for the semconv DB-client histogram (see
		// DBDurationBuckets). The View matches by instrument name, so it applies
		// to ANY emitter of this semconv instrument — today that is only otelpgx
		// (redisotel v9.21 emits db.client.connections.*, not this name), and the
		// semconv-advised boundaries are the right treatment for the instrument
		// regardless of emitter. No AttributeFilter: otelpgx records only the
		// bounded pgx.operation.type + db.system.name pair (v0.11.1 source), and
		// a View without a filter never widens an attribute set anyway.
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "db.client.operation.duration"},
			sdkmetric.Stream{
				Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: DBDurationBuckets},
			},
		),
	}
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}
