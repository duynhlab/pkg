// Package authmw provides a single, shared, fail-closed gin middleware that
// validates a request's bearer token by calling the auth service's GetMe over
// gRPC. It replaces the per-service copy-paste auth middleware so the
// security-critical fail-closed behaviour lives in exactly one place (the
// previous duplication is what let a fail-open regression slip into one service).
package authmw

import (
	"context"
	"net/http"

	"github.com/duynhne/pkg/grpcx"
	authv1 "github.com/duynhne/pkg/proto/auth/v1"
	"github.com/gin-gonic/gin"
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
}
