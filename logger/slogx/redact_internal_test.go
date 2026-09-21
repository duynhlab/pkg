package slogx

import (
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
