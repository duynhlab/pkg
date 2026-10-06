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
	"net/url"
	"strings"
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
	return startPostgresImage(t, "postgres:16-alpine")
}

// startPostgresImage boots a throwaway Postgres from image and returns the
// superuser DSN.
func startPostgresImage(t *testing.T, image string) string {
	t.Helper()
	ctx := context.Background()
	c, err := postgres.Run(ctx, image,
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

// setupThreeRoles reproduces the RFC-0029 shape on a fresh database: the
// database (and, on PG15+, its public schema) belongs to a NOLOGIN owner, and
// the migrator is a NOINHERIT member of it with SET only. It returns the
// migrator's DSN.
func setupThreeRoles(t *testing.T, adminDSN string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer conn.Close(ctx)
	for _, stmt := range []string{
		`CREATE ROLE svc_owner NOLOGIN`,
		`CREATE ROLE other_owner NOLOGIN`,
		`CREATE ROLE svc_migrator LOGIN NOINHERIT PASSWORD 'migrator'`,
		`GRANT svc_owner TO svc_migrator WITH INHERIT FALSE, SET TRUE, ADMIN FALSE`,
		`ALTER DATABASE app OWNER TO svc_owner`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.User = url.UserPassword("svc_migrator", "migrator")
	return u.String()
}

func widgetsFS() fstest.MapFS {
	return fstest.MapFS{
		"sql/0001_widgets.up.sql":   &fstest.MapFile{Data: []byte(`CREATE TABLE widgets (id BIGINT PRIMARY KEY);`)},
		"sql/0001_widgets.down.sql": &fstest.MapFile{Data: []byte(`DROP TABLE widgets;`)},
	}
}

// relOwners returns the owner of each named relation in public.
func relOwners(t *testing.T, adminDSN string, rels ...string) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, `SELECT c.relname, pg_get_userbyid(c.relowner)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname = ANY($1)`, rels)
	if err != nil {
		t.Fatalf("query owners: %v", err)
	}
	owners, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var p [2]string
		err := r.Scan(&p[0], &p[1])
		return p, err
	})
	if err != nil {
		t.Fatalf("scan owners: %v", err)
	}
	out := map[string]string{}
	for _, p := range owners {
		out[p[0]] = p[1]
	}
	return out
}

// Without WithSetRole a NOINHERIT migrator has no CREATE on the owner's
// schema, so Run fails and nothing is created (RFC-0029 PG-04).
func TestRun_MigratorWithoutSetRoleIsDenied(t *testing.T) {
	admin := startPostgresImage(t, "postgres:18-alpine")
	migrator := setupThreeRoles(t, admin)

	err := Run(widgetsFS(), "sql", migrator)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Run without SET ROLE = %v, want permission denied", err)
	}
	if got := relOwners(t, admin, "widgets", "schema_migrations"); len(got) != 0 {
		t.Fatalf("objects created without SET ROLE: %v", got)
	}
}

// With WithSetRole every object, including golang-migrate's own version
// table, is owned by the owner role, never by the migrator login.
func TestRun_WithSetRoleCreatesOwnerOwnedObjects(t *testing.T) {
	admin := startPostgresImage(t, "postgres:18-alpine")
	migrator := setupThreeRoles(t, admin)

	if err := Run(widgetsFS(), "sql", migrator, WithSetRole("svc_owner")); err != nil {
		t.Fatalf("Run with SET ROLE: %v", err)
	}
	if err := Run(widgetsFS(), "sql", migrator, WithSetRole("svc_owner")); err != nil {
		t.Fatalf("second Run must be a no-op, got: %v", err)
	}
	got := relOwners(t, admin, "widgets", "schema_migrations")
	for _, rel := range []string{"widgets", "schema_migrations"} {
		if got[rel] != "svc_owner" {
			t.Errorf("owner of %s = %q, want svc_owner (all: %v)", rel, got[rel], got)
		}
	}
}

// A role the login is not a member of is denied, and Run fails instead of
// continuing as the login.
func TestRun_WithSetRoleDeniedFails(t *testing.T) {
	admin := startPostgresImage(t, "postgres:18-alpine")
	migrator := setupThreeRoles(t, admin)

	err := Run(widgetsFS(), "sql", migrator, WithSetRole("other_owner"))
	if err == nil || !strings.Contains(err.Error(), "SET ROLE other_owner") {
		t.Fatalf("Run(WithSetRole(other_owner)) = %v, want SET ROLE error", err)
	}
	if got := relOwners(t, admin, "widgets", "schema_migrations"); len(got) != 0 {
		t.Fatalf("objects created after a denied SET ROLE: %v", got)
	}
}

// The switch is in effect on the migration connection itself: a migration
// that records current_user sees the owner, not the login.
func TestRun_WithSetRoleMigrationRunsAsOwner(t *testing.T) {
	admin := startPostgresImage(t, "postgres:18-alpine")
	migrator := setupThreeRoles(t, admin)
	fsys := fstest.MapFS{
		"sql/0001_who.up.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE who (name TEXT NOT NULL); INSERT INTO who VALUES (current_user);`)},
		"sql/0001_who.down.sql": &fstest.MapFile{Data: []byte(`DROP TABLE who;`)},
	}
	if err := Run(fsys, "sql", migrator, WithSetRole("svc_owner")); err != nil {
		t.Fatalf("Run with SET ROLE: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer conn.Close(ctx)
	var name string
	if err := conn.QueryRow(ctx, `SELECT name FROM who`).Scan(&name); err != nil {
		t.Fatalf("read who: %v", err)
	}
	if name != "svc_owner" {
		t.Fatalf("current_user during migration = %q, want svc_owner", name)
	}
}
