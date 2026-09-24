package slogx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/duynhlab/pkg/logger/slogx"
)

type notFound struct{ id string }

func (e *notFound) Error() string { return "order " + e.id + " not found" }

type wrapper struct{ inner error }

func (w wrapper) Error() string { return "reserve: " + w.inner.Error() }
func (w wrapper) Unwrap() error { return w.inner }

type validationErrors []error

func (v validationErrors) Error() string   { return "invalid" }
func (v validationErrors) Unwrap() []error { return v }

type boomErr struct{}

func (boomErr) Error() string { panic("Error() exploded") }

// Err is the one shape for an error on a record: a stable type label and a
// message, both flat keys, the message scanned by the redaction boundary.
func TestErr_ShapeOnBothSinks(t *testing.T) {
	lg, buf, exp := newPair(t, slogx.Config{})
	base := &notFound{id: "8"}
	lg.Error(context.Background(), "could not read the order",
		slogx.Err(fmt.Errorf("fetch: %w", fmt.Errorf("db: %w", base))),
		slog.String("order_id", "8"))
	std := decode(t, buf)
	if std["error.type"] != "slogx_test.notFound" {
		t.Errorf("the stdlib wrappers must be unwrapped: %v", std["error.type"])
	}
	if std["exception.message"] != "fetch: db: order 8 not found" {
		t.Errorf("message: %v", std["exception.message"])
	}
	if _, nested := std["error"]; nested {
		t.Errorf("the keys must be flat, not a nested object: %v", std)
	}
	// Flat STRING attributes on the wire too: the log store indexes
	// attributes as a string map, and a group would arrive as one JSON blob.
	var gotType, gotMsg bool
	exp.records()[0].WalkAttributes(func(kv attribute.KeyValue) bool {
		switch string(kv.Key) {
		case "error.type":
			gotType = kv.Value.Type() == attribute.STRING && kv.Value.AsString() == "slogx_test.notFound"
		case "exception.message":
			gotMsg = kv.Value.Type() == attribute.STRING
		}
		return true
	})
	if !gotType || !gotMsg {
		t.Errorf("otlp must carry flat string attributes: type=%v message=%v", gotType, gotMsg)
	}
}

func TestErr_MessageIsRedactedAndNilIsFree(t *testing.T) {
	lg, buf, _ := newPair(t, slogx.Config{})
	lg.Error(context.Background(), "x",
		slogx.Err(fmt.Errorf("dial postgres://app:hunter2@db:5432/x: %w", errors.New("refused"))))
	if strings.Contains(buf.String(), "hunter2") {
		t.Errorf("error text is not redacted: %s", buf.String())
	}

	// A nil error, and the typed nil an "if err != nil" lets through, must
	// neither panic nor look like a dropped attribute.
	var typed *notFound
	buf.Reset()
	lg.Info(context.Background(), "hello", slogx.Err(nil), slogx.Err(typed), slog.String("k", "v"))
	std := decode(t, buf)
	if _, dropped := std["_slogx.dropped"]; dropped {
		t.Errorf("a nil error must not inflate the drop counter: %v", std)
	}
	if _, ok := std["error.type"]; ok || std["k"] != "v" {
		t.Errorf("a nil error contributes nothing: %v", std)
	}

	// An Error() that panics is caught here, not in the caller's goroutine.
	buf.Reset()
	lg.Error(context.Background(), "x", slogx.Err(boomErr{}))
	if std = decode(t, buf); !strings.Contains(std["exception.message"].(string), "panics") {
		t.Errorf("panicking Error(): %v", std["exception.message"])
	}
}

func TestErrorType(t *testing.T) {
	base := &notFound{id: "8"}
	other := errors.New("y")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"typed nil", (*notFound)(nil), "slogx_test.notFound"},
		{"stdlib string", other, "errors.errorString"},
		{"concrete", base, "slogx_test.notFound"},
		{"wrapped once", fmt.Errorf("a: %w", base), "slogx_test.notFound"},
		{"wrapped twice", fmt.Errorf("a: %w", fmt.Errorf("b: %w", base)), "slogx_test.notFound"},
		{"own wrapper wins", wrapper{inner: base}, "slogx_test.wrapper"},
		{"own wrapper behind stdlib", fmt.Errorf("a: %w", wrapper{inner: base}), "slogx_test.wrapper"},
		{"join names its first cause", errors.Join(base, other), "slogx_test.notFound"},
		{"multi-%w names its first cause", fmt.Errorf("a: %w %w", base, other), "slogx_test.notFound"},
		{"own aggregate names itself", validationErrors{base}, "slogx_test.validationErrors"},
		{"empty join", errors.Join(), ""},
	} {
		if got := slogx.ErrorType(tc.err); got != tc.want {
			t.Errorf("%s: ErrorType = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A chain that unwraps into itself must not hang the process.
type selfWrap struct{}

func (selfWrap) Error() string { return "loop" }
func (s selfWrap) Unwrap() error {
	return fmt.Errorf("again: %w", s)
}

func TestErrorType_BoundedOnACycle(t *testing.T) {
	done := make(chan string, 1)
	go func() { done <- slogx.ErrorType(fmt.Errorf("a: %w", selfWrap{})) }()
	select {
	case got := <-done:
		if got != "slogx_test.selfWrap" {
			t.Errorf("got %q", got)
		}
	case <-context.Background().Done():
	}
}

func TestErr_IsUsableFromABuffer(t *testing.T) {
	buf := &bytes.Buffer{}
	slogx.New(slogx.Config{Stdout: buf}).Error(context.Background(), "x", slogx.Err(errors.New("boom")))
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if m["exception.message"] != "boom" {
		t.Errorf("%v", m)
	}
}
