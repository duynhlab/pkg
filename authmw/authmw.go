// Package authmw provides a single, shared, fail-closed gin middleware that
// verifies OIDC JWT bearer tokens locally against a cached, periodically
// refreshed JWKS. It replaces the per-service copy-paste auth middleware so the
// security-critical fail-closed behaviour lives in exactly one place.
//
// JWT is the only supported credential (RFC-0009 Phase 5): the legacy opaque
// session tokens and the auth.GetMe gRPC fallback were removed once every
// caller presented JWTs. RFC-0022 evolved the verifier from auth-service
// defaults to a generic OIDC configuration (Keycloak as the platform IdP):
// exact-issuer match, audience containment (array aud supported), pinned
// signing algorithm, required non-empty sub, and role normalization from a
// configurable claim path.
package authmw

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/MicahParks/keyfunc/v3"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Context keys set on a successful authentication.
const (
	CtxUserID   = "user_id"
	CtxUsername = "username"
	CtxEmail    = "email"
	// CtxRoles holds the normalized []string of roles read from the verified
	// token's roles claim path. Always set on success — an absent or malformed
	// roles claim yields an empty slice, never a rejection; authorization is
	// MiddlewareRequireRole's job.
	CtxRoles = "roles"
)

// msgInvalidToken is the shared 401 response body.
const msgInvalidToken = "Invalid or expired token"

// codeForbidden mirrors httpx.CodeForbidden. authmw and httpx deliberately do
// not import each other (both are Layer 1 gin modules); the platform error
// envelope {"error", "code"} is a wire contract, so the constant is duplicated
// here rather than imported.
const codeForbidden = "FORBIDDEN"

// Defaults applied by NewVerifier for optional Config fields.
const (
	// defaultRequiredAlgorithm pins RS256 unless overridden, defending against
	// algorithm-confusion attacks.
	defaultRequiredAlgorithm = "RS256"
	// defaultRolesClaimPath is Keycloak's standard realm-roles claim.
	defaultRolesClaimPath = "realm_access.roles"
	// keycloakCertsPath is appended to the issuer when Config.JWKSURL is empty
	// (Keycloak realm JWKS endpoint).
	keycloakCertsPath = "/protocol/openid-connect/certs"
)

// errTransient marks a key-fetch / JWKS-unavailable failure, as opposed to an
// invalid token. The two map to different HTTP statuses (503 vs 401), so the
// middleware must be able to tell them apart.
var errTransient = errors.New("authmw: key verification temporarily unavailable")

// Config configures a Verifier. Issuer and Audience are required; everything
// else has a safe default.
type Config struct {
	// Issuer is the expected `iss` claim, matched exactly. Required.
	Issuer string
	// Audience is the expected audience. Verification is a containment test:
	// a scalar or array `aud` claim passes when it contains this value
	// (jwt/v5 semantics). Required.
	Audience string
	// JWKSURL overrides the JWKS endpoint. When empty it is derived from the
	// issuer as <Issuer>/protocol/openid-connect/certs (Keycloak realm certs).
	JWKSURL string
	// RequiredAlgorithm pins the accepted JWS algorithm; tokens signed with
	// any other algorithm are rejected. Defaults to "RS256".
	RequiredAlgorithm string
	// JWKSCacheTTL is the interval of the background JWKS refresh. Zero keeps
	// the keyfunc default (one hour). Unknown-kid lookups additionally trigger
	// a bounded refresh independent of this interval (see NewVerifier).
	JWKSCacheTTL time.Duration
	// RolesClaimPath is the dot-separated path to the roles array inside the
	// token claims. Defaults to "realm_access.roles" (Keycloak realm roles).
	RolesClaimPath string
}

// Verifier verifies OIDC JWTs locally against a cached, periodically-refreshed
// JWKS, enforcing issuer, audience, expiration, a pinned algorithm, and a
// non-empty subject.
type Verifier struct {
	kf        keyfunc.Keyfunc
	issuer    string
	audience  string
	algorithm string
	rolesPath []string
}

// verifiedClaims holds the subset of claims the middleware propagates.
type verifiedClaims struct {
	sub      string
	username string
	email    string
	roles    []string
}

