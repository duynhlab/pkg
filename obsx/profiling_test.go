package obsx

import (
	"context"
	"errors"
	"testing"

	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestServiceNameFromEnv(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "auth-service")
	if got := serviceNameFromEnv(); got != "auth-service" {
		t.Fatalf("serviceNameFromEnv() = %q, want auth-service", got)
	}
}

func TestServiceNameFromEnv_FallsBackWhenUnset(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	// Falls back to hostname (or the sentinel); must never be empty.
	if got := serviceNameFromEnv(); got == "" {
		t.Fatal("serviceNameFromEnv() returned empty string")
	}
}

func TestResolveServiceName(t *testing.T) {
	cases := []struct {
		name, otel, host, want string
	}{
		{"otel wins", "auth-service", "auth-7c9-x", "auth-service"},
		{"hostname fallback", "", "auth-7c9-x", "auth-7c9-x"},
		{"sentinel when both empty", "", "", unknownService},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveServiceName(c.otel, c.host); got != c.want {
				t.Fatalf("resolveServiceName(%q,%q) = %q, want %q", c.otel, c.host, got, c.want)
			}
		})
	}
}

func TestEndpointFromEnv(t *testing.T) {
	t.Setenv("PYROSCOPE_ENDPOINT", "http://pyroscope.example:4040")
	if got := endpointFromEnv(); got != "http://pyroscope.example:4040" {
		t.Fatalf("endpointFromEnv() = %q, want the env value", got)
	}
}

func TestProfilingTags(t *testing.T) {
	// Spaces are trimmed; the duplicate service.namespace must be last-wins.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES",
		"service.namespace=identity, deployment.environment=prod ,service.version=v1.2.3,service.namespace=catalog")

	got := profilingTags()
	want := map[string]string{
		"service_namespace":      "catalog",
		"deployment_environment": "prod",
		"service_version":        "v1.2.3",
	}
	if len(got) != len(want) {
		t.Fatalf("profilingTags() = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("tags[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestProfilingTags_EmptyAndMalformed(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	if got := profilingTags(); len(got) != 0 {
		t.Fatalf("expected no tags from empty env, got %v", got)
	}
	// no '=', empty value, and unmapped keys are all skipped.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "garbage,service.version=,cloud.region=us-east-1")
	if got := profilingTags(); len(got) != 0 {
		t.Fatalf("expected no mapped tags, got %v", got)
	}
}

// TestStartProfiler_EmptyEndpoint covers the misconfiguration guard: profiling
// enabled but PYROSCOPE_ENDPOINT unset must return an error (not a silent no-op)
// and must not start a profiler. Calls startProfiler directly to bypass the
// sync.Once in SetupProfiling.
func TestStartProfiler_EmptyEndpoint(t *testing.T) {
	t.Setenv("PYROSCOPE_ENDPOINT", "")
	p, err := startProfiler()
	if err == nil {
		t.Fatal("startProfiler() with empty endpoint = nil error, want error")
	}
	if p != nil {
		t.Fatalf("startProfiler() returned a non-nil profiler on error: %v", p)
	}
}

// TestStartProfiler_Success covers the happy path (valid endpoint → profiler).
func TestStartProfiler_Success(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "auth-service")
	t.Setenv("PYROSCOPE_ENDPOINT", "http://127.0.0.1:4040")
	p, err := startProfiler()
	if err != nil {
		t.Fatalf("startProfiler() = %v", err)
	}
	if p == nil {
		t.Fatal("startProfiler() returned a nil profiler")
	}
	if err := p.Stop(); err != nil {
		t.Errorf("profiler.Stop() = %v", err)
	}
}

func TestPyroErrorLogger(t *testing.T) {
	// The adapter must satisfy pyroscope.Logger; Infof/Debugf are no-ops and
	// Errorf forwards. Exercising all three keeps the contract covered.
	var l pyroErrorLogger
	l.Infof("info %d", 1)
	l.Debugf("debug %s", "x")
	l.Errorf("err %v", errors.New("boom"))
}

func TestTracerProviderWithProfiles(t *testing.T) {
	if got := TracerProviderWithProfiles(tracenoop.NewTracerProvider()); got == nil {
		t.Fatal("TracerProviderWithProfiles returned nil")
	}
}

// TestShutdownProfiling_NoProfiler covers the nil-profiler branch. It must run
// before TestSetupProfiling (which sets the package-global profiler), so it is
// declared first.
func TestShutdownProfiling_NoProfiler(t *testing.T) {
	if profiler != nil {
		t.Skip("profiler already started by an earlier test")
	}
	if err := shutdownProfiling(context.Background()); err != nil {
		t.Fatalf("shutdownProfiling(nil) = %v, want nil", err)
	}
}

// TestSetupProfiling exercises the full start path and the returned shutdown
// func (the non-nil profiler branch). It mirrors TestSetupMetricsIdempotent.
func TestSetupProfiling(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "auth-service")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.namespace=identity,service.version=v1.0.0")
	t.Setenv("PYROSCOPE_ENDPOINT", "http://127.0.0.1:4040")

	stop, err := SetupProfiling()
	if err != nil {
		t.Fatalf("SetupProfiling: %v", err)
	}
	if stop == nil {
		t.Fatal("SetupProfiling returned a nil shutdown func")
	}
	if err := stop(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
