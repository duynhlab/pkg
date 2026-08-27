package temporalx

import (
	"strings"
	"testing"

	"go.temporal.io/sdk/client"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestWithLogger_BridgesToZap(t *testing.T) {
	core, observed := observer.New(zap.InfoLevel)

	var options client.Options
	WithLogger(zap.New(core))(&options)
	if options.Logger == nil {
		t.Fatal("WithLogger did not set client.Options.Logger")
	}

	options.Logger.Info("poller started", "TaskQueue", "checkout")
	entries := observed.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry through the zap core, got %d", len(entries))
	}
	e := entries[0]
	if e.Message != "poller started" {
		t.Errorf("message = %q, want %q", e.Message, "poller started")
	}
	fields := e.ContextMap()
	if got := fields["TaskQueue"]; got != "checkout" {
		t.Errorf("TaskQueue field = %v, want %q", got, "checkout")
	}
}

func TestWithLogger_NilLoggerFailsDial(t *testing.T) {
	if WithLogger(nil) != nil {
		t.Fatal("WithLogger(nil) should return a nil DialOption")
	}
	c, err := Dial(Config{HostPort: "127.0.0.1:1", Namespace: "mop"}, WithLogger(nil))
	if err == nil {
		c.Close()
		t.Fatal("expected an error for a nil DialOption, got nil")
	}
	if !strings.Contains(err.Error(), "nil DialOption") {
		t.Errorf("error %q does not mention the nil DialOption", err.Error())
	}
}
