package dbx

import (
	"context"
	"math"
	"testing"
)

// A DSN pgx cannot parse must surface as an error, not a panic or a nil pool.
func TestNewPool_InvalidDSN(t *testing.T) {
	_, err := NewPool(context.Background(), "postgres://user@host:notaport/db")
	if err == nil {
		t.Fatal("expected error for invalid DSN, got nil")
	}
}

// WithMaxConns keeps a valid size and ignores out-of-range values (both clamp
// branches), so a bad config falls back to the pgx default rather than a bogus
// pool size. Pure option logic — no database needed.
func TestWithMaxConns_Clamp(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int32
	}{
		{"valid", 7, 7},
		{"zero ignored", 0, 0},
		{"negative ignored", -1, 0},
		{"overflow ignored", math.MaxInt32 + 1, 0},
		{"max int32 kept", math.MaxInt32, math.MaxInt32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c config
			WithMaxConns(tt.in)(&c)
			if c.maxConns != tt.want {
				t.Errorf("WithMaxConns(%d) => maxConns %d, want %d", tt.in, c.maxConns, tt.want)
			}
		})
	}
}
