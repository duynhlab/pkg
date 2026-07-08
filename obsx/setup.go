// SetupObservability (RFC-0014 P0) is the single wiring point for the
// OpenTelemetry SDK. One call in main() builds the shared
// Resource and the per-signal providers — traces (OTLP), metrics (OTLP,
// semconv-shaped via Views) and logs (OTLP via the otelzap bridge) — and
// returns one Shutdown for all of them.
//
// Metrics and logs are opt-in (Config.MetricsEnabled / LogsEnabled, wired to
// OTEL_METRICS_ENABLED / OTEL_LOGS_ENABLED by ConfigFromEnv) so services can
// dual-emit behind env flags during the RFC-0014 rollout. When MetricsEnabled
// is set, the global MeterProvider becomes the OTLP one and supersedes the
// Prometheus bridge installed by SetupMetrics: otelgrpc and Temporal SDK
// metrics then flow over OTLP instead of the scraped /metrics endpoint.
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
	// semconv Views) and starts the Go runtime instrumentation.
	MetricsEnabled bool
	// MetricsInterval is the PeriodicReader export interval. The default
	// (15s) deliberately matches the platform's historical scrape interval
	// so dashboard granularity and burn-rate math don't change (D-7); the
	// SDK's own 60s default would be a silent 4x regression.
	MetricsInterval time.Duration
	// LogsEnabled builds the OTLP LoggerProvider; bridge it into zap with
	// Observability.ZapCore.
	LogsEnabled bool
}

// ConfigFromEnv builds a Config from the platform's canonical env vars:
// OTEL_SERVICE_NAME/SERVICE_NAME, SERVICE_VERSION, OTEL_COLLECTOR_ENDPOINT,
// TRACING_ENABLED (default true), OTEL_SAMPLE_RATE (default 0.1),
// OTEL_METRICS_ENABLED and OTEL_LOGS_ENABLED (default false — RFC-0014
// rollout flags), OTEL_METRIC_EXPORT_INTERVAL_SECONDS (default 15).
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
		ServiceName:     name,
		ServiceVersion:  os.Getenv("SERVICE_VERSION"),
		Endpoint:        endpoint,
		TracesEnabled:   envBool("TRACING_ENABLED", true),
		SampleRate:      envFloat("OTEL_SAMPLE_RATE", 0.1),
		MetricsEnabled:  envBool("OTEL_METRICS_ENABLED", false),
		MetricsInterval: time.Duration(envFloat("OTEL_METRIC_EXPORT_INTERVAL_SECONDS", 15)) * time.Second,
		LogsEnabled:     envBool("OTEL_LOGS_ENABLED", false),
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
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{}, propagation.Baggage{},
		))
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
			interval := cfg.MetricsInterval
			if interval < time.Second {
				interval = 15 * time.Second
			}
			reader = sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(interval))
		}
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(reader),
			sdkmetric.WithView(metricViews()...),
		)
		otel.SetMeterProvider(mp)
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
		global.SetLoggerProvider(lp)
		obs.LoggerProvider = lp
		obs.shutdowns = append(obs.shutdowns, lp.Shutdown)
	}

	return obs, nil
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
// SLO-preserving buckets for the semconv HTTP histograms and the pod-IP
// cardinality guard on gRPC client metrics (server.address/server.port are
// per-pod under headless DNS — a full series churn every rollout).
func metricViews() []sdkmetric.View {
	return []sdkmetric.View{
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.request.duration"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: DurationBuckets}},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.request.body.size"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: BodySizeBuckets}},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "http.server.response.body.size"},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{Boundaries: BodySizeBuckets}},
		),
		sdkmetric.NewView(
			sdkmetric.Instrument{Name: "rpc.client.call.duration"},
			sdkmetric.Stream{AttributeFilter: attribute.NewDenyKeysFilter("server.address", "server.port")},
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
