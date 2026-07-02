package authmw

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// --- JWT / JWKS test harness -------------------------------------------------

const testKID = "test-key-1"

// jwksServer serves a JWKS for the given RSA public key under the given kid.
func jwksServer(t *testing.T, pub *rsa.PublicKey, kid string) *httptest.Server {
	t.Helper()
	jwks := map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": kid,
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	}
	body, err := json.Marshal(jwks)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// signRS256 signs claims with key under kid using RS256.
func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign RS256: %v", err)
	}
	return s
}

// newVerifierFor builds a Verifier pointing at jwksURL with iss/aud.
func newVerifierFor(t *testing.T, jwksURL, iss, aud string) *Verifier {
	t.Helper()
	v, err := NewVerifier(jwksURL, iss, aud)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func validClaims(iss, aud string, exp time.Time) jwt.MapClaims {
	return jwt.MapClaims{
		"sub":      "42",
		"username": "carol",
		"email":    "carol@example.com",
		"iss":      iss,
		"aud":      aud,
		"exp":      exp.Unix(),
		"iat":      time.Now().Add(-time.Minute).Unix(),
	}
}

func TestMiddlewareJWT(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const iss = "https://auth.duynhlab.dev"
	const aud = "duynhlab-api"

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}

	jwks := jwksServer(t, &key.PublicKey, testKID)
	verifier := newVerifierFor(t, jwks.URL, iss, aud)

	// alg:none token, manually assembled (jwt lib refuses to sign "none").
	noneToken := func() string {
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"` + testKID + `"}`))
		c := validClaims(iss, aud, time.Now().Add(time.Hour))
		cb, _ := json.Marshal(c)
		body := base64.RawURLEncoding.EncodeToString(cb)
		return hdr + "." + body + "."
	}()

	// RS256-signed token whose header omits "alg" entirely (missing alg). The
	// signature is valid RS256 but the header lies about it — must be rejected.
	missingAlgToken := func() string {
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","kid":"` + testKID + `"}`))
		cb, _ := json.Marshal(validClaims(iss, aud, time.Now().Add(time.Hour)))
		body := base64.RawURLEncoding.EncodeToString(cb)
		signing := hdr + "." + body
		sig, err := jwt.SigningMethodRS256.Sign(signing, key)
		if err != nil {
			t.Fatalf("sign missing-alg: %v", err)
		}
		return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	}()

	// HS256 token using the RSA public-key bytes as the HMAC secret
	// (classic algorithm-confusion attack payload).
	hsToken := func() string {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims(iss, aud, time.Now().Add(time.Hour)))
		tok.Header["kid"] = testKID
		s, err := tok.SignedString(key.N.Bytes())
		if err != nil {
			t.Fatalf("sign HS256: %v", err)
		}
		return s
	}()

	tests := []struct {
		name       string
		token      string // raw token; "" means no Authorization header
		bearer     bool   // wrap token in "Bearer " prefix
		verifier   *Verifier
		wantStatus int
		wantUserID string
	}{
		{
			name:       "valid JWT",
			token:      signRS256(t, key, testKID, validClaims(iss, aud, time.Now().Add(time.Hour))),
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusOK,
			wantUserID: "42",
		},
		{
			name:       "expired JWT -> 401",
			token:      signRS256(t, key, testKID, validClaims(iss, aud, time.Now().Add(-time.Hour))),
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong issuer -> 401",
			token:      signRS256(t, key, testKID, validClaims("https://evil.example", aud, time.Now().Add(time.Hour))),
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong audience -> 401",
			token:      signRS256(t, key, testKID, validClaims(iss, "other-aud", time.Now().Add(time.Hour))),
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "different signing key (same kid) -> 401",
			token:      signRS256(t, otherKey, testKID, validClaims(iss, aud, time.Now().Add(time.Hour))),
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			// Unknown kid against a HEALTHY, populated JWKS is an invalid token
			// (forged/rotated kid), NOT a JWKS outage: must be 401, never 503.
			name:       "unknown kid (healthy JWKS) -> 401",
			token:      signRS256(t, key, "no-such-kid", validClaims(iss, aud, time.Now().Add(time.Hour))),
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing alg header -> 401",
			token:      missingAlgToken,
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "alg:none -> 401",
			token:      noneToken,
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "HS256 alg-confusion -> 401",
			token:      hsToken,
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			// Opaque tokens are no longer a credential (RFC-0009 Phase 5):
			// anything that is not a compact JWS is rejected outright.
			name:       "opaque token -> 401",
			token:      "opaque-token",
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing header -> 401, verifier not consulted",
			token:      "",
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			// Defence-in-depth: a nil verifier means the service cannot verify
			// anything — deny as transient (503), never fall open.
			name:       "nil verifier -> 503",
			token:      "aaa.bbb.ccc",
			bearer:     true,
			verifier:   nil,
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotUserID string
			r := gin.New()
			r.Use(MiddlewareJWT(tt.verifier))
			r.GET("/x", func(c *gin.Context) {
				gotUserID = c.GetString(CtxUserID)
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tt.token != "" {
				hv := tt.token
				if tt.bearer {
					hv = "Bearer " + tt.token
				}
				req.Header.Set("Authorization", hv)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantUserID != "" && gotUserID != tt.wantUserID {
				t.Errorf("user_id = %q, want %q", gotUserID, tt.wantUserID)
			}
		})
	}
}

// TestMiddlewareJWT_TransientKeyFetch verifies that a JWT-shaped token whose
// JWKS endpoint never delivered any keys (genuine outage: the cached set is
// empty) yields 503 (transient), not 401 — the opposite of the unknown-kid
// case above, where the set IS populated.
func TestMiddlewareJWT_TransientKeyFetch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const iss = "https://auth.duynhlab.dev"
	const aud = "duynhlab-api"

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	// Spin up a JWKS server, build the verifier, then close the server so the
	// kid is uncached and refresh fails -> key cannot be supplied.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	verifier := newVerifierFor(t, srv.URL, iss, aud)
	srv.Close()

	token := signRS256(t, key, testKID, validClaims(iss, aud, time.Now().Add(time.Hour)))

	r := gin.New()
	r.Use(MiddlewareJWT(verifier))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d (body %s)", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
}
