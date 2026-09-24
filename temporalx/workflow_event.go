package temporalx

import (
	"log/slog"
	"regexp"

	"go.temporal.io/sdk/workflow"
)

// eventNamePattern is the catalog grammar (RFC-0031 § Event catalog): dotted
// lowercase segments of [a-z][a-z0-9_]*, at least two, at most 64 bytes.
var eventNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// WorkflowEvent emits a catalog event from WORKFLOW code — a saga outcome, a
// compensation step, a retry that gave up — where only the SDK's replay-aware
// logger may run. It writes through workflow.GetLogger, so a replayed history
// writes nothing and a restarted worker never emits the event twice; the
// logger WithLogger installed carries it to the platform handler. Like the
// workflow-lifecycle events, it writes the "event" key directly because this
// module does not depend on the logging facade.
//
// A name outside the grammar is written under "event.invalid" instead, and a
// caller's own "event" attribute is renamed, as the facade does. Activities
// and ordinary code use the facade's Event, not this.
func WorkflowEvent(ctx workflow.Context, level slog.Level, name, msg string, attrs ...slog.Attr) {
	key := "event"
	if len(name) > 64 || !eventNamePattern.MatchString(name) {
		key = "event.invalid"
		if len(name) > 64 {
			name = name[:64]
		}
	}
	kv := make([]any, 0, len(attrs)+1)
	kv = append(kv, slog.String(key, name))
	for _, a := range attrs {
		if a.Key == "event" || a.Key == "event.invalid" {
			a.Key += ".conflict"
		}
		kv = append(kv, a)
	}
	l := workflow.GetLogger(ctx)
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
