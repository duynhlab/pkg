package slogx_test

import (
	"context"
	"testing"

	"github.com/duynhlab/pkg/logger/slogx"
)

func TestProcessLifecycleEvents(t *testing.T) {
	cases := []struct {
		name      string
		emit      func(l *slogx.Logger)
		event     string
		level     string
		component string
		outcome   string
	}{
		{"started api", func(l *slogx.Logger) { l.ProcessStarted(context.Background(), slogx.ComponentAPI) },
			"process.started", "info", "api", ""},
		{"stopped graceful", func(l *slogx.Logger) {
			l.ProcessStopped(context.Background(), slogx.ComponentWorker, slogx.OutcomeGraceful)
		}, "process.stopped", "info", "worker", "graceful"},
		{"stopped error is Error", func(l *slogx.Logger) {
			l.ProcessStopped(context.Background(), slogx.ComponentMockpay, slogx.OutcomeError)
		}, "process.stopped", "error", "mockpay", "error"},
		{"unknown values are bounded", func(l *slogx.Logger) {
			l.ProcessStopped(context.Background(), "cron-7f9c", "crashed")
		}, "process.stopped", "error", "invalid", "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, buf := newTestLogger(t, "info")
			tc.emit(l)
			recs := lines(t, buf)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			r := recs[0]
			if r["event"] != tc.event || r["level"] != tc.level || r["component"] != tc.component {
				t.Errorf("record = %v", r)
			}
			if tc.outcome != "" && r["outcome"] != tc.outcome {
				t.Errorf("outcome = %v, want %s", r["outcome"], tc.outcome)
			}
		})
	}
}
