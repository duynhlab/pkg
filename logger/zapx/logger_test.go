package zapx

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  zapcore.Level
	}{
		{"debug", "debug", zapcore.DebugLevel},
		{"warn", "warn", zapcore.WarnLevel},
		{"error", "error", zapcore.ErrorLevel},
		{"info explicit", "info", zapcore.InfoLevel},
		{"unknown defaults to info", "verbose", zapcore.InfoLevel},
		{"empty defaults to info", "", zapcore.InfoLevel},
		{"mixed case and surrounding spaces", "  DeBuG ", zapcore.DebugLevel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseLevel(tt.input); got != tt.want {
				t.Errorf("parseLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNew(t *testing.T) {
	t.Parallel()
	for _, level := range []string{"debug", "info", "warn", "error", "nonsense"} {
		t.Run(level, func(t *testing.T) {
			t.Parallel()
			l, err := New(level)
			if err != nil {
				t.Fatalf("New(%q) error = %v", level, err)
			}
			if l == nil {
				t.Fatalf("New(%q) returned a nil logger", level)
			}
			_ = l.Sync() // best-effort flush; stderr sync can error on some platforms
		})
	}
}

// New must apply the requested level so lower-severity logs are dropped.
func TestNewAppliesLevel(t *testing.T) {
	t.Parallel()
	l, err := New("error")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if l.Core().Enabled(zapcore.InfoLevel) {
		t.Error("info should be disabled when the logger is built at error level")
	}
	if !l.Core().Enabled(zapcore.ErrorLevel) {
		t.Error("error level should be enabled")
	}
}

func TestContextRoundTrip(t *testing.T) {
	t.Parallel()

	// No logger attached -> the global zap logger (never nil).
	if got := FromContext(context.Background()); got == nil {
		t.Fatal("FromContext(empty) = nil, want the global logger")
	}

	l := zap.NewNop()
	ctx := WithContext(context.Background(), l)
	if got := FromContext(ctx); got != l {
		t.Errorf("FromContext after WithContext = %p, want %p", got, l)
	}
}
