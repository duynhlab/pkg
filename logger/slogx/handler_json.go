package slogx

import (
	"context"
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
// when the caller passed a context without one — no empty strings. It wraps
// the stdout sink only: the OTLP bridge reads the span context itself and
// carries the ids in the record's own fields. The level gate lives above the
// fanout (levelHandler); this handler's Enabled defers to its sink.
//
// Groups are held here rather than forwarded to the JSON handler. Forwarding
// them would nest everything this handler appends — the trace ids included —
// inside the open group, and trace_id is an envelope key the stored queries
// read at the top level. So once a group is open, the groups and the
// attributes bound after them are remembered in order and rebuilt as one
// nested attribute at Handle time, while the ids stay outside it. Before the
// first group nothing is deferred: attributes are preformatted by the JSON
// handler as usual.
type traceHandler struct {
	next slog.Handler
	// goas records WithGroup and WithAttrs calls in the order they were
	// made; each element is either a group name or a batch of attributes
	// bound inside the groups opened so far.
	goas []groupOrAttrs
}

type groupOrAttrs struct {
	group string
	attrs []slog.Attr
}

func (h traceHandler) with(g groupOrAttrs) traceHandler {
	goas := make([]groupOrAttrs, len(h.goas)+1)
	copy(goas, h.goas)
	goas[len(h.goas)] = g
	return traceHandler{next: h.next, goas: goas}
}

func (h traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	var ids [2]slog.Attr
	n := 0
	if ctx != nil {
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			ids[0] = slog.String(keyTraceID, sc.TraceID().String())
			ids[1] = slog.String(keySpanID, sc.SpanID().String())
			n = 2
		}
	}
	if len(h.goas) == 0 {
		if n == 0 {
			return h.next.Handle(ctx, r)
		}
		// Copies of a Record share their attribute backing array; a handler
		// that appends must Clone first or the next sink in a fanout sees
		// this sink's attributes (slog inserts a "!BUG" attr when it detects
		// it). Clone is a slice clip — no allocation.
		r = r.Clone()
		r.AddAttrs(ids[:n]...)
		return h.next.Handle(ctx, r)
	}
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	out.AddAttrs(ids[:n]...)
	own := make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool { own = append(own, a); return true })
	out.AddAttrs(h.rebuild(own)...)
	return h.next.Handle(ctx, out)
}

// rebuild folds the remembered groups and attributes around the record's own
// attributes, innermost first, into the attribute list the sink receives.
func (h traceHandler) rebuild(own []slog.Attr) []slog.Attr {
	cur := own
	for i := len(h.goas) - 1; i >= 0; i-- {
		if g := h.goas[i]; g.group != "" {
			if len(cur) == 0 {
				continue // slog drops empty groups; so do we
			}
			cur = []slog.Attr{{Key: g.group, Value: slog.GroupValue(cur...)}}
		} else {
			cur = append(append([]slog.Attr{}, g.attrs...), cur...)
		}
	}
	return cur
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	if len(h.goas) == 0 {
		// No group is open, so the sink can preformat them as usual.
		return traceHandler{next: h.next.WithAttrs(attrs)}
	}
	return h.with(groupOrAttrs{attrs: attrs})
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h // the slog.Handler contract: an empty name is a no-op
	}
	return h.with(groupOrAttrs{group: name})
}
