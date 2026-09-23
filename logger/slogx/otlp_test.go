package slogx_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/duynhlab/pkg/logger/slogx"
)

// memExporter is the smallest sdk/log exporter: it keeps every record.
type memExporter struct {
	mu   sync.Mutex
	recs []sdklog.Record
}

func (e *memExporter) Export(_ context.Context, recs []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range recs {
		e.recs = append(e.recs, r.Clone())
	}
	return nil
}
func (e *memExporter) Shutdown(context.Context) error   { return nil }
func (e *memExporter) ForceFlush(context.Context) error { return nil }

func (e *memExporter) records() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sdklog.Record(nil), e.recs...)
}

// attrs flattens a record's attributes; groups (map values) are joined with
// a dot so assertions read like the stdout envelope.
func attrs(r sdklog.Record) map[string]string {
	out := map[string]string{}
	var walk func(prefix string, kv attribute.KeyValue)
	walk = func(prefix string, kv attribute.KeyValue) {
		if kv.Value.Type() == attribute.MAP {
			for _, m := range kv.Value.AsMap() {
				walk(prefix+string(kv.Key)+".", m)
			}
			return
		}
		out[prefix+string(kv.Key)] = kv.Value.String()
	}
	r.WalkAttributes(func(kv attribute.KeyValue) bool { walk("", kv); return true })
	return out
}

func newPair(t *testing.T, cfg slogx.Config) (*slogx.Logger, *bytes.Buffer, *memExporter) {
	t.Helper()
	exp := &memExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
	buf := &bytes.Buffer{}
	cfg.Stdout = buf
	cfg.LoggerProvider = lp
	return slogx.New(cfg), buf, exp
}

// One redacted record, two renderings: the same attributes reach stdout and
// OTLP, the deny list has already run for both, and the trace ids travel in
// the OTLP record's own fields — never doubled as attributes.
func TestOTLP_SameRedactedRecordOnBothSinks(t *testing.T) {
	log_, buf, exp := newPair(t, slogx.Config{})
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	log_.With(slog.String("component", "http")).Info(ctx, "request completed",
		slog.String("user_id", "u1"), slog.String("password", "hunter2"), slog.Int("http.response.status_code", 200),
		slog.Group("req", slog.String("cookie", "sid=1"), slog.String("route", "/orders")))

	std := decode(t, buf)
	recs := exp.records()
	if len(recs) != 1 {
		t.Fatalf("OTLP records = %d, want 1", len(recs))
	}
	r := recs[0]
	a := attrs(r)
	if r.Body().AsString() != "request completed" || r.Severity() != log.SeverityInfo {
		t.Errorf("body/severity: %q %v", r.Body().AsString(), r.Severity())
	}
	if a["password"] != red || a["req.cookie"] != red || a["user_id"] != "u1" || a["req.route"] != "/orders" || a["component"] != "http" || a["http.response.status_code"] != "200" {
		t.Errorf("OTLP attributes not the redacted set: %v", a)
	}
	if std["password"] != red || std["req"].(map[string]any)["cookie"] != red || std["user_id"] != "u1" || std["component"] != "http" {
		t.Errorf("stdout attributes: %v", std)
	}
	if r.TraceID() != tid || r.SpanID() != sid {
		t.Errorf("OTLP record must carry the span context in its own fields: %v %v", r.TraceID(), r.SpanID())
	}
	if _, doubled := a["trace_id"]; doubled {
		t.Errorf("trace_id must not be doubled as an OTLP attribute: %v", a)
	}
	if std["trace_id"] != tid.String() || std["span_id"] != sid.String() {
		t.Errorf("stdout envelope keeps trace_id/span_id: %v %v", std["trace_id"], std["span_id"])
	}
	// RFC-0031 § Adoption: an operator recognises an adopted process by
	// records whose ScopeName is the facade's package path. The service name
	// rides on the Resource, not here.
	if r.InstrumentationScope().Name != "github.com/duynhlab/pkg/logger/slogx" {
		t.Errorf("scope = %q, want the facade's package path", r.InstrumentationScope().Name)
	}
	if _, ok := a["code.line.number"]; !ok {
		t.Errorf("source enabled: OTLP record should carry code.* attributes: %v", a)
	}
}

