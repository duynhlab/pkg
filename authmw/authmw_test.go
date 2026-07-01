package authmw

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authv1 "github.com/duynhlab/pkg/proto/auth/v1"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeValidator struct {
	resp   *authv1.GetMeResponse
	err    error
	called bool
}

func (f *fakeValidator) GetMe(_ context.Context, _ *authv1.GetMeRequest, _ ...grpc.CallOption) (*authv1.GetMeResponse, error) {
	f.called = true
	return f.resp, f.err
}

func TestMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	okResp := &authv1.GetMeResponse{User: &authv1.User{Id: "7", Username: "bob", Email: "bob@example.com"}}

	tests := []struct {
		name       string
		authHeader string
		val        *fakeValidator
		wantStatus int
		wantUserID string
		wantCalled bool
	}{
		{name: "missing header fails closed without calling auth", authHeader: "", val: &fakeValidator{}, wantStatus: http.StatusUnauthorized, wantCalled: false},
		{name: "unauthenticated maps to 401", authHeader: "Bearer bad", val: &fakeValidator{err: status.Error(codes.Unauthenticated, "nope")}, wantStatus: http.StatusUnauthorized, wantCalled: true},
		{name: "auth unreachable maps to 503", authHeader: "Bearer x", val: &fakeValidator{err: status.Error(codes.Unavailable, "down")}, wantStatus: http.StatusServiceUnavailable, wantCalled: true},
		{name: "internal error maps to 503", authHeader: "Bearer x", val: &fakeValidator{err: status.Error(codes.Internal, "boom")}, wantStatus: http.StatusServiceUnavailable, wantCalled: true},
		{name: "success sets user and continues", authHeader: "Bearer good", val: &fakeValidator{resp: okResp}, wantStatus: http.StatusOK, wantUserID: "7", wantCalled: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotUserID string
			r := gin.New()
			r.Use(Middleware(tt.val))
			r.GET("/x", func(c *gin.Context) {
				gotUserID = c.GetString(CtxUserID)
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if tt.val.called != tt.wantCalled {
				t.Errorf("validator called = %v, want %v", tt.val.called, tt.wantCalled)
			}
			if tt.wantUserID != "" && gotUserID != tt.wantUserID {
				t.Errorf("user_id = %q, want %q", gotUserID, tt.wantUserID)
			}
		})
	}
}

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
		name        string
		token       string // raw token; "" means no Authorization header
		bearer      bool   // wrap token in "Bearer " prefix
		verifier    *Verifier
		fallback    *fakeValidator
		wantStatus  int
		wantUserID  string
		wantFBCalls bool
	}{
		{
			name:       "valid JWT",
			token:      signRS256(t, key, testKID, validClaims(iss, aud, time.Now().Add(time.Hour))),
			bearer:     true,
			verifier:   verifier,
			fallback:   &fakeValidator{},
			wantStatus: http.StatusOK,
			wantUserID: "42",
		},
		{
			name:       "expired JWT -> 401",
			token:      signRS256(t, key, testKID, validClaims(iss, aud, time.Now().Add(-time.Hour))),
			bearer:     true,
			verifier:   verifier,
			fallback:   &fakeValidator{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong issuer -> 401",
			token:      signRS256(t, key, testKID, validClaims("https://evil.example", aud, time.Now().Add(time.Hour))),
			bearer:     true,
			verifier:   verifier,
			fallback:   &fakeValidator{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "wrong audience -> 401",
			token:      signRS256(t, key, testKID, validClaims(iss, "other-aud", time.Now().Add(time.Hour))),
			bearer:     true,
			verifier:   verifier,
			fallback:   &fakeValidator{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "different signing key (same kid) -> 401",
			token:      signRS256(t, otherKey, testKID, validClaims(iss, aud, time.Now().Add(time.Hour))),
			bearer:     true,
			verifier:   verifier,
			fallback:   &fakeValidator{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			// Unknown kid against a HEALTHY, populated JWKS is an invalid token
			// (forged/rotated kid), NOT a JWKS outage: must be 401, never 503.
			name:       "unknown kid (healthy JWKS) -> 401",
			token:      signRS256(t, key, "no-such-kid", validClaims(iss, aud, time.Now().Add(time.Hour))),
			bearer:     true,
			verifier:   verifier,
			fallback:   &fakeValidator{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing alg header -> 401",
			token:      missingAlgToken,
			bearer:     true,
			verifier:   verifier,
			fallback:   &fakeValidator{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "alg:none -> 401",
			token:      noneToken,
			bearer:     true,
			verifier:   verifier,
			fallback:   &fakeValidator{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "HS256 alg-confusion -> 401",
			token:      hsToken,
			bearer:     true,
			verifier:   verifier,
			fallback:   &fakeValidator{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:        "opaque success -> fallback used 200",
			token:       "opaque-token",
			bearer:      true,
			verifier:    verifier,
			fallback:    &fakeValidator{resp: &authv1.GetMeResponse{User: &authv1.User{Id: "7", Username: "bob", Email: "bob@example.com"}}},
			wantStatus:  http.StatusOK,
			wantUserID:  "7",
			wantFBCalls: true,
		},
		{
			name:        "opaque Unauthenticated -> 401",
			token:       "opaque-token",
			bearer:      true,
			verifier:    verifier,
			fallback:    &fakeValidator{err: status.Error(codes.Unauthenticated, "nope")},
			wantStatus:  http.StatusUnauthorized,
			wantFBCalls: true,
		},
		{
			name:        "opaque Unavailable -> 503",
			token:       "opaque-token",
			bearer:      true,
			verifier:    verifier,
			fallback:    &fakeValidator{err: status.Error(codes.Unavailable, "down")},
			wantStatus:  http.StatusServiceUnavailable,
			wantFBCalls: true,
		},
		{
			name:       "missing header -> 401, verifier not consulted",
			token:      "",
			verifier:   verifier,
			fallback:   &fakeValidator{},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "nil fallback + opaque token -> 401",
			token:      "opaque-token",
			bearer:     true,
			verifier:   verifier,
			fallback:   nil,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotUserID string
			r := gin.New()
			var fb Validator
			if tt.fallback != nil {
				fb = tt.fallback
			}
			r.Use(MiddlewareJWT(tt.verifier, fb))
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
			if tt.fallback != nil && tt.fallback.called != tt.wantFBCalls {
				t.Errorf("fallback called = %v, want %v", tt.fallback.called, tt.wantFBCalls)
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
	r.Use(MiddlewareJWT(verifier, &fakeValidator{}))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d (body %s)", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
}

// TestMiddlewareJWT_NilVerifierFallsThrough verifies a JWT-shaped token with a
// nil verifier is treated as opaque (fallback consulted).
func TestMiddlewareJWT_NilVerifierFallsThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)

	fb := &fakeValidator{resp: &authv1.GetMeResponse{User: &authv1.User{Id: "9", Username: "dan", Email: "dan@example.com"}}}
	r := gin.New()
	r.Use(MiddlewareJWT(nil, fb))
	var gotUserID string
	r.GET("/x", func(c *gin.Context) {
		gotUserID = c.GetString(CtxUserID)
		c.Status(http.StatusOK)
	})

	// A JWT-shaped string (two dots) but verifier is nil.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer aaa.bbb.ccc")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !fb.called {
		t.Error("fallback not called for nil-verifier JWT-shaped token")
	}
	if gotUserID != "9" {
		t.Errorf("user_id = %q, want 9", gotUserID)
	}
}
