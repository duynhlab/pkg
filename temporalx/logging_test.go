package temporalx

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/client"
	sdklog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// capture is a slog.Handler that counts records by message.
type capture struct {
	mu   sync.Mutex
	msgs []string
	recs []slog.Record
}

func (h *capture) Enabled(context.Context, slog.Level) bool { return true }
func (h *capture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *capture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capture) WithGroup(string) slog.Handler      { return h }

func (h *capture) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

func sdkLogger(t *testing.T, h *capture) sdklog.Logger {
	t.Helper()
	var o client.Options
	WithLogger(slog.New(h))(&o)
	if o.Logger == nil {
		t.Fatal("WithLogger did not set client.Options.Logger")
	}
	return o.Logger
}

const workflowLine = "workflow decided"

// logOnceWorkflow logs once through the SDK's replay-aware logger and returns;
// testdata/history_log_once.json is its recorded history.
func logOnceWorkflow(ctx workflow.Context) error {
	workflow.GetLogger(ctx).Info(workflowLine, "order_id", "8")
	return nil
}

// The SDK's own lines and the key/value pairs a caller passes reach the
// structured logger as slog attributes.
func TestWithLogger_BridgesToSlog(t *testing.T) {
	h := &capture{}
	sdkLogger(t, h).Info("poller started", "TaskQueue", "checkout")
	if h.count("poller started") != 1 {
		t.Fatalf("want one record, got %v", h.msgs)
	}
	var tq string
	h.recs[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "TaskQueue" {
			tq = a.Value.String()
		}
		return true
	})
	if tq != "checkout" {
		t.Errorf("TaskQueue = %q, want checkout", tq)
	}
}

// Task 2.2: workflow logging is replay safe and performs no exporter side
// effect. A live run writes the workflow's line exactly once; replaying the
// same history through the same logger writes it zero times. Without the live
// half the replay half would pass vacuously.
func TestWithLogger_WorkflowLogIsReplaySafe(t *testing.T) {
	live := &capture{}
	var s testsuite.WorkflowTestSuite
	s.SetLogger(sdkLogger(t, live))
	env := s.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(logOnceWorkflow)
	env.ExecuteWorkflow(logOnceWorkflow)
	if !env.IsWorkflowCompleted() || env.GetWorkflowError() != nil {
		t.Fatalf("live run: completed=%v err=%v", env.IsWorkflowCompleted(), env.GetWorkflowError())
	}
	if n := live.count(workflowLine); n != 1 {
		t.Fatalf("a live run must log the workflow line once, got %d", n)
	}

	replayed := &capture{}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(logOnceWorkflow)
	if err := replayer.ReplayWorkflowHistoryFromJSONFile(sdkLogger(t, replayed), "testdata/history_log_once.json"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n := replayed.count(workflowLine); n != 0 {
		t.Errorf("replay must write nothing from workflow code, got %d records", n)
	}
}

func TestWithLogger_NilLoggerFailsDial(t *testing.T) {
	if WithLogger(nil) != nil {
		t.Fatal("WithLogger(nil) should return a nil DialOption")
	}
	c, err := Dial(Config{HostPort: "127.0.0.1:1", Namespace: "mop"}, WithLogger(nil))
	if err == nil {
		c.Close()
		t.Fatal("expected an error for a nil DialOption, got nil")
	}
	if !strings.Contains(err.Error(), "nil DialOption") {
		t.Errorf("error %q does not mention the nil DialOption", err.Error())
	}
}
