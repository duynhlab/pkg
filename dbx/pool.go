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
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type config struct {
	maxConns        int32
	tracerProvider  trace.TracerProvider
	meterProvider   metric.MeterProvider
	passwordFile    string
	maxConnLifetime time.Duration
	pingTimeout     *time.Duration // nil = defaultPingTimeout unless the DSN set one
	noAcquireCheck  bool
}

// defaultPingTimeout bounds the liveness ping pgxpool sends before handing out
// a connection that has sat idle for over a second. pgx's own default is no
// timeout, so a half-open TCP connection (pooler restart, lost node) would
// hold Acquire until the request context or TCP keepalive gave up. In-cluster
// pooler round trips are milliseconds; 2s only trips on a dead connection,
// which pgxpool then destroys and replaces. A false positive costs one
// reconnect.
const defaultPingTimeout = 2 * time.Second

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

// WithPasswordFile makes the pool read the connection password from a file on
// every new connection (via pgxpool BeforeConnect) instead of taking it from the
// DSN. This is the credential hot-reload path (RFC-0008 / ADR-025 pattern A):
// when an external rotator (OpenBAO static-role → ESO → mounted Secret) updates
// the file, new connections pick up the new password without a pod restart. Only
// a trailing newline is stripped (surrounding whitespace is preserved) and an
// empty file is rejected. Prefer a DSN with no password when using this so no
// static secret remains in the connection string for a caller to log.
//
// Because an existing connection keeps its original password until it is
// recycled, WithPasswordFile also applies a default MaxConnLifetime of 30m
// (jitter 3m) so the whole pool migrates onto a rotated password within a
// bounded window; override with WithMaxConnLifetime. An empty path is ignored
// (the DSN password stands), so existing callers are unaffected.
//
// A new connection opened after the server's password changed but before the
// file does fails with 28P01 and is not retried; it is counted in
// pgx.pool.connect.failures. Pooled connections stay up through that window.
func WithPasswordFile(path string) Option {
	return func(c *config) { c.passwordFile = path }
}

// WithMaxConnLifetime caps how long a connection is reused before it is closed
// and re-established (pgxpool MaxConnLifetime), with a 10% jitter to avoid a
// thundering-herd reconnect. Values <= 0 are ignored (the pgx default — or the
// WithPasswordFile default — applies). Pair it with WithPasswordFile to bound
// how quickly a rotated password takes effect across the pool.
func WithMaxConnLifetime(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.maxConnLifetime = d
		}
	}
}

// WithPingTimeout overrides how long pgxpool waits for the pre-acquire ping
// before it treats the connection as dead (pgxpool Config.PingTimeout). The
// default is defaultPingTimeout; 0 restores pgx's no-timeout behaviour and a
// negative value is treated as 0. An explicit option wins over a
// pool_ping_timeout parameter in the DSN.
func WithPingTimeout(d time.Duration) Option {
	return func(c *config) {
		if d < 0 {
			d = 0
		}
		c.pingTimeout = &d
	}
}

// WithAcquireCheck turns the pre-acquire dead-connection check on (the
// default) or off. See acquireCheck for what it catches; turn it off only to
// compare behaviour in a test.
func WithAcquireCheck(enabled bool) Option {
	return func(c *config) { c.noAcquireCheck = !enabled }
}

// Instrument names, declared in pkg/semconv/registry/model/metrics.yaml.
const (
	metricConnectFailures  = "pgx.pool.connect.failures"
	metricAcquireEvictions = "pgx.pool.acquire.evictions"
	meterName              = "github.com/duynhlab/pkg/dbx"
)

// connectFailureTracer counts connections the pool failed to open. pgxpool
// returns such an error straight to the caller and never retries it, so this
// counter is the only signal that, for example, a rotated password has not
// reached the pod yet (28P01) or the pooler is refusing logins while it shuts
// down (57P01). error.type is the SQLSTATE when the server answered, else
// "connect".
type connectFailureTracer struct {
	failures metric.Int64Counter
}

