package httpmw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
)

// traceIDFromContext returns the active span's trace id, or "" when there is no
// span. This is a direct read of the OTel API, which every module may import.
func traceIDFromContext(ctx context.Context) string {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if sc.HasTraceID() {
		return sc.TraceID().String()
	}
	return ""
}

const (
	// TraceIDHeader is the response header carrying the correlation id back to
	// the client.
	TraceIDHeader = "X-Trace-ID"
	// TraceParentHeader is the W3C Trace Context request header.
	TraceParentHeader = "traceparent"

	ctxKeyTraceID = "trace_id"
	ctxKeyLogger  = "logger"
)

// TraceID returns the correlation id for a request, preferring the active
// span's trace id, then an inbound traceparent, then X-Trace-ID, and finally a
// freshly generated id.
//
// The generated fallback is a CLIENT contract, not telemetry: it lets a caller
// correlate by header even when nothing is sampled. It must never be logged as
// trace_id — see Logging, which only ever logs the span's id.
func TraceID(c *gin.Context) string {
	if id := traceIDFromContext(c.Request.Context()); id != "" {
		return id
	}
	if tp := c.GetHeader(TraceParentHeader); tp != "" {
		// traceparent is version-traceid-parentid-flags.
		if parts := strings.Split(tp, "-"); len(parts) >= 2 && parts[1] != "" {
			return parts[1]
		}
	}
	if id := c.GetHeader(TraceIDHeader); id != "" {
		return id
	}
	return generateTraceID()
}

func generateTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b)
}

// Logging returns the platform's HTTP access-log middleware: one record per
// served request, at a level chosen by status class, carrying the canonical
// semantic attributes and nothing request-derived beyond them (RFC-0031 §
// Canonical attributes, ADR-071).
//
// The record carries http.request.method, http.route — the matched,
// low-cardinality route template, omitted when nothing matched rather than
// replaced by the raw path — http.response.status_code, and error.type for a
// server failure. It deliberately carries no raw path or query, client address,
// User-Agent, or duration: the path and the query leak identifiers, the address
// and User-Agent are personal data, and the span and the RED histogram already
// measure the duration exactly.
//
// Correlation comes from the context, not from a bound field: the record is
// written with the request's context, so a context-aware handler (the platform
// facade on stdout, the OTel bridge on OTLP) stamps the trace and span ids
// itself. Mount it after Tracing so the span exists.
//
// extraSkipRoutes must match what was passed to Tracing. Both read
// DefaultSkipRoutes, so the two skip lists agree by construction; the extras are
// the only part a caller can get out of step.
func Logging(logger *slog.Logger, extraSkipRoutes ...string) gin.HandlerFunc {
	if logger == nil {
		logger = discard
	}
	skip := skipper(extraSkipRoutes...)

	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// The response header is a CLIENT contract, not telemetry: it lets a
		// caller correlate by header even when nothing is sampled, so it falls
		// back to an inbound or generated id. That fallback never reaches a log
		// record — only the span's id does, and the handler takes it from ctx.
		headerTraceID := traceIDFromContext(ctx)
		if headerTraceID == "" {
			headerTraceID = TraceID(c)
		}
		c.Set(ctxKeyTraceID, headerTraceID)
		c.Header(TraceIDHeader, headerTraceID)
		c.Set(ctxKeyLogger, logger)

		c.Next()

		status := c.Writer.Status()

		// Routine SUCCESSFUL probes are traffic about the platform, not the
		// domain — Tracing excludes them from spans and RED metrics through this
		// same skip list, and excluding them here is what makes that contract
		// true for logs too. A FAILING probe is always kept: that is the one time
		// a probe is worth reading.
		if skip(c) && status < 400 {
			return
		}

		attrs := make([]slog.Attr, 0, 4)
		attrs = append(attrs, slog.String("http.request.method", c.Request.Method))
		if route := c.FullPath(); route != "" {
			attrs = append(attrs, slog.String("http.route", route))
		}
		attrs = append(attrs, slog.Int("http.response.status_code", status))
		if status >= 500 {
			// The semantic conventions name a server failure without an
			// exception by its status code, as a string.
			attrs = append(attrs, slog.String("error.type", strconv.Itoa(status)))
		}
		logger.LogAttrs(ctx, levelFor(status), "HTTP request", attrs...)
	}
}

// levelFor is the access-outcome severity: Error for a server failure, Warn for
// a client error, Info otherwise. One record per request, never an Info and an
// Error pair for the same call.
func levelFor(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// discard is the logger used when none was given, and the fallback LoggerFrom
// returns when Logging was not mounted: silent, so a missing middleware shows up
// as missing logs rather than as a second, uncorrelated stream.
var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// LoggerFrom returns the logger Logging was given. It carries no request state:
// write with the request context (logger.InfoContext(c.Request.Context(), …))
// and a context-aware handler adds the trace and span ids. Falls back to a
// silent logger when Logging was not mounted.
func LoggerFrom(c *gin.Context) *slog.Logger {
	if v, ok := c.Get(ctxKeyLogger); ok {
		if l, ok := v.(*slog.Logger); ok {
			return l
		}
	}
	return discard
}
