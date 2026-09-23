package grpcx

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func TestNewServer_HealthAndReflection(t *testing.T) {
	srv, hs := NewServer(nil)
	t.Cleanup(srv.Stop)

	if srv == nil {
		t.Fatal("NewServer returned a nil *grpc.Server")
	}
	if hs == nil {
		t.Fatal("NewServer returned a nil *health.Server")
	}

	// Default serving status for the overall server ("") must be SERVING.
	resp, err := hs.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health Check: %v", err)
	}
	if got := resp.GetStatus(); got != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("health status = %v, want SERVING", got)
	}

	// The health service must be registered on the server.
	if _, ok := srv.GetServiceInfo()["grpc.health.v1.Health"]; !ok {
		t.Error("grpc.health.v1.Health service not registered")
	}
}

func TestDial_LazyConn(t *testing.T) {
	// grpc.NewClient is lazy: it must not error or block on a well-formed
	// dns:/// target even though nothing is listening.
	conn, err := Dial("dns:///shipping-grpc.shipping.svc.cluster.local:9090")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if conn == nil {
		t.Fatal("Dial returned a nil conn")
	}
	t.Cleanup(func() { _ = conn.Close() })
}

func TestRecovery_UnaryAndStreamRecoverPanics(t *testing.T) {
	logger, h := newLogger()
	unary, stream := recovery(logger)

	resp, err := unary(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"},
		func(context.Context, any) (any, error) { panic("boom") })
	if resp != nil {
		t.Errorf("resp = %v, want nil", resp)
	}
	if status.Code(err) != codes.Internal {
		t.Errorf("unary code = %v, want Internal", status.Code(err))
	}

	err = stream(nil, fakeServerStream{ctx: context.Background()},
		&grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"},
		func(any, grpc.ServerStream) error { panic("boom") })
	if status.Code(err) != codes.Internal {
		t.Errorf("stream code = %v, want Internal", status.Code(err))
	}
	if n := len(h.byMessage("gRPC handler panicked")); n != 2 {
		t.Errorf("want one structured record per panic, got %d", n)
	}

	// A handler that does not panic passes straight through.
	if _, err := unary(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x/y"},
		func(context.Context, any) (any, error) { return "ok", nil }); err != nil {
		t.Errorf("non-panicking handler: %v", err)
	}
}

func hasReflection(srv *grpc.Server) bool {
	for name := range srv.GetServiceInfo() {
		if strings.Contains(name, "ServerReflection") {
			return true
		}
	}
	return false
}

func TestNewServer_ReflectionGating(t *testing.T) {
	// Default: reflection registered.
	def, _ := NewServer(nil)
	t.Cleanup(def.Stop)
	if !hasReflection(def) {
		t.Error("reflection should be registered by default")
	}

	// GRPC_REFLECTION=false: reflection omitted.
	t.Setenv("GRPC_REFLECTION", "false")
	off, _ := NewServer(nil)
	t.Cleanup(off.Stop)
	if hasReflection(off) {
		t.Error("reflection must be omitted when GRPC_REFLECTION=false")
	}
}
