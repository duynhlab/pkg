package httpmw

import (
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel"
)

// Tracing returns the platform's HTTP tracing middleware: the root server span
// for every request, plus the http.server.* RED metrics that otelgin emits from
// the same instrumentation. There is no separate metrics middleware.
//
// Mount it first, before Logging, so the logger can read the trace id.
//
//	r.Use(httpmw.Tracing(cfg.ServiceName))
//	r.Use(httpmw.Logging(logger))
//
// serviceName names the tracer and meter scope; otelgin takes it positionally.
// It is a parameter rather than package state on purpose — the per-service
// copies this replaced kept it in a package-level variable written by a setter
// at startup and read from the request path, which was only safe by convention.
//
// extraSkipRoutes adds to DefaultSkipRoutes for this service only. Pass Gin
// route patterns (/orders/:id), not request paths.
func Tracing(serviceName string, extraSkipRoutes ...string) gin.HandlerFunc {
	skip := skipper(extraSkipRoutes...)

	return otelgin.Middleware(
		serviceName,
		otelgin.WithTracerProvider(otel.GetTracerProvider()),
		otelgin.WithPropagators(otel.GetTextMapPropagator()),
		// The filter runs before the span is started, so a skipped route costs
		// nothing and — verified by TestTracing_SkippedRouteEmitsNoSpanAndNoMetric
		// — produces no metric either. The pair matters: the platform contract is
		// "no span, no metric" for probes, and otelgin drives both from here.
		otelgin.WithGinFilter(func(c *gin.Context) bool { return !skip(c) }),
	)
}
