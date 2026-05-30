package grpcx

import (
	"context"
	"testing"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"
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
