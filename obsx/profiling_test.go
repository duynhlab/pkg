package obsx

import (
	"context"
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

func TestAddResourceTag(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.namespace=identity, deployment.environment=prod ,service.version=v1.2.3")

	tags := map[string]string{}
	addResourceTag(tags, "service_namespace", "service.namespace")
	addResourceTag(tags, "deployment_environment", "deployment.environment")
	addResourceTag(tags, "service_version", "service.version")
	addResourceTag(tags, "region", "cloud.region") // absent → not added

	want := map[string]string{
		"service_namespace":      "identity",
		"deployment_environment": "prod",
		"service_version":        "v1.2.3",
	}
	if len(tags) != len(want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
	for k, v := range want {
		if tags[k] != v {
			t.Errorf("tags[%q] = %q, want %q", k, tags[k], v)
		}
	}
}

func TestAddResourceTag_EmptyEnv(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	tags := map[string]string{}
	addResourceTag(tags, "service_namespace", "service.namespace")
	if len(tags) != 0 {
		t.Fatalf("expected no tags from empty env, got %v", tags)
	}
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
