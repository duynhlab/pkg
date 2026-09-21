// Package slogx is the platform's application logging facade (RFC-0031,
// ADR-070): one context-first API over the standard library's slog that renders
// each record to stdout as the platform JSON envelope. The remaining halves of
// the contract land in later changes of the same task: redaction before every
// sink, the OTLP sink through the OpenTelemetry slog bridge, and Event for the
// reviewed catalog of named records. Until they land no service is cut over.
//
// A service imports this package and nothing else for logging. It never
// constructs providers or exporters: the OTel logger provider is installed by
// pkg/obsx and read from the OTel global, so this module depends on the OTel
// API only and can be used by every layer.
//
// The facade is deliberately narrow. Every emission takes a context so the
// active span's ids reach the record without business code building them, and
// Fatal is the one method that ends the process, for bootstrap failures only.
//
// The caller recorded on a line is the function that called the facade. A
// service helper that wraps Info or Error therefore shows up as the caller,
// exactly as it did with zap without AddCallerSkip; the fleet has no such
// wrappers today, so there is no skip option.
package slogx

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"time"
)

// Config drives New. Zero values are production defaults: info level, stdout,
// caller locations on.
type Config struct {
	// Level is the LOG_LEVEL contract value: debug|info|warn|error (trace is
	// accepted for local investigation). Unknown values mean info.
	Level string
	// Service names the instrumentation scope of the OTLP records. Typically
	// the service name; used once the OTLP handler is attached.
	Service string
	// Stdout receives the JSON envelope. nil means os.Stdout; tests inject a
	// buffer.
	Stdout io.Writer
	// NoSource omits the caller location. The default (false) matches zapx,
	// which always emitted `caller`.
	NoSource bool
	// Exit is what Fatal calls after the record is written. nil means
	// os.Exit; a bootstrap test injects a recorder to assert the FATAL path
	// without forking the process.
	Exit func(int)
	// Redact is the privacy boundary applied before every sink. The zero
	// value means DefaultRedactPolicy — the ADR-071 deny list and bounds.
	// Services widen it only by adding keys; there is no way to turn it off.
	Redact RedactPolicy
}

// Logger is the facade. Construct it with New; the zero value is not usable.
// It is safe for concurrent use; With returns children that share the level.
type Logger struct {
	h     slog.Handler
	level *slog.LevelVar
	exit  func(int)
}

// New builds a Logger from cfg. It never fails: a misconfigured level means
// info, a nil writer means stdout.
func New(cfg Config) *Logger {
	w := cfg.Stdout
	if w == nil {
		w = os.Stdout
	}
	exit := cfg.Exit
	if exit == nil {
		exit = os.Exit
	}
	level := &slog.LevelVar{}
	level.Set(parseLevel(cfg.Level))
	return &Logger{
		h:     newStdoutHandler(w, level, !cfg.NoSource, compile(cfg.Redact)),
		level: level,
		exit:  exit,
	}
}

// SetLevel changes the level at runtime for this logger and every child made
// with With — the zap.AtomicLevel parity an operator expects from LOG_LEVEL
// hot-reload. Unknown values mean info.
func (l *Logger) SetLevel(level string) {
	l.level.Set(parseLevel(level))
}

// Trace emits fine-grained diagnostic evidence. Disabled unless Level is
// "trace".
func (l *Logger) Trace(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.log(ctx, LevelTrace, msg, attrs)
}

// Debug emits troubleshooting evidence.
func (l *Logger) Debug(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.log(ctx, slog.LevelDebug, msg, attrs)
}

// Info emits an expected lifecycle or decision record.
func (l *Logger) Info(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.log(ctx, slog.LevelInfo, msg, attrs)
}

// Warn emits a handled degradation or unexpected condition.
func (l *Logger) Warn(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.log(ctx, slog.LevelWarn, msg, attrs)
}

// Error emits a final failed operation or exhausted retry.
func (l *Logger) Error(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.log(ctx, slog.LevelError, msg, attrs)
}

// Fatal emits a FATAL record and ends the process with exit status 1. It is
// for bootstrap failure only — after shutdown and flush have been attempted —
// never for a handler, workflow, activity or business decision. Stdout is
// unbuffered; the OTLP side is flushed by obsx.Shutdown, which the caller
// runs before reaching here.
func (l *Logger) Fatal(ctx context.Context, msg string, attrs ...slog.Attr) {
	l.log(ctx, LevelFatal, msg, attrs)
	l.exit(1)
}

// Enabled reports whether records at level would be emitted.
func (l *Logger) Enabled(ctx context.Context, level slog.Level) bool {
	return l.h.Enabled(ctx, level)
}

// With returns a Logger that adds attrs to every record. Bind identity that
// is fixed for the logger's lifetime (component, worker name); never bind a
// request context — the ids come from the ctx passed to each call.
func (l *Logger) With(attrs ...slog.Attr) *Logger {
	if len(attrs) == 0 {
		return l
	}
	return &Logger{h: l.h.WithAttrs(attrs), level: l.level, exit: l.exit}
}

// Slog exposes the facade as a *slog.Logger for SDK bridges that require one
// (the Temporal SDK's structured logger, HTTP router recovery hooks). It is
// not for application code: the returned logger accepts calls without a
// context, and a record logged without one loses its trace correlation.
func (l *Logger) Slog() *slog.Logger {
	return slog.New(l.h)
}

// log builds the record with the CALLER's source location. slog.Logger's own
// methods would attribute every line to this file, so the record is built by
// hand: skip runtime.Callers, log, and the exported method that called it.
func (l *Logger) log(ctx context.Context, level slog.Level, msg string, attrs []slog.Attr) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !l.h.Enabled(ctx, level) {
		return
	}
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:])
	r := slog.NewRecord(time.Now(), level, msg, pcs[0])
	r.AddAttrs(attrs...)
	// A write error on stdout has nowhere better to go than stdout; zap made
	// the same choice.
	_ = l.h.Handle(ctx, r)
}

type ctxKey struct{}

// WithContext returns a context carrying l, for code paths that receive a
// context but no logger.
func WithContext(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the Logger attached with WithContext, or the process
// default when none is present — a request that lost its logger still logs,
// it just logs without the bound identity. main() installs the configured
// logger as the default with SetDefault; before that the default is info
// level on stdout.
func FromContext(ctx context.Context) *Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(*Logger); ok && l != nil {
			return l
		}
	}
	return defaultLogger()
}

// SetDefault installs l as the logger FromContext falls back to. Call it once
// in main() with the configured logger so a lost context does not downgrade a
// LOG_LEVEL=error service to info-level lines.
func SetDefault(l *Logger) {
	if l == nil {
		return
	}
	defaultMu.Lock()
	defaultL = l
	defaultMu.Unlock()
}

var (
	defaultMu sync.RWMutex
	defaultL  *Logger
)

func defaultLogger() *Logger {
	defaultMu.RLock()
	l := defaultL
	defaultMu.RUnlock()
	if l != nil {
		return l
	}
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultL == nil {
		defaultL = New(Config{})
	}
	return defaultL
}
