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
	srv, hs := NewServer()
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

func TestRecoveryUnary_RecoversPanic(t *testing.T) {
	panicking := func(context.Context, any) (any, error) { panic("boom") }
	resp, err := recoveryUnary(
		context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"},
		panicking,
	)
	if resp != nil {
		t.Errorf("resp = %v, want nil", resp)
	}
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
	}
}

func TestRecoveryStream_RecoversPanic(t *testing.T) {
	panicking := func(any, grpc.ServerStream) error { panic("boom") }
	err := recoveryStream(
		nil, nil,
		&grpc.StreamServerInfo{FullMethod: "/test.Service/Stream"},
		panicking,
	)
	if status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
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
	def, _ := NewServer()
	t.Cleanup(def.Stop)
	if !hasReflection(def) {
		t.Error("reflection should be registered by default")
	}

	// GRPC_REFLECTION=false: reflection omitted.
	t.Setenv("GRPC_REFLECTION", "false")
	off, _ := NewServer()
	t.Cleanup(off.Stop)
	if hasReflection(off) {
		t.Error("reflection must be omitted when GRPC_REFLECTION=false")
	}
}
