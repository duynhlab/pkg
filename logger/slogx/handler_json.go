package slogx

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Stdout envelope keys. They are the keys every service has emitted since the
// zapx cutover and the keys VictoriaLogs and the runbooks query, so the facade
// keeps them verbatim (ADR-071: the envelope is unchanged).
const (
	keyTimestamp = "timestamp"
	keyLevel     = "level"
	keyMessage   = "message"
	keyCaller    = "caller"
	keyTraceID   = "trace_id"
	keySpanID    = "span_id"
)

// timestampLayout is the shape zap's ISO8601 encoder produced, rendered in
// UTC, kept so a stored query written against the zapx era parses the slogx
// era: "2026-07-09T02:12:04.455Z".
const timestampLayout = "2006-01-02T15:04:05.000Z07:00"

// newStdoutHandler builds the JSON handler behind the facade. Level gating,
// source capture and the envelope renames all live in slog's own handler
// options; only the trace correlation needs a wrapper (traceHandler), because
// the ids come from the context and ReplaceAttr never sees the context.
func newStdoutHandler(w io.Writer, level slog.Leveler, addSource bool) slog.Handler {
	return traceHandler{level: level, next: slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		AddSource:   addSource,
		ReplaceAttr: replaceEnvelope,
	})}
}

// replaceEnvelope renames slog's built-in keys to the platform envelope and
// renders their values the way zapx did. It touches only top-level built-ins:
// a user attribute that happens to be called "time" inside a group is left
// alone, and a user attribute called "time" at the top level is a key
// collision the caller owns, exactly as it was with zap.
func replaceEnvelope(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	// The type assertions always hold for slog's own JSON handler (time.Time,
	// slog.Level, *slog.Source — verified against the Go 1.26 handler); the
	// rename-only fallbacks keep the envelope keys stable should that change.
	switch a.Key {
	case slog.TimeKey:
		if t, ok := a.Value.Any().(time.Time); ok {
			return slog.String(keyTimestamp, t.UTC().Format(timestampLayout))
		}
		a.Key = keyTimestamp
	case slog.LevelKey:
		if l, ok := a.Value.Any().(slog.Level); ok {
			return slog.String(keyLevel, levelName(l))
		}
		a.Key = keyLevel
	case slog.MessageKey:
		a.Key = keyMessage
	case slog.SourceKey:
		if src, ok := a.Value.Any().(*slog.Source); ok {
			if src == nil || src.File == "" {
				// A record built with PC 0 (an SDK bridge constructing its own
				// records) has no location; the contract says "when enabled",
				// not ":0".
				return slog.Attr{}
			}
			return slog.String(keyCaller, shortCaller(src))
		}
		a.Key = keyCaller
	}
	return a
}

// shortCaller renders a source location as zap's short caller did:
// "<parent dir>/<file>.go:<line>", enough to find the line without leaking
// the build machine's paths into every record.
func shortCaller(src *slog.Source) string {
	dir, file := filepath.Split(src.File)
	parent := filepath.Base(strings.TrimSuffix(dir, string(filepath.Separator)))
	if parent == "." || parent == string(filepath.Separator) || parent == "" {
		return file + ":" + strconv.Itoa(src.Line)
	}
	return parent + "/" + file + ":" + strconv.Itoa(src.Line)
}

// traceHandler stamps trace_id and span_id from the active span onto the
// record before the JSON handler renders it. The ids are added only when a
// valid span context exists (the contract makes them conditional), and never
// when the caller passed a context without one — no empty strings.
//
// It also owns the level gate: one Leveler, shared by the Logger and every
// child, decides for every sink. When the OTLP sink joins as a fanout, its
// Enabled must not be OR-ed in — the gate stays here, above the fanout, so a
// debug line an info-level service suppresses on stdout never leaves the pod
// over OTLP either.
type traceHandler struct {
	level slog.Leveler
	next  slog.Handler
}

func (h traceHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			// Copies of a Record share their attribute backing array; a
			// handler that appends must Clone first or the next sink in a
			// fanout sees this sink's attributes (slog inserts a "!BUG" attr
			// when it detects it). Clone is a slice clip — no allocation.
			r = r.Clone()
			r.AddAttrs(
				slog.String(keyTraceID, sc.TraceID().String()),
				slog.String(keySpanID, sc.SpanID().String()),
			)
		}
	}
	return h.next.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{level: h.level, next: h.next.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{level: h.level, next: h.next.WithGroup(name)}
}
