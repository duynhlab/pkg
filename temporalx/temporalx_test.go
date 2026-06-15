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
