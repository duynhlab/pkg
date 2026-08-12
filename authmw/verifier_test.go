package authmw

import (
	"strings"
	"testing"
)

// TestNewVerifier_ConfigValidation covers construction-time failures: the two
// required fields, and a JWKS URL (explicit or derived from the issuer) that
// the HTTP client cannot even build a request for. An unreachable-but-valid
// URL deliberately does NOT fail here: keyfunc starts with background refresh
// and the middleware handles the empty key set at request time as a 503
// (covered by TestMiddlewareJWT_TransientKeyFetch).
func TestNewVerifier_ConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string // substring the error must contain
	}{
		{
			name:    "missing Issuer",
			cfg:     Config{Audience: "aud"},
			wantErr: "Issuer",
		},
		{
			name:    "missing Audience",
			cfg:     Config{Issuer: "https://idp.example"},
			wantErr: "Audience",
		},
		{
			name:    "malformed explicit JWKS URL",
			cfg:     Config{Issuer: "https://idp.example", Audience: "aud", JWKSURL: "://not-a-url"},
			wantErr: "",
		},
		{
			name:    "malformed derived JWKS URL (malformed issuer)",
			cfg:     Config{Issuer: "://not-a-url", Audience: "aud"},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := NewVerifier(tt.cfg)
			if err == nil {
				t.Fatalf("NewVerifier(%+v) succeeded, want error", tt.cfg)
			}
			if v != nil {
				t.Errorf("verifier = %v, want nil on error", v)
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
