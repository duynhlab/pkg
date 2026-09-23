package grpcx

import (
	"reflect"
	"unicode/utf8"
)

// Bounds for a recovered panic. A stack is worth its bytes on the rare record
// that carries one, but a deep one runs to tens of kilobytes and the record may
// be kept for months; the message is a display phrase, not a dump.
const (
	maxPanicMessage = 256
	maxPanicStack   = 4096
	truncatedMarker = "…(truncated)"
)

// panicMessage renders a recovered panic without rendering the panic VALUE:
// the type is named, and only a string or error payload contributes text. A %v
// of an arbitrary value prints every field of a struct, unexported ones
// included — the fields its author hid — and with a plain slog handler nothing
// downstream would redact them. The same rule the logging facade applies.
func panicMessage(r any) string {
	msg := "!PANIC (" + reflect.TypeOf(r).String() + ")"
	switch v := r.(type) {
	case string:
		msg += ": " + v
	case error:
		msg += ": " + safeErrorText(v)
	}
	return bound(msg, maxPanicMessage)
}

// safeErrorText calls Error() on a value whose Error() may itself panic.
func safeErrorText(err error) (s string) {
	defer func() {
		if recover() != nil {
			s = "error whose Error() panics"
		}
	}()
	return err.Error()
}

// bound cuts s to at most limit bytes on a rune boundary and marks the cut, so a
// reader can tell the value is not whole and no multi-byte rune is split.
func bound(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMarker
}
