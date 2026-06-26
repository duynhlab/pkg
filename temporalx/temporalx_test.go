package temporalx

import (
	"strings"
	"testing"
)

// Dial should wrap the SDK's connection failure with a temporalx-prefixed error
// that names the host and namespace, so a misconfigured worker fails with a
// message that points at the cause rather than a bare SDK error.
func TestDial_WrapsConnectionError(t *testing.T) {
	// 127.0.0.1:1 is refused immediately, so Dial returns fast.
	c, err := Dial(Config{HostPort: "127.0.0.1:1", Namespace: "mop"})
	if err == nil {
		c.Close()
		t.Fatal("expected an error dialing an unreachable frontend, got nil")
	}
	for _, want := range []string{"temporalx: dial", "127.0.0.1:1", "mop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// clientOptions must wire both the OTel tracing interceptor and the SDK metrics
// handler — without the handler, workflow/activity RED metrics never reach the
// service's /metrics endpoint.
func TestClientOptions_WiresTracingAndMetrics(t *testing.T) {
	opts, err := clientOptions(Config{HostPort: "frontend:7233", Namespace: "mop"})
	if err != nil {
		t.Fatalf("clientOptions returned %v, want nil", err)
	}
	if opts.MetricsHandler == nil {
		t.Error("MetricsHandler not wired; workflow/activity RED metrics would be absent")
	}
	if len(opts.Interceptors) != 1 {
		t.Errorf("got %d interceptors, want 1 (tracing)", len(opts.Interceptors))
	}
	if opts.HostPort != "frontend:7233" || opts.Namespace != "mop" {
		t.Errorf("opts host/ns = %q/%q, want frontend:7233/mop", opts.HostPort, opts.Namespace)
	}
}
