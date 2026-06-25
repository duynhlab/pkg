package obsx

import "testing"

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
