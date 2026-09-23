// SetupObservability (RFC-0014 P0) is the single wiring point for the
// OpenTelemetry SDK. One call in main() builds the shared
// Resource and the per-signal providers — traces (OTLP), metrics (OTLP,
// semconv-shaped via Views) and logs (OTLP; the logger/slogx facade reads the
// provider from the OTel global this installs) — and returns one Shutdown for
// all of them.
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
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
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
	// and installs it globally. The W3C propagator is installed regardless
	// (RFC-0031 Task 1.2): trace context must cross every hop even when this
	// process exports no spans, or the services behind it lose correlation.
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
	// LogsEnabled builds the OTLP LoggerProvider and installs it as the OTel
	// global, where the logger/slogx facade finds it.
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

// Observability holds the providers built by SetupObservability. Its exported
// surface speaks OpenTelemetry API types only (RFC-0031 / ADR-072, the
// shared-package rule): a service imports obsx and the API, never the SDK.
// The SDK providers stay unexported so that Shutdown, ForceFlush-in-tests and
// the profiling wrapper remain obsx's business.
type Observability struct {
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	loggerProvider *sdklog.LoggerProvider
	res            *resource.Resource

	// flushes export what each built provider has buffered, without stopping
	// it — ForceFlush runs them for the FATAL path, where nothing runs after.
	flushes []func(context.Context) error

	// globalTracerProvider is whatever was installed with otel.SetTracerProvider
	// — the stock provider (possibly profiling-wrapped), or the
	// WithTracerProviderFactory result. Unlike tracerProvider it is non-nil on
	// every path where traces are enabled, so nil-checks about "are traces on"
	// belong here.
	globalTracerProvider trace.TracerProvider

	factoryTracerProvider ShutdownTracerProvider

	shutdowns []func(context.Context) error
}

// Signals reports which signals SetupObservability built. It replaces the
// pre-v0.39 pattern of nil-checking the exported SDK providers in main().
type Signals struct {
	Traces  bool
	Metrics bool
	Logs    bool
}

// Enabled reports which signals were built. Safe on a nil receiver.
func (o *Observability) Enabled() Signals {
	if o == nil {
		return Signals{}
	}
	return Signals{
		Traces:  o.globalTracerProvider != nil,
		Metrics: o.meterProvider != nil,
		Logs:    o.loggerProvider != nil,
	}
}

// TracerProvider returns the tracer provider installed as the OTel global —
// the stock SDK provider, its profiling wrapper, or the
// WithTracerProviderFactory result — as the API type. It is nil (a true nil,
// not a typed nil in an interface) when traces are disabled.
func (o *Observability) TracerProvider() trace.TracerProvider {
	if o == nil || o.globalTracerProvider == nil {
		return nil
	}
	return o.globalTracerProvider
}

// MeterProvider returns the installed meter provider as the API type, or nil
// when metrics are disabled.
func (o *Observability) MeterProvider() metric.MeterProvider {
	if o == nil || o.meterProvider == nil {
		return nil
	}
	return o.meterProvider
}

// LoggerProvider returns the installed logger provider as the API type, or nil
// when logs are disabled. Log bridges built outside obsx (the RFC-0031 slogx
// facade) take it from here or from the otel/log/global it is installed into.
func (o *Observability) LoggerProvider() otellog.LoggerProvider {
	if o == nil || o.loggerProvider == nil {
		return nil
	}
	return o.loggerProvider
}

// SetupOption customizes SetupObservability. The exporter/reader injections
// are unexported and test-only; WithTracerProviderFactory is the one option
// meant for production callers (Temporal services — see its doc).
type SetupOption func(*setupState)

type setupState struct {
	metricReader  sdkmetric.Reader
	spanExporter  sdktrace.SpanExporter
	logExporter   sdklog.Exporter
	tracerFactory TracerProviderFactory
}

// ShutdownTracerProvider is what a WithTracerProviderFactory result must
// satisfy: a usable tracer provider obsx can shut down with the other signals.
type ShutdownTracerProvider interface {
	trace.TracerProvider
	Shutdown(context.Context) error
}

// TracerProviderConfig carries the option set obsx assembled for the stock
// tracer provider — Resource, ParentBased sampler, OTLP batcher — to a
// WithTracerProviderFactory. It is opaque so that a service main never names
// an SDK type; the one way to read it is SDKOptions, below.
type TracerProviderConfig struct {
	opts []sdktrace.TracerProviderOption
}

