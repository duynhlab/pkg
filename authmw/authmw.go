// Package authmw provides a single, shared, fail-closed gin middleware that
// validates a request's bearer token by calling the auth service's GetMe over
// gRPC. It replaces the per-service copy-paste auth middleware so the
// security-critical fail-closed behaviour lives in exactly one place (the
// previous duplication is what let a fail-open regression slip into one service).
//
// MiddlewareJWT additionally supports LOCAL verification of RS256 JWTs against a
// cached JWKS (Verifier), falling back to the opaque GetMe path for non-JWT
// tokens. Both paths remain fail-closed.
package authmw

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/duynhlab/pkg/grpcx"
	authv1 "github.com/duynhlab/pkg/proto/auth/v1"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Context keys set on a successful authentication.
const (
	CtxUserID   = "user_id"
	CtxUsername = "username"
	CtxEmail    = "email"
)

// Validator is the subset of authv1.AuthServiceClient the middleware needs.
// The generated client satisfies it; tests provide a fake.
type Validator interface {
	GetMe(ctx context.Context, in *authv1.GetMeRequest, opts ...grpc.CallOption) (*authv1.GetMeResponse, error)
}

// Middleware returns a gin middleware that validates the Authorization bearer
// token via auth.GetMe (gRPC), forwarding the token in gRPC metadata. It fails
// closed:
//   - missing Authorization header           -> 401
//   - auth reports Unauthenticated            -> 401
//   - auth is unreachable / any other error   -> 503 (still denies the request)
//
// On success it sets user_id/username/email in the gin context.
func Middleware(client Validator) gin.HandlerFunc {
	return func(c *gin.Context) {
		authz := c.GetHeader("Authorization")
		if authz == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}
		authenticateOpaque(c, client, authz)
	}
}

// authenticateOpaque runs the opaque-token path: validate authz via auth.GetMe
// (gRPC), mapping Unauthenticated -> 401 and any other/unreachable error -> 503,
// and on success set the Ctx* values and call c.Next(). It is the single source
// of truth shared by Middleware and MiddlewareJWT's opaque branch.
func authenticateOpaque(c *gin.Context, client Validator, authz string) {
	ctx := grpcx.WithAuthToken(c.Request.Context(), authz)
	resp, err := client.GetMe(ctx, &authv1.GetMeRequest{})
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
			return
		}
		// Auth unreachable or internal error: deny, but signal it's transient
		// rather than masquerading as an auth failure.
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "Authentication temporarily unavailable"})
		return
	}

	user := resp.GetUser()
	c.Set(CtxUserID, user.GetId())
	c.Set(CtxUsername, user.GetUsername())
	c.Set(CtxEmail, user.GetEmail())
	c.Next()
}

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
// when the signing key cannot be supplied (JWKS unreachable / refresh failed),
// and a plain validation error for an invalid token (bad signature, expired,
// wrong issuer/audience, malformed, missing exp, disallowed alg). RS256 is
// pinned to defend against algorithm-confusion attacks.
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
		// A key-supply failure (JWKS unreachable, kid not found) surfaces as
		// ErrTokenUnverifiable wrapping keyfunc.ErrKeyfunc; classify it as
		// transient so the caller can answer 503 instead of 401. A token whose
		// key WAS found but failed verification (signature/expiry/iss/aud)
		// surfaces as a distinct sentinel and stays an invalid-token error.
		if errors.Is(err, keyfunc.ErrKeyfunc) || errors.Is(err, jwt.ErrTokenUnverifiable) {
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

func stringClaim(claims jwt.MapClaims, key string) string {
	if s, ok := claims[key].(string); ok {
		return s
	}
	return ""
}

// MiddlewareJWT returns a dual-verify gin middleware. JWT-shaped tokens are
// verified locally by verifier; opaque tokens fall back to fallback.GetMe. Both
// paths are fail-closed. Behaviour:
//   - missing Authorization header             -> 401 (neither path consulted)
//   - JWT-shaped, valid                         -> sets Ctx* and continues
//   - JWT-shaped, invalid (sig/exp/iss/aud/alg) -> 401
//   - JWT-shaped, key unavailable (JWKS down)   -> 503
//   - JWT-shaped but verifier is nil            -> treated as opaque (fallback)
//   - opaque, fallback success                  -> sets Ctx* and continues
//   - opaque, Unauthenticated                   -> 401
//   - opaque, unreachable / other               -> 503
//   - opaque with no fallback                   -> 401
func MiddlewareJWT(verifier *Verifier, fallback Validator) gin.HandlerFunc {
	return func(c *gin.Context) {
		authz := c.GetHeader("Authorization")
		if authz == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		// Strip a case-insensitive "Bearer " prefix for shape detection only;
		// the original header is forwarded unchanged to the opaque fallback.
		tok := authz
		if len(tok) >= 7 && strings.EqualFold(tok[:7], "bearer ") {
			tok = strings.TrimSpace(tok[7:])
		}

		// A compact JWS has exactly two dots; otherwise treat it as opaque.
		if strings.Count(tok, ".") == 2 && verifier != nil {
			claims, err := verifier.verify(tok)
			if err != nil {
				if errors.Is(err, errTransient) {
					c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "Authentication temporarily unavailable"})
					return
				}
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
				return
			}
			c.Set(CtxUserID, claims.sub)
			c.Set(CtxUsername, claims.username)
			c.Set(CtxEmail, claims.email)
			c.Next()
			return
		}

		// Opaque path (or JWT-shaped with no verifier): require a fallback.
		if fallback == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid or expired token"})
			return
		}
		authenticateOpaque(c, fallback, authz)
	}
}
