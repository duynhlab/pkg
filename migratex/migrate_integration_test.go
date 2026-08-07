//go:build integration

// Integration tests for migratex.Run against a real Postgres via
// testcontainers-go. Run with:
//
//	go test -tags=integration ./migratex/...
//
// Requires a reachable Docker daemon.
package migratex

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
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

// Run must apply pending migrations, and a second identical Run must be a
// clean no-op (golang-migrate reports ErrNoChange, which Run swallows by
// contract — services call it unconditionally at startup).
func TestRun_AppliesMigrationsAndIsIdempotent(t *testing.T) {
	dsn := startPostgres(t)
	fsys := fstest.MapFS{
		"sql/0001_widgets.up.sql": &fstest.MapFile{
			Data: []byte(`CREATE TABLE widgets (id BIGINT PRIMARY KEY, name TEXT NOT NULL);`),
		},
		"sql/0001_widgets.down.sql": &fstest.MapFile{
			Data: []byte(`DROP TABLE widgets;`),
		},
	}

	if err := Run(fsys, "sql", dsn); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := Run(fsys, "sql", dsn); err != nil {
		t.Fatalf("second Run must be a no-op, got: %v", err)
	}

	// The migration really executed: the table exists and accepts writes.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `INSERT INTO widgets (id, name) VALUES (1, 'w')`); err != nil {
		t.Fatalf("migrated table not usable: %v", err)
	}
}

// A failed migration must surface its error (and golang-migrate marks the
// version dirty — recovery is operator-driven by contract, not Run's job).
func TestRun_SurfacesFailedMigration(t *testing.T) {
	dsn := startPostgres(t)
	fsys := fstest.MapFS{
		"sql/0001_broken.up.sql": &fstest.MapFile{
			Data: []byte(`THIS IS NOT SQL;`),
		},
		"sql/0001_broken.down.sql": &fstest.MapFile{
			Data: []byte(`SELECT 1;`),
		},
	}
	if err := Run(fsys, "sql", dsn); err == nil {
		t.Fatal("Run succeeded with a broken migration")
	}
}
