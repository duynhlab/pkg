package obsx

import (
	"context"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// traceContextFieldKey names the carrier field. It never appears in output:
// the otelzap bridge consumes the field by Interface type-assertion, and the
// stdout core skips it (SkipType). The key exists only for debuggability.
const traceContextFieldKey = "otel.trace_context"

// TraceContext returns a zap field that binds ctx's trace context to a log
// call so the otelzap bridge (Observability.ZapCore) stamps the OTLP log record
// with the native trace_id/span_id — the semconv-standard log↔trace link,
// stronger than a hand-added string field.
//
// The field is SkipType carrying ctx as its Interface: the otelzap bridge finds
// it via `field.Interface.(context.Context)` (otelzap core.go convertField),
// while every zap encoder skips SkipType — so the raw context never pollutes
// stdout under a zapcore.NewTee(stdout, obs.ZapCore(...)) fan-out. Verified
// against otelzap v0.19.0 (the bridge `continue`s on the context field rather
// than encoding it).
//
// Prefer per-call use, which is what repository/handler code should do:
//
//	log.Error("query failed", zap.Error(err), obsx.TraceContext(ctx))
//
// Binding once with `log.With(obsx.TraceContext(ctx))` is valid ONLY for a
// logger whose lifetime is scoped to the same operation as ctx: the bound ctx
// is retained for the logger's whole life, so a long-lived logger would keep
// stamping the original (possibly finished) trace onto later records. Do not
// bind a request ctx onto a service-lived logger.
//
// Pass a real request/operation context. A genuinely nil ctx is normalized to
// a no-op field; a span-less ctx is harmless (the bridge finds no span and
// emits without trace ids).
func TraceContext(ctx context.Context) zap.Field {
	if ctx == nil {
		return zap.Skip()
	}
	return zap.Field{Key: traceContextFieldKey, Type: zapcore.SkipType, Interface: ctx}
}
