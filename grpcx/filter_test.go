package grpcx

import (
	"context"
	"net"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/stats"
)

func TestTelemetryFilter_Semantics(t *testing.T) {
	cases := []struct {
		fullMethod string
		want       bool
	}{
		{"/grpc.health.v1.Health/Check", false},
		{"/grpc.health.v1.Health/Watch", false},
		{"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo", false},
		{"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo", false},
		{"/shipping.v1.ShippingService/CreateShipment", true},
		{"/payment.v1.PaymentService/Authorize", true},
	}
	for _, tc := range cases {
		t.Run(tc.fullMethod, func(t *testing.T) {
			got := telemetryFilter(&stats.RPCTagInfo{FullMethodName: tc.fullMethod})
			if got != tc.want {
				t.Errorf("telemetryFilter(%q) = %v, want %v (true = instrumented)", tc.fullMethod, got, tc.want)
			}
		})
	}
}

func TestTelemetryFilter_HealthProducesNoSpans(t *testing.T) {
	// Wire an in-memory recorder as the global TracerProvider — the otelgrpc
	// stats handlers resolve the global lazily per RPC.
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	srv, _ := NewServer()
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
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("health check over the wire: %v", err)
	}

	_ = tp.ForceFlush(ctx)
	for _, span := range recorder.Ended() {
		t.Errorf("health RPC produced span %q — telemetryFilter is not attached", span.Name())
	}
}
