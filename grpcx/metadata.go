package grpcx

import (
	"context"

	"google.golang.org/grpc/metadata"
)

// authMetadataKey is the gRPC metadata key that carries the caller's bearer
// token on east-west calls. Lower-case per the gRPC metadata convention.
const authMetadataKey = "authorization"

// WithAuthToken returns a context that forwards the given Authorization header
// value (e.g. "Bearer <token>") as outgoing gRPC metadata. Use it on the client
// side before invoking an RPC that needs the caller's identity, e.g. when a
// service validates a request by calling auth.GetMe over gRPC. A blank value is
// a no-op.
func WithAuthToken(ctx context.Context, authorization string) context.Context {
	if authorization == "" {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, authMetadataKey, authorization)
}

// TokenFromContext extracts the Authorization metadata value from an incoming
// gRPC request on the server side. It returns the raw value (e.g.
// "Bearer <token>") and ok=false when the metadata is absent or empty.
func TokenFromContext(ctx context.Context) (string, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", false
	}
	vals := md.Get(authMetadataKey)
	if len(vals) == 0 || vals[0] == "" {
		return "", false
	}
	return vals[0], true
}
