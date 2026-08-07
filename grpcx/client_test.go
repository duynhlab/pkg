package grpcx

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
)

// captureDeadlineInvoker records the deadline the interceptor hands to the
// real invoker.
func captureDeadlineInvoker(deadline *time.Time, ok *bool) grpc.UnaryInvoker {
	return func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		*deadline, *ok = ctx.Deadline()
		return nil
	}
}

func TestDeadlineInterceptor_AppliesDefaultTimeout(t *testing.T) {
	var deadline time.Time
	var ok bool

	err := deadlineInterceptor(context.Background(), "/svc/M", nil, nil, nil,
		captureDeadlineInvoker(&deadline, &ok))
	if err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	if !ok {
		t.Fatal("no deadline applied to a context without one")
	}
	until := time.Until(deadline)
	if until <= 0 || until > DefaultCallTimeout {
		t.Errorf("applied deadline %v from now, want (0, %v]", until, DefaultCallTimeout)
	}
}

func TestDeadlineInterceptor_KeepsCallerDeadline(t *testing.T) {
	callerCtx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	var deadline time.Time
	var ok bool
	err := deadlineInterceptor(callerCtx, "/svc/M", nil, nil, nil,
		captureDeadlineInvoker(&deadline, &ok))
	if err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	if !ok {
		t.Fatal("caller deadline lost")
	}
	if until := time.Until(deadline); until <= DefaultCallTimeout {
		t.Errorf("caller's 1h deadline was tightened to %v", until)
	}
}

func TestPeerAddr(t *testing.T) {
	addr := &net.TCPAddr{IP: net.IPv4(10, 0, 0, 7), Port: 9090}
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
	if got := peerAddr(ctx); got != addr.String() {
		t.Errorf("peerAddr = %q, want %q", got, addr.String())
	}
	if got := peerAddr(context.Background()); got != "" {
		t.Errorf("peerAddr(no peer) = %q, want empty", got)
	}
}

// Every declared reason constant must be classified: business reasons are
// never retryable, transient ones always are — even when the carrying status
// code says otherwise. A new constant left out of both maps silently falls
// back to code-based classification; this table forces the two-map decision.
func TestRetryable_ClassifiesEveryDeclaredReason(t *testing.T) {
	business := []string{
		ReasonValidationError, ReasonNotFound, ReasonSKUNotFound,
		ReasonWarehouseNotFound, ReasonInsufficientStock,
		ReasonIdempotencyConflict, ReasonInvalidTransition, ReasonPaymentDeclined,
	}
	transient := []string{
		ReasonConcurrencyConflict, ReasonDependencyUnavailable, ReasonInternalError,
	}

	for _, r := range business {
		// codes.Unavailable is retryable by code — the business reason must win.
		err := ErrorWithReason(codes.Unavailable, r, "m", nil)
		if Retryable(err) {
			t.Errorf("business reason %s classified retryable", r)
		}
	}
	for _, r := range transient {
		// codes.FailedPrecondition is non-retryable by code — the transient
		// reason must win.
		err := ErrorWithReason(codes.FailedPrecondition, r, "m", nil)
		if !Retryable(err) {
			t.Errorf("transient reason %s classified non-retryable", r)
		}
	}
	// Tripwire on the PRODUCTION maps: a new reason constant classified in
	// businessReasons or transientReasons must also be added to this table.
	if got := len(businessReasons) + len(transientReasons); got != len(business)+len(transient) {
		t.Errorf("classification maps cover %d reasons but this table covers %d — extend the table", got, len(business)+len(transient))
	}
	if ReasonDomain == "" {
		t.Error("ReasonDomain must be non-empty; details are matched by domain")
	}
}

// The stream access-log interceptor tests live in logging_test.go beside the
// unary ones.
