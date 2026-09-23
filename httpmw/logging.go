package httpmw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
)

const (
	// TraceIDHeader is the response header carrying the correlation id back to
	// the client.
	TraceIDHeader = "X-Trace-ID"
	// TraceParentHeader is the W3C Trace Context request header.
	TraceParentHeader = "traceparent"

	ctxKeyLogger = "logger"
)

// TraceID returns the correlation id for a request, preferring the active
// span's trace id, then an inbound traceparent, then X-Trace-ID, and finally a
// freshly generated id.
//
// The generated fallback is a CLIENT contract, not telemetry: it lets a caller
// correlate by header even when nothing is sampled. It is never logged as a
// trace id — the access record takes its ids from the span, through the
// context. An inbound value is echoed only when it is a well-formed 32-hex trace
// id, so a client cannot bounce an arbitrary string off the response header.
func TraceID(c *gin.Context) string {
	if sc := trace.SpanFromContext(c.Request.Context()).SpanContext(); sc.HasTraceID() {
		return sc.TraceID().String()
	}
	if tp := c.GetHeader(TraceParentHeader); tp != "" {
		// traceparent is version-traceid-parentid-flags.
		if parts := strings.Split(tp, "-"); len(parts) >= 2 && isTraceID(parts[1]) {
			return parts[1]
		}
	}
	if id := c.GetHeader(TraceIDHeader); isTraceID(id) {
		return id
	}
	return generateTraceID()
}

// isTraceID reports a W3C trace id: 32 lowercase hex digits, not all zero.
func isTraceID(s string) bool {
	if len(s) != 32 || s == "00000000000000000000000000000000" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
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
// The record carries http.request.method (normalised as the semantic
// conventions define it: the nine standard methods, anything else "_OTHER" —
// the value the span carries, and not a field a client can fill), http.route —
// the matched, low-cardinality route template, omitted when nothing matched
// rather than replaced by the raw path — http.response.status_code, and
// error.type for a server failure. It deliberately carries no raw path or
// query, client address, User-Agent, or duration: the path and the query leak
// identifiers, the address and User-Agent are personal data, and the span and
// the RED histogram already measure the duration exactly.
//
// Correlation comes from the context, not from a bound field: the record is
// written with the request's context, so a context-aware handler (the platform
// facade on stdout, the OTel bridge on OTLP) stamps the trace and span ids
// itself. Mount it after Tracing so the span exists, and mount Recovery after
// it so a panicking request still gets its summary.
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
		c.Header(TraceIDHeader, TraceID(c))
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
		attrs = append(attrs, slog.String("http.request.method", normalizeMethod(c.Request.Method)))
		if route := c.FullPath(); route != "" {
			attrs = append(attrs, slog.String("http.route", route))
		}
		attrs = append(attrs, slog.Int("http.response.status_code", status))
		if status >= 500 {
			// The semantic conventions name a server failure that carries no
			// exception by its status code, as a string. The pinned otelgin puts
			// no such value on the span — it marks the span Error and sets
			// error.type only from gin's c.Errors — so this is the convention's
			// value, not a copy of the span's.
			attrs = append(attrs, slog.String("error.type", strconv.Itoa(status)))
		}
		logger.LogAttrs(c.Request.Context(), levelFor(status), "HTTP request", attrs...)
	}
}

// normalizeMethod maps a request method onto the semantic conventions' set: the
// nine standard methods as they are, anything else "_OTHER". net/http accepts
// any token as a method, of any length, so the raw value is a field a client
// fills; the pinned otelgin applies the same rule on the span.
func normalizeMethod(m string) string {
	switch m {
	case "GET", "HEAD", "POST", "PUT", "DELETE", "CONNECT", "OPTIONS", "TRACE", "PATCH":
		return m
	}
	return "_OTHER"
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
var discard = slog.New(slog.DiscardHandler)

// LoggerFrom returns the logger Logging was given, bound to the request: a call
// that carries no span of its own — logger.Info(msg), the shape every handler
// wrote with zap — is still written with the request's context, so it keeps
// the trace and span ids. A call that passes a context with a span of its own
// (a child span a handler started) keeps that one. Falls back to a silent
// logger when Logging was not mounted.
func LoggerFrom(c *gin.Context) *slog.Logger {
	v, ok := c.Get(ctxKeyLogger)
	if !ok {
		return discard
	}
	l, ok := v.(*slog.Logger)
	if !ok || l == nil {
		return discard
	}
	return slog.New(requestBound{next: l.Handler(), req: c.Request.Context()})
}

// requestBound supplies the request context to a record whose own context has
// no span.
type requestBound struct {
	next slog.Handler
	req  context.Context
}

func (h requestBound) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(h.pick(ctx), l)
}

func (h requestBound) Handle(ctx context.Context, r slog.Record) error {
	return h.next.Handle(h.pick(ctx), r)
}

func (h requestBound) WithAttrs(attrs []slog.Attr) slog.Handler {
	return requestBound{next: h.next.WithAttrs(attrs), req: h.req}
}

func (h requestBound) WithGroup(name string) slog.Handler {
	return requestBound{next: h.next.WithGroup(name), req: h.req}
}

func (h requestBound) pick(ctx context.Context) context.Context {
	if ctx != nil && trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	return h.req
}