func (connectFailureTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (connectFailureTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (connectFailureTracer) TraceConnectStart(ctx context.Context, _ pgx.TraceConnectStartData) context.Context {
	return ctx
}

func (t connectFailureTracer) TraceConnectEnd(ctx context.Context, data pgx.TraceConnectEndData) {
	if data.Err == nil {
		return
	}
	t.failures.Add(ctx, 1, metric.WithAttributes(attribute.String("error.type", connectErrorType(data.Err))))
}

// connectErrorType names a connect failure by the server's SQLSTATE when it
// sent one (28P01 wrong password, 57P01 shutting down, 53300 too many
// connections), and "connect" for a dial, TLS or timeout failure.
func connectErrorType(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return "connect"
}

// acquireCheck evicts a pooled connection the server has already closed
// before pgxpool hands it out. A pooler that shuts down (PgDog on a rollout
// or certificate renewal) sends FATAL 57P01 to its idle clients and closes the
// socket; pgx does not read an idle socket, so without this check the next
// query on that connection fails. pgxpool pings only connections idle for
// over a second, which misses the busy ones: duynhlab/pkg#124 measured 5 of
// 1993 requests failing that way during a PgDog roll.
//
// A healthy idle connection has nothing waiting on its socket, which a
// non-blocking peek confirms in about a microsecond. Only when bytes or EOF
// are waiting does the check read one message with CheckConn, which then
// returns at once: an error (the FATAL, or the closed socket) evicts, while a
// notice or notification is consumed as pgx normally would. CheckConn alone
// waits its full 1 ms deadline on a healthy socket, which would add that to
// every query. Returning (false, nil) makes pgxpool destroy the connection and
// acquire another before any query is sent, so the retry cannot repeat a
// statement.
func acquireCheck(evictions metric.Int64Counter) func(context.Context, *pgx.Conn) (bool, error) {
	return func(ctx context.Context, c *pgx.Conn) (bool, error) {
		if idle, known := socketIdle(c.PgConn().Conn()); idle || !known {
			return true, nil
		}
		//nolint:staticcheck // SA1019: deprecated in favour of Ping; here it only reads a message already waiting, which Ping cannot do without a round trip.
		if err := c.PgConn().CheckConn(); err != nil {
			evictions.Add(ctx, 1)
			return false, nil
		}
		return true, nil
	}
}

// applyPingTimeout sets poolCfg.PingTimeout: an explicit WithPingTimeout wins,
// then a positive pool_ping_timeout from the DSN, then defaultPingTimeout.
func applyPingTimeout(poolCfg *pgxpool.Config, cfg config) {
	switch {
	case cfg.pingTimeout != nil:
		poolCfg.PingTimeout = *cfg.pingTimeout
	case poolCfg.PingTimeout > 0:
		// pool_ping_timeout in the DSN
	default:
		poolCfg.PingTimeout = defaultPingTimeout
	}
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
	applyPingTimeout(poolCfg, cfg)

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

	mp := cfg.meterProvider
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	meter := mp.Meter(meterName)
	counter := func(name, desc string) (metric.Int64Counter, error) {
		c, err := meter.Int64Counter(name, metric.WithUnit("{connection}"), metric.WithDescription(desc))
		if err != nil {
			return nil, fmt.Errorf("dbx: create %s counter: %w", name, err)
		}
		return c, nil
	}
	failures, err := counter(metricConnectFailures, "Connections the pool failed to open, by error.type (SQLSTATE or connect)")
	if err != nil {
		return nil, err
	}
	poolCfg.ConnConfig.Tracer = multitracer.New(otelpgx.NewTracer(tracerOpts...), connectFailureTracer{failures: failures})

	if !cfg.noAcquireCheck {
		evictions, err := counter(metricAcquireEvictions, "Pooled connections found closed by the server at acquire and replaced")
		if err != nil {
			return nil, err
		}
		poolCfg.PrepareConn = acquireCheck(evictions)
	}

	// Credential hot-reload (RFC-0008 / ADR-025 pattern A): read the password
	// from a mounted file on every new connection so an externally rotated
	// password is used without a restart. Set only when a file is configured,
	// leaving DSN-password callers untouched. Default a bounded connection
	// lifetime (30m + jitter) when a file is used so old-password connections
	// recycle promptly; WithMaxConnLifetime overrides it.
	if cfg.passwordFile != "" {
		path := cfg.passwordFile
		poolCfg.BeforeConnect = func(_ context.Context, cc *pgx.ConnConfig) error {
			data, err := os.ReadFile(path) //nolint:gosec // G304: path is an operator-controlled mount (WithPasswordFile), not user input
			if err != nil {
				return fmt.Errorf("dbx: read password file: %w", err)
			}
			// Strip only a trailing newline (the file-mount convention); keep any
			// other bytes so a password with surrounding whitespace stays intact.
			pw := string(bytes.TrimRight(data, "\r\n"))
			if pw == "" {
				// Fail closed with a clear signal rather than authenticating with
				// an empty password (a truncated/not-yet-written mount).
				return fmt.Errorf("dbx: password file %q is empty", path)
			}
			cc.Password = pw
			return nil
		}
		if cfg.maxConnLifetime == 0 {
			cfg.maxConnLifetime = 30 * time.Minute
		}
	}
	if cfg.maxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.maxConnLifetime
		poolCfg.MaxConnLifetimeJitter = cfg.maxConnLifetime / 10
	}

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
