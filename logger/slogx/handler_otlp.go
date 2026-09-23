package slogx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
)

// scopeName is the OpenTelemetry instrumentation scope of every record this
// facade emits. It is the facade's own package path, never the service name:
// deployment identity already rides on the Resource as service.name, and
// RFC-0031 § Adoption asks an operator to recognise an adopted process by
// records whose ScopeName is exactly this string.
const scopeName = "github.com/duynhlab/pkg/logger/slogx"

// newHandler assembles the facade's handler chain:
//
//	redactHandler → levelHandler → fanout{ traceHandler → JSON stdout, otelslog }
//
// Redaction sits outermost so every sink receives one identically redacted
// record and the envelope's own attributes never spend its budget. The level
// gate is one Leveler shared by the Logger and every child, above the
// fanout, so a debug line an info-level service suppresses on stdout never
// leaves the pod over OTLP either — the OTLP handler's own Enabled (which
// asks the provider) is deliberately not consulted. The trace ids are
// stamped only on the stdout branch: the bridge takes them from the context
// itself, and a second copy as attributes would double them in ClickHouse.
//
// The OTLP handler reads the logger provider from the OTel global (installed
// by pkg/obsx; the global delegates, so New may run before obsx.Setup). With
// OTLP logs disabled the global is a no-op provider whose Enabled is false,
// and the fanout skips the branch without building a record. lp overrides the
// global for tests.
//
// Two renderings of one record are not byte-identical, and the differences
// are contract, not accident:
//   - severity: the OTLP record carries the RFC's severity NUMBER (trace 1 …
//     fatal 21), which is the field to query. Its severity TEXT comes from
//     the standard library's slog.Level.String(), which has no name for the
//     two levels this package adds and renders them "DEBUG-4" and "ERROR+4";
//     stdout spells all six (levelName).
//   - source: stdout shortens the caller to "parent/file.go:line"; the
//     bridge emits the compiler's path in code.file.path. Build service
//     images with -trimpath to keep that a module path.
//   - export failures: the bridge reports none — a failed OTLP export is
//     invisible here by design, because a log line must not fail the
//     operation that wrote it. Detect it on the collector's own metrics.
func newHandler(w io.Writer, level slog.Leveler, addSource bool, r *redactor, lp log.LoggerProvider) slog.Handler {
	stdout := traceHandler{next: slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		AddSource:   addSource,
		ReplaceAttr: replaceEnvelope,
	})}
	if lp == nil {
		lp = global.GetLoggerProvider()
	}
	otlp := otelslog.NewHandler(scopeName, otelslog.WithLoggerProvider(lp), otelslog.WithSource(addSource))
	return redactHandler{r: r, next: levelHandler{level: level, next: fanout{stdout, otlp}}}
}

// levelHandler owns the level gate for every sink below it. Handle enforces
// it as well as Enabled: the slog.Handler contract says a caller checks
// Enabled first, and Logger and slog.Logger both do, but Logger.Slog() hands
// this chain to third-party SDKs whose handlers this package does not own.
type levelHandler struct {
	level slog.Leveler
	next  slog.Handler
}

func (h levelHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h levelHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < h.level.Level() {
		return nil
	}
	return h.next.Handle(ctx, r)
}

func (h levelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return levelHandler{level: h.level, next: h.next.WithAttrs(attrs)}
}

func (h levelHandler) WithGroup(name string) slog.Handler {
	return levelHandler{level: h.level, next: h.next.WithGroup(name)}
}

// fanout hands one record to every sink that wants it. It has no gate of its
// own — the levelHandler above has already decided the record is emitted —
// but it honours each sink's Enabled, which is an AND with that gate and
// never an OR: a line stdout suppresses can still not leave over OTLP, while
// a record nobody is listening for costs no conversion. With OTLP logs off
// the bridge's Enabled is false and the branch is skipped entirely.
//
// Every sink gets its own Clone, so a sink that appends attributes cannot
// hand the next one its own (slog marks that with a "!BUG" attribute). Clone
// clips the record's backing array; it allocates nothing until a sink
// actually appends.
//
// A sink that panics is contained and reported, never propagated: the
// provider behind the OTLP branch is injected configuration, and the rule
// the redactor already follows applies to the whole facade — a log line must
// never take the process down. A sink that BLOCKS still blocks the caller;
// the OTLP side must therefore be wired with a batching processor, which
// drops rather than waits, and never a synchronous one.
type fanout []slog.Handler

func (f fanout) Enabled(context.Context, slog.Level) bool { return true }

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := safeHandle(ctx, h, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func safeHandle(ctx context.Context, h slog.Handler, r slog.Record) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("slogx: log sink panicked: %v", p)
		}
	}()
	return h.Handle(ctx, r)
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}
