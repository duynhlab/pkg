package temporalx

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/client"
	sdklog "go.temporal.io/sdk/log"
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
	return h.next.Handle(ctx, r)
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
