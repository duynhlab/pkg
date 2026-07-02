// Package authmw provides a single, shared, fail-closed gin middleware that
// verifies RS256 JWT bearer tokens locally against a cached, periodically
// refreshed JWKS. It replaces the per-service copy-paste auth middleware so the
// security-critical fail-closed behaviour lives in exactly one place.
//
// JWT is the only supported credential (RFC-0009 Phase 5): the legacy opaque
// session tokens and the auth.GetMe gRPC fallback were removed once every
// caller presented JWTs.
package authmw

import (
	"context"
	"errors"
	"net/http"
	"strings"

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
)

// msgInvalidToken is the shared 401 response body.
const msgInvalidToken = "Invalid or expired token"

// errTransient marks a key-fetch / JWKS-unavailable failure, as opposed to an
// invalid token. The two map to different HTTP statuses (503 vs 401), so the
// middleware must be able to tell them apart.
var errTransient = errors.New("authmw: key verification temporarily unavailable")

// Verifier verifies RS256 JWTs locally against a cached, periodically-refreshed
// JWKS, enforcing issuer, audience and expiration.
type Verifier struct {
	kf       keyfunc.Keyfunc
	issuer   string
	audience string
}

// verifiedClaims holds the subset of claims the middleware propagates.
type verifiedClaims struct {
	sub      string
	username string
	email    string
}

// NewVerifier builds a Verifier that fetches the JWKS from jwksURL with
// background caching + periodic refresh, and pins the expected issuer/audience.
func NewVerifier(jwksURL, issuer, audience string) (*Verifier, error) {
	kf, err := keyfunc.NewDefault([]string{jwksURL})
	if err != nil {
		return nil, err
	}
	return &Verifier{kf: kf, issuer: issuer, audience: audience}, nil
}

// verify parses and validates tokenString. It returns errTransient (wrapped)
// ONLY on genuine JWKS unavailability (the endpoint is unreachable and the key
// set was never successfully loaded), and a plain validation error for an
// invalid token — bad signature, expired, wrong issuer/audience, malformed,
// missing exp, disallowed/mismatched alg, or an unknown kid. RS256 is pinned to
// defend against algorithm-confusion attacks.
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
		jwt.WithValidMethods([]string{"RS256"}),
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
	return &verifiedClaims{
		sub:      stringClaim(claims, "sub"),
		username: stringClaim(claims, "username"),
		email:    stringClaim(claims, "email"),
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

// MiddlewareJWT returns a JWT-only, fail-closed gin middleware. Behaviour:
//   - missing Authorization header             -> 401 (verifier not consulted)
//   - nil verifier                              -> 503 (cannot verify anything;
//     services treat a failed NewVerifier as fatal, this is defence-in-depth)
//   - not JWT-shaped (no compact-JWS form)      -> 401
//   - valid JWT                                 -> sets Ctx* and continues
//   - invalid JWT (sig/exp/iss/aud/alg/kid)     -> 401
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
	c.Next()
}
