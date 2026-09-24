package slogx

import (
	"context"
	"log/slog"
)

// Process lifecycle vocabulary (RFC-0031 § Event catalog, Startup/shutdown).
const (
	ComponentAPI     = "api"
	ComponentWorker  = "worker"
	ComponentMockpay = "mockpay"

	OutcomeGraceful = "graceful"
	OutcomeError    = "error"

	// invalidValue replaces a component or outcome outside the catalog's
	// bounded set: the record still goes out, and the value a caller got
	// wrong cannot grow the label set of anything that groups by it.
	invalidValue = "invalid"
)

// ProcessStarted emits process.started when an entry point is ready to
// serve. component is ComponentAPI, ComponentWorker or ComponentMockpay.
func (l *Logger) ProcessStarted(ctx context.Context, component string) {
	l.Event(ctx, slog.LevelInfo, "process.started", "process started",
		slog.String("component", bounded(component, ComponentAPI, ComponentWorker, ComponentMockpay)))
}

// ProcessStopped emits process.stopped when an entry point has finished
// shutting down. outcome is OutcomeGraceful or OutcomeError; an error
// shutdown is written at Error so it reaches the streams an operator watches.
func (l *Logger) ProcessStopped(ctx context.Context, component, outcome string) {
	outcome = bounded(outcome, OutcomeGraceful, OutcomeError)
	level := slog.LevelInfo
	if outcome != OutcomeGraceful {
		level = slog.LevelError
	}
	l.Event(ctx, level, "process.stopped", "process stopped",
		slog.String("component", bounded(component, ComponentAPI, ComponentWorker, ComponentMockpay)),
		slog.String("outcome", outcome))
}

func bounded(v string, allowed ...string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return invalidValue
}
