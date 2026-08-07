package httpx

import "testing"

// The Code* constants are a wire contract — clients switch on the strings, so
// a rename or a copy-paste collision is a breaking change. Pin every value:
// the map keys are the constants themselves, so two constants sharing a value
// collapse the map and fail the count check.
func TestErrorCodesAreStableAndDistinct(t *testing.T) {
	pinned := map[string]string{
		CodeValidation:             "VALIDATION_ERROR",
		CodeNotFound:               "NOT_FOUND",
		CodeUnauthorized:           "UNAUTHORIZED",
		CodeForbidden:              "FORBIDDEN",
		CodeConflict:               "CONFLICT",
		CodeInternal:               "INTERNAL_ERROR",
		CodeIdempotencyKeyRequired: "IDEMPOTENCY_KEY_REQUIRED",
		CodeIdempotencyConflict:    "IDEMPOTENCY_CONFLICT",
		CodeInvalidTransition:      "INVALID_TRANSITION",
		CodePaymentExists:          "PAYMENT_EXISTS",
		CodeRefundExceedsCapture:   "REFUND_EXCEEDS_CAPTURE",
		CodePaymentDeclined:        "PAYMENT_DECLINED",
		CodeSessionExpired:         "SESSION_EXPIRED",
		CodePriceChanged:           "PRICE_CHANGED",
		CodeStockUnavailable:       "STOCK_UNAVAILABLE",
		CodePromoInvalid:           "PROMO_INVALID",
		CodePromoExpired:           "PROMO_EXPIRED",
		CodePromoExhausted:         "PROMO_EXHAUSTED",
	}
	if len(pinned) != 18 {
		t.Fatalf("code constants collide: %d distinct values, want 18", len(pinned))
	}
	for got, want := range pinned {
		if got != want {
			t.Errorf("code constant = %q, want %q (renaming breaks clients)", got, want)
		}
	}
}
