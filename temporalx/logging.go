package temporalx

import (
	"log/slog"

	"go.temporal.io/sdk/client"
	sdklog "go.temporal.io/sdk/log"
	"go.uber.org/zap"
	"go.uber.org/zap/exp/zapslog"
)

// DialOption customizes the client options Dial builds. Options are additive
// and optional — Dial(cfg) keeps the exact prior behavior.
type DialOption func(*client.Options)

// WithLogger routes the Temporal SDK's own log lines (poller lifecycle, task
// failures, worker shutdown) through the service's zap logger instead of the
// SDK default (plain text on stderr), so they carry the same JSON shape as
// every other log line and reach the OTLP pipeline. Bridge: zap core →
// zapslog handler → slog → the SDK's structured logger.
//
// Workflow and activity code keeps using workflow.GetLogger /
// activity.GetLogger; this only replaces the sink underneath them.
func WithLogger(l *zap.Logger) DialOption {
	if l == nil {
		// Dial rejects a nil option with an actionable error — the same
		// failure class as NewWorker's nil WorkerOption.
		return nil
	}
	return func(o *client.Options) {
		o.Logger = sdklog.NewStructuredLogger(slog.New(zapslog.NewHandler(l.Core())))
	}
}