// SDKOptions returns a copy of the assembled SDK options. It is the single,
// deliberate place where an SDK type crosses obsx's exported surface, and it
// exists for exactly one caller shape: forwarding into a constructor that
// itself takes sdktrace.TracerProviderOption, such as
// temporalx.NewReplaySafeTracerProvider. Used as
//
//	obsx.WithTracerProviderFactory(func(c obsx.TracerProviderConfig) obsx.ShutdownTracerProvider {
//		return temporalx.NewReplaySafeTracerProvider(c.SDKOptions()...)
//	})
//
// the calling package imports neither go.opentelemetry.io/otel/sdk nor any
// SDK type: Go infers the variadic element type. The SDK is still BUILT only
// here (the options) and in temporalx (the constructor the layering rules
// already exempt, ADR-063); the service forwards a value it cannot inspect.
func (c TracerProviderConfig) SDKOptions() []sdktrace.TracerProviderOption {
	return append([]sdktrace.TracerProviderOption(nil), c.opts...)
}

// TracerProviderFactory builds the tracer provider obsx installs as the OTel
// global, from the option set obsx assembled. See WithTracerProviderFactory.
type TracerProviderFactory func(TracerProviderConfig) ShutdownTracerProvider

// WithTracerProviderFactory replaces the stock sdktrace.NewTracerProvider
// constructor while obsx keeps owning the option set (Resource, sampler, OTLP
// batcher), the shutdown ordering, and the global installation. It exists for
// exactly one consumer class today: Temporal workers, whose OTel v2
// integration requires the GLOBAL tracer provider to be the contrib module's
// ReplaySafeTracerProvider — its interceptors and workflow Tracer type-assert
// the global and panic on anything else. Service mains forward
// TracerProviderConfig.SDKOptions into temporalx.NewReplaySafeTracerProvider;
// services without Temporal never set it and nothing changes for them.
//
// Two deliberate consequences when the factory is set:
//   - The stock SDK provider is never built; TracerProvider() returns the
//     factory's provider (it is what was installed as the global).
//   - The otelpyroscope span→profile wrapper is SKIPPED even when
//     cfg.ProfilingEnabled is true, because wrapping would change the global's
//     concrete type and re-trigger the very panic this factory avoids. Profile
//     COLLECTION (SetupProfiling) is unaffected — only the span→profile link
//     attribute is lost on these services.
//
// Breaking in obsx v0.39.0: the factory used to take
// (...sdktrace.TracerProviderOption); it now takes TracerProviderConfig so
// that no service imports the SDK to call obsx.
func WithTracerProviderFactory(f TracerProviderFactory) SetupOption {
	return func(s *setupState) { s.tracerFactory = f }
}

func withMetricReader(r sdkmetric.Reader) SetupOption {
	return func(s *setupState) { s.metricReader = r }
}

func withSpanExporter(e sdktrace.SpanExporter) SetupOption {
	return func(s *setupState) { s.spanExporter = e }
}

func withLogExporter(e sdklog.Exporter) SetupOption {
	return func(s *setupState) { s.logExporter = e }
}

