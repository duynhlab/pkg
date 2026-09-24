package slogx

import (
	"log/slog"
	"reflect"
)

// Error attribute keys. RFC-0031 § Canonical attributes asks a failing record
// for `error.type` — a bounded, low-cardinality label safe as a metric or
// dashboard dimension — beside a safe message. They are flat keys, not a
// nested group: the platform's log store indexes attributes as a string map
// (`LogAttributes['error.type']`), a group would arrive there as one JSON
// blob under `error`, and `error.type` is also the key the matching span
// carries, so log and span correlate on the same name.
//
// The text goes under exception.message, the stable upstream key for the
// message of an error a record reports: semantic conventions v1.41 deprecated
// error.message ("use a domain-specific attribute"), and the platform's
// registry check treats a deprecated key as a violation.
const (
	keyErrorType    = "error.type"
	keyErrorMessage = "exception.message"
)

// Err is the one shape for an error on a record: the zap.Error(err) of this
// facade. It records the error's TYPE as the stable label and its text as the
// message, which the redaction boundary scans and bounds like any other value
// — an error string routinely carries a DSN, a URL with credentials or a
// payload echo.
//
//	log.Error(ctx, "could not reserve stock", slogx.Err(err),
//	    slog.String("order_id", id))
//
// Both keys are emitted through an inlined (empty-key) group, so they land at
// the top level of the record. A nil error — including a typed nil, the
// interface holding a nil pointer that an "if err != nil" has already let
// through — yields an empty group, which slog drops, so a caller need not
// branch on it.
func Err(err error) slog.Attr {
	if err == nil || isTypedNil(err) {
		return slog.Attr{Value: slog.GroupValue()}
	}
	return slog.Attr{Value: slog.GroupValue(
		slog.String(keyErrorType, ErrorType(err)),
		// Err is evaluated at the call site, outside every recover the
		// handlers install, so a panicking Error() is caught here.
		slog.String(keyErrorMessage, safeError(err)),
	)}
}

// ErrorType names the error for `error.type`: the concrete Go type of the
// deepest error that is not one of the standard library's own transparent
// wrappers, with the pointer star dropped. Unwrapping those wrappers is what
// keeps the label stable — otherwise every error that passed through
// fmt.Errorf("%w") would report *fmt.wrapError and the label would say
// nothing about what failed. An aggregate names itself unless it is one of
// the standard library's, in which case the first cause is named.
//
// It never renders the error's message, so the label stays low-cardinality
// and free of request data. Note that every errors.New sentinel reports
// errors.errorString: a domain that wants its errors to be distinguishable
// here declares error types rather than sentinel values.
func ErrorType(err error) string {
	for i := 0; err != nil && i < maxUnwrapDepth; i++ {
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			if !isStdWrapper(err) {
				return typeName(err)
			}
			inner := u.Unwrap()
			if inner == nil {
				return typeName(err)
			}
			err = inner
		case interface{ Unwrap() []error }:
			// A service's own aggregate (type ValidationErrors []error) is
			// the informative name; the standard library's join and multi-%w
			// wrapper are not, so their first cause is named instead.
			if n := typeName(err); n != "errors.joinError" && n != "fmt.wrapErrors" {
				return n
			}
			for _, e := range u.Unwrap() {
				if e != nil {
					return ErrorType(e)
				}
			}
			return typeName(err)
		default:
			return typeName(err)
		}
	}
	return ""
}

// maxUnwrapDepth bounds a chain that unwraps into itself. A real chain is a
// handful deep; this only has to stop a pathological one.
const maxUnwrapDepth = 100

// isStdWrapper reports the standard library's own transparent wrappers, the
// ones that carry no information of their own.
func isStdWrapper(err error) bool {
	return typeName(err) == "fmt.wrapError"
}

// typeName is the error's concrete type with pointers dropped, rendered with
// the short package name (reflect.Type.String) — never the full import path,
// which would put build-time module layout into every record.
func typeName(err error) string {
	t := reflect.TypeOf(err)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.String()
}