// NewVerifier builds a Verifier from cfg. It validates the required fields,
// derives the Keycloak JWKS URL when cfg.JWKSURL is empty, and starts a JWKS
// client with background caching + periodic refresh at cfg.JWKSCacheTTL.
//
// Unknown-kid single-flight note: the underlying jwkset HTTP client keeps its
// default RefreshUnknownKID rate limiter (one refresh per 5 minutes, burst 1),
// so a burst of tokens with an unknown kid triggers at most one JWKS fetch per
// window — concurrent readers wait on the limiter (bounded by the client's
// RateLimitWaitMax) instead of each firing its own request. keyfunc's
// NewDefaultOverrideCtx preserves that default; only the periodic refresh
// interval is overridden here.
func NewVerifier(cfg Config) (*Verifier, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("authmw: Config.Issuer is required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("authmw: Config.Audience is required")
	}
	jwksURL := cfg.JWKSURL
	if jwksURL == "" {
		jwksURL = strings.TrimSuffix(cfg.Issuer, "/") + keycloakCertsPath
	}
	algorithm := cfg.RequiredAlgorithm
	if algorithm == "" {
		algorithm = defaultRequiredAlgorithm
	}
	rolesPath := cfg.RolesClaimPath
	if rolesPath == "" {
		rolesPath = defaultRolesClaimPath
	}

	kf, err := keyfunc.NewDefaultOverrideCtx(context.Background(), []string{jwksURL}, keyfunc.Override{
		// Zero keeps keyfunc's one-hour default, matching the previous
		// NewDefault behavior exactly.
		RefreshInterval: cfg.JWKSCacheTTL,
	})
	if err != nil {
		return nil, err
	}
	return &Verifier{
		kf:        kf,
		issuer:    cfg.Issuer,
		audience:  cfg.Audience,
		algorithm: algorithm,
		rolesPath: strings.Split(rolesPath, "."),
	}, nil
}

// verify parses and validates tokenString. It returns errTransient (wrapped)
// ONLY on genuine JWKS unavailability (the endpoint is unreachable and the key
// set was never successfully loaded), and a plain validation error for an
// invalid token — bad signature, expired, wrong issuer/audience, malformed,
// missing exp, disallowed/mismatched alg, an unknown kid, or an empty/missing
// sub. The configured algorithm is pinned to defend against
// algorithm-confusion attacks.
//
// Classification note: an unknown kid and a JWKS outage BOTH surface from
// keyfunc as jwkset.ErrKeyNotFound — jwkset swallows a failed refresh and falls
// through to "not found", so the sentinel alone cannot tell them apart. An
// attacker-controlled forged kid must be a 401, so ErrKeyNotFound defaults to
// invalid; it is only reclassified as transient (503) when the cached JWK set is
// actually empty, i.e. the endpoint never delivered any keys.
func (v *Verifier) verify(tokenString string) (*verifiedClaims, error) {
	token, err := jwt.Parse(
		tokenString,
		v.kf.Keyfunc,
		jwt.WithValidMethods([]string{v.algorithm}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		// Genuine key unavailability (JWKS never loaded) is the only transient
		// case → 503. Everything else — unknown kid against a loaded set, alg
		// mismatch/missing alg, bad signature, expiry, iss/aud — is an invalid
		// token → 401.
		if errors.Is(err, jwkset.ErrKeyNotFound) && v.jwksUnavailable() {
			return nil, errors.Join(errTransient, err)
		}
		return nil, err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, jwt.ErrTokenInvalidClaims
	}
	sub := stringClaim(claims, "sub")
	if sub == "" {
		// A token without a subject cannot be attributed to a principal —
		// reject (401) rather than propagate an empty identity downstream.
		return nil, jwt.ErrTokenInvalidClaims
	}
	return &verifiedClaims{
		sub:      sub,
		username: stringClaim(claims, "username"),
		email:    stringClaim(claims, "email"),
		roles:    rolesClaim(claims, v.rolesPath),
	}, nil
}

// jwksUnavailable reports whether the JWK set is genuinely unavailable: the
// backing storage errors or holds no keys at all (the endpoint was never
// reachable / never delivered a key). It is the discriminator that separates a
// real JWKS outage (→ 503) from an attacker-supplied unknown kid checked
// against a healthy, populated set (→ 401), since both otherwise surface as
// jwkset.ErrKeyNotFound.
func (v *Verifier) jwksUnavailable() bool {
	keys, err := v.kf.Storage().KeyReadAll(context.Background())
	return err != nil || len(keys) == 0
}

func stringClaim(claims jwt.MapClaims, key string) string {
	if s, ok := claims[key].(string); ok {
		return s
	}
	return ""
}

// rolesClaim walks the dot-separated path into claims and normalizes the value
// found there to []string. Any shape mismatch — a missing segment, a non-object
// intermediate, or a non-array leaf — yields an empty slice, never an error:
// role ABSENCE is an authorization decision (MiddlewareRequireRole), not an
// authentication failure. Non-string array entries are skipped.
func rolesClaim(claims jwt.MapClaims, path []string) []string {
	var current any = map[string]any(claims)
	for _, segment := range path {
		obj, ok := current.(map[string]any)
		if !ok {
			return []string{}
		}
		current, ok = obj[segment]
		if !ok {
			return []string{}
		}
	}
	items, ok := current.([]any)
	if !ok {
		return []string{}
	}
	roles := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			roles = append(roles, s)
		}
	}
	return roles
}

