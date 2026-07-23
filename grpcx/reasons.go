package grpcx

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReasonDomain is the google.rpc.ErrorInfo.Domain for all platform reasons.
const ReasonDomain = "duynhlab.dev"

// Machine-readable gRPC error reasons, carried as a google.rpc.ErrorInfo
// detail on the status. These are a stable contract — callers (including the
// Temporal saga's retryable/non-retryable mapping) switch on them, so treat
// renames as breaking changes. They mirror the httpx code contract where the
// same condition exists on both transports. INSUFFICIENT_STOCK is deliberately
// distinct from httpx's STOCK_UNAVAILABLE: the former is the inventory
// reservation authority rejecting a reserve; the latter is checkout's
// confirm-time re-validation of a session snapshot.
const (
	ReasonValidationError       = "VALIDATION_ERROR"
	ReasonNotFound              = "NOT_FOUND"
	ReasonSKUNotFound           = "SKU_NOT_FOUND"
	ReasonWarehouseNotFound     = "WAREHOUSE_NOT_FOUND"
	ReasonInsufficientStock     = "INSUFFICIENT_STOCK"
	ReasonIdempotencyConflict   = "IDEMPOTENCY_CONFLICT"
	ReasonInvalidTransition     = "INVALID_TRANSITION"
	ReasonConcurrencyConflict   = "CONCURRENCY_CONFLICT"
	ReasonPaymentDeclined       = "PAYMENT_DECLINED"
	ReasonDependencyUnavailable = "DEPENDENCY_UNAVAILABLE"
	ReasonInternalError         = "INTERNAL_ERROR"
)

// businessReasons are definite rejections of the request itself: retrying the
// identical call cannot succeed, so workflow callers map them to non-retryable
// errors. transientReasons are safe to retry. Adding a reason is a deliberate
// two-map decision; a reason in neither map (version skew — a newer server
// emitting a reason this pkg predates) falls back to the status code, which
// the same server set and which classifies correctly either way.
var businessReasons = map[string]bool{
	ReasonValidationError:     true,
	ReasonNotFound:            true,
	ReasonSKUNotFound:         true,
	ReasonWarehouseNotFound:   true,
	ReasonInsufficientStock:   true,
	ReasonIdempotencyConflict: true,
	ReasonInvalidTransition:   true,
	ReasonPaymentDeclined:     true,
}

var transientReasons = map[string]bool{
	ReasonConcurrencyConflict:   true,
	ReasonDependencyUnavailable: true,
	ReasonInternalError:         true,
}

// ErrorWithReason builds a gRPC status error carrying a google.rpc.ErrorInfo
// detail with the given reason, ReasonDomain, and optional metadata (bounded
// business identifiers only — never secrets or free-form text). msg must be
// client-safe: it can surface in HTTP translations, logs, and Temporal
// workflow history, so no internal errors, connection strings, or PII.
// The detail survives the wire, so clients recover the reason with Reason.
// codes.OK is a caller bug and returns nil (a status with code OK is no
// error); don't call this on success paths.
func ErrorWithReason(c codes.Code, reason, msg string, metadata map[string]string) error {
	st := status.New(c, msg)
	withInfo, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason:   reason,
		Domain:   ReasonDomain,
		Metadata: metadata,
	})
	if err != nil {
		// WithDetails fails only for codes.OK; degrade to the bare status
		// (nil for OK) rather than masking the original error.
		return st.Err()
	}
	return withInfo.Err()
}

// Reason extracts the platform error reason from a (possibly wrapped) gRPC
// status error. It returns "" when the error carries no ErrorInfo detail in
// ReasonDomain — including nil errors and plain statuses.
func Reason(err error) string {
	st, ok := status.FromError(err)
	if !ok || st == nil {
		return ""
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == ReasonDomain {
			return info.GetReason()
		}
	}
	return ""
}

// Retryable reports whether the caller should retry the identical call.
// A business reason is never retryable; a transient reason always is; an
// unrecognized reason (or none) falls back to the status code: transient
// classes (Unavailable, DeadlineExceeded, ResourceExhausted, Aborted,
// Internal, Unknown) retry; definite rejections do not. A nil error is not
// retryable. Internal/Unknown retrying means deterministic bugs are retried
// too — callers MUST pair this with a bounded, backoff-enabled retry policy
// (e.g. Temporal RetryPolicy with MaximumAttempts) and idempotent callees.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if r := Reason(err); r != "" {
		if businessReasons[r] {
			return false
		}
		if transientReasons[r] {
			return true
		}
		// Unknown reason: fall through to the status code.
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted,
		codes.Aborted, codes.Internal, codes.Unknown:
		return true
	default:
		return false
	}
}
