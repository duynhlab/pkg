package grpcx

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// obsLogger returns a zap logger backed by an in-memory observer so tests can
// assert on the emitted access-log entries.
func obsLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.InfoLevel)
	return zap.New(core), logs
}

func TestAccessLogUnary_OKLogsInfo(t *testing.T) {
	logger, logs := obsLogger()
	interceptor := accessLogUnary(logger)

	info := &grpc.UnaryServerInfo{FullMethod: "/product.v1.ProductService/ReserveStock"}
	_, err := interceptor(context.Background(), nil, info,
		func(context.Context, any) (any, error) { return "ok", nil })
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 log entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Level != zap.InfoLevel {
		t.Errorf("level = %v, want Info", e.Level)
	}
	if e.Message != "gRPC request" {
		t.Errorf("message = %q, want %q", e.Message, "gRPC request")
	}
	fields := e.ContextMap()
	if fields["method"] != info.FullMethod {
		t.Errorf("method = %v, want %s", fields["method"], info.FullMethod)
	}
	if fields["code"] != "OK" {
		t.Errorf("code = %v, want OK", fields["code"])
	}
	if _, ok := fields["duration"]; !ok {
		t.Error("duration field missing")
	}
	if _, ok := fields["trace_id"]; !ok {
		t.Error("trace_id field missing")
	}
}

func TestAccessLogUnary_ErrorLogsErrorLevelWithCode(t *testing.T) {
	logger, logs := obsLogger()
	interceptor := accessLogUnary(logger)

	info := &grpc.UnaryServerInfo{FullMethod: "/product.v1.ProductService/GetProduct"}
	_, err := interceptor(context.Background(), nil, info,
		func(context.Context, any) (any, error) {
			return nil, status.Error(codes.NotFound, "missing")
		})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}

	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("want 1 log entry, got %d", len(entries))
	}
	if entries[0].Level != zap.ErrorLevel {
		t.Errorf("level = %v, want Error", entries[0].Level)
	}
	if entries[0].ContextMap()["code"] != "NotFound" {
		t.Errorf("code = %v, want NotFound", entries[0].ContextMap()["code"])
	}
}

func TestAccessLogUnary_SkipsInfraMethods(t *testing.T) {
	logger, logs := obsLogger()
	interceptor := accessLogUnary(logger)

	for _, m := range []string{
		"/grpc.health.v1.Health/Check",
		"/grpc.health.v1.Health/Watch",
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
	} {
		info := &grpc.UnaryServerInfo{FullMethod: m}
		if _, err := interceptor(context.Background(), nil, info,
			func(context.Context, any) (any, error) { return nil, nil }); err != nil {
			t.Fatalf("handler error for %s: %v", m, err)
		}
	}
	if n := logs.Len(); n != 0 {
		t.Errorf("infra methods produced %d log entries, want 0", n)
	}
}

func TestAccessLogUnary_NilLoggerIsNoop(t *testing.T) {
	interceptor := accessLogUnary(nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/x.Y/Z"}
	called := false
	if _, err := interceptor(context.Background(), nil, info,
		func(context.Context, any) (any, error) { called = true; return nil, nil }); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if !called {
		t.Error("nil-logger interceptor must still call the handler")
	}
}

func TestIsInfraMethod(t *testing.T) {
	cases := map[string]bool{
		"/grpc.health.v1.Health/Check":                              true,
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo": true,
		"/product.v1.ProductService/ReserveStock":                   false,
		"/review.v1.ReviewService/GetProductReviews":                false,
	}
	for m, want := range cases {
		if got := isInfraMethod(m); got != want {
			t.Errorf("isInfraMethod(%q) = %v, want %v", m, got, want)
		}
	}
}

// panicDesc registers one unary method "/grpcx.test.Panic/Boom" whose handler
// panics — it invokes the server's chained interceptor so recovery + access
// log run exactly as in production.
var panicDesc = grpc.ServiceDesc{
	ServiceName: "grpcx.test.Panic",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Boom",
		Handler: func(_ any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			if err := dec(&emptypb.Empty{}); err != nil {
				return nil, err
			}
			impl := func(context.Context, any) (any, error) { panic("boom") }
			info := &grpc.UnaryServerInfo{FullMethod: "/grpcx.test.Panic/Boom"}
			if interceptor == nil {
				return impl(ctx, nil)
			}
			return interceptor(ctx, &emptypb.Empty{}, info, impl)
		},
	}},
	Metadata: "test",
}

// TestNewServer_LogsRecoveredPanicAsInternal is the regression guard for the
// interceptor chain order: a panicking handler must surface as a single
// code=Internal access-log entry at Error level (not vanish). Runs over the
// wire through NewServer so it catches a re-inversion of the chain in
// server.go, not just the interceptor in isolation.
func TestNewServer_LogsRecoveredPanicAsInternal(t *testing.T) {
	logger, logs := obsLogger()
	srv, _ := NewServer(logger)
	srv.RegisterService(&panicDesc, struct{}{})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := Dial(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = conn.Invoke(ctx, "/grpcx.test.Panic/Boom", &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("recovered panic should surface as codes.Internal, got %v", status.Code(err))
	}

	entries := logs.FilterMessage("gRPC request").All()
	if len(entries) != 1 {
		t.Fatalf("want 1 access-log entry for the panicking RPC, got %d", len(entries))
	}
	if entries[0].Level != zap.ErrorLevel {
		t.Errorf("level = %v, want Error", entries[0].Level)
	}
	if got := entries[0].ContextMap()["code"]; got != "Internal" {
		t.Errorf("code = %v, want Internal", got)
	}
}
