package obsx

import (
	"bytes"
	"context"
	"strings"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// captureExporter records the log records handed to it, for assertions.
type captureExporter struct{ records []sdklog.Record }

func (c *captureExporter) Export(_ context.Context, recs []sdklog.Record) error {
	c.records = append(c.records, recs...)
	return nil
}
func (c *captureExporter) Shutdown(context.Context) error   { return nil }
func (c *captureExporter) ForceFlush(context.Context) error { return nil }

// teeLogger builds a logger teeing a stdout JSON core and the OTLP bridge, plus
// the capture exporter and stdout buffer to assert on.
func teeLogger(t *testing.T) (*zap.Logger, *captureExporter, *bytes.Buffer) {
	t.Helper()
	exp := &captureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
	obs := &Observability{LoggerProvider: lp}

	buf := &bytes.Buffer{}
	stdout := zapcore.NewCore(
		zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		zapcore.AddSync(buf), zapcore.InfoLevel,
	)
	logger := zap.New(zapcore.NewTee(stdout, obs.ZapCore("test", zapcore.InfoLevel)))
	return logger, exp, buf
}

func assertNoLeak(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "query failed") {
		t.Errorf("stdout missing the log message: %s", out)
	}
	if strings.Contains(out, traceContextFieldKey) || strings.Contains(strings.ToLower(out), "context.background") {
		t.Errorf("stdout leaked the trace-context field: %s", out)
	}
}

// Per-call use: TraceContext must (1) stamp the native OTLP trace_id on the
// exported log record and (2) not leak the raw context into the stdout tee.
func TestTraceContext_PerCall_NativeTraceID_NoStdoutLeak(t *testing.T) {
	logger, exp, buf := teeLogger(t)
	ctx, span := sdktrace.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	wantTID := span.SpanContext().TraceID()

	logger.Info("query failed", TraceContext(ctx))
	span.End()

	if len(exp.records) != 1 {
		t.Fatalf("exported %d records, want 1", len(exp.records))
	}
	if got := exp.records[0].TraceID(); got != wantTID {
		t.Errorf("record TraceID = %v, want %v (native correlation missing)", got, wantTID)
	}
	assertNoLeak(t, buf.String())
}

// Bound use: `log.With(TraceContext(ctx))` must carry the same native trace_id
// to later records. Guards the second supported path against an otelzap bump
// that could break either usage independently.
func TestTraceContext_WithBinding_NativeTraceID_NoStdoutLeak(t *testing.T) {
	logger, exp, buf := teeLogger(t)
	ctx, span := sdktrace.NewTracerProvider().Tracer("t").Start(context.Background(), "s")
	wantTID := span.SpanContext().TraceID()

	bound := logger.With(TraceContext(ctx))
	bound.Info("query failed")
	span.End()

	if len(exp.records) != 1 {
		t.Fatalf("exported %d records, want 1", len(exp.records))
	}
	if got := exp.records[0].TraceID(); got != wantTID {
		t.Errorf("record TraceID = %v, want %v (native correlation missing)", got, wantTID)
	}
	assertNoLeak(t, buf.String())
}

// A nil ctx must be a no-op field: no panic, a record still emitted (with no
// trace id), stdout clean.
func TestTraceContext_NilCtx_Safe(t *testing.T) {
	logger, exp, buf := teeLogger(t)

	logger.Info("query failed", TraceContext(nil)) //nolint:staticcheck // exercising nil ctx guard

	if len(exp.records) != 1 {
		t.Fatalf("exported %d records, want 1", len(exp.records))
	}
	if exp.records[0].TraceID().IsValid() {
		t.Errorf("nil ctx produced a valid TraceID, want none")
	}
	assertNoLeak(t, buf.String())
}
