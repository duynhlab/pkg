//go:build integration

// Integration tests for dbx.NewPool against a real Postgres via
// testcontainers-go. Run with:
//
//	go test -tags=integration ./dbx/...
//
// Requires a reachable Docker daemon.
package dbx

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// startPostgres boots a throwaway Postgres and returns its DSN.
func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("app"), postgres.WithUsername("app"), postgres.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	return dsn
}

// A query through a dbx pool must produce a span, and that span must carry NO
// parameter values (RFC-0017 D-2: WithIncludeQueryParameters is never set, so
// PII/secrets in bind params never reach the tracing backend).
func TestNewPool_TracesQueriesWithoutParameters(t *testing.T) {
	dsn := startPostgres(t)
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))

	pool, err := NewPool(context.Background(), dsn, WithTracerProvider(tp))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	// otelpgx only spans a query that runs under an already-recording parent
	// span (tracer.go: `if !SpanFromContext(ctx).IsRecording()`), which is the
	// production reality — every repo call executes inside an HTTP/gRPC/logic
	// span. Start a parent so the child query span is produced.
	pctx, parent := tp.Tracer("test").Start(context.Background(), "parent")

	// Pass a bind parameter that would be a red flag if leaked onto the span.
	const secret = "super-secret-value"
	if _, err := pool.Exec(pctx, "SELECT $1::text", secret); err != nil {
		t.Fatalf("exec: %v", err)
	}
	parent.End()

	var found bool
	for _, s := range rec.Ended() {
		if !strings.Contains(strings.ToUpper(s.Name()), "SELECT") {
			continue
		}
		found = true
		for _, a := range s.Attributes() {
			if strings.Contains(strings.ToLower(string(a.Key)), "parameter") {
				t.Errorf("query span carries a parameters attribute %q (PII leak)", a.Key)
			}
			if strings.Contains(a.Value.Emit(), secret) {
				t.Errorf("query span attribute %q leaks the bind parameter value", a.Key)
			}
		}
	}
	if !found {
		t.Fatalf("no query span recorded; got %d spans", len(rec.Ended()))
	}
}

// NewPool must register the otelpgx pool-stat metrics (pgxpool.*) on the
// provided MeterProvider (RFC-0017 D-3).
func TestNewPool_RecordsPoolStats(t *testing.T) {
	dsn := startPostgres(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	pool, err := NewPool(context.Background(), dsn, WithMeterProvider(mp))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if strings.HasPrefix(m.Name, "pgxpool.") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no pgxpool.* metrics registered")
	}
}

// The pool must keep the transaction-mode-pooler-safe settings (RFC-0017 D-5)
// and apply WithMaxConns.
func TestNewPool_PoolerSafeConfig(t *testing.T) {
	dsn := startPostgres(t)
	pool, err := NewPool(context.Background(), dsn, WithMaxConns(7))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	cfg := pool.Config()
	if cfg.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Errorf("DefaultQueryExecMode = %v, want SimpleProtocol", cfg.ConnConfig.DefaultQueryExecMode)
	}
	if cfg.ConnConfig.StatementCacheCapacity != 0 {
		t.Errorf("StatementCacheCapacity = %d, want 0", cfg.ConnConfig.StatementCacheCapacity)
	}
	if cfg.ConnConfig.DescriptionCacheCapacity != 0 {
		t.Errorf("DescriptionCacheCapacity = %d, want 0", cfg.ConnConfig.DescriptionCacheCapacity)
	}
	if cfg.MaxConns != 7 {
		t.Errorf("MaxConns = %d, want 7", cfg.MaxConns)
	}
}
