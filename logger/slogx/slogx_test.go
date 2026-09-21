package slogx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/duynhlab/pkg/logger/slogx"
)

func newTestLogger(t *testing.T, level string) (*slogx.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	return slogx.New(slogx.Config{Level: level, Service: "test", Stdout: buf}), buf
}

func lines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stdout line is not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// The stdout record is the platform envelope every service has emitted since
// the zapx cutover — same keys, same timestamp shape, same lowercase level —
// so queries and runbooks written against zapx read slogx unchanged.
func TestStdoutEnvelope_MatchesTheZapxContract(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	log.Info(context.Background(), "HTTP request", slog.String("http.route", "/orders"), slog.Int("http.response.status_code", 200))

	recs := lines(t, buf)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	r := recs[0]
	for _, k := range []string{"timestamp", "level", "message", "caller", "http.route", "http.response.status_code"} {
		if _, ok := r[k]; !ok {
			t.Errorf("missing envelope key %q in %v", k, r)
		}
	}
	for _, k := range []string{"time", "level_name", "msg", "source", "trace_id", "span_id"} {
		if _, ok := r[k]; ok {
			t.Errorf("unexpected key %q (slog default names must be renamed; trace ids only with a span)", k)
		}
	}
	if r["level"] != "info" || r["message"] != "HTTP request" {
		t.Errorf("level/message = %v/%v", r["level"], r["message"])
	}
	ts, _ := r["timestamp"].(string)
	if _, err := time.Parse("2006-01-02T15:04:05.000Z07:00", ts); err != nil || !strings.HasSuffix(ts, "Z") {
		t.Errorf("timestamp %q is not zap's ISO8601 UTC shape: %v", ts, err)
	}
	if caller, _ := r["caller"].(string); !strings.HasPrefix(caller, "slogx/slogx_test.go:") {
		t.Errorf("caller = %q, want this test file — the facade must not attribute lines to itself", caller)
	}
}

func TestLevels_NamesAndGate(t *testing.T) {
	log, buf := newTestLogger(t, "trace")
	ctx := context.Background()
	log.Trace(ctx, "t")
	log.Debug(ctx, "d")
	log.Info(ctx, "i")
	log.Warn(ctx, "w")
	log.Error(ctx, "e")
	var got []string
	for _, r := range lines(t, buf) {
		got = append(got, r["level"].(string))
	}
	if strings.Join(got, ",") != "trace,debug,info,warn,error" {
		t.Errorf("level names = %v", got)
	}

	// The configured level gates the stdout sink — and, through Enabled, will
	// gate the OTLP sink identically, so a debug line never leaves the pod on
	// an info-level service.
	log2, buf2 := newTestLogger(t, "warn")
	log2.Debug(ctx, "hidden")
	log2.Info(ctx, "hidden")
	log2.Warn(ctx, "shown")
	if n := len(lines(t, buf2)); n != 1 {
		t.Errorf("records at warn = %d, want 1", n)
	}
	if log2.Enabled(ctx, slog.LevelInfo) || !log2.Enabled(ctx, slog.LevelWarn) {
		t.Error("Enabled must follow the configured level")
	}
	// Unknown and blank levels mean info, the zapx default — a typo never
	// silences a service.
	log3, buf3 := newTestLogger(t, "verbose")
	log3.Info(ctx, "still logged")
	log3.Debug(ctx, "not logged")
	if n := len(lines(t, buf3)); n != 1 {
		t.Errorf("records at unknown level = %d, want 1 (info default)", n)
	}
}

func TestTraceIDs_OnlyWithAValidSpan(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	log.Info(ctx, "with span")
	log.Info(context.Background(), "without span")
	log.Info(nil, "nil ctx is tolerated") //nolint:staticcheck // the facade normalises a nil ctx on purpose

	recs := lines(t, buf)
	if len(recs) != 3 {
		t.Fatalf("records = %d, want 3", len(recs))
	}
	if recs[0]["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || recs[0]["span_id"] != "00f067aa0ba902b7" {
		t.Errorf("trace ids not stamped from the span context: %v", recs[0])
	}
	for _, r := range recs[1:] {
		if _, ok := r["trace_id"]; ok {
			t.Errorf("trace_id present without a span: %v", r)
		}
	}
}

