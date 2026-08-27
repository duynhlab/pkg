package temporalx

import (
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	temporalotel "go.temporal.io/sdk/contrib/opentelemetry-v2"
)

// ReplaySafeTracerProvider is the tracer provider the OTel v2 plugin requires
// as the OTel GLOBAL. Re-exported so service mains and obsx never import the
// experimental contrib module directly.
type ReplaySafeTracerProvider = temporalotel.ReplaySafeTracerProvider

// WorkflowTracer creates replay-safe spans inside workflow code.
type WorkflowTracer = temporalotel.WorkflowTracer

// NewReplaySafeTracerProvider builds the provider whose span/trace IDs come
// from workflow.GetRandomStream inside workflows (identical IDs on replay —
// no duplicate spans) and crypto/rand everywhere else, so it is safe as the
// process-wide global even for the API half of a shared binary.
//
// Service mains hand this to obsx.WithTracerProviderFactory; obsx keeps
// supplying the Resource/sampler/OTLP-batcher options and installs the result
// as the OTel global — which Dial's plugin then type-asserts (ADR-063).
func NewReplaySafeTracerProvider(opts ...sdktrace.TracerProviderOption) *ReplaySafeTracerProvider {
	return temporalotel.NewReplaySafeTracerProvider(opts...)
}

// Tracer returns a replay-safe tracer for spans created INSIDE workflow code:
//
//	ctx, span := temporalx.Tracer("github.com/duynhlab/order-service/internal/saga").Start(ctx, "confirm-order")
//	defer span.End()
//
// Plain otel.Tracer must not be used in workflow code — its spans are not
// replay-safe. Activities are ordinary Go code and use otel.Tracer as usual.
// Requires the global from NewReplaySafeTracerProvider (Dial guards this).
func Tracer(name string) WorkflowTracer {
	return temporalotel.Tracer(name)
}