// The facade's six levels land exactly on the RFC-0031 severity table.
func TestOTLP_SeverityTable(t *testing.T) {
	var exited int
	log_, _, exp := newPair(t, slogx.Config{Level: "trace", NoSource: true, Exit: func(c int) { exited = c }})
	ctx := context.Background()
	log_.Trace(ctx, "t")
	log_.Debug(ctx, "d")
	log_.Info(ctx, "i")
	log_.Warn(ctx, "w")
	log_.Error(ctx, "e")
	log_.Fatal(ctx, "f")
	want := []log.Severity{log.SeverityTrace, log.SeverityDebug, log.SeverityInfo, log.SeverityWarn, log.SeverityError, log.SeverityFatal}
	recs := exp.records()
	if len(recs) != len(want) {
		t.Fatalf("records = %d, want %d", len(recs), len(want))
	}
	for i, r := range recs {
		if r.Severity() != want[i] {
			t.Errorf("record %d severity = %d, want %d", i, r.Severity(), want[i])
		}
	}
	if recs[0].Severity() != 1 || recs[5].Severity() != 21 {
		t.Errorf("TRACE/FATAL must be 1/21: %d %d", recs[0].Severity(), recs[5].Severity())
	}
	if exited != 1 {
		t.Errorf("Fatal exit code = %d", exited)
	}
	if _, ok := attrs(recs[2])["code.line.number"]; ok {
		t.Errorf("NoSource must suppress code.* on OTLP too")
	}
}

// One gate for both sinks: what stdout suppresses never leaves over OTLP,
// even though the provider itself would accept the record.
func TestOTLP_LevelGateCoversBothSinks(t *testing.T) {
	log_, buf, exp := newPair(t, slogx.Config{Level: "warn"})
	log_.Info(context.Background(), "quiet")
	log_.Slog().WithGroup("g").Info("quiet too")
	if buf.Len() != 0 || len(exp.records()) != 0 {
		t.Fatalf("suppressed record leaked: stdout=%q otlp=%d", buf.String(), len(exp.records()))
	}
	log_.SetLevel("debug")
	log_.Debug(context.Background(), "loud")
	if buf.Len() == 0 || len(exp.records()) != 1 {
		t.Errorf("after SetLevel both sinks emit: stdout=%q otlp=%d", buf.String(), len(exp.records()))
	}
}

func TestEvent_TagsBothSinks(t *testing.T) {
	log_, buf, exp := newPair(t, slogx.Config{})
	log_.Event(context.Background(), slog.LevelInfo, "order.confirmed", "order confirmed", slog.Int64("order_id", 8))
	std := decode(t, buf)
	a := attrs(exp.records()[0])
	if std["event"] != "order.confirmed" || a["event"] != "order.confirmed" || std["order_id"] != float64(8) || a["order_id"] != "8" {
		t.Errorf("event attribute on both sinks: %v / %v", std, a)
	}
	if std["level"] != "info" || exp.records()[0].Severity() != log.SeverityInfo {
		t.Errorf("event level: %v", std["level"])
	}

	buf.Reset()
	log_.Event(context.Background(), slog.LevelWarn, "Order Confirmed!", "bad name", slog.String("k", "v"))
	std = decode(t, buf)
	if _, ok := std["event"]; ok {
		t.Errorf("an invalid name must not be emitted as an event: %v", std)
	}
	if std["event.invalid"] != "Order Confirmed!" || std["k"] != "v" || std["level"] != "warn" {
		t.Errorf("invalid name is reported on the record itself: %v", std)
	}
	buf.Reset()
	log_.Event(context.Background(), slog.LevelInfo, strings.Repeat("a", 62)+".b", "exactly 64 bytes")
	if decode(t, buf)["event"] != strings.Repeat("a", 62)+".b" {
		t.Errorf("a 64-byte name is valid: %s", buf.String())
	}
	buf.Reset()
	log_.Event(context.Background(), slog.LevelInfo, strings.Repeat("a", 100)+".b", "long")
	if v := decode(t, buf)["event.invalid"].(string); len(v) > 64+len("…(truncated)") {
		t.Errorf("invalid name is bounded: %d", len(v))
	}
}

