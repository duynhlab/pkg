package temporalx

import (
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
)

// installReplaySafeGlobal satisfies Dial's ADR-063 precondition for tests and
// restores the previous global afterwards.
func installReplaySafeGlobal(t *testing.T) {
	t.Helper()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(NewReplaySafeTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
}

// Dial must refuse to build the OTel v2 plugin when the global tracer
// provider is not replay-safe — an actionable error, never the contrib
// module's panic (this repo forbids library panics).
func TestDial_RequiresReplaySafeGlobal(t *testing.T) {
	c, err := Dial(Config{HostPort: "127.0.0.1:1", Namespace: "mop"})
	if err == nil {
		c.Close()
		t.Fatal("expected an error with a non-replay-safe global provider, got nil")
	}
	for _, want := range []string{"replay-safe", "WithTracerProviderFactory", "ADR-063"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// Dial should wrap the SDK's connection failure with a temporalx-prefixed error
// that names the host and namespace, so a misconfigured worker fails with a
// message that points at the cause rather than a bare SDK error.
func TestDial_WrapsConnectionError(t *testing.T) {
	installReplaySafeGlobal(t)
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

func TestLogMetricsError_DoesNotPanic(t *testing.T) {
	// The SDK default OnError panics; ours must log and return.
	logMetricsError(errors.New("instrument boom"))
}
