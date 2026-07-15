// Package dbx builds pgx connection pools pre-wired with OpenTelemetry:
// query tracing (otelpgx) and pool-stat metrics, configured once for the whole
// fleet so every service instruments its database layer identically.
//
// Layering: telemetry SDK wiring lives in obsx; dbx consumes the providers
// obsx installs as OTel globals and attaches them to the pool. Services that
// don't touch Postgres never import dbx (and so never pull in pgx) — database
// instrumentation is opt-in per service, not baked into the telemetry base.
//
// The instrumentation defaults are the safe ones fixed by RFC-0017:
//   - D-1 WithTrimSQLInSpanName: span name is the leading SQL keyword, so span
//     cardinality stays bounded instead of one name per distinct statement.
//   - D-2 no WithIncludeQueryParameters + WithDisableConnectionDetailsInAttributes:
//     bind-parameter values (PII/secrets) and connection host/user never reach
//     the tracing backend. The parameterized statement text (db.query.text) is
//     deliberately kept on the span — it is safe precisely because values are
//     bound via $1 placeholders, never interpolated into the SQL.
//   - D-3 RecordStats failure fails boot rather than running half-instrumented.
//   - D-4 WithDisableAcquireTracer: no per-acquire span noise.
//   - D-5 SimpleProtocol + caches off: unchanged transaction-mode pooler
//     (PgDog/PgBouncer) safety, carried over verbatim from the per-service
//     Connect this replaces.
package dbx

import (
	"context"
	"fmt"
	"math"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type config struct {
	maxConns       int32
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
}

// Option customizes NewPool.
type Option func(*config)

// WithMaxConns caps the pool size. Values <= 0 or > math.MaxInt32 are ignored
// (the pgx default applies), mirroring the per-service cfg.Database.MaxConnections
// guard.
func WithMaxConns(n int) Option {
	return func(c *config) {
		if n > 0 && n <= math.MaxInt32 {
			c.maxConns = int32(n)
		}
	}
}

// WithTracerProvider overrides the TracerProvider for query spans. Defaults to
// the global provider installed by obsx.SetupObservability; pass this only to
// inject a provider in tests.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) { c.tracerProvider = tp }
}

// WithMeterProvider overrides the MeterProvider for pool-stat metrics. Defaults
// to the global provider; test-injection only.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) { c.meterProvider = mp }
}

// NewPool parses dsn, applies the transaction-mode-pooler-safe settings and the
// RFC-0017 telemetry defaults, opens the pool, registers pool-stat metrics and
// pings. It returns a ready *pgxpool.Pool or an error; on any post-open failure
// the pool is closed before returning so no connections leak.
//
// otelpgx v0.11.1 API verified against pinned source (tracer.go / options.go /
// meter.go): NewTracer(opts ...Option) *Tracer, RecordStats(PoolStats,
// ...StatsOption) error.
//
// Query spans are children of the active span in the query ctx: otelpgx skips
// tracing when ctx has no recording span. In this platform every repo call
// runs inside an HTTP/gRPC/business span, so DB spans appear; a query issued
// with a bare context.Background() produces no span by design.
func NewPool(ctx context.Context, dsn string, opts ...Option) (*pgxpool.Pool, error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("dbx: parse dsn: %w", err)
	}
	if cfg.maxConns > 0 {
		poolCfg.MaxConns = cfg.maxConns
	}

	// D-5: transaction-mode pooler safety (PgDog/PgBouncer). Simple protocol
	// avoids server-side prepared statements; caches off because prepared
	// statements are connection-scoped and break under a transaction pooler.
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	poolCfg.ConnConfig.StatementCacheCapacity = 0
	poolCfg.ConnConfig.DescriptionCacheCapacity = 0

	// D-1/D-2/D-4: bounded span names, no PII, no acquire noise. Provider is
	// injected only in tests; in production otelpgx falls back to the OTel
	// global that obsx already installed in main().
	tracerOpts := []otelpgx.Option{
		otelpgx.WithTrimSQLInSpanName(),
		otelpgx.WithDisableAcquireTracer(),
		otelpgx.WithDisableConnectionDetailsInAttributes(),
	}
	if cfg.tracerProvider != nil {
		tracerOpts = append(tracerOpts, otelpgx.WithTracerProvider(cfg.tracerProvider))
	}
	poolCfg.ConnConfig.Tracer = otelpgx.NewTracer(tracerOpts...)

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("dbx: create pool: %w", err)
	}

	// Ping before registering stats: RecordStats installs meter callbacks that
	// capture the pool with no unregister handle, so a pool that can't connect
	// must never leave a dangling callback behind.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("dbx: ping: %w", err)
	}

	// D-3: pool-stat metrics (pgxpool.*). RecordStats errors only on metric
	// registration failure — fail boot rather than serve half-instrumented.
	var statsOpts []otelpgx.StatsOption
	if cfg.meterProvider != nil {
		statsOpts = append(statsOpts, otelpgx.WithStatsMeterProvider(cfg.meterProvider))
	}
	if err := otelpgx.RecordStats(pool, statsOpts...); err != nil {
		pool.Close()
		return nil, fmt.Errorf("dbx: record pool stats: %w", err)
	}

	return pool, nil
}
