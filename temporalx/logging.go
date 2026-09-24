package temporalx

import (
	"context"
	"errors"
	"log/slog"
	"reflect"

	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/client"
	sdklog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
)

// DialOption customizes the client options Dial builds. Options are additive
// and optional — Dial(cfg) keeps the exact prior behavior.
type DialOption func(*client.Options)

// WithLogger routes the Temporal SDK's own log lines (poller lifecycle, task
// failures, worker shutdown) and everything workflow.GetLogger /
// activity.GetLogger write through the service's structured logger instead of
// the SDK default (plain text on stderr). Pass the platform facade's Slog(), so
// the lines are redacted and reach stdout and OTLP like every other record.
//
// Replay safety is the SDK's, and it holds only on that path: workflow code
// must log through workflow.GetLogger, whose replay-aware wrapper drops every
// call made while history is being replayed, so a restarted worker re-running
// a workflow writes nothing a second time and triggers no export. Workflow code
// must never call the facade directly — that bypasses the wrapper. Activities
// run once per attempt and log normally.
//
// The same logger writes temporal.workflow.started for every unambiguous
// client start (see startEvents); pass WithLogger once, or each copy installs
// its own interceptor and the event is written twice.
//
// Correlation: the SDK logs with context.Background(), and its tracing
// interceptor attaches the ids as "TraceID"/"SpanID" attributes instead. The
// logger is wrapped so those attributes become the record's span context —
// the platform handler then stamps trace_id/span_id like any other record —
// rather than a second, differently spelled pair of fields. The interceptor
// passes no trace flags, so the lifted span context reads unsampled; nothing
// filters logs on that flag today, and forcing it would be untrue for an
// unsampled span.
func WithLogger(l *slog.Logger) DialOption {
	if l == nil {
		// Dial rejects a nil option with an actionable error — the same
		// failure class as NewWorker's nil WorkerOption.
		return nil
	}
	return func(o *client.Options) {
		o.Logger = sdklog.NewStructuredLogger(slog.New(spanFromAttrs{next: l.Handler()}))
		o.Interceptors = append(o.Interceptors, &startEvents{log: l})
	}
}

// spanFromAttrs lifts the Temporal tracing interceptor's TraceID/SpanID
// attributes into the record's context. A record whose context already carries
// a span keeps that one.
type spanFromAttrs struct {
	next slog.Handler
	sc   trace.SpanContext
}

func (h spanFromAttrs) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h spanFromAttrs) Handle(ctx context.Context, r slog.Record) error {
	if h.sc.IsValid() && !trace.SpanContextFromContext(ctx).IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, h.sc)
	}
	return h.next.Handle(ctx, withErrorShape(r))
}

// withErrorShape rewrites the SDK's own "Error" attribute (a raw error, on its
// poll-failure and activity-error lines) into the platform's error shape:
// error.type and error.message, the keys slogx.Err writes. The message stays an
// error value, so the facade's redactor still stringifies it — under its own
// recover and typed-nil guard — and bounds it. A non-error value under "Error"
// becomes error.message alone. Records without the key pass through unchanged.
func withErrorShape(r slog.Record) slog.Record {
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "Error" {
			found = true
			return false
		}
		return true
	})
	if !found {
		return r
	}
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(errorShaped(a)...)
		return true
	})
	return out
}

// errorShaped maps one attribute: "Error" becomes error.type + error.message,
// anything else is returned as it is.
func errorShaped(a slog.Attr) []slog.Attr {
	if a.Key != "Error" {
		return []slog.Attr{a}
	}
	err, ok := a.Value.Any().(error)
	if !ok || err == nil {
		return []slog.Attr{slog.Any("error.message", a.Value.Any())}
	}
	return []slog.Attr{slog.String("error.type", sdkErrorType(err)), slog.Any("error.message", err)}
}

// sdkErrorType names an error the way slogx.ErrorType does — pointer dropped,
// the standard library's transparent wrappers (fmt.wrapError, join) seen
// through — so one error reads the same on Temporal lines and service lines.
// This module does not import the facade, so the rule is mirrored here; the one
// addition is a Temporal application error, named by its bounded Type. A
// typed-nil error or one whose method panics is named, never followed.
func sdkErrorType(err error) (name string) {
	defer func() {
		if recover() != nil {
			name = "unknown"
		}
	}()
	for i := 0; err != nil && i < 100; i++ {
		if isTypedNil(err) {
			return errTypeName(err)
		}
		var appErr *temporal.ApplicationError
		if errors.As(err, &appErr) && !isTypedNil(appErr) && appErr.Type() != "" {
			return appErr.Type()
		}
		switch u := err.(type) {
		case interface{ Unwrap() []error }:
			if n := errTypeName(err); n != "errors.joinError" && n != "fmt.wrapErrors" {
				return n
			}
			next := error(nil)
			for _, e := range u.Unwrap() {
				if e != nil {
					next = e
					break
				}
			}
			if next == nil {
				return errTypeName(err)
			}
			err = next
		case interface{ Unwrap() error }:
			if errTypeName(err) != "fmt.wrapError" || u.Unwrap() == nil {
				return errTypeName(err)
			}
			err = u.Unwrap()
		default:
			return errTypeName(err)
		}
	}
	return "unknown"
}

func errTypeName(err error) string {
	t := reflect.TypeOf(err)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.String()
}

func isTypedNil(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

func (h spanFromAttrs) WithAttrs(attrs []slog.Attr) slog.Handler {
	cfg := trace.SpanContextConfig{TraceID: h.sc.TraceID(), SpanID: h.sc.SpanID(), TraceFlags: h.sc.TraceFlags()}
	kept := attrs[:0:0]
	for _, bound := range attrs {
		for _, a := range errorShaped(bound) {
			kept = appendLifted(kept, a, &cfg)
		}
	}
	next := h.next
	if len(kept) > 0 {
		next = next.WithAttrs(kept)
	}
	return spanFromAttrs{next: next, sc: trace.NewSpanContext(cfg)}
}

// appendLifted lifts a TraceID/SpanID attribute into cfg, or keeps it.
func appendLifted(kept []slog.Attr, a slog.Attr, cfg *trace.SpanContextConfig) []slog.Attr {
	switch v := a.Value.Any().(type) {
	case trace.TraceID:
		if a.Key == "TraceID" {
			cfg.TraceID = v
			return kept
		}
	case trace.SpanID:
		if a.Key == "SpanID" {
			cfg.SpanID = v
			return kept
		}
	}
	return append(kept, a)
}

func (h spanFromAttrs) WithGroup(name string) slog.Handler {
	return spanFromAttrs{next: h.next.WithGroup(name), sc: h.sc}
}
