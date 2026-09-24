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
// error.type — the bounded application-error type when there is one, else the
// Go type — and error.message. A raw error string under a key the facade does
// not know would otherwise reach both sinks untouched. Records without it pass
// through unchanged.
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
		if a.Key != "Error" {
			out.AddAttrs(a)
			return true
		}
		err, ok := a.Value.Any().(error)
		if !ok || err == nil {
			out.AddAttrs(slog.String("error.message", a.Value.String()))
			return true
		}
		out.AddAttrs(slog.String("error.type", sdkErrorType(err)), slog.String("error.message", err.Error()))
		return true
	})
	return out
}

func sdkErrorType(err error) string {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.Type() != "" {
		return appErr.Type()
	}
	return reflect.TypeOf(err).String()
}

func (h spanFromAttrs) WithAttrs(attrs []slog.Attr) slog.Handler {
	cfg := trace.SpanContextConfig{TraceID: h.sc.TraceID(), SpanID: h.sc.SpanID(), TraceFlags: h.sc.TraceFlags()}
	kept := attrs[:0:0]
	for _, a := range attrs {
		switch v := a.Value.Any().(type) {
		case trace.TraceID:
			if a.Key == "TraceID" {
				cfg.TraceID = v
				continue
			}
		case trace.SpanID:
			if a.Key == "SpanID" {
				cfg.SpanID = v
				continue
			}
		}
		kept = append(kept, a)
	}
	next := h.next
	if len(kept) > 0 {
		next = next.WithAttrs(kept)
	}
	return spanFromAttrs{next: next, sc: trace.NewSpanContext(cfg)}
}

func (h spanFromAttrs) WithGroup(name string) slog.Handler {
	return spanFromAttrs{next: h.next.WithGroup(name), sc: h.sc}
}
