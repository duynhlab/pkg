package authmw

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// --- JWT / JWKS test harness -------------------------------------------------

const testKID = "test-key-1"

// jwksBody marshals a single-key JWK Set for the given RSA public key under the
// given kid, advertising alg (empty alg omits the parameter).
func jwksBody(t *testing.T, pub *rsa.PublicKey, kid, alg string) []byte {
	t.Helper()
	key := map[string]string{
		"kty": "RSA",
		"use": "sig",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
	if alg != "" {
		key["alg"] = alg
	}
	body, err := json.Marshal(map[string]any{"keys": []map[string]string{key}})
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return body
}

// jwksServerAlg serves a JWKS for the given RSA public key under the given kid,
// advertising the given alg.
func jwksServerAlg(t *testing.T, pub *rsa.PublicKey, kid, alg string) *httptest.Server {
	t.Helper()
	body := jwksBody(t, pub, kid, alg)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// jwksServer serves an RS256 JWKS for the given RSA public key under the given kid.
func jwksServer(t *testing.T, pub *rsa.PublicKey, kid string) *httptest.Server {
	t.Helper()
	return jwksServerAlg(t, pub, kid, "RS256")
}

// signRS256 signs claims with key under kid using RS256.
func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	return signAlg(t, jwt.SigningMethodRS256, key, kid, claims)
}

// signAlg signs claims with key under kid using the given RSA signing method.
func signAlg(t *testing.T, method jwt.SigningMethod, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign %s: %v", method.Alg(), err)
	}
	return s
}

