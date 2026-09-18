package obsx

// SetupProfiling wires Grafana Pyroscope continuous profiling for a service.
//
// Identity is the OTel resource, not a second parse of the environment: the
// Pyroscope application name (the `service_name` series) and the three labels
// come from resourceAttributes(ConfigFromEnv()) — the same list the tracer,
// meter and logger provider build — so a profile is found from a span by the
// same service.name, service.namespace, deployment.environment.name and
// service.version, and an attribute missing on one signal is missing on all
// (RFC-0031 Task 1.4, ADR-074). Labels are emitted with underscores because
// Pyroscope label names must match [a-zA-Z_][a-zA-Z0-9_]*.
//
// The four mutex/block profile types collect nothing unless the Go runtime
// sampling rates are turned on, so SetupProfiling sets low-overhead production
// rates after the profiler starts.

import (
	"context"
	"errors"
	"log"
	"os"
	"runtime"
	"sync"

	otelpyroscope "github.com/grafana/otel-profiling-go"
	"github.com/grafana/pyroscope-go"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// Process-global runtime sampling rates, set once after a successful profiler
// start (RFC-0031 profiling contract: central, budgeted, never per service).
//
//   - mutexProfileFraction 100: one in a hundred contended mutex events is
//     sampled. The Go runtime's cost is proportional to contention, not to
//     lock operations, so an uncontended service pays ~0; a contended one pays
//     one stack capture per hundred contention events.
//   - blockProfileRateNanos 100ms: only blocking events of at least 100 ms are
//     recorded (shorter ones are sampled proportionally). Channel and select
//     waits under that threshold — the common case in request handlers — are
//     essentially free.
//
// Changing either is a fleet-wide overhead change: it needs a benchmark under
// the service's real lock profile and a review of the overhead budget in
// docs/api/profiling.md, not a local edit.
const (
	mutexProfileFraction  = 100
	blockProfileRateNanos = 100_000_000
)

var (
	profileOnce sync.Once
	profileErr  error
	profiler    *pyroscope.Profiler
)

// SetupProfiling starts the Pyroscope profiler and returns a shutdown func that
// flushes and stops it; call the func during graceful shutdown. It is
// idempotent — repeated calls return the same shutdown func and start the
// profiler once. Callers gate it behind their own PROFILING_ENABLED config; the
// Pyroscope endpoint comes from PYROSCOPE_ENDPOINT. It returns an error (rather
// than silently no-op'ing) when profiling is enabled but PYROSCOPE_ENDPOINT is
// unset, so misconfiguration is visible to the caller.
//
// Identity is read through ConfigFromEnv, so the "same resource as the other
// signals" guarantee holds for a service that also hands ConfigFromEnv() to
// SetupObservability — which every deployed service does. A hand-built Config
// passed to SetupObservability would not be seen here.
func SetupProfiling() (func(context.Context) error, error) {
	profileOnce.Do(func() { profiler, profileErr = startProfiler() })
	if profileErr != nil {
		return nil, profileErr
	}
	return shutdownProfiling, nil
}

// startProfiler builds the config and starts Pyroscope. It is split out of the
// sync.Once in SetupProfiling so both the misconfiguration and success paths are
// unit-testable. Process-global runtime rates are flipped only after a
// successful start, so an empty endpoint or a failed start never incurs the
// sampling overhead (or overrides a caller's own pprof tuning).
func startProfiler() (*pyroscope.Profiler, error) {
	endpoint := endpointFromEnv()
	if endpoint == "" {
		return nil, errors.New("PYROSCOPE_ENDPOINT is not set")
	}

	// Identity comes from the same list every other signal uses (RFC-0031
	// Task 1.4): resourceAttributes(ConfigFromEnv()) — so a profile can be
	// found from a span by the same service.name / namespace / version /
	// environment, and an attribute missing on one is missing on all.
	cfg := ConfigFromEnv()
	p, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: cfg.ServiceName,
		ServerAddress:   endpoint,
		Logger:          pyroErrorLogger{}, // surface upload failures (otherwise silent)
		Tags:            profilingTags(resourceAttributes(cfg)),
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileInuseSpace,
			pyroscope.ProfileGoroutines,
			pyroscope.ProfileMutexCount,
			pyroscope.ProfileMutexDuration,
			pyroscope.ProfileBlockCount,
			pyroscope.ProfileBlockDuration,
		},
	})
	if err != nil {
		return nil, err
	}

	runtime.SetMutexProfileFraction(mutexProfileFraction)
	runtime.SetBlockProfileRate(blockProfileRateNanos)
	return p, nil
}

// shutdownProfiling flushes and stops the profiler started by SetupProfiling.
// It deliberately does not reset the runtime profiling rates: shutdown means the
// process is exiting, so restoring them would be noise.
func shutdownProfiling(context.Context) error {
	if profiler == nil {
		return nil
	}
	return profiler.Stop()
}

// pyroErrorLogger adapts the Pyroscope logger interface to surface only upload
// errors — its Infof/Debugf are chatty (every flush), so they are dropped.
type pyroErrorLogger struct{}

func (pyroErrorLogger) Infof(string, ...any)  {}
func (pyroErrorLogger) Debugf(string, ...any) {}
func (pyroErrorLogger) Errorf(format string, args ...any) {
	log.Printf("pyroscope: "+format, args...)
}

// TracerProviderWithProfiles wraps tp so CPU profiles are labelled with the
// active span and spans carry the pyroscope.profile.id attribute, enabling
// Grafana's traces-to-profiles link. Only CPU profiles are span-scoped (heap /
// goroutine / mutex / block are not), so the Grafana link resolves to CPU
// flame graphs. Call it on the TracerProvider before otel.SetTracerProvider,
// only when both tracing and profiling are enabled.
func TracerProviderWithProfiles(tp trace.TracerProvider) trace.TracerProvider {
	return otelpyroscope.NewTracerProvider(tp)
}

// endpointFromEnv resolves the Pyroscope server address from PYROSCOPE_ENDPOINT.
func endpointFromEnv() string {
	return os.Getenv("PYROSCOPE_ENDPOINT")
}

// profilingTags maps the shared resource attributes to the CLOSED set of
// Pyroscope labels the contract admits: service_namespace,
// deployment_environment and service_version (service_name is the profile's
// application name). Nothing else from the resource becomes a label — pod,
// instance id and Kubernetes identity stay on spans, metrics and logs where
// their cardinality is bounded by the store; on a profile they would multiply
// series per pod restart. The SDK adds span_name on span-scoped CPU profiles
// and pyroscope_spy on every profile; both are admitted by the contract and
// are not this function's business.
func profilingTags(attrs []attribute.KeyValue) map[string]string {
	tags := map[string]string{}
	for _, kv := range attrs {
		var label string
		switch kv.Key {
		case semconv.ServiceNamespaceKey:
			label = "service_namespace"
		case semconv.DeploymentEnvironmentNameKey:
			label = "deployment_environment"
		case semconv.ServiceVersionKey:
			label = "service_version"
		default:
			continue
		}
		if v := kv.Value.String(); v != "" {
			tags[label] = v
		}
	}
	return tags
}
