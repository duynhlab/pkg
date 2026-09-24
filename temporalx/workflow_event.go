package temporalx

import (
	"log/slog"
	"regexp"
	"strings"
	"unicode/utf8"

	sdklog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/workflow"
)

// eventNamePattern is the catalog grammar (RFC-0031 § Event catalog): dotted
// lowercase segments of [a-z][a-z0-9_]*, at least two, at most 64 bytes.
var eventNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

const maxEventName = 64

// WorkflowEvent emits a catalog event from WORKFLOW code — a saga outcome, a
// compensation step, a retry that gave up — where only the SDK's replay-aware
// logger may run. It writes through workflow.GetLogger, so a replayed history
// writes nothing and a restarted worker never emits the event twice; the
// logger WithLogger installed carries it to the platform handler. Like the
// workflow-lifecycle events, it writes the "event" key directly because this
// module does not depend on the logging facade.
//
// A name outside the grammar is written under "event.invalid" instead,
// bounded as the facade bounds it, and a caller's own "event" attribute is
// renamed. The SDK logger has no level parameter, so a non-standard level is
// rounded down to Debug, Info, Warn or Error. Replay safety holds while
// worker.Options.EnableLoggingInReplay stays false (the default) and no
// workflow interceptor replaces GetLogger with a logger that is not
// replay-aware. Activities and ordinary code use the facade's Event, not this.
func WorkflowEvent(ctx workflow.Context, level slog.Level, name, msg string, attrs ...slog.Attr) {
	key := "event"
	if len(name) > maxEventName || !eventNamePattern.MatchString(name) {
		key = "event.invalid"
		name = boundName(name, maxEventName)
	}
	kv := make([]any, 0, len(attrs)+1)
	kv = append(kv, slog.String(key, name))
	for _, a := range attrs {
		if a.Key == "event" || a.Key == "event.invalid" {
			a.Key += ".conflict"
		}
		kv = append(kv, a)
	}
	// Skip one frame so the record's source is WorkflowEvent's caller.
	l := sdklog.Skip(workflow.GetLogger(ctx), 1)
	switch {
	case level >= slog.LevelError:
		l.Error(msg, kv...)
	case level >= slog.LevelWarn:
		l.Warn(msg, kv...)
	case level >= slog.LevelInfo:
		l.Info(msg, kv...)
	default:
		l.Debug(msg, kv...)
	}
}

// boundName repairs invalid UTF-8 and cuts s to at most limit bytes on a rune
// boundary with a marker — the facade's bound, copied because this module does
// not import it, so an event.invalid value reads the same on both paths.
func boundName(s string, limit int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…(truncated)"
}
