// Package temporalx provides thin, opinionated bootstrap helpers for connecting
// to Temporal and running workers, mirroring grpcx/obsx so every service wires
// Temporal the same way. Telemetry rides the SDK's OpenTelemetry v2 integration
// (ADR-063): one plugin carries tracing (replay-safe, corrected span parenting)
// and the SDK's workflow/activity RED metrics (monotonic counters) to the same
// global OTel providers everything else uses, exported over OTLP by obsx —
// there is no scrape endpoint. Workflow code may create spans via Tracer,
// which is replay-safe; plain otel.Tracer inside workflow code is not.
//
// contrib/opentelemetry-v2 is experimental (v0.1.x): its API may change
// between releases. This package pins and wraps it so services never import
// the contrib module directly.
package temporalx

import (
	"errors"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.temporal.io/sdk/client"
	temporalotel "go.temporal.io/sdk/contrib/opentelemetry-v2"
	"go.temporal.io/sdk/interceptor/tracing"
	"go.temporal.io/sdk/worker"
)

// Config holds the settings for a Temporal client connection.
type Config struct {
	// HostPort is the Temporal frontend address, e.g.
	// "temporal-frontend.temporal.svc.cluster.local:7233".
	HostPort string
	// Namespace is the Temporal namespace workflows run in, e.g. "mop".
	Namespace string
}

// Dial connects to the Temporal frontend with the OpenTelemetry v2 plugin
// wired in (ADR-063).
//
// The plugin registers client- and worker-side tracing interceptors (workers
// created from this client via NewWorker inherit them) plus the SDK metrics
// handler, so a single registration covers client calls, workflow/activity
// execution, and RED metrics. UseMonotonicCounters makes SDK counters export
// as monotonic sums so rate()/increase() classify them correctly.
// AllowInvalidParentSpans stays on while any caller in the fleet still runs
// the v1 interceptor; tighten it once the fleet has converged.
//
// PRECONDITION: the OTel GLOBAL tracer provider must be the replay-safe one —
// the service main passes NewReplaySafeTracerProvider through
// obsx.WithTracerProviderFactory before calling Dial. The v2 plugin
// type-asserts the global EAGERLY (it would panic inside NewPlugin); Dial
// guards first and returns an actionable error instead, per this repo's
// no-panics rule.
//
// Transport is plaintext for in-cluster east-west traffic; mTLS is a later phase
// (mirrors grpcx).
func Dial(cfg Config, opts ...DialOption) (client.Client, error) {
	for _, opt := range opts {
		if opt == nil {
			// WithLogger(nil) lands here. Erroring beats silently keeping the
			// SDK's default stderr logger the caller meant to replace.
			return nil, errors.New("temporalx: nil DialOption — WithLogger was given a nil logger")
		}
	}

	if _, ok := otel.GetTracerProvider().(*temporalotel.ReplaySafeTracerProvider); !ok {
		return nil, errors.New(
			"temporalx: the global OTel tracer provider is not replay-safe; pass " +
				"temporalx.NewReplaySafeTracerProvider through obsx.WithTracerProviderFactory " +
				"before Dial (ADR-063)")
	}

	plugin, err := temporalotel.NewPlugin(temporalotel.PluginOptions{
		TracerOptions: tracing.TracerOptions{
			AddTemporalSpans:        true,
			AllowInvalidParentSpans: true,
		},
		// OnError must never keep the default (panic): an instrument-creation
		// error would crash the whole worker over telemetry.
		MetricsHandlerOptions: &temporalotel.MetricsHandlerOptions{
			UseMonotonicCounters: true,
			OnError:              logMetricsError,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("temporalx: build opentelemetry-v2 plugin: %w", err)
	}

	options := client.Options{
		HostPort:  cfg.HostPort,
		Namespace: cfg.Namespace,
		Plugins:   []client.Plugin{plugin},
	}
	for _, opt := range opts {
		opt(&options)
	}

	c, err := client.Dial(options)
	if err != nil {
		return nil, fmt.Errorf("temporalx: dial %q (namespace %q): %w", cfg.HostPort, cfg.Namespace, err)
	}
	return c, nil
}

// NewWorker creates a Temporal worker that polls taskQueue on the given client.
// Register workflows and activities on the returned worker, then call Run to
// block until interrupted. The worker inherits the client's tracing interceptor.
//
// Options are additive and optional — NewWorker(c, taskQueue) keeps the exact
// pre-versioning behavior. See versioning.go for Worker Deployment Versioning
// (MustVersioningFromEnv is the usual call).
func NewWorker(c client.Client, taskQueue string, opts ...WorkerOption) worker.Worker {
	options := worker.Options{}
	for _, opt := range opts {
		if opt == nil {
			// VersioningFromEnv returns (nil, err); a caller who ignored the
			// error lands here. Skipping instead would build an UNVERSIONED
			// worker, which is the failure class this package exists to prevent.
			panic("temporalx.NewWorker: nil WorkerOption — a VersioningFromEnv error was ignored")
		}
		opt(&options)
	}
	// Resolve the option set as a whole rather than per option, so order cannot
	// change the worker and no half-configured combination reaches the SDK.
	normalizeVersioning(&options)
	return worker.New(c, taskQueue, options)
}

// logMetricsError replaces the Temporal SDK's default OnError (panic): an
// instrument-creation failure must never crash the worker over telemetry.
func logMetricsError(err error) {
	slog.Error("temporalx: metrics handler error", "error", err)
}
