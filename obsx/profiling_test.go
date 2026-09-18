package obsx

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestEndpointFromEnv(t *testing.T) {
	t.Setenv("PYROSCOPE_ENDPOINT", "http://pyroscope.example:4040")
	if got := endpointFromEnv(); got != "http://pyroscope.example:4040" {
		t.Fatalf("endpointFromEnv() = %q, want the env value", got)
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
// func (the non-nil profiler branch).
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

// The profiler's labels come from the SAME resource list as the tracer and
// meter (RFC-0031 Task 1.4). Before this the helper re-parsed
// OTEL_RESOURCE_ATTRIBUTES on its own and looked for the retired key
// deployment.environment, which no manifest sets — so deployment_environment
// was empty on every profile while the spans of the same process carried
// deployment.environment.name.
func TestProfilingTags_FromSharedResource(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "order")
	// The retired key deployment.environment=stale is what the old parser
	// looked for; it must be ignored. SERVICE_VERSION is the Config source and
	// wins over the env list, which proves the shared precedence, not a copy.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.namespace=order,service.instance.id=order-abc,service.version=from-env,deployment.environment=stale")
	t.Setenv("SERVICE_VERSION", "2.7.2")
	t.Setenv("K8S_NAMESPACE_NAME", "order")
	t.Setenv("K8S_POD_NAME", "order-abc")
	t.Setenv("DEPLOYMENT_ENVIRONMENT", "production")

	got := profilingTags(resourceAttributes(ConfigFromEnv()))
	want := map[string]string{
		"service_namespace":      "order",
		"deployment_environment": "production",
		"service_version":        "2.7.2",
	}
	if len(got) != len(want) {
		t.Fatalf("tags = %v, want exactly the closed set %v (no pod, instance or k8s labels)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("tags[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestProfilingTags_EmptyWhenIdentityIsMissing(t *testing.T) {
	for _, k := range []string{"OTEL_RESOURCE_ATTRIBUTES", "K8S_NAMESPACE_NAME", "K8S_POD_NAME", "DEPLOYMENT_ENVIRONMENT"} {
		t.Setenv(k, "")
	}
	if got := profilingTags(resourceAttributes(Config{ServiceName: "t"})); len(got) != 0 {
		t.Fatalf("tags = %v, want none when only service.name is known", got)
	}
}

// profilingTags is a pure function of the attribute list: the closed set is
// the three labels below and nothing else, whatever else the resource carries.
func TestProfilingTags_ClosedSet(t *testing.T) {
	got := profilingTags([]attribute.KeyValue{
		semconv.ServiceName("order"),
		semconv.ServiceNamespace("order"),
		semconv.ServiceVersion("2.7.2"),
		semconv.DeploymentEnvironmentNameKey.String("production"),
		attribute.String("deployment.environment", "stale"), // the retired key
		semconv.K8SPodName("order-abc"),
		semconv.K8SNamespaceName("order"),
		semconv.ServiceInstanceID("order-abc"),
		attribute.String("cloud.region", "hn"),
	})
	want := map[string]string{"service_namespace": "order", "deployment_environment": "production", "service_version": "2.7.2"}
	if len(got) != len(want) {
		t.Fatalf("tags = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("tags[%q] = %q, want %q", k, got[k], v)
		}
	}
}
