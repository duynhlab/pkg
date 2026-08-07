package zerolog

// Setup and WithContext mutate package-level zerolog state (global level,
// log.Logger), so no test here may use t.Parallel().

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/trace"
)

func TestParseZerologLevel(t *testing.T) {
	cases := []struct {
		in   string
		want zerolog.Level
	}{
		{"debug", zerolog.DebugLevel},
		{"info", zerolog.InfoLevel},
		{"warn", zerolog.WarnLevel},
		{"error", zerolog.ErrorLevel},
		{"  DeBuG ", zerolog.DebugLevel},
		{"", zerolog.InfoLevel},
		{"verbose", zerolog.InfoLevel},
	}
	for _, tc := range cases {
		t.Run("in="+tc.in, func(t *testing.T) {
			if got := parseZerologLevel(tc.in); got != tc.want {
				t.Errorf("parseZerologLevel(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSetup_AppliesGlobalLevel(t *testing.T) {
	prevLevel := zerolog.GlobalLevel()
	prevLogger := log.Logger
	prevTimeFormat := zerolog.TimeFieldFormat
	t.Cleanup(func() {
		zerolog.SetGlobalLevel(prevLevel)
		log.Logger = prevLogger
		zerolog.TimeFieldFormat = prevTimeFormat
	})

	for _, tc := range []struct {
		in   string
		want zerolog.Level
	}{
		{"debug", zerolog.DebugLevel},
		{"error", zerolog.ErrorLevel},
		{"nonsense", zerolog.InfoLevel},
	} {
		Setup(tc.in)
		if got := zerolog.GlobalLevel(); got != tc.want {
			t.Errorf("Setup(%q): global level = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// spanContext returns a ctx carrying a valid remote span context, plus the
// hex IDs the log line must carry. API-only — no OTel SDK needed.
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

func TestWithContext_InjectsTraceAndSpanIDs(t *testing.T) {
	prevLogger := log.Logger
	t.Cleanup(func() { log.Logger = prevLogger })

	var buf bytes.Buffer
	log.Logger = zerolog.New(&buf)

	ctx, wantTrace, wantSpan := spanContext(t)
	FromContext(WithContext(ctx)).Info().Msg("hello")

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

func TestWithContext_NoSpanMeansNoTraceFields(t *testing.T) {
	prevLogger := log.Logger
	t.Cleanup(func() { log.Logger = prevLogger })

	var buf bytes.Buffer
	log.Logger = zerolog.New(&buf)

	FromContext(WithContext(context.Background())).Info().Msg("hello")

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

func TestFromContext_BareContextIsUsable(t *testing.T) {
	l := FromContext(context.Background())
	if l == nil {
		t.Fatal("FromContext(Background()) returned nil")
	}
	// The disabled fallback logger must not panic when used.
	l.Info().Msg("noop")
}
