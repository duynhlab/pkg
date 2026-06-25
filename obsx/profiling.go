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
// rates before starting the profiler.

import (
	"context"
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
//   - block: ~1 sample per N nanoseconds spent blocked (one per 100ms).
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
// Pyroscope endpoint comes from PYROSCOPE_ENDPOINT.
func SetupProfiling() (func(context.Context) error, error) {
	profileOnce.Do(func() {
		serviceName := serviceNameFromEnv()
		tags := map[string]string{}
		addResourceTag(tags, "service_namespace", "service.namespace")
		addResourceTag(tags, "deployment_environment", "deployment.environment")
		addResourceTag(tags, "service_version", "service.version")

		runtime.SetMutexProfileFraction(mutexProfileFraction)
		runtime.SetBlockProfileRate(blockProfileRateNanos)

		profiler, profileErr = pyroscope.Start(pyroscope.Config{
			ApplicationName: serviceName,
			ServerAddress:   endpointFromEnv(),
			Tags:            tags,
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
	})
	if profileErr != nil {
		return nil, profileErr
	}
	return shutdownProfiling, nil
}

// shutdownProfiling flushes and stops the profiler started by SetupProfiling.
func shutdownProfiling(context.Context) error {
	if profiler == nil {
		return nil
	}
	return profiler.Stop()
}

// TracerProviderWithProfiles wraps tp so CPU profiles are labelled with the
// active span and spans carry the pyroscope.profile.id attribute, enabling
// Grafana's traces-to-profiles link. Call it on the TracerProvider before
// otel.SetTracerProvider, only when both tracing and profiling are enabled.
func TracerProviderWithProfiles(tp trace.TracerProvider) trace.TracerProvider {
	return otelpyroscope.NewTracerProvider(tp)
}

// serviceNameFromEnv resolves the profiling application name from
// OTEL_SERVICE_NAME, falling back to the hostname and then a sentinel.
func serviceNameFromEnv() string {
	if name := os.Getenv("OTEL_SERVICE_NAME"); name != "" {
		return name
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return unknownService
}

// endpointFromEnv resolves the Pyroscope server address from PYROSCOPE_ENDPOINT.
func endpointFromEnv() string {
	return os.Getenv("PYROSCOPE_ENDPOINT")
}

// addResourceTag copies a dotted OTel resource attribute from
// OTEL_RESOURCE_ATTRIBUTES into tags under an underscored Pyroscope label, if
// present and non-empty.
func addResourceTag(tags map[string]string, label, otelKey string) {
	for _, kv := range strings.Split(os.Getenv("OTEL_RESOURCE_ATTRIBUTES"), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok && k == otelKey && v != "" {
			tags[label] = v
			return
		}
	}
}