// MiddlewareJWT returns a JWT-only, fail-closed gin middleware. Behaviour:
//   - missing Authorization header             -> 401 (verifier not consulted)
//   - nil verifier                              -> 503 (cannot verify anything;
//     services treat a failed NewVerifier as fatal, this is defence-in-depth)
//   - not JWT-shaped (no compact-JWS form)      -> 401
//   - valid JWT                                 -> sets Ctx* and continues
//   - invalid JWT (sig/exp/iss/aud/alg/kid/sub) -> 401
//   - key unavailable (JWKS never loaded)       -> 503
func MiddlewareJWT(verifier *Verifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		authz := c.GetHeader("Authorization")
		if authz == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		if verifier == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "Authentication temporarily unavailable"})
			return
		}

		// A compact JWS has exactly two dots; anything else is not a JWT and is
		// rejected outright (opaque tokens are no longer a credential).
		tok := bearerToken(authz)
		if strings.Count(tok, ".") != 2 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msgInvalidToken})
			return
		}

		authenticateJWT(c, verifier, tok)
	}
}

// MiddlewareRequireRole returns a gin middleware that lets the request through
// only when the authenticated principal's verified roles (CtxRoles, set by
// MiddlewareJWT) contain role. On a miss it responds 403 with the platform
// error envelope {"error", "code": "FORBIDDEN"}.
//
// Fail closed: an absent or wrongly-typed CtxRoles — the auth middleware never
// ran, or the value was tampered with — is treated as "no roles" and rejected
// with the same 403. A 401 would misreport a service wiring bug as an
// authentication problem, and a 500 would leak that detail to the caller;
// denying with FORBIDDEN is the safest uniform answer.
func MiddlewareRequireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		value, _ := c.Get(CtxRoles)
		roles, _ := value.([]string)
		if !slices.Contains(roles, role) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "Insufficient permissions",
				"code":  codeForbidden,
			})
			return
		}
		c.Next()
	}
}

// bearerToken strips a case-insensitive "Bearer " prefix.
func bearerToken(authz string) string {
	if len(authz) >= 7 && strings.EqualFold(authz[:7], "bearer ") {
		return strings.TrimSpace(authz[7:])
	}
	return authz
}

// authenticateJWT verifies a JWT-shaped token locally and, on success, sets the
// Ctx* values and calls c.Next(). A transient key-fetch failure maps to 503; any
// other (invalid-token) error maps to 401. Fail-closed on every path.
func authenticateJWT(c *gin.Context, verifier *Verifier, tok string) {
	claims, err := verifier.verify(tok)
	if err != nil {
		if errors.Is(err, errTransient) {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "Authentication temporarily unavailable"})
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msgInvalidToken})
		return
	}
	c.Set(CtxUserID, claims.sub)
	c.Set(CtxUsername, claims.username)
	c.Set(CtxEmail, claims.email)
	c.Set(CtxRoles, claims.roles)
	c.Next()
}
