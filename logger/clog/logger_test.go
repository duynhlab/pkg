package clog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestParseSlogLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"  DeBuG ", slog.LevelDebug},
		{"", slog.LevelInfo},
		{"verbose", slog.LevelInfo},
	}
	for _, tc := range cases {
		t.Run("in="+tc.in, func(t *testing.T) {
			if got := parseSlogLevel(tc.in); got != tc.want {
				t.Errorf("parseSlogLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// newBufHandler returns a TracingHandler over a JSON handler writing into a
// buffer, at the given level.
func newBufHandler(buf *bytes.Buffer, level slog.Level) *TracingHandler {
	return &TracingHandler{handler: slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level})}
}

// spanContext returns a ctx carrying a valid span context plus the hex IDs
// the record must carry. API-only — no OTel SDK needed.
func spanContext(t *testing.T) (context.Context, string, string) {
	t.Helper()
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(context.Background(), sc),
		sc.TraceID().String(), sc.SpanID().String()
}

func TestTracingHandler_InjectsTraceAndSpanIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newBufHandler(&buf, slog.LevelInfo))
	ctx, wantTrace, wantSpan := spanContext(t)

	logger.InfoContext(ctx, "hello")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log output is not JSON: %v (%q)", err, buf.String())
	}
	if line["trace_id"] != wantTrace {
		t.Errorf("trace_id = %v, want %s", line["trace_id"], wantTrace)
	}
	if line["span_id"] != wantSpan {
		t.Errorf("span_id = %v, want %s", line["span_id"], wantSpan)
	}
}

func TestTracingHandler_NoSpanMeansNoTraceFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(newBufHandler(&buf, slog.LevelInfo))

	logger.InfoContext(context.Background(), "hello")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log output is not JSON: %v", err)
	}
	if _, ok := line["trace_id"]; ok {
		t.Errorf("trace_id present for a context without a span: %v", line)
	}
	if _, ok := line["span_id"]; ok {
		t.Errorf("span_id present for a context without a span: %v", line)
	}
}

func TestTracingHandler_EnabledDelegatesToInner(t *testing.T) {
	var buf bytes.Buffer
	h := newBufHandler(&buf, slog.LevelWarn)

	if h.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Enabled(Debug) = true with a Warn-level inner handler")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled(Error) = false with a Warn-level inner handler")
	}
}

// WithAttrs/WithGroup must return a *TracingHandler, not the bare inner
// handler — otherwise every derived logger silently stops injecting trace
// IDs. This is the regression this file exists to catch.
func TestTracingHandler_WithAttrsKeepsTraceInjection(t *testing.T) {
	var buf bytes.Buffer
	wrapped := newBufHandler(&buf, slog.LevelInfo).WithAttrs([]slog.Attr{slog.String("component", "test")})

	if _, ok := wrapped.(*TracingHandler); !ok {
		t.Fatalf("WithAttrs returned %T, want *TracingHandler", wrapped)
	}

	ctx, wantTrace, _ := spanContext(t)
	slog.New(wrapped).InfoContext(ctx, "hello")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log output is not JSON: %v", err)
	}
	if line["trace_id"] != wantTrace {
		t.Errorf("trace_id = %v after WithAttrs, want %s", line["trace_id"], wantTrace)
	}
	if line["component"] != "test" {
		t.Errorf("component attr = %v, want test", line["component"])
	}
}

func TestTracingHandler_WithGroupKeepsTraceInjection(t *testing.T) {
	var buf bytes.Buffer
	wrapped := newBufHandler(&buf, slog.LevelInfo).WithGroup("req")

	if _, ok := wrapped.(*TracingHandler); !ok {
		t.Fatalf("WithGroup returned %T, want *TracingHandler", wrapped)
	}

	ctx, wantTrace, _ := spanContext(t)
	slog.New(wrapped).InfoContext(ctx, "hello")

	// Record attrs land inside the open group; asserting on the raw output
	// keeps the test independent of the nesting shape.
	if !strings.Contains(buf.String(), wantTrace) {
		t.Errorf("output after WithGroup lacks the trace ID %s: %q", wantTrace, buf.String())
	}
}

func TestWithLoggerFromContext_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	ctx := WithLogger(context.Background(), slog.New(newBufHandler(&buf, slog.LevelInfo)))

	FromContext(ctx).InfoContext(ctx, "routed")

	if !strings.Contains(buf.String(), "routed") {
		t.Errorf("FromContext logger did not write to the attached handler: %q", buf.String())
	}
}

func TestFromContext_BareContextIsUsable(t *testing.T) {
	l := FromContext(context.Background())
	if l == nil {
		t.Fatal("FromContext(Background()) returned nil")
	}
}

func TestContextAliases_RespectHandlerLevel(t *testing.T) {
	cases := []struct {
		name        string
		logFn       func(context.Context, string, ...any)
		wantEmitted bool
	}{
		{"InfoContext", InfoContext, true},
		{"WarnContext", WarnContext, true},
		{"ErrorContext", ErrorContext, true},
		{"DebugContext", DebugContext, false}, // handler level is Info
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			ctx := WithLogger(context.Background(), slog.New(newBufHandler(&buf, slog.LevelInfo)))

			tc.logFn(ctx, "msg-"+tc.name)

			if got := strings.Contains(buf.String(), "msg-"+tc.name); got != tc.wantEmitted {
				t.Errorf("%s emitted = %v, want %v (output %q)", tc.name, got, tc.wantEmitted, buf.String())
			}
		})
	}
}
