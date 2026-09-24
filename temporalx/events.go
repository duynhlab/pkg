package temporalx

import (
	"context"
	"log/slog"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
)

// Workflow lifecycle events (RFC-0031 § Event catalog). This module imports
// log/slog only, so it writes the catalog name under the same "event" key the
// logging facade's Event uses; the facade passes it through unchanged.
const (
	eventWorkflowStarted = "temporal.workflow.started"
	eventWorkflowFailed  = "temporal.workflow.failed"
)

// startEvents emits temporal.workflow.started from the CLIENT, never from
// workflow code (which is replayed). It fires only when the start is
// unambiguous: with WorkflowExecutionErrorWhenAlreadyStarted unset, the SDK
// answers a rejected duplicate with a nil error and a handle to the existing
// run, so a nil error would not prove anything started. SignalWithStart is
// skipped for the same reason — its caller cannot tell a start from a signal.
type startEvents struct {
	interceptor.ClientInterceptorBase
	log *slog.Logger
}

func (s *startEvents) InterceptClient(next interceptor.ClientOutboundInterceptor) interceptor.ClientOutboundInterceptor {
	return &startEventsOutbound{ClientOutboundInterceptorBase: interceptor.ClientOutboundInterceptorBase{Next: next}, log: s.log}
}

type startEventsOutbound struct {
	interceptor.ClientOutboundInterceptorBase
	log *slog.Logger
}

func (o *startEventsOutbound) ExecuteWorkflow(ctx context.Context, in *interceptor.ClientExecuteWorkflowInput) (client.WorkflowRun, error) {
	run, err := o.Next.ExecuteWorkflow(ctx, in)
	if err == nil && in.Options != nil && in.Options.WorkflowExecutionErrorWhenAlreadyStarted {
		o.log.LogAttrs(ctx, slog.LevelInfo, "workflow started",
			slog.String("event", eventWorkflowStarted),
			slog.String("temporal.workflow.type", in.WorkflowType),
			slog.String("temporal.task_queue", in.Options.TaskQueue))
	}
	return run, err
}

// WorkflowFailed emits temporal.workflow.failed for a run the caller has
// observed ending failed, terminated or timed out — a dispatcher or reconciler
// that described the run, or an activity that watched it. Workflow code must
// never call it: it is replayed. Any other status is not a failure and writes
// nothing; a nil logger writes nothing.
func WorkflowFailed(ctx context.Context, l *slog.Logger, workflowType string, status enumspb.WorkflowExecutionStatus) {
	var runStatus string
	switch status { //nolint:exhaustive // only the failure statuses are events
	case enumspb.WORKFLOW_EXECUTION_STATUS_FAILED:
		runStatus = "failed"
	case enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		runStatus = "terminated"
	case enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		runStatus = "timed_out"
	default:
		return
	}
	if l == nil {
		return
	}
	l.LogAttrs(ctx, slog.LevelError, "workflow failed",
		slog.String("event", eventWorkflowFailed),
		slog.String("temporal.workflow.type", workflowType),
		slog.String("temporal.run_status", runStatus))
}
