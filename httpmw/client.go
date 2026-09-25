package httpmw

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Transport wraps base so every outbound request carries the W3C
// traceparent (the global propagator obsx installed) and opens a client span
// that joins the caller's trace. It is the one sanctioned way for a service to
// instrument a plain net/http client: the fleet lint policy (ADR-072) rejects
// a direct otelhttp import outside the shared package, so a service that
// talks to a provider over HTTP takes its transport from here. Only headers
// are touched — a body-level signature stays intact. A nil base means
// http.DefaultTransport.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base)
}

// Handler wraps a plain net/http handler (one that does not run under Gin, so
// Tracing cannot see it) in a server span named operation, joining the
// incoming traceparent. The mockpay stub in payment-service is the case it
// exists for: an in-process provider whose webhook must land in the same
// trace as the charge that caused it.
func Handler(h http.Handler, operation string) http.Handler {
	return otelhttp.NewHandler(h, operation)
}
