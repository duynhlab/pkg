package obsx

// SetupProfiling wires Grafana Pyroscope continuous profiling for a service.
//
// Identity is OTel-aligned: the Pyroscope application name (which becomes the
// `service_name` series in Pyroscope v1) is read from OTEL_SERVICE_NAME — the
// same value the tracing SDK uses for service.name — so profiles, traces, and
// metrics share one identity in Grafana. Extra labels are derived from
// OTEL_RESOURCE_ATTRIBUTES and emitted with underscores (Pyroscope labels must
// match [a-zA-Z_][a-zA-Z0-9_]*; dots are invalid), mirroring the OTel resource
// attributes service.namespace / deployment.environment / service.version.
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
	"strings"
	"sync"

	otelpyroscope "github.com/grafana/otel-profiling-go"
	"github.com/grafana/pyroscope-go"
	"go.opentelemetry.io/otel/trace"
)

const unknownService = "unknown-service"

// Production-safe Go runtime profiling rates (see runtime docs):
//   - mutex: report ~1 of every N contention events (1% sampling).
//   - block: record every blocking event whose duration is ≥ N nanoseconds
//     (here one per 100ms blocked).
//
// Both default to 0 (disabled), which would make the mutex/block profile types
// ship empty. Drop these far lower only for targeted, short investigations.
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

	p, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: serviceNameFromEnv(),
		ServerAddress:   endpoint,
		Logger:          pyroErrorLogger{}, // surface upload failures (otherwise silent)
		Tags:            profilingTags(),
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

// serviceNameFromEnv resolves the profiling application name from
// OTEL_SERVICE_NAME, falling back to the hostname and then a sentinel.
func serviceNameFromEnv() string {
	host, _ := os.Hostname()
	return resolveServiceName(os.Getenv("OTEL_SERVICE_NAME"), host)
}

// resolveServiceName picks OTEL_SERVICE_NAME, then the hostname, then a
// sentinel. Split out from env access so every branch is unit-testable.
func resolveServiceName(otelName, hostname string) string {
	if otelName != "" {
		return otelName
	}
	if hostname != "" {
		return hostname
	}
	return unknownService
}

// endpointFromEnv resolves the Pyroscope server address from PYROSCOPE_ENDPOINT.
func endpointFromEnv() string {
	return os.Getenv("PYROSCOPE_ENDPOINT")
}

// profilingTags parses OTEL_RESOURCE_ATTRIBUTES once and maps the relevant
// attributes to underscored Pyroscope labels. Duplicate keys follow the OTel
// convention of last-wins; malformed or empty entries are skipped.
func profilingTags() map[string]string {
	attrs := map[string]string{}
	for _, kv := range strings.Split(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"), ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok && v != "" {
			attrs[k] = v // last wins
		}
	}
	tags := map[string]string{}
	for label, key := range map[string]string{
		"service_namespace":      "service.namespace",
		"deployment_environment": "deployment.environment",
		"service_version":        "service.version",
	} {
		if v := attrs[key]; v != "" {
			tags[label] = v
		}
	}
	return tags
}
