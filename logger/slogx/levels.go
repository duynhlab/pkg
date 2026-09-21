package slogx

import (
	"log/slog"
	"strings"
)

// Levels beyond slog's four. The OTel bridge maps slog levels to severity
// numbers with a fixed offset (Debug −4 → 5, Info 0 → 9, Warn 4 → 13, Error 8
// → 17), so these two land exactly on the RFC-0031 table: TRACE → 1, FATAL →
// 21. They are deliberately the only additions — the facade has six levels,
// not a ladder.
const (
	// LevelTrace is fine-grained diagnostic evidence, disabled by default.
	LevelTrace = slog.Level(-8)
	// LevelFatal is reserved for bootstrap failure after shutdown/flush has
	// been attempted. Logger.Fatal exits the process; nothing else may.
	LevelFatal = slog.Level(12)
)

// levelName renders a level the way the stdout contract spells it: lowercase,
// one word, the platform's six names. slog's own String() would print
// "DEBUG-4" for TRACE and "ERROR+4" for FATAL.
func levelName(l slog.Level) string {
	switch {
	case l < slog.LevelDebug:
		return "trace"
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	case l < LevelFatal:
		return "error"
	default:
		return "fatal"
	}
}

// parseLevel maps the LOG_LEVEL contract (debug|info|warn|error, case- and
// whitespace-insensitive; "warning" and "trace" are accepted as well) to a
// slog level. Unknown values mean info — the same default zapx used, so a typo
// can never silence a service.
func parseLevel(s string) slog.Level {
	switch normalize(s) {
	case "trace":
		return LevelTrace
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
