package temporalx

import (
	"log/slog"
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
	cases := []struct {
		name  string
		level slog.Level
		ev    string
		key   string
		want  slog.Level
	}{
		{"error", slog.LevelError, "order.retry.exhausted", "event", slog.LevelError},
		{"warn", slog.LevelWarn, "order.compensation.completed", "event", slog.LevelWarn},
		{"debug", slog.LevelDebug, "order.debug_probe", "event", slog.LevelDebug},
		{"bad grammar", slog.LevelInfo, "Order Failed", "event.invalid", slog.LevelInfo},
		{"too long", slog.LevelInfo, "a." + strings.Repeat("b", 80), "event.invalid", slog.LevelInfo},
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
			if attrOf(r, tc.key) == "" || r.Level != tc.want || len(attrOf(r, tc.key)) > 64 {
				t.Errorf("record = %v %v", r.Level, r)
			}
			if attrOf(r, "event.conflict") != "spoof" {
				t.Error("a caller's own event attribute must be renamed, not overwrite the name")
			}
		})
	}
}
