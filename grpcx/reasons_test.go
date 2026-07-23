package grpcx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestReasonRoundTripInProcess(t *testing.T) {
	err := ErrorWithReason(codes.FailedPrecondition, ReasonInsufficientStock,
		"2 of 5 requested units available", map[string]string{"sku_id": "1001"})

	if got := Reason(err); got != ReasonInsufficientStock {
		t.Fatalf("Reason = %q, want %q", got, ReasonInsufficientStock)
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	wrapped := fmt.Errorf("reserve failed: %w", err)
	if got := Reason(wrapped); got != ReasonInsufficientStock {
		t.Fatalf("Reason(wrapped) = %q, want %q", got, ReasonInsufficientStock)
	}
}

func TestErrorWithReasonOKIsNil(t *testing.T) {
	// codes.OK is a caller bug; the constructor documents nil, pin it.
	if err := ErrorWithReason(codes.OK, ReasonInternalError, "not an error", nil); err != nil {
		t.Fatalf("ErrorWithReason(OK) = %v, want nil", err)
	}
}

func TestReasonAbsent(t *testing.T) {
	cases := map[string]error{
		"nil":          nil,
		"plain status": status.Error(codes.NotFound, "nope"),
		"non-status":   errors.New("boom"),
	}
	for name, err := range cases {
		if got := Reason(err); got != "" {
			t.Errorf("%s: Reason = %q, want empty", name, got)
		}
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"business reason wins over transient code",
			ErrorWithReason(codes.Unavailable, ReasonPaymentDeclined, "declined", nil), false},
		{"insufficient stock", ErrorWithReason(codes.FailedPrecondition, ReasonInsufficientStock, "short", nil), false},
		{"idempotency conflict", ErrorWithReason(codes.AlreadyExists, ReasonIdempotencyConflict, "conflict", nil), false},
		{"concurrency conflict retries", ErrorWithReason(codes.Aborted, ReasonConcurrencyConflict, "cas", nil), true},
		{"dependency unavailable retries", ErrorWithReason(codes.Unavailable, ReasonDependencyUnavailable, "db down", nil), true},
		{"unknown reason falls back to code (rejection)",
			ErrorWithReason(codes.InvalidArgument, "QUOTA_EXCEEDED_V2", "future reason", nil), false},
		{"unknown reason falls back to code (transient)",
			ErrorWithReason(codes.Unavailable, "QUOTA_EXCEEDED_V2", "future reason", nil), true},
		{"no reason, unavailable", status.Error(codes.Unavailable, "conn refused"), true},
		{"no reason, invalid argument", status.Error(codes.InvalidArgument, "bad"), false},
		{"non-status error maps to Unknown", errors.New("boom"), true},
	}
	for _, tc := range cases {
		if got := Retryable(tc.err); got != tc.want {
			t.Errorf("%s: Retryable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// failingHealth returns a reasoned error from a real handler so the test
// proves the ErrorInfo detail survives the wire, not just in-process casting.
type failingHealth struct {
	healthpb.UnimplementedHealthServer
}

func (failingHealth) Check(context.Context, *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	return nil, ErrorWithReason(codes.FailedPrecondition, ReasonInvalidTransition,
		"reservation already committed", map[string]string{"reservation_id": "order-42"})
}

func TestReasonSurvivesWire(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, failingHealth{})
	go srv.Serve(lis) //nolint:errcheck // closed by srv.Stop
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	_, err = healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err == nil {
		t.Fatal("expected error from Check")
	}
	if got := Reason(err); got != ReasonInvalidTransition {
		t.Fatalf("Reason over wire = %q, want %q", got, ReasonInvalidTransition)
	}
	if Retryable(err) {
		t.Fatal("INVALID_TRANSITION must not be retryable")
	}
}
