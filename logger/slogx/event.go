package slogx

import (
	"context"
	"log/slog"
)

// Event keys. "event" carries the catalog name (RFC-0031 § Event catalog,
// ADR-076); "event.invalid" carries a name that failed the grammar, so the
// record is still emitted and the mistake is visible where it happened
// instead of vanishing.
const (
	keyEvent        = "event"
	keyEventInvalid = "event.invalid"
	// conflictSuffix renames a caller's own "event" attribute: two keys of
	// the same name would be last-wins in every consumer, which would let a
	// request-derived attribute overwrite the catalog name.
	conflictSuffix = ".conflict"
	maxEventName   = 64
)

// Event emits a named record from the reviewed catalog. The name is the
// domain-scoped, dot-separated identifier the catalog registers
// ("order.confirmed", "checkout.session_expired"): lowercase ASCII letters,
// digits and underscores in each segment, a letter first, at least two
// segments, at most 64 bytes. Only Event may set the "event" attribute; a
// plain Info with slog.String("event", …) is not a catalog event and the
// registry lint will flag it. The one carve-out is temporalx, which owns the
// two temporal.workflow.* names and carries events written from workflow code
// (WorkflowEvent, through the SDK's replay-aware logger); it writes the key
// directly so the module does not depend on this facade, and the lint must
// allowlist it.
//
// A name that fails the grammar is not silently dropped and not silently
// emitted as an event: the record goes out at the requested level with
// "event.invalid" set to the (bounded) name instead of "event", so a
// misspelt catalog name shows up in the very stream an operator reads.
func (l *Logger) Event(ctx context.Context, level slog.Level, name, msg string, attrs ...slog.Attr) {
	l.event(ctx, 4, level, name, msg, attrs)
}

// event is Event with the runtime.Callers skip of its entry point: skip
// Callers, logAt, event and the exported method, so the record names the
// method's caller, never this package.
func (l *Logger) event(ctx context.Context, skip int, level slog.Level, name, msg string, attrs []slog.Attr) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !l.h.Enabled(ctx, level) {
		return
	}
	tag := slog.String(keyEvent, name)
	if !ValidEventName(name) {
		tag = slog.String(keyEventInvalid, bound(name, maxEventName))
	}
	out := make([]slog.Attr, 0, len(attrs)+1)
	out = append(out, tag)
	for _, a := range attrs {
		if a.Key == keyEvent || a.Key == keyEventInvalid {
			a.Key += conflictSuffix
		}
		out = append(out, a)
	}
	l.logAt(ctx, skip, level, msg, out)
}

// ValidEventName reports whether name satisfies the catalog grammar:
// segments of [a-z][a-z0-9_]* joined by single dots, at least two segments,
// at most 64 bytes.
func ValidEventName(name string) bool {
	if len(name) > maxEventName {
		return false
	}
	segments := 1
	start := true // at the first byte of a segment
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '.':
			if start {
				return false // empty segment: leading, trailing or double dot
			}
			segments++
			start = true
		case c >= 'a' && c <= 'z':
			start = false
		case (c >= '0' && c <= '9' || c == '_') && !start:
		default:
			return false
		}
	}
	return !start && segments >= 2
}