func TestWith_BindsIdentityToEveryRecord(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	worker := log.With(slog.String("component", "order-worker"))
	worker.Info(context.Background(), "started")
	log.Info(context.Background(), "plain")

	recs := lines(t, buf)
	if recs[0]["component"] != "order-worker" {
		t.Errorf("bound attribute missing: %v", recs[0])
	}
	if _, ok := recs[1]["component"]; ok {
		t.Error("With must not mutate the parent logger")
	}
	if log.With() != log {
		t.Error("With() with no attrs must return the same logger")
	}
}

func TestFromContext_FallsBackToTheDefault(t *testing.T) {
	log, _ := newTestLogger(t, "info")
	ctx := slogx.WithContext(context.Background(), log)
	if slogx.FromContext(ctx) != log {
		t.Error("FromContext must return the bound logger")
	}
	if slogx.FromContext(context.Background()) == nil || slogx.FromContext(nil) == nil { //nolint:staticcheck
		t.Error("FromContext must never return nil")
	}
}

func TestSlog_ExposesTheSameHandler(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	log.Slog().Info("from an sdk bridge", "k", "v")
	recs := lines(t, buf)
	if len(recs) != 1 || recs[0]["message"] != "from an sdk bridge" || recs[0]["k"] != "v" {
		t.Errorf("Slog() must render through the same envelope: %v", recs)
	}
}

func TestNoSource_OmitsCaller(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slogx.New(slogx.Config{Stdout: buf, NoSource: true})
	log.Info(context.Background(), "x")
	if _, ok := lines(t, buf)[0]["caller"]; ok {
		t.Error("NoSource must drop the caller key")
	}
}

// Copies of a slog.Record share their attribute backing array. The trace
// handler appends two attributes, so it must Clone first — otherwise, once a
// second sink sits beside stdout, it sees this sink's ids (slog stamps a "!BUG"
// attribute when it catches that). Twenty-five attributes push the record
// past its inline array into the shared backing slice, which is where the
// aliasing shows.
func TestTraceHandler_ClonesBeforeAppending(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	attrs := make([]slog.Attr, 0, 25)
	for i := 0; i < 25; i++ {
		attrs = append(attrs, slog.Int("k"+string(rune('a'+i)), i))
	}
	log.Info(ctx, "many", attrs...)
	r := lines(t, buf)[0]
	if _, bug := r["!BUG"]; bug {
		t.Fatalf("slog detected an unsafe AddAttrs on a shared record: %v", r)
	}
	if r["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || r["ky"] != float64(24) {
		t.Errorf("record lost attributes or ids: %v", r)
	}
}

// The caller is the exact line that called the facade, not one off.
func TestCaller_ExactLine(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	_, _, line, _ := runtime.Caller(0)
	log.Info(context.Background(), "here") // line+1
	got := lines(t, buf)[0]["caller"].(string)
	want := "slogx/slogx_test.go:" + strconv.Itoa(line+1)
	if got != want {
		t.Errorf("caller = %q, want %q", got, want)
	}
}

func TestSetLevel_AppliesToChildren(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	child := log.With(slog.String("component", "c"))
	child.Debug(context.Background(), "hidden")
	log.SetLevel("debug")
	child.Debug(context.Background(), "shown")
	if n := len(lines(t, buf)); n != 1 {
		t.Errorf("records = %d, want 1 — SetLevel on the parent must reach the child", n)
	}
}

func TestSetDefault_ChangesTheFallback(t *testing.T) {
	log, buf := newTestLogger(t, "error")
	slogx.SetDefault(log)
	t.Cleanup(func() { slogx.SetDefault(slogx.New(slogx.Config{})) })
	slogx.FromContext(context.Background()).Info(context.Background(), "suppressed at error level")
	slogx.FromContext(context.Background()).Error(context.Background(), "kept")
	if n := len(lines(t, buf)); n != 1 {
		t.Errorf("records via the default = %d, want 1 (the configured level must apply)", n)
	}
}

func TestConcurrentUse(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				log.With(slog.Int("g", g)).Info(context.Background(), "x", slog.Int("i", i))
			}
		}(g)
	}
	wg.Wait()
	if n := len(lines(t, buf)); n != 400 {
		t.Errorf("records = %d, want 400 intact JSON lines", n)
	}
}
