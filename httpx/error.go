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
