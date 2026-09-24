package temporalx

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/trace"
	"go.temporal.io/sdk/client"
	sdklog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// capture is a slog.Handler that counts records by message.
type capture struct {
	mu   sync.Mutex
	msgs []string
	recs []slog.Record
	ctxs []context.Context
}

func (h *capture) Enabled(context.Context, slog.Level) bool { return true }
func (h *capture) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ctxs = append(h.ctxs, ctx)
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
// logOnceRuns counts executions of its code, so the replay half can prove the
// code actually ran — a replay that never reached the log call would pass the
// zero-records assertion vacuously.
var logOnceRuns atomic.Int32

func logOnceWorkflow(ctx workflow.Context) error {
	logOnceRuns.Add(1)
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
	before := logOnceRuns.Load()
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(logOnceWorkflow)
	if err := replayer.ReplayWorkflowHistoryFromJSONFile(sdkLogger(t, replayed), "testdata/history_log_once.json"); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if logOnceRuns.Load() == before {
		t.Fatal("replay never executed the workflow code; the zero-record check below would be vacuous")
	}
	if n := replayed.count(workflowLine); n != 0 {
		t.Errorf("replay must write nothing from workflow code, got %d records", n)
	}
}

// The tracing interceptor's TraceID/SpanID attributes become the record's span
// context, so the platform handler stamps the canonical trace_id/span_id; they
// are not written as a second, differently spelled pair. Other attributes pass.
func TestWithLogger_LiftsTracingAttrsIntoContext(t *testing.T) {
	h := &capture{}
	tid := trace.TraceID{0x4b, 0xf9, 1}
	sid := trace.SpanID{0x00, 0xf0, 2}
	l := sdklog.With(sdkLogger(t, h), "TraceID", tid, "SpanID", sid, "WorkflowType", "checkout")
	l.Info("activity started")

	sc := trace.SpanContextFromContext(h.ctxs[0])
	if sc.TraceID() != tid || sc.SpanID() != sid {
		t.Errorf("span context = %s/%s, want %s/%s", sc.TraceID(), sc.SpanID(), tid, sid)
	}
	// capture drops With attributes, so check what reaches a real handler.
	var buf strings.Builder
	var o client.Options
	WithLogger(slog.New(slog.NewJSONHandler(&buf, nil)))(&o)
	sdklog.With(o.Logger, "TraceID", tid, "SpanID", sid, "WorkflowType", "checkout").Info("x")
	if out := buf.String(); strings.Contains(out, "TraceID") || strings.Contains(out, "SpanID") ||
		!strings.Contains(out, `"WorkflowType":"checkout"`) {
		t.Errorf("attributes reaching the handler: %s", out)
	}

	// Without the pair, the record keeps a spanless context.
	h2 := &capture{}
	sdkLogger(t, h2).Info("plain")
	if trace.SpanContextFromContext(h2.ctxs[0]).IsValid() {
		t.Error("a record without tracing attributes must carry no span")
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

// The SDK logs its own failures under "Error" with a raw error value; the
// handler rewrites it into error.type + error.message so nothing unknown to the
// facade carries raw error text.
func TestWithLogger_RewritesTheSDKErrorKey(t *testing.T) {
	var buf strings.Builder
	var o client.Options
	WithLogger(slog.New(slog.NewJSONHandler(&buf, nil)))(&o)
	o.Logger.Warn("Activity error.", "Error",
		temporal.NewNonRetryableApplicationError("payment not authorized", "PaymentDeclined", nil), "ActivityType", "AuthorizePayment")
	o.Logger.Warn("Failed to poll for task.", "Error", errors.New("connection refused"))
	o.Logger.Info("no error here", "Attempt", 1)
	o.Logger.Warn("stringly error", "Error", "plain text")
	out := buf.String()
	for _, want := range []string{`"error.type":"PaymentDeclined"`, `"error.type":"*errors.errorString"`, `"error.message":"connection refused"`, `"ActivityType":"AuthorizePayment"`, `"Attempt":1`, `"error.message":"plain text"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(out, `"Error":`) {
		t.Errorf("the raw Error key must not survive: %s", out)
	}
}
