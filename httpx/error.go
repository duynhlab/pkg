// Package httpx provides shared HTTP response helpers (a consistent error
// envelope and a list-pagination envelope) for the duynhlab microservices.
//
// The error envelope is intentionally *additive*: it keeps the human-readable
// "error" string that existing clients already depend on and adds a stable,
// machine-readable "code". This avoids a breaking change to the v1 contract
// while giving clients something safe to branch on (string messages are not).
package httpx

import "github.com/gin-gonic/gin"

// Machine-readable error codes. These are a stable contract — clients may switch
// on them, so treat renames as breaking changes. Add new codes as needed.
const (
	CodeValidation   = "VALIDATION_ERROR"
	CodeNotFound     = "NOT_FOUND"
	CodeUnauthorized = "UNAUTHORIZED"
	CodeForbidden    = "FORBIDDEN"
	CodeConflict     = "CONFLICT"
	CodeInternal     = "INTERNAL_ERROR"

	// Payment codes (RFC-0010). PAYMENT_DECLINED is returned with HTTP 422 —
	// the request is semantically valid but the provider declined it; 422 is
	// deliberately new to the platform (the documented set previously stopped
	// at 400/401/403/404/409/500).
	CodeIdempotencyKeyRequired = "IDEMPOTENCY_KEY_REQUIRED" // 400: Idempotency-Key header missing
	CodeIdempotencyConflict    = "IDEMPOTENCY_CONFLICT"     // 409: same key, different request hash
	CodeInvalidTransition      = "INVALID_TRANSITION"       // 409: state machine rejected the move
	CodePaymentExists          = "PAYMENT_EXISTS"           // 409: order already has a payment
	CodeRefundExceedsCapture   = "REFUND_EXCEEDS_CAPTURE"   // 409: refunds would exceed captured amount
	CodePaymentDeclined        = "PAYMENT_DECLINED"         // 422: provider declined the charge
)

// RespondError writes the standard error envelope:
//
//	{"error": "<message>", "code": "<CODE>"}
//
// The "error" field remains for backward compatibility; "code" is the stable,
// machine-readable signal. Callers should return immediately after calling it.
func RespondError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": message, "code": code})
}
