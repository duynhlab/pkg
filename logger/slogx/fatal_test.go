package slogx

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// Fatal is the one method that ends the process. The exit hook is internal so
// the contract — a FATAL record, then exit status 1 — is testable without
// forking.
func TestFatal_LogsThenExitsWithOne(t *testing.T) {
	buf := &bytes.Buffer{}
	code := -1
	log := New(Config{Stdout: buf, Exit: func(c int) { code = c }})

	log.Fatal(context.Background(), "bootstrap failed")

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), `"level":"fatal"`) {
		t.Errorf("no fatal record before exit: %s", buf.String())
	}
	// Fatal → log → Callers(3): the caller must be this test, not the facade.
	if !strings.Contains(buf.String(), `"caller":"slogx/fatal_test.go:`) {
		t.Errorf("fatal record does not name the caller: %s", buf.String())
	}
}

func TestLevelName_Boundaries(t *testing.T) {
	cases := map[string]string{"trace": "trace", "debug": "debug", "info": "info", "warn": "warn", "warning": "warn", "error": "error", "": "info", " INFO ": "info", "nonsense": "info"}
	for in, want := range cases {
		if got := levelName(parseLevel(in)); got != want {
			t.Errorf("parseLevel(%q) → %q, want %q", in, got, want)
		}
	}
	if levelName(LevelFatal) != "fatal" || levelName(LevelFatal+4) != "fatal" || levelName(LevelTrace-4) != "trace" {
		t.Error("levelName must clamp beyond the six names")
	}
}
