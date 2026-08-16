// Package httpmw holds the platform's shared Gin middleware: the tracing and
// logging pair every HTTP service mounts, in that order.
//
// It exists because those two middleware were copied into every service under
// <svc>-service/middleware/ and drifted apart release by release — 4 variants of
// tracing.go and 7 of logging.go across the fleet. gin and otelgin are imported
// here and nowhere else in pkg, which is why this is a module of its own rather
// than part of obsx: a gRPC-only service must be able to use the span helpers
// without taking a dependency on a web framework.
//
// The OTel SDK itself (providers, exporters, resource, sampler) is wired once in
// main() by obsx.SetupObservability (RFC-0014). This package only consumes the
// globals that installs.
package httpmw

import "github.com/gin-gonic/gin"

// DefaultSkipRoutes are the routes excluded from every request-scoped signal:
// no span, no http.server.* metric, no access log. Routine probes are traffic
// about the platform, not about the domain.
//
// Matching is EXACT and runs against the Gin route pattern (c.FullPath()), not
// the raw request path. Two consequences worth knowing before you debug a
// missing trace:
//
//   - A request that matches no route has an empty FullPath and is therefore
//     traced. A probe aimed at a path this service never registered — /metrics,
//     say — now shows up as a traced 404 instead of vanishing. That is the
//     point: a misconfigured probe should be visible.
//   - Prefix matching, which this replaced, also swallowed anything that merely
//     started with a listed value. A route named /healthy-users was silently
//     untraceable.
//
// Both middleware in this package read this one map, so the tracing and logging
// skip lists cannot drift apart.
var DefaultSkipRoutes = map[string]struct{}{
	"/health":      {},
	"/healthz":     {},
	"/ready":       {},
	"/readyz":      {},
	"/livez":       {},
	"/metrics":     {},
	"/favicon.ico": {},
}

// skipper returns a predicate reporting whether a request should be skipped,
// given the default routes plus any the caller adds.
func skipper(extra ...string) func(*gin.Context) bool {
	skip := make(map[string]struct{}, len(DefaultSkipRoutes)+len(extra))
	for route := range DefaultSkipRoutes {
		skip[route] = struct{}{}
	}
	for _, route := range extra {
		skip[route] = struct{}{}
	}
	return func(c *gin.Context) bool {
		_, blocked := skip[c.FullPath()]
		return blocked
	}
}
