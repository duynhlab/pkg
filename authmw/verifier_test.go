package authmw

import (
	"testing"
)

// NewVerifier's only construction-time failure is a JWKS URL the HTTP client
// cannot even build a request for. An unreachable-but-valid URL deliberately
// does NOT fail here: keyfunc starts with background refresh and the
// middleware handles the empty key set at request time as a 503 (covered by
// TestMiddlewareJWT_TransientKeyFetch).
func TestNewVerifier_FailsOnMalformedURL(t *testing.T) {
	v, err := NewVerifier("://not-a-url", "iss", "aud")
	if err == nil {
		t.Fatal("NewVerifier with a malformed URL succeeded, want error")
	}
	if v != nil {
		t.Errorf("verifier = %v, want nil on error", v)
	}
}