// SetupObservability wires the OpenTelemetry SDK for a service. Call it once
// in main() and defer Shutdown. Signals are built independently per Config;
// enabled providers are also installed as the OTel globals so contrib
// instrumentation (otelgin, otelgrpc, Temporal SDK, log bridges) picks them
// up without further wiring. The W3C TraceContext+Baggage propagator is
// installed unconditionally — the one global set even when every signal is
// off — so trace context crosses this process whether or not it exports. opts are internal (test injection); external
// callers pass none.
func SetupObservability(ctx context.Context, cfg Config, opts ...SetupOption) (*Observability, error) {
	if cfg.ServiceName == "" {
		return nil, errors.New("obsx: Config.ServiceName is required")
	}
	var st setupState
	for _, o := range opts {
		o(&st)
	}

	res := buildResource(ctx, cfg)
	obs := &Observability{res: res}

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
		tpOpts := []sdktrace.TracerProviderOption{
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(rate))),
			sdktrace.WithBatcher(exp),
		}
		if st.tracerFactory != nil {
			ftp := st.tracerFactory(TracerProviderConfig{opts: tpOpts})
			obs.factoryTracerProvider = ftp
			obs.shutdowns = append(obs.shutdowns, ftp.Shutdown)
			if f, ok := ftp.(interface{ ForceFlush(context.Context) error }); ok {
				obs.flushes = append(obs.flushes, f.ForceFlush)
			}
		} else {
			tp := sdktrace.NewTracerProvider(tpOpts...)
			obs.tracerProvider = tp
			obs.shutdowns = append(obs.shutdowns, tp.Shutdown)
			obs.flushes = append(obs.flushes, tp.ForceFlush)
		}
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
		obs.meterProvider = mp
		obs.shutdowns = append(obs.shutdowns, mp.Shutdown)
		obs.flushes = append(obs.flushes, mp.ForceFlush)
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
		obs.loggerProvider = lp
		obs.shutdowns = append(obs.shutdowns, lp.Shutdown)
		obs.flushes = append(obs.flushes, lp.ForceFlush)
	}

	// Every enabled signal built — only now touch process-wide state. A
	// partial failure above therefore never leaves an already-shut-down
	// provider installed as a global (which would silently drop every span
	// for the process lifetime while the service keeps serving).
	//
	// The propagator is independent of TracesEnabled. A process that exports
	// no spans still has to forward traceparent/baggage on every outbound
	// call, otherwise a single service with TRACING_ENABLED=false breaks the
	// trace for everything downstream of it (RFC-0031 goal: W3C correlation
	// whether export is enabled or not).
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	if obs.tracerProvider != nil || obs.factoryTracerProvider != nil {
		var tp trace.TracerProvider
		if obs.factoryTracerProvider != nil {
			// No profiling wrap here on purpose — see WithTracerProviderFactory.
			tp = obs.factoryTracerProvider
		} else {
			tp = obs.tracerProvider
			if cfg.ProfilingEnabled {
				tp = TracerProviderWithProfiles(tp)
			}
		}
		otel.SetTracerProvider(tp)
		obs.globalTracerProvider = tp
	}
	if obs.meterProvider != nil {
		otel.SetMeterProvider(obs.meterProvider)
	}
	if obs.loggerProvider != nil {
		global.SetLoggerProvider(obs.loggerProvider)
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

// ForceFlush exports what every provider built by SetupObservability has
// buffered, without stopping them. It exists for the one record nothing runs
// after: wire it into the facade as slogx.Config{Flush: obs.ForceFlush} and a
// FATAL record leaves the process before it exits, instead of dying in the
// batch processor. Call Fatal before Shutdown — a stopped provider exports
// nothing. Safe on a nil or signal-less Observability.
func (o *Observability) ForceFlush(ctx context.Context) error {
	if o == nil {
		return nil
	}
	var errs []error
	for _, f := range o.flushes {
		if err := f(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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

// resourceAttributes is the ONE place the platform's resource identity is
// derived from the environment (RFC-0031 Task 1.2). The tracer, meter and
// logger provider read this list today; Task 1.4 switches the profiler's
// labels to it as well, so that no signal can disagree about who a process is.
//
// Sources, lowest precedence first:
//
//  1. OTEL_RESOURCE_ATTRIBUTES — the standard comma-separated key=value list
//     (the domain ResourceSets put service.namespace, service.instance.id and
//     service.version there). Parsed like the SDK's env detector — keys and
//     values trimmed, values percent-decoded (raw value kept when decoding
//     fails), last occurrence of a key wins — with two deliberate differences:
//     a pair with an empty key or an empty value is dropped rather than kept
//     as an empty attribute, and a malformed entry is skipped rather than
//     reported as a partial-resource error (identity never fails a service).
//  2. The Downward-API variables the manifests set: K8S_NAMESPACE_NAME →
//     k8s.namespace.name AND service.namespace, K8S_POD_NAME → k8s.pod.name,
//     DEPLOYMENT_ENVIRONMENT → deployment.environment.name (the current
//     semconv key — never the retired deployment.environment).
//  3. Config: service.name always, service.version when set (SERVICE_VERSION).
//
// The contract deliberately produces nothing else: no container, node,
// cluster or region attribute — those are collector-side enrichment, decided
// separately (RFC-0031 Task 4.4). A key listed under (2) or (3) overrides the
// same key from (1); TestResourceAttributes_Precedence pins that order.
func resourceAttributes(cfg Config) []attribute.KeyValue {
	merged := map[attribute.Key]string{}
	var order []attribute.Key
	set := func(k attribute.Key, v string) {
		if v == "" {
			return
		}
		if _, seen := merged[k]; !seen {
			order = append(order, k)
		}
		merged[k] = v
	}
	for _, kv := range strings.Split(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		raw := strings.TrimSpace(v)
		val, err := url.PathUnescape(raw)
		if err != nil {
			val = raw
		}
		set(attribute.Key(strings.TrimSpace(k)), val)
	}
	// One variable, two keys: the namespace is both the Kubernetes identity
	// and the semconv service.namespace the fleet groups on.
	if v := os.Getenv("K8S_NAMESPACE_NAME"); v != "" {
		set(semconv.K8SNamespaceNameKey, v)
		set(semconv.ServiceNamespaceKey, v)
	}
	set(semconv.K8SPodNameKey, os.Getenv("K8S_POD_NAME"))
	set(semconv.DeploymentEnvironmentNameKey, os.Getenv("DEPLOYMENT_ENVIRONMENT"))
	set(semconv.ServiceNameKey, cfg.ServiceName)
	set(semconv.ServiceVersionKey, cfg.ServiceVersion)

	attrs := make([]attribute.KeyValue, 0, len(order))
	for _, k := range order {
		attrs = append(attrs, k.String(merged[k]))
	}
	return attrs
}

// buildResource assembles the semconv v1.41 Resource from resourceAttributes
// plus the SDK's own telemetry.sdk.* identity. Partial failure (the classic
// semconv schema-URL conflict) is tolerated: whatever resource.New assembled
// is used — never fail the service over telemetry identity (RFC-0013 lesson
// from product-service).
func buildResource(ctx context.Context, cfg Config) *resource.Resource {
	attrs := resourceAttributes(cfg)
	res, err := resource.New(ctx,
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

// metricViews returns the platform's mandatory metric View (RFC-0014,
// RFC-0031 Task 1.3). It is ONE dispatcher rather than a list of NewView
// matchers because the SDK creates a stream for EVERY matching View: a
// wildcard "every seconds histogram" View beside the named ones would give
// http.server.request.duration two streams under one name. Explicit
// precedence — named instruments first, then the unit-based fallback —
// keeps each instrument on exactly one stream.
//
// Named streams: SLO-preserving buckets for the semconv HTTP histograms and
// the server.address/server.port cardinality guard on BOTH metric families
// that carry them. On rpc.client they are per-pod IPs under headless DNS
// (full series churn every rollout); on the http.server instruments otelgin
// derives the port from the client-supplied Host header when the service
// name has no port, so any caller can mint arbitrary label values
// (cardinality DoS). semconv marks them opt-in for HTTP server metrics for
// exactly this reason.
//
// Fallback: every OTHER histogram declared in seconds gets DurationBuckets.
// The SDK's default boundaries are millisecond-shaped (0, 5, 10, … 10000),
// so a seconds-unit business histogram that matched no View — the state of
// order.inventory.commit_lag and payment.reconciliation.run.duration before
// this — collapsed into its first bucket and every quantile read as ~0 while
// the dashboard looked plausible. Declaring WithUnit("s") is now what a
// service does; the View supplies the fleet boundaries (ADR-073). A
// histogram that needs a different scale keeps its own unit (money in cents,
// ratings) or is added above by name with a reviewed set.
func metricViews() []sdkmetric.View {
	return []sdkmetric.View{platformView}
}

// platformView is the dispatcher metricViews documents. It returns exactly
// one Stream for an instrument the platform shapes, and false for everything
// else so the SDK default applies.
func platformView(i sdkmetric.Instrument) (sdkmetric.Stream, bool) {
	denyServerAddr := attribute.NewDenyKeysFilter("server.address", "server.port")
	durations := sdkmetric.AggregationExplicitBucketHistogram{Boundaries: DurationBuckets}
	stream := func(agg sdkmetric.Aggregation, filter attribute.Filter) (sdkmetric.Stream, bool) {
		return sdkmetric.Stream{
			Name:            i.Name,
			Description:     i.Description,
			Unit:            i.Unit,
			Aggregation:     agg,
			AttributeFilter: filter,
		}, true
	}
	switch i.Name {
	case "http.server.request.duration":
		return stream(durations, denyServerAddr)
	case "http.server.request.body.size", "http.server.response.body.size":
		return stream(sdkmetric.AggregationExplicitBucketHistogram{Boundaries: BodySizeBuckets}, denyServerAddr)
	case "rpc.client.call.duration":
		return stream(durations, denyServerAddr)
	// DB-scale buckets for the semconv DB-client histogram (see
	// DBDurationBuckets). Matched by name, so it applies to ANY emitter of
	// this semconv instrument — today only otelpgx (redisotel v9.21 emits
	// db.client.connections.*, not this name), and the semconv-advised
	// boundaries are the right treatment regardless of emitter. No
	// AttributeFilter: otelpgx records only the bounded pgx.operation.type +
	// db.system.name pair, and a View never widens an attribute set anyway.
	case "db.client.operation.duration":
		return stream(sdkmetric.AggregationExplicitBucketHistogram{Boundaries: DBDurationBuckets}, nil)
	}
	if i.Kind == sdkmetric.InstrumentKindHistogram && i.Unit == "s" {
		return stream(durations, nil)
	}
	return sdkmetric.Stream{}, false
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
