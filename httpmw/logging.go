package httpmw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// traceContextFieldKey names the carrier field. It never appears in output: the
// otelzap bridge consumes it by Interface type-assertion and every zap encoder
// skips SkipType. The key exists only for debuggability.
const traceContextFieldKey = "otel.trace_context"

// traceContextField binds ctx to a log call so the otelzap bridge stamps the
// OTLP record with native trace_id/span_id — the semconv log-to-trace link,
// stronger than a hand-added string field.
//
// The field is SkipType carrying ctx as its Interface: the bridge finds it via
// field.Interface.(context.Context) (otelzap core.go convertField), while every
// zap encoder skips SkipType, so the raw context never pollutes stdout under a
// zapcore.NewTee(stdout, obs.ZapCore(...)) fan-out.
//
// obsx.TraceContext is the same five lines and stays the public API services
// call directly. This copy is deliberate and unexported: pkg is thirteen
// independent modules with no cross-module edge, and importing obsx from here
// would be the first — buying a tag-ordering dance on every release to save
// five lines. TestTraceContextField_IsSkipTypeCarryingContext pins the shape, so
// if otelzap ever changes how it detects the field, this copy fails on its own
// rather than drifting quietly.
func traceContextField(ctx context.Context) zap.Field {
	if ctx == nil {
		return zap.Skip()
	}
	return zap.Field{Key: traceContextFieldKey, Type: zapcore.SkipType, Interface: ctx}
}

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

// Logging returns the platform's HTTP access-log middleware: one structured
// record per request, at a level chosen by status class.
//
// Mount it after Tracing so the active span exists and its id can be bound.
//
// extraSkipRoutes must match what was passed to Tracing. Both read
// DefaultSkipRoutes, so the two skip lists agree by construction; the extras are
// the only part a caller can get out of step.
func Logging(logger *zap.Logger, extraSkipRoutes ...string) gin.HandlerFunc {
	skip := skipper(extraSkipRoutes...)

	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		method := c.Request.Method
		ctx := c.Request.Context()

		// The span's trace id is the ONLY id that may reach telemetry. TraceID's
		// generated fallback never consults the span, so logging it would let a
		// record advertise an id that is not the trace id — searching by it finds
		// nothing in the backend even when a trace exists. Probes have no span by
		// design, so their records carry no trace_id.
		spanTraceID := traceIDFromContext(ctx)

		headerTraceID := spanTraceID
		if headerTraceID == "" {
			headerTraceID = TraceID(c)
		}
		c.Set(ctxKeyTraceID, headerTraceID)
		c.Header(TraceIDHeader, headerTraceID)

		// The request logger always carries the trace CONTEXT so the otelzap
		// bridge stamps native trace_id/span_id on every OTLP record. The
		// readable string field is bound only when a span exists.
		fields := []zap.Field{traceContextField(ctx)}
		if spanTraceID != "" {
			fields = append(fields, zap.String("trace_id", spanTraceID))
		}
		reqLogger := logger.With(fields...)
		c.Set(ctxKeyLogger, reqLogger)

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

		logByStatus(reqLogger, status, []zap.Field{
			zap.String("method", method),
			zap.String("path", path),
			zap.Int("status", status),
			zap.Duration("duration", time.Since(start)),
			zap.String("client_ip", c.ClientIP()),
			zap.String("user_agent", c.Request.UserAgent()),
		})
	}
}

// logByStatus emits one request log at the level the status class deserves:
// Error for 5xx, Warn for 4xx, Info otherwise. One line per request, never a
// duplicate Info+Error pair.
func logByStatus(logger *zap.Logger, status int, fields []zap.Field) {
	switch {
	case status >= 500:
		logger.Error("HTTP request", fields...)
	case status >= 400:
		logger.Warn("HTTP request", fields...)
	default:
		logger.Info("HTTP request", fields...)
	}
}

// LoggerFrom returns the request-scoped logger Logging bound to the context,
// already carrying trace context. It falls back to zap.NewNop rather than
// building a logger: a missing entry means Logging was not mounted, and a silent
// logger surfaces that faster than a second, uncorrelated one.
func LoggerFrom(c *gin.Context) *zap.Logger {
	if v, ok := c.Get(ctxKeyLogger); ok {
		if l, ok := v.(*zap.Logger); ok {
			return l
		}
	}
	return zap.NewNop()
}

// LoggerWithTraceID returns baseLogger bound to the request's correlation id.
// Prefer LoggerFrom, which also carries the native trace context.
func LoggerWithTraceID(c *gin.Context, baseLogger *zap.Logger) *zap.Logger {
	v, ok := c.Get(ctxKeyTraceID)
	if !ok {
		return baseLogger
	}
	id, ok := v.(string)
	if !ok {
		return baseLogger
	}
	return baseLogger.With(zap.String(ctxKeyTraceID, id))
}