func TestValidEventName(t *testing.T) {
	ok := []string{"order.confirmed", "checkout.session_expired", "a.b", "inventory.receipt.applied", "svc2.event_1"}
	bad := []string{"", "order", "Order.confirmed", "order..confirmed", ".order", "order.", "order.Confirmed", "order confirmed", "order.1st", "order._x", "order-confirmed", "đơn.hàng", strings.Repeat("a", 63) + ".b"}
	for _, n := range ok {
		if !slogx.ValidEventName(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	for _, n := range bad {
		if slogx.ValidEventName(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
}

// Without a provider the facade reads the OTel global, which is a no-op
// until obsx installs one: stdout still works and nothing panics.
func TestOTLP_GlobalProviderDefault(t *testing.T) {
	buf := &bytes.Buffer{}
	slogx.New(slogx.Config{Stdout: buf}).Info(context.Background(), "x")
	if decode(t, buf)["message"] != "x" {
		t.Errorf("stdout: %s", buf.String())
	}
}

// A caller's own "event" attribute must not overwrite the catalog name. Two
// attributes with the same key are last-wins in every JSON parser and are
// deduplicated to the last on the OTLP record, so the caller's value would
// win on both sinks; it is renamed instead.
func TestEvent_CallerCannotOverrideTheCatalogName(t *testing.T) {
	lg, buf, exp := newPair(t, slogx.Config{})
	lg.Event(context.Background(), slog.LevelInfo, "order.confirmed", "m",
		slog.String("event", "order.cancelled"), slog.String("event.invalid", "x"))
	std := decode(t, buf)
	if std["event"] != "order.confirmed" || std["event.conflict"] != "order.cancelled" {
		t.Errorf("stdout: %v", std)
	}
	if strings.Count(buf.String(), `"event":`) != 1 {
		t.Errorf("duplicate JSON key: %s", buf.String())
	}
	recs := exp.records()
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	if a := attrs(recs[0]); a["event"] != "order.confirmed" || a["event.conflict"] != "order.cancelled" {
		t.Errorf("otlp: %v", a)
	}
}

// The envelope's keys belong to the platform at the top level: a caller
// attribute with one of those names would make the two sinks disagree about
// the same record. Nested uses are the caller's own field and survive.
func TestRedact_EnvelopeKeysAreReserved(t *testing.T) {
	lg, buf, exp := newPair(t, slogx.Config{})
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	lg.Info(ctx, "real",
		slog.String("trace_id", "deadbeefdeadbeefdeadbeefdeadbeef"), slog.String("span_id", "dead"),
		slog.String("level", "fatal"), slog.String("message", "forged"),
		slog.String("timestamp", "1970"), slog.String("caller", "x"), slog.Int("_slogx.dropped", 99),
		slog.Group("upstream", slog.String("trace_id", "a1b2")))
	std := decode(t, buf)
	if std["trace_id"] != tid.String() || std["span_id"] != sid.String() || std["level"] != "info" || std["message"] != "real" {
		t.Errorf("forged envelope reached stdout: %v", std)
	}
	if std["_slogx.dropped"] != float64(7) {
		t.Errorf("seven reserved keys dropped and counted: %v", std["_slogx.dropped"])
	}
	if std["upstream"].(map[string]any)["trace_id"] != "a1b2" {
		t.Errorf("a nested trace_id is the caller's own field: %v", std["upstream"])
	}
	a := attrs(exp.records()[0])
	if _, forged := a["trace_id"]; forged {
		t.Errorf("forged trace_id reached OTLP, where nothing overrides it: %v", a)
	}
	if a["upstream.trace_id"] != "a1b2" {
		t.Errorf("otlp nested: %v", a)
	}
}

// With a group open, the ids stay in the envelope where every stored query
// reads them, and the record's own attributes go inside the group.
func TestOTLP_GroupedLoggerKeepsIdsInTheEnvelope(t *testing.T) {
	lg, buf, exp := newPair(t, slogx.Config{})
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	lg.Slog().WithGroup("req").With("bound", "b").WithGroup("inner").InfoContext(ctx, "grouped", "k", "v")
	std := decode(t, buf)
	if std["trace_id"] != tid.String() || std["span_id"] != sid.String() {
		t.Errorf("ids must stay at the top level: %v", std)
	}
	req, _ := std["req"].(map[string]any)
	if req == nil || req["bound"] != "b" || req["inner"].(map[string]any)["k"] != "v" {
		t.Errorf("attributes must sit inside their groups: %v", std["req"])
	}
	r := exp.records()[0]
	if r.TraceID() != tid {
		t.Errorf("otlp record field: %v", r.TraceID())
	}
	if a := attrs(r); a["req.bound"] != "b" || a["req.inner.k"] != "v" {
		t.Errorf("otlp group is a map attribute: %v", a)
	}
}

// The severity NUMBER is the RFC's table and the field to query. The bridge
// writes severity TEXT from the standard library's Level.String(), which has
// no name for the two levels this package adds — pinned here so the
// divergence from stdout cannot drift unnoticed.
func TestOTLP_SeverityTextIsTheStdlibSpelling(t *testing.T) {
	lg, _, exp := newPair(t, slogx.Config{Level: "trace", NoSource: true, Exit: func(int) {}})
	ctx := context.Background()
	lg.Trace(ctx, "t")
	lg.Info(ctx, "i")
	lg.Fatal(ctx, "f")
	recs := exp.records()
	if len(recs) != 3 {
		t.Fatalf("records = %d", len(recs))
	}
	for i, want := range []struct {
		num  log.Severity
		text string
	}{{1, "DEBUG-4"}, {9, "INFO"}, {21, "ERROR+4"}} {
		if recs[i].Severity() != want.num || recs[i].SeverityText() != want.text {
			t.Errorf("record %d: %d/%q, want %d/%q", i, recs[i].Severity(), recs[i].SeverityText(), want.num, want.text)
		}
	}
}

// Fatal is the one record nothing runs after. With a batching processor —
// the production shape — it leaves the process only because Config.Flush is
// called before the exit.
func TestFatal_FlushesBeforeExit(t *testing.T) {
	for _, wired := range []bool{false, true} {
		exp := &memExporter{}
		lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)))
		t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
		var atExit int
		cfg := slogx.Config{
			Stdout: &bytes.Buffer{}, LoggerProvider: lp,
			Exit: func(int) { atExit = len(exp.records()) },
		}
		if wired {
			cfg.Flush = lp.ForceFlush
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // a crashing process often holds a cancelled context
		slogx.New(cfg).Fatal(ctx, "bootstrap failed")
		if wired && atExit != 1 {
			t.Errorf("with Flush wired the FATAL record must be exported before exit, got %d", atExit)
		}
		if !wired && atExit != 0 {
			t.Errorf("without Flush the batch is still queued, got %d", atExit)
		}
	}
}

// The design rests on the OTel global delegating to a provider installed
// later: services call New at startup and obsx.Setup after it. If that ever
// regressed, every production record would vanish with the suite still green.
func TestOTLP_GlobalProviderInstalledAfterNew(t *testing.T) {
	buf := &bytes.Buffer{}
	lg := slogx.New(slogx.Config{Stdout: buf}) // no provider: the global is a no-op
	exp := &memExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exp)))
	t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })
	global.SetLoggerProvider(lp) // process-global and irreversible, as obsx.Setup does
	lg.Info(context.Background(), "after setup")
	if len(exp.records()) != 1 || exp.records()[0].Body().AsString() != "after setup" {
		t.Fatalf("a provider installed after New must receive records: %d", len(exp.records()))
	}
	if decode(t, buf)["message"] != "after setup" {
		t.Errorf("stdout: %s", buf.String())
	}
}
