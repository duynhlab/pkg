package temporalx

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
)

// fakeNext answers ExecuteWorkflow with err; every other method is unused.
type fakeNext struct {
	interceptor.ClientOutboundInterceptor
	err error
}

func (f fakeNext) ExecuteWorkflow(context.Context, *interceptor.ClientExecuteWorkflowInput) (client.WorkflowRun, error) {
	return nil, f.err
}

func attrOf(r slog.Record, key string) string {
	var v string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.String()
		}
		return true
	})
	return v
}

func TestStartEvents_OnlyForAnUnambiguousStart(t *testing.T) {
	cases := []struct {
		name      string
		errOnDup  bool
		err       error
		wantEvent bool
	}{
		{"started, duplicates error", true, nil, true},
		{"nil error without the duplicate error proves nothing", false, nil, false},
		{"rejected start", true, errors.New("already started"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &capture{}
			out := (&startEvents{log: slog.New(h)}).InterceptClient(fakeNext{err: tc.err})
			_, err := out.ExecuteWorkflow(context.Background(), &interceptor.ClientExecuteWorkflowInput{
				Options:      &client.StartWorkflowOptions{TaskQueue: "order-fulfillment", WorkflowExecutionErrorWhenAlreadyStarted: tc.errOnDup},
				WorkflowType: "OrderSaga",
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v passed through", err, tc.err)
			}
			if got := h.count("workflow started") == 1; got != tc.wantEvent {
				t.Fatalf("event emitted = %v, want %v", got, tc.wantEvent)
			}
			if tc.wantEvent {
				r := h.recs[0]
				if attrOf(r, "event") != "temporal.workflow.started" || attrOf(r, "temporal.workflow.type") != "OrderSaga" ||
					attrOf(r, "temporal.task_queue") != "order-fulfillment" {
					t.Errorf("attributes wrong on %v", r)
				}
			}
		})
	}
}

func TestWithLogger_InstallsTheStartEventInterceptor(t *testing.T) {
	var o client.Options
	WithLogger(slog.New(&capture{}))(&o)
	if len(o.Interceptors) != 1 {
		t.Fatalf("interceptors = %d, want the start-event interceptor", len(o.Interceptors))
	}
	if _, ok := o.Interceptors[0].(*startEvents); !ok {
		t.Errorf("interceptor = %T", o.Interceptors[0])
	}
}

func TestWorkflowFailed(t *testing.T) {
	for status, want := range map[enumspb.WorkflowExecutionStatus]string{
		enumspb.WORKFLOW_EXECUTION_STATUS_FAILED:           "failed",
		enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED:       "terminated",
		enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:        "timed_out",
		enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED:        "",
		enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:         "",
		enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:          "",
		enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW: "",
	} {
		h := &capture{}
		WorkflowFailed(context.Background(), slog.New(h), "OrderSaga", status)
		if want == "" {
			if len(h.recs) != 0 {
				t.Errorf("%v: not a failure, must write nothing", status)
			}
			continue
		}
		if len(h.recs) != 1 {
			t.Fatalf("%v: records = %d, want 1", status, len(h.recs))
		}
		r := h.recs[0]
		if r.Level != slog.LevelError || attrOf(r, "event") != "temporal.workflow.failed" ||
			attrOf(r, "temporal.run_status") != want || attrOf(r, "temporal.workflow.type") != "OrderSaga" {
			t.Errorf("%v: record = %v %v", status, r.Level, r)
		}
	}
	WorkflowFailed(context.Background(), nil, "OrderSaga", enumspb.WORKFLOW_EXECUTION_STATUS_FAILED) // must not panic
}
