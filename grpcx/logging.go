package grpcx

import (
	"context"
	"strings"
	"time"

	"github.com/duynhlab/pkg/obsx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// isInfraMethod reports whether a full method is pure plumbing that must not
// produce an access-log line: gRPC health checks (kubelet probes + client
// keepalive pings fire constantly) and server reflection (grpcurl). It mirrors
// telemetryFilter's skip set so logs, traces and metrics all ignore the same
// infrastructure RPCs.
func isInfraMethod(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, "/grpc.health.v1.Health/") ||
		strings.HasPrefix(fullMethod, "/grpc.reflection.")
}

// peerAddr returns the client address from ctx, or "" when unavailable. It is
// the gRPC analog of the HTTP access log's client_ip field.
func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

// accessLogUnary logs one line per incoming unary RPC, the gRPC counterpart of
// the HTTP LoggingMiddleware: OK calls at Info, everything else at Error, with
// the trace_id so a log line joins the same trace as its span. Health and
// reflection RPCs are skipped (isInfraMethod). A nil logger disables logging
// (the interceptor still forwards the call).
func accessLogUnary(logger *zap.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if logger == nil || isInfraMethod(info.FullMethod) {
			return handler(ctx, req)
		}
		start := time.Now()
		resp, err := handler(ctx, req)
		logRPC(ctx, logger, info.FullMethod, err, time.Since(start))
		return resp, err
	}
}

// accessLogStream is the streaming counterpart of accessLogUnary. It logs once
// when the stream completes (duration covers the whole stream lifetime).
func accessLogStream(logger *zap.Logger) grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		if logger == nil || isInfraMethod(info.FullMethod) {
			return handler(srv, ss)
		}
		start := time.Now()
		err := handler(srv, ss)
		logRPC(ss.Context(), logger, info.FullMethod, err, time.Since(start))
		return err
	}
}

// logRPC emits the shared access-log line. Kept separate so the unary and
// stream interceptors stay in lockstep on fields and level policy.
//
// Field naming vs the HTTP access log (product-service middleware): trace_id,
// method and duration match exactly, so a "give me everything for trace_id=X"
// query spans both protocols. The outcome and caller fields DELIBERATELY
// differ — gRPC uses code/peer, HTTP uses status/client_ip — because a gRPC
// status code is a distinct enum (OK/NotFound/Internal…), not an HTTP status
// int; reusing "status" would make that VictoriaLogs field mixed-type. peer is
// the in-cluster calling pod, not the edge client behind Kong. Cross-protocol
// outcome filtering therefore keys on trace_id, not a shared status field.
func logRPC(ctx context.Context, logger *zap.Logger, method string, err error, d time.Duration) {
	code := status.Code(err)
	level := zapcore.InfoLevel
	if code != codes.OK {
		level = zapcore.ErrorLevel
	}
	logger.Log(level, "gRPC request",
		zap.String("trace_id", obsx.TraceIDFromContext(ctx)),
		zap.String("method", method),
		zap.String("code", code.String()),
		zap.Duration("duration", d),
		zap.String("peer", peerAddr(ctx)),
	)
}
