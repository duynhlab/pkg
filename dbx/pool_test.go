package dbx

import (
	"context"
	"math"
	"testing"
	"time"
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

// WithPasswordFile records the path; an empty path is left empty so the hook is
// never wired and DSN-password callers are unaffected. Pure option logic.
func TestWithPasswordFile(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"path set", "/etc/db/secret/password", "/etc/db/secret/password"},
		{"empty stays empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c config
			WithPasswordFile(tt.in)(&c)
			if c.passwordFile != tt.want {
				t.Errorf("WithPasswordFile(%q) => %q, want %q", tt.in, c.passwordFile, tt.want)
			}
		})
	}
}

// WithMaxConnLifetime keeps a positive duration and ignores <= 0 (both guard
// branches), so a bad value falls back to the default rather than a zero or
// negative lifetime.
func TestWithMaxConnLifetime_Clamp(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"positive kept", 45 * time.Minute, 45 * time.Minute},
		{"zero ignored", 0, 0},
		{"negative ignored", -time.Second, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c config
			WithMaxConnLifetime(tt.in)(&c)
			if c.maxConnLifetime != tt.want {
				t.Errorf("WithMaxConnLifetime(%v) => %v, want %v", tt.in, c.maxConnLifetime, tt.want)
			}
		})
	}
}
