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
	"os"
	"path/filepath"
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

// A password delivered via WithPasswordFile is read per new connection, so a
// rotated password (rewrite the file after ALTER ROLE) is picked up by new
// connections without recreating the pool — RFC-0008 / ADR-025 pattern A. The
// DSN still carries the original password, so success also proves BeforeConnect
// overrides the DSN, and a stale file then fails auth.
func TestNewPool_PasswordFileRotation(t *testing.T) {
	ctx := context.Background()
	dsn := startPostgres(t) // role "app" is the container superuser (password "secret")

	pwFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(pwFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}

	pool, err := NewPool(ctx, dsn, WithPasswordFile(pwFile))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	// A password file with no explicit lifetime applies the bounded default
	// (30m + 10% jitter) so rotated connections recycle within a known window.
	if got := pool.Config().MaxConnLifetime; got != 30*time.Minute {
		t.Errorf("default MaxConnLifetime = %v, want 30m", got)
	}
	if got := pool.Config().MaxConnLifetimeJitter; got != 3*time.Minute {
		t.Errorf("default MaxConnLifetimeJitter = %v, want 3m", got)
	}

	if _, err := pool.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("query with initial file password: %v", err)
	}

	// Rotate the role's password, then update the file to match.
	if _, err := pool.Exec(ctx, "ALTER ROLE app PASSWORD 'rotated'"); err != nil {
		t.Fatalf("rotate password: %v", err)
	}
	if err := os.WriteFile(pwFile, []byte("rotated\n"), 0o600); err != nil {
		t.Fatalf("rewrite password file: %v", err)
	}
	pool.Reset() // drop pooled connections; the next acquire re-reads the file

	if _, err := pool.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("query after rotation should use the new file password: %v", err)
	}

	// A stale file (the pre-rotation password) must now fail closed. The earlier
	// success with file=rotated while the DSN still carried "secret" is what
	// proved the file overrides the DSN; this confirms a wrong file is rejected.
	if err := os.WriteFile(pwFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write stale password file: %v", err)
	}
	pool.Reset()
	badCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(badCtx, "SELECT 1"); err == nil {
		t.Fatal("expected auth failure with a stale password file, got nil")
	}
}

// A missing password file must fail pool creation (BeforeConnect errors on the
// initial ping) rather than silently connecting.
func TestNewPool_PasswordFileMissing(t *testing.T) {
	dsn := startPostgres(t)
	_, err := NewPool(context.Background(), dsn,
		WithPasswordFile(filepath.Join(t.TempDir(), "does-not-exist")))
	if err == nil {
		t.Fatal("expected error for missing password file, got nil")
	}
}

// An explicit WithMaxConnLifetime overrides the WithPasswordFile 30m default.
func TestNewPool_PasswordFileLifetimeOverride(t *testing.T) {
	dsn := startPostgres(t)
	pwFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(pwFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write password file: %v", err)
	}
	pool, err := NewPool(context.Background(), dsn,
		WithPasswordFile(pwFile), WithMaxConnLifetime(45*time.Minute))
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConnLifetime; got != 45*time.Minute {
		t.Errorf("MaxConnLifetime = %v, want 45m (override of the default)", got)
	}
}

// An empty password file must fail pool creation (fail closed) rather than
// authenticating with an empty password.
func TestNewPool_PasswordFileEmpty(t *testing.T) {
	dsn := startPostgres(t)
	pwFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(pwFile, []byte("\n"), 0o600); err != nil {
		t.Fatalf("write empty password file: %v", err)
	}
	_, err := NewPool(context.Background(), dsn, WithPasswordFile(pwFile))
	if err == nil {
		t.Fatal("expected error for empty password file, got nil")
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
