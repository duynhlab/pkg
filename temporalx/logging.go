package temporalx

import (
	"log/slog"

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
func WithLogger(l *slog.Logger) DialOption {
	if l == nil {
		// Dial rejects a nil option with an actionable error — the same
		// failure class as NewWorker's nil WorkerOption.
		return nil
	}
	return func(o *client.Options) {
		o.Logger = sdklog.NewStructuredLogger(l)
	}
}
