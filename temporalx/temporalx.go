// Package temporalx provides thin, opinionated bootstrap helpers for connecting
// to Temporal and running workers, mirroring grpcx/obsx so every service wires
// Temporal the same way. It keeps OpenTelemetry tracing consistent with the rest
// of the platform: workflow and activity spans join the same trace as the
// gRPC/HTTP request that started the workflow, correlated through the existing
// Tempo backend (see homelab/docs/api/temporal-order-fulfillment.md).
package temporalx

import (
	"fmt"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/contrib/opentelemetry"
	"go.temporal.io/sdk/interceptor"
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

// Dial connects to the Temporal frontend with OpenTelemetry tracing wired in.
//
// The tracing interceptor is registered on the client; workers created from this
// client via NewWorker inherit the worker-side of the same interceptor, so a
// single registration here covers both client calls and workflow/activity
// execution. Its tracer is obtained from the global OpenTelemetry tracer
// provider (which services already configure at startup), so no tracer needs to
// be threaded through.
//
// Transport is plaintext for in-cluster east-west traffic; mTLS is a later phase
// (mirrors grpcx).
func Dial(cfg Config) (client.Client, error) {
	tracing, err := opentelemetry.NewTracingInterceptor(opentelemetry.TracerOptions{})
	if err != nil {
		return nil, fmt.Errorf("temporalx: build tracing interceptor: %w", err)
	}

	c, err := client.Dial(client.Options{
		HostPort:     cfg.HostPort,
		Namespace:    cfg.Namespace,
		Interceptors: []interceptor.ClientInterceptor{tracing},
	})
	if err != nil {
		return nil, fmt.Errorf("temporalx: dial %q (namespace %q): %w", cfg.HostPort, cfg.Namespace, err)
	}
	return c, nil
}

// NewWorker creates a Temporal worker that polls taskQueue on the given client.
// Register workflows and activities on the returned worker, then call Run to
// block until interrupted. The worker inherits the client's tracing interceptor.
func NewWorker(c client.Client, taskQueue string) worker.Worker {
	return worker.New(c, taskQueue, worker.Options{})
}
