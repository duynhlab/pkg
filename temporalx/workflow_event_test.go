package temporalx

import (
	"log/slog"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

var eventRuns atomic.Int32

// eventWorkflow emits one catalog event and returns. It is registered under
// the logOnceWorkflow name for replay, whose recorded history (start, one
// decision, complete) fits any workflow that schedules nothing.
func eventWorkflow(ctx workflow.Context) error {
	eventRuns.Add(1)
	WorkflowEvent(ctx, slog.LevelInfo, "order.failed", "order failed",
		slog.String("order.id", "8"), slog.String("outcome", "compensated"))
	return nil
}

func eventRecords(h *capture) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.recs {
		if attrOf(r, "event") != "" || attrOf(r, "event.invalid") != "" {
			out = append(out, r)
		}
	}
	return out
}

// A live run writes the event once with its attributes; replaying the same
// history writes it zero times — and the replay provably ran the code.
func TestWorkflowEvent_LiveOnceReplayNever(t *testing.T) {
	live := &capture{}
	var s testsuite.WorkflowTestSuite
	s.SetLogger(sdkLogger(t, live))
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(eventWorkflow)
	env.ExecuteWorkflow(eventWorkflow)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("live run: completed=%v err=%v", env.IsWorkflowCompleted(), env.GetWorkflowError())
	}
	ev := eventRecords(live)
	if len(ev) != 1 {
		t.Fatalf("live events = %d, want 1", len(ev))
	}
	if r := ev[0]; attrOf(r, "event") != "order.failed" || attrOf(r, "outcome") != "compensated" ||
		attrOf(r, "order.id") != "8" || r.Level != slog.LevelInfo {
		t.Errorf("event = %v", r)
	}
	if f, _ := runtime.CallersFrames([]uintptr{ev[0].PC}).Next(); !strings.HasSuffix(f.File, "workflow_event_test.go") {
		t.Errorf("source = %s:%d, want the calling workflow, not temporalx", f.File, f.Line)
	}

	replayed := &capture{}
	before := eventRuns.Load()
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(eventWorkflow, workflow.RegisterOptions{Name: "logOnceWorkflow"})
	if err := replayer.ReplayWorkflowHistoryFromJSONFile(sdkLogger(t, replayed), "testdata/history_log_once.json"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if eventRuns.Load() == before {
		t.Fatal("replay never ran the workflow code; the zero-event check would be vacuous")
	}
	if n := len(eventRecords(replayed)); n != 0 {
		t.Errorf("replay wrote %d events, want 0", n)
	}
}

func TestWorkflowEvent_LevelsAndGrammar(t *testing.T) {
	exactly64 := "a." + strings.Repeat("b", 62)
	cases := []struct {
		name  string
		level slog.Level
		ev    string
		key   string
		value string
		want  slog.Level
	}{
		{"error", slog.LevelError, "order.retry.exhausted", "event", "order.retry.exhausted", slog.LevelError},
		{"warn", slog.LevelWarn, "order.compensation.completed", "event", "order.compensation.completed", slog.LevelWarn},
		{"debug", slog.LevelDebug, "order.debug_probe", "event", "order.debug_probe", slog.LevelDebug},
		{"custom level rounds down", slog.LevelWarn + 2, "order.failed", "event", "order.failed", slog.LevelWarn},
		{"exactly 64 bytes is valid", slog.LevelInfo, exactly64, "event", exactly64, slog.LevelInfo},
		{"65 bytes is invalid", slog.LevelInfo, exactly64 + "c", "event.invalid", exactly64 + "…(truncated)", slog.LevelInfo},
		{"cut on a rune boundary", slog.LevelInfo, strings.Repeat("a", 63) + "é", "event.invalid", strings.Repeat("a", 63) + "…(truncated)", slog.LevelInfo},
		{"bad grammar", slog.LevelInfo, "Order Failed", "event.invalid", "Order Failed", slog.LevelInfo},
		{"leading digit segment", slog.LevelInfo, "order.1x", "event.invalid", "order.1x", slog.LevelInfo},
		{"trailing dot", slog.LevelInfo, "order.", "event.invalid", "order.", slog.LevelInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &capture{}
			var s testsuite.WorkflowTestSuite
			s.SetLogger(sdkLogger(t, h))
			env := s.NewTestWorkflowEnvironment()
			wf := func(ctx workflow.Context) error {
				WorkflowEvent(ctx, tc.level, tc.ev, "m", slog.String("event", "spoof"))
				return nil
			}
			env.RegisterWorkflowWithOptions(wf, workflow.RegisterOptions{Name: "wf"})
			env.ExecuteWorkflow("wf")
			ev := eventRecords(h)
			if len(ev) != 1 {
				t.Fatalf("events = %d", len(ev))
			}
			r := ev[0]
			if got := attrOf(r, tc.key); got != tc.value || r.Level != tc.want {
				t.Errorf("%s = %q level %v, want %q level %v", tc.key, got, r.Level, tc.value, tc.want)
			}
			if tc.key == "event.invalid" && attrOf(r, "event") != "" {
				t.Error("an invalid name must not be written under event")
			}
			if attrOf(r, "event.conflict") != "spoof" {
				t.Error("a caller's own event attribute must be renamed, not overwrite the name")
			}
		})
	}
}