// newVerifierFor builds a Verifier pointing at jwksURL with iss/aud.
func newVerifierFor(t *testing.T, jwksURL, iss, aud string) *Verifier {
	t.Helper()
	v, err := NewVerifier(Config{Issuer: iss, Audience: aud, JWKSURL: jwksURL})
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

	// Array aud containing the expected audience (jwt/v5 audience check is a
	// containment test — Keycloak issues array aud when a token has several
	// audiences).
	arrayAudClaims := validClaims(iss, aud, time.Now().Add(time.Hour))
	arrayAudClaims["aud"] = []string{"other-consumer", aud}

	// Array aud NOT containing the expected audience.
	wrongArrayAudClaims := validClaims(iss, aud, time.Now().Add(time.Hour))
	wrongArrayAudClaims["aud"] = []string{"other-consumer", "another"}

	missingAudClaims := validClaims(iss, aud, time.Now().Add(time.Hour))
	delete(missingAudClaims, "aud")

	emptySubClaims := validClaims(iss, aud, time.Now().Add(time.Hour))
	emptySubClaims["sub"] = ""

	missingSubClaims := validClaims(iss, aud, time.Now().Add(time.Hour))
	delete(missingSubClaims, "sub")

	tests := []struct {
		name       string
		token      string // raw token; "" means no Authorization header
		bearer     bool   // wrap token in "Bearer " prefix
		verifier   *Verifier
		wantStatus int
		wantUserID string
	}{
		{
			name:       "valid JWT (scalar aud)",
			token:      signRS256(t, key, testKID, validClaims(iss, aud, time.Now().Add(time.Hour))),
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusOK,
			wantUserID: "42",
		},
		{
			name:       "valid JWT (array aud containing audience)",
			token:      signRS256(t, key, testKID, arrayAudClaims),
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusOK,
			wantUserID: "42",
		},
		{
			name:       "array aud without audience -> 401",
			token:      signRS256(t, key, testKID, wrongArrayAudClaims),
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing aud -> 401",
			token:      signRS256(t, key, testKID, missingAudClaims),
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "empty sub -> 401",
			token:      signRS256(t, key, testKID, emptySubClaims),
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "missing sub -> 401",
			token:      signRS256(t, key, testKID, missingSubClaims),
			bearer:     true,
			verifier:   verifier,
			wantStatus: http.StatusUnauthorized,
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
			// RS512 is a valid RSA signature but not the pinned algorithm
			// (default RS256) — rejected before the key is even consulted.
			name:       "RS512 under default RS256 pin -> 401",
			token:      signAlg(t, jwt.SigningMethodRS512, key, testKID, validClaims(iss, aud, time.Now().Add(time.Hour))),
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

// TestMiddlewareJWT_Roles verifies role normalization: roles are read from the
// configured dot-separated claim path (default Keycloak realm_access.roles),
// normalized to []string on CtxRoles, and a missing/malformed claim yields an
// EMPTY slice, never a rejection — the role gate (MiddlewareRequireRole)
// decides authorization later.
func TestMiddlewareJWT_Roles(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const iss = "https://auth.duynhlab.dev"
	const aud = "duynhlab-api"

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	jwks := jwksServer(t, &key.PublicKey, testKID)

	tests := []struct {
		name      string
		rolesPath string // "" = default (realm_access.roles)
		mutate    func(claims jwt.MapClaims)
		wantRoles []string
	}{
		{
			name: "roles at default path realm_access.roles",
			mutate: func(claims jwt.MapClaims) {
				claims["realm_access"] = map[string]any{"roles": []string{"customer", "backoffice_admin"}}
			},
			wantRoles: []string{"customer", "backoffice_admin"},
		},
		{
			name:      "roles at custom path resource_access.app.roles",
			rolesPath: "resource_access.app.roles",
			mutate: func(claims jwt.MapClaims) {
				claims["resource_access"] = map[string]any{"app": map[string]any{"roles": []string{"customer"}}}
			},
			wantRoles: []string{"customer"},
		},
		{
			name:      "missing roles claim -> empty slice, request passes",
			mutate:    func(jwt.MapClaims) {},
			wantRoles: []string{},
		},
		{
			name: "non-string entries in roles array are skipped",
			mutate: func(claims jwt.MapClaims) {
				claims["realm_access"] = map[string]any{"roles": []any{"customer", 7, true, nil, "auditor"}}
			},
			wantRoles: []string{"customer", "auditor"},
		},
		{
			name: "roles claim not an array -> empty slice",
			mutate: func(claims jwt.MapClaims) {
				claims["realm_access"] = map[string]any{"roles": "customer"}
			},
			wantRoles: []string{},
		},
		{
			name: "intermediate path segment not an object -> empty slice",
			mutate: func(claims jwt.MapClaims) {
				claims["realm_access"] = "not-an-object"
			},
			wantRoles: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := NewVerifier(Config{
				Issuer:         iss,
				Audience:       aud,
				JWKSURL:        jwks.URL,
				RolesClaimPath: tt.rolesPath,
			})
			if err != nil {
				t.Fatalf("NewVerifier: %v", err)
			}

			claims := validClaims(iss, aud, time.Now().Add(time.Hour))
			tt.mutate(claims)
			token := signRS256(t, key, testKID, claims)

			var gotRoles []string
			var gotSet bool
			r := gin.New()
			r.Use(MiddlewareJWT(v))
			r.GET("/x", func(c *gin.Context) {
				var raw any
				raw, gotSet = c.Get(CtxRoles)
				gotRoles, _ = raw.([]string)
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, http.StatusOK, w.Body.String())
			}
			if !gotSet {
				t.Fatalf("CtxRoles not set on the gin context")
			}
			if gotRoles == nil {
				t.Fatalf("CtxRoles = nil, want a non-nil []string (empty slice for missing claim)")
			}
			if len(gotRoles) != len(tt.wantRoles) {
				t.Fatalf("roles = %v, want %v", gotRoles, tt.wantRoles)
			}
			for i := range gotRoles {
				if gotRoles[i] != tt.wantRoles[i] {
					t.Fatalf("roles = %v, want %v", gotRoles, tt.wantRoles)
				}
			}
		})
	}
}

// TestMiddlewareRequireRole verifies the role gate: 403 with the platform
// FORBIDDEN envelope when the verified roles lack the required role, when the
// auth middleware never ran (absent context — fail closed), or when the
// context value has the wrong type. Allowed only on an explicit match.
func TestMiddlewareRequireRole(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		setCtx     func(c *gin.Context) // nil = auth middleware never ran
		wantStatus int
	}{
		{
			name:       "role present -> pass",
			setCtx:     func(c *gin.Context) { c.Set(CtxRoles, []string{"customer", "backoffice_admin"}) },
			wantStatus: http.StatusOK,
		},
		{
			name:       "role absent -> 403",
			setCtx:     func(c *gin.Context) { c.Set(CtxRoles, []string{"customer"}) },
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "empty roles -> 403",
			setCtx:     func(c *gin.Context) { c.Set(CtxRoles, []string{}) },
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "auth middleware never ran (no CtxRoles) -> 403",
			setCtx:     nil,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "CtxRoles wrong type -> 403",
			setCtx:     func(c *gin.Context) { c.Set(CtxRoles, "backoffice_admin") },
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := gin.New()
			if tt.setCtx != nil {
				r.Use(func(c *gin.Context) { tt.setCtx(c); c.Next() })
			}
			r.Use(MiddlewareRequireRole("backoffice_admin"))
			r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tt.wantStatus, w.Body.String())
			}
			if tt.wantStatus == http.StatusForbidden {
				var body struct {
					Error string `json:"error"`
					Code  string `json:"code"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("unmarshal 403 body %q: %v", w.Body.String(), err)
				}
				if body.Code != "FORBIDDEN" {
					t.Errorf("code = %q, want %q", body.Code, "FORBIDDEN")
				}
				if body.Error == "" {
					t.Errorf("error message empty, want a sanitized message")
				}
			}
		})
	}
}

// TestNewVerifier_JWKSCacheTTL proves the JWKSCacheTTL is wired into the
// jwkset background refresh: with a short TTL the JWKS endpoint is re-fetched
// repeatedly (the library default is one hour, which would produce exactly one
// fetch within this test's lifetime).
func TestNewVerifier_JWKSCacheTTL(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	body := jwksBody(t, &key.PublicKey, testKID, "RS256")

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	_, err = NewVerifier(Config{
		Issuer:       "https://auth.duynhlab.dev",
		Audience:     "duynhlab-api",
		JWKSURL:      srv.URL,
		JWKSCacheTTL: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	// One synchronous fetch at construction + at least two TTL refreshes.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("JWKS fetched %d time(s) in 5s with a 25ms TTL, want >= 3 (TTL not wired into refresh)", hits.Load())
}

// TestNewVerifier_RequiredAlgorithm verifies the pin is configurable: with
// RS512 required, an RS512 token passes and an RS256 token is rejected.
func TestNewVerifier_RequiredAlgorithm(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const iss = "https://auth.duynhlab.dev"
	const aud = "duynhlab-api"

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	// The JWKS advertises no "alg" so the same key serves both signing methods;
	// only the verifier-side pin decides.
	jwks := jwksServerAlg(t, &key.PublicKey, testKID, "")

	v, err := NewVerifier(Config{
		Issuer:            iss,
		Audience:          aud,
		JWKSURL:           jwks.URL,
		RequiredAlgorithm: "RS512",
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	tests := []struct {
		name       string
		method     jwt.SigningMethod
		wantStatus int
	}{
		{name: "pinned RS512 -> pass", method: jwt.SigningMethodRS512, wantStatus: http.StatusOK},
		{name: "RS256 under RS512 pin -> 401", method: jwt.SigningMethodRS256, wantStatus: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := signAlg(t, tt.method, key, testKID, validClaims(iss, aud, time.Now().Add(time.Hour)))

			r := gin.New()
			r.Use(MiddlewareJWT(v))
			r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", w.Code, tt.wantStatus, w.Body.String())
			}
		})
	}
}

// TestNewVerifier_DerivedJWKSURL verifies that an empty Config.JWKSURL derives
// the Keycloak realm certs endpoint <issuer>/protocol/openid-connect/certs,
// end-to-end: the derived path is fetched and a token from that key verifies.
func TestNewVerifier_DerivedJWKSURL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const aud = "duynhlab-api"

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	body := jwksBody(t, &key.PublicKey, testKID, "RS256")

	var certsHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/protocol/openid-connect/certs", func(w http.ResponseWriter, _ *http.Request) {
		certsHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Both the canonical Keycloak issuer form (no trailing slash) and a
	// trailing-slash issuer must derive the same certs URL.
	for _, tc := range []struct {
		name string
		iss  string
	}{
		{name: "issuer without trailing slash", iss: srv.URL},
		{name: "issuer with trailing slash", iss: srv.URL + "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := NewVerifier(Config{Issuer: tc.iss, Audience: aud})
			if err != nil {
				t.Fatalf("NewVerifier: %v", err)
			}

			token := signRS256(t, key, testKID, validClaims(tc.iss, aud, time.Now().Add(time.Hour)))

			r := gin.New()
			r.Use(MiddlewareJWT(v))
			r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, http.StatusOK, w.Body.String())
			}
		})
	}
	if certsHits.Load() == 0 {
		t.Fatal("derived certs endpoint was never fetched")
	}
}

// preferred_username is the OIDC standard claim; "username" is the legacy
// shape. Reading the wrong one leaves the handle empty in every downstream
// response, which is a silent regression rather than a failure — so pin the
// precedence and the fallback.
func TestMiddlewareJWT_UsernameClaimPrecedence(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const iss = "https://id.example.test/realms/r"
	const aud = "platform"

	tests := []struct {
		name     string
		mutate   func(jwt.MapClaims)
		wantUser string
	}{
		{
			name: "preferred_username wins over legacy username",
			mutate: func(c jwt.MapClaims) {
				c["preferred_username"] = "alice"
				c["username"] = "legacy"
			},
			wantUser: "alice",
		},
		{
			name:     "legacy username still read when preferred_username is absent",
			mutate:   func(c jwt.MapClaims) { c["username"] = "legacy" },
			wantUser: "legacy",
		},
		{
			name:     "neither claim leaves the handle empty without failing the request",
			mutate:   func(c jwt.MapClaims) { delete(c, "username") },
			wantUser: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generate key: %v", err)
			}
			const kid = "k1"
			srv := jwksServer(t, &key.PublicKey, kid)

			claims := validClaims(iss, aud, time.Now().Add(time.Hour))
			tt.mutate(claims)
			token := signRS256(t, key, kid, claims)

			v := newVerifierFor(t, srv.URL, iss, aud)

			var gotUser string
			r := gin.New()
			r.Use(MiddlewareJWT(v))
			r.GET("/x", func(c *gin.Context) {
				gotUser = c.GetString(CtxUsername)
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if gotUser != tt.wantUser {
				t.Errorf("username = %q, want %q", gotUser, tt.wantUser)
			}
		})
	}
}
