package slogx

import (
	"fmt"
	"log/slog"
	"reflect"
)

// Error attribute keys. RFC-0031 § Canonical attributes asks a failing record
// for `error.type` — a bounded, low-cardinality label safe as a metric or
// dashboard dimension — beside a safe message. Both live under one "error"
// group, so the stdout envelope renders them as the dotted fields the stored
// queries use (`error.type`, `error.message`) and the OTLP record carries them
// as one map attribute.
const (
	keyError        = "error"
	keyErrorType    = "type"
	keyErrorMessage = "message"
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
// A nil error yields an empty attribute, which slog drops, so a caller need
// not branch on err == nil.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.Group(keyError,
		slog.String(keyErrorType, ErrorType(err)),
		slog.String(keyErrorMessage, err.Error()),
	)
}

// ErrorType names the error for `error.type`: the concrete Go type of the
// deepest error that is not one of the standard library's own wrappers, with
// the pointer star dropped. Unwrapping those wrappers is what keeps the label
// stable — otherwise every error that passed through fmt.Errorf("%w") would
// report *fmt.wrapError and the label would say nothing about what failed.
//
// It never renders the error's message, so the label stays low-cardinality
// and free of request data.
func ErrorType(err error) string {
	if err == nil {
		return ""
	}
	for {
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
			// errors.Join: the group has no single type; name the join.
			return "errors.joinError"
		default:
			return typeName(err)
		}
	}
}

// isStdWrapper reports the standard library's own transparent wrappers, the
// ones that carry no information of their own.
func isStdWrapper(err error) bool {
	switch typeName(err) {
	case "fmt.wrapError", "fmt.wrapErrors":
		return true
	}
	return false
}

func typeName(err error) string {
	t := reflect.TypeOf(err)
	if t == nil {
		return fmt.Sprintf("%T", err)
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.PkgPath() == "" {
		return t.String()
	}
	return t.String()
}
