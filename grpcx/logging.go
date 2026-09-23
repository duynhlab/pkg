package grpcx

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
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

// codeLevel maps a status code to the access-log level, following the gRPC
// ecosystem's server-side convention (go-grpc-middleware
// logging.DefaultServerCodeToLevel) verbatim. The class distinction is who the
// line is FOR:
//
//   - Info: the server answered correctly; the outcome belongs to the caller.
//     A NotFound or InvalidArgument is a question answered, not a fault — the
//     old blanket non-OK=Error policy meant one loop probing missing rows wrote
//     an incident's worth of stacktraces per day, and every real Internal had
//     to be found inside that noise.
//   - Warn: degraded but explicable — quota, contention, a precondition that
//     did not hold, a dependency refusing. Worth a trend line, not a page.
//   - Error: this process (or the platform under it) failed; someone looks.
//
// The default is Error, not Info: a code this build does not know is a fault
// by definition, and new codes must surface loudly rather than inherit quiet.
func codeLevel(code codes.Code) slog.Level {
	switch code {
	case codes.OK, codes.NotFound, codes.Canceled, codes.AlreadyExists,
		codes.InvalidArgument, codes.Unauthenticated:
		return slog.LevelInfo
	case codes.DeadlineExceeded, codes.PermissionDenied, codes.ResourceExhausted,
		codes.FailedPrecondition, codes.Aborted, codes.OutOfRange, codes.Unavailable:
		return slog.LevelWarn
	case codes.Unknown, codes.Unimplemented, codes.Internal, codes.DataLoss:
		return slog.LevelError
	}
	return slog.LevelError
}

// canonicalCode is the status code's spec name ("NOT_FOUND", "CANCELLED") — the
// value the pinned otelgrpc puts on the server span as rpc.response.status_code.
// grpc-go's Code.String spells them differently ("NotFound", "Canceled"), and a
// log that disagrees with its span on the same attribute defeats the join.
func canonicalCode(c codes.Code) string {
	switch c {
	case codes.OK:
		return "OK"
	case codes.Canceled:
		return "CANCELLED"
	case codes.Unknown:
		return "UNKNOWN"
	case codes.InvalidArgument:
		return "INVALID_ARGUMENT"
	case codes.DeadlineExceeded:
		return "DEADLINE_EXCEEDED"
	case codes.NotFound:
		return "NOT_FOUND"
	case codes.AlreadyExists:
		return "ALREADY_EXISTS"
	case codes.PermissionDenied:
		return "PERMISSION_DENIED"
	case codes.ResourceExhausted:
		return "RESOURCE_EXHAUSTED"
	case codes.FailedPrecondition:
		return "FAILED_PRECONDITION"
	case codes.Aborted:
		return "ABORTED"
	case codes.OutOfRange:
		return "OUT_OF_RANGE"
	case codes.Unimplemented:
		return "UNIMPLEMENTED"
	case codes.Internal:
		return "INTERNAL"
	case codes.Unavailable:
		return "UNAVAILABLE"
	case codes.DataLoss:
		return "DATA_LOSS"
	case codes.Unauthenticated:
		return "UNAUTHENTICATED"
	}
	// A code outside the spec reaches a server only from a peer that minted
	// it; name it by number rather than fold it into UNKNOWN, a code it is not.
	return "CODE(" + strconv.FormatUint(uint64(c), 10) + ")"
}

// serverError reports the codes the pinned otelgrpc marks the server span Error
// for. Only those carry error.type on the access record, so the record and its
// span agree on whether the call failed. Severity is a different question —
// who should look — which is why DeadlineExceeded is a Warn here and still an
// error.type.
func serverError(c codes.Code) bool {
	switch c {
	case codes.Unknown, codes.DeadlineExceeded, codes.Unimplemented,
		codes.Internal, codes.Unavailable, codes.DataLoss:
		return true
	}
	return false
}

// accessLogUnary logs one record per incoming unary RPC, the gRPC counterpart
// of the HTTP access log. Health and reflection RPCs are skipped
// (isInfraMethod). A nil logger disables the access log; the call still runs.
func accessLogUnary(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if logger == nil || isInfraMethod(info.FullMethod) {
			return handler(ctx, req)
		}
		resp, err := handler(ctx, req)
		logRPC(ctx, logger, info.FullMethod, err)
		return resp, err
	}
}

// accessLogStream is the streaming counterpart of accessLogUnary. It logs once,
// when the stream completes.
func accessLogStream(logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(
		srv any,
		ss grpc.ServerStream,
		info *grpc.StreamServerInfo,
		handler grpc.StreamHandler,
	) error {
		if logger == nil || isInfraMethod(info.FullMethod) {
			return handler(srv, ss)
		}
		err := handler(srv, ss)
		logRPC(ss.Context(), logger, info.FullMethod, err)
		return err
	}
}

// logRPC writes the access record with the canonical attributes of the pinned
// semantic conventions (RFC-0031 § Canonical attributes): rpc.system.name,
// rpc.method as the fully-qualified "package.Service/Method" — the value the
// span carries, without the leading slash — and rpc.response.status_code, plus
// error.type for a server failure. It carries no peer address (in-cluster pod
// identity is not the record's business), no duration (the span and the RPC
// histogram measure it exactly), and no trace id field: the record is written
// with the call's context, so a context-aware handler stamps the ids itself.
func logRPC(ctx context.Context, logger *slog.Logger, fullMethod string, err error) {
	code := statusCode(err)
	attrs := make([]slog.Attr, 0, 4)
	attrs = append(attrs,
		slog.String("rpc.system.name", "grpc"),
		slog.String("rpc.method", rpcMethod(fullMethod)),
		slog.String("rpc.response.status_code", canonicalCode(code)),
	)
	if serverError(code) {
		attrs = append(attrs, slog.String("error.type", canonicalCode(code)))
	}
	logger.LogAttrs(ctx, codeLevel(code), "gRPC request", attrs...)
}

// statusCode is the code the client sees for err. A handler that returns a bare
// context error — ctx.Err() from a cancelled or expired call — reaches the wire
// as CANCELLED or DEADLINE_EXCEEDED, not UNKNOWN; status.Code alone would log
// the latter and disagree with the span.
func statusCode(err error) codes.Code {
	if s, ok := status.FromError(err); ok {
		return s.Code()
	}
	return status.FromContextError(err).Code()
}

// maxRPCMethod bounds rpc.method. The server only routes registered methods,
// but an unknown-service handler would see whatever path a peer sent.
const maxRPCMethod = 256

// rpcMethod is the full method as the span carries it: "package.Service/Method",
// without the leading slash, bounded.
func rpcMethod(fullMethod string) string {
	return bound(strings.TrimPrefix(fullMethod, "/"), maxRPCMethod)
}
