package slogx_test

import (
	"context"
	"log/slog"
	"strings"
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
			if caller, _ := r["caller"].(string); !strings.HasPrefix(caller, "slogx/process_test.go:") {
				t.Errorf("caller = %q, want this test file, never the facade", caller)
			}
			if tc.outcome != "" && r["outcome"] != tc.outcome {
				t.Errorf("outcome = %v, want %s", r["outcome"], tc.outcome)
			}
		})
	}
}

// Event names its own caller too — the refactor that let the process helpers
// share its body must not shift the frame.
func TestProcessHelpers_CallerIsTheCallSite(t *testing.T) {
	l, buf := newTestLogger(t, "info")
	l.ProcessStarted(context.Background(), slogx.ComponentAPI)
	l.ProcessStopped(context.Background(), slogx.ComponentAPI, slogx.OutcomeGraceful)
	for _, r := range lines(t, buf) {
		if caller, _ := r["caller"].(string); !strings.HasPrefix(caller, "slogx/process_test.go:") {
			t.Errorf("caller = %q, want this test file (called directly, no closure)", caller)
		}
	}
}

func TestEvent_CallerIsTheCallSite(t *testing.T) {
	l, buf := newTestLogger(t, "info")
	l.Event(context.Background(), slog.LevelInfo, "order.created", "order created")
	r := lines(t, buf)[0]
	if caller, _ := r["caller"].(string); !strings.HasPrefix(caller, "slogx/process_test.go:") {
		t.Errorf("caller = %q, want this test file", caller)
	}
}
