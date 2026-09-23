package slogx

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// The regexp scanPAN replaced, kept as the oracle: a 13–19 digit run with
// optional single separators on word boundaries, Luhn- and IIN-checked.
var oraclePAN = regexp.MustCompile(`\b(?:\d[ .\-_]?){12,18}\d\b`)

func oracle(s string) string {
	return oraclePAN.ReplaceAllStringFunc(s, func(m string) string {
		if looksLikePAN(m) {
			return redacted
		}
		return m
	})
}

// scanPAN must redact at least everything the oracle redacts (a leftover
// digit run the oracle would have caught is a leak), and must not touch a
// string the oracle leaves alone.
func TestScanPAN_Differential(t *testing.T) {
	rng := rand.New(rand.NewSource(20260921))
	pans := []string{"4111111111111111", "5500000000000004", "6011111111111117", "378282246310005", "4111 1111 1111 1111", "4111-1111-1111-1111"}
	junk := []string{"order", "ref", "id_", "x", "abc", "2026-09-21", "12:30", "a11ce000-0000-4000-8000-000000000001", "1789959127000000000", "4111111111111112", "12345", "7", "", "/", "="}
	seps := []string{" ", "", "-", ".", "_", ", ", "\n"}
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for n := rng.Intn(5) + 1; n > 0; n-- {
			if rng.Intn(3) == 0 {
				b.WriteString(pans[rng.Intn(len(pans))])
			} else {
				b.WriteString(junk[rng.Intn(len(junk))])
			}
			b.WriteString(seps[rng.Intn(len(seps))])
		}
		in := b.String()
		got, want := scanPAN(in), oracle(in)
		if got == want {
			continue
		}
		// Not byte-identical: acceptable only when ours redacted MORE, i.e.
		// no digit run the oracle would redact survives in ours.
		if oracle(got) != got {
			t.Fatalf("leak vs oracle\n in   %q\n got  %q\n want %q", in, got, want)
		}
		if !strings.Contains(in, "4111") && !strings.Contains(in, "5500") && !strings.Contains(in, "6011") && !strings.Contains(in, "3782") {
			t.Fatalf("redacted where the oracle would not\n in   %q\n got  %q\n want %q", in, got, want)
		}
	}
}

func TestScanKV_NoAllocOnNoMatch(t *testing.T) {
	r := compile(RedactPolicy{})
	in := "rpc error: code = NotFound desc = order 42 not found at 10:30"
	if out := r.scan(in); out != in {
		t.Fatalf("changed: %q", out)
	}
	if n := testing.AllocsPerRun(100, func() { _ = r.scan(in) }); n != 0 {
		t.Errorf("scan of a benign string allocates %v times", n)
	}
}

type failingSink struct{ err error }

func (f failingSink) Enabled(context.Context, slog.Level) bool  { return true }
func (f failingSink) Handle(context.Context, slog.Record) error { return f.err }
func (f failingSink) WithAttrs([]slog.Attr) slog.Handler        { return f }
func (f failingSink) WithGroup(string) slog.Handler             { return f }

type countingSink struct{ n *int }

func (c countingSink) Enabled(context.Context, slog.Level) bool  { return true }
func (c countingSink) Handle(context.Context, slog.Record) error { *c.n++; return nil }
func (c countingSink) WithAttrs([]slog.Attr) slog.Handler        { return c }
func (c countingSink) WithGroup(string) slog.Handler             { return c }

// A failing sink does not starve the others, and every failure is reported.
func TestFanout_DeliversToAllAndJoinsErrors(t *testing.T) {
	n := 0
	e1, e2 := errors.New("otlp down"), errors.New("disk full")
	f := fanout{failingSink{e1}, countingSink{&n}, failingSink{e2}}
	err := f.WithAttrs([]slog.Attr{slog.String("k", "v")}).WithGroup("g").Handle(context.Background(), slog.Record{})
	if n != 1 || !errors.Is(err, e1) || !errors.Is(err, e2) {
		t.Errorf("n=%d err=%v", n, err)
	}
	if !f.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("fanout has no gate of its own")
	}
}

type disabledSink struct{ n *int }

func (d disabledSink) Enabled(context.Context, slog.Level) bool  { return false }
func (d disabledSink) Handle(context.Context, slog.Record) error { *d.n++; return nil }
func (d disabledSink) WithAttrs([]slog.Attr) slog.Handler        { return d }
func (d disabledSink) WithGroup(string) slog.Handler             { return d }

type panickingSink struct{}

func (panickingSink) Enabled(context.Context, slog.Level) bool  { return true }
func (panickingSink) Handle(context.Context, slog.Record) error { panic("provider exploded") }
func (panickingSink) WithAttrs([]slog.Attr) slog.Handler        { return panickingSink{} }
func (panickingSink) WithGroup(string) slog.Handler             { return panickingSink{} }

// A sink that is switched off costs no record conversion, and a sink that
// panics is reported rather than propagated — a log line must never take the
// process down, and the sinks after it still get the record.
func TestFanout_SkipsDisabledSinksAndContainsPanics(t *testing.T) {
	disabled, delivered := 0, 0
	f := fanout{disabledSink{&disabled}, panickingSink{}, countingSink{&delivered}}
	err := f.Handle(context.Background(), slog.Record{Level: slog.LevelInfo})
	if disabled != 0 {
		t.Errorf("a sink whose Enabled is false must not be handed the record")
	}
	if delivered != 1 {
		t.Errorf("sinks after a panicking one still receive it: %d", delivered)
	}
	if err == nil || !strings.Contains(err.Error(), "sink panicked") {
		t.Errorf("the panic must be reported as an error: %v", err)
	}
}

// The gate is enforced in Handle too, for SDKs that take the handler from
// Slog() and call it without asking Enabled.
func TestLevelHandler_HandleEnforcesTheGate(t *testing.T) {
	n := 0
	lv := &slog.LevelVar{}
	lv.Set(slog.LevelError)
	h := levelHandler{level: lv, next: countingSink{&n}}
	_ = h.Handle(context.Background(), slog.Record{Level: slog.LevelDebug})
	if n != 0 {
		t.Errorf("a record below the level must not reach the sinks")
	}
	_ = h.Handle(context.Background(), slog.Record{Level: slog.LevelError})
	if n != 1 {
		t.Errorf("a record at the level must: %d", n)
	}
}
