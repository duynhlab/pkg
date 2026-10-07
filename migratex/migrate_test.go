package migratex

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestRunBadSource verifies Run surfaces the iofs source error (and never
// reaches the database) when the migration directory is missing or empty.
func TestRunBadSource(t *testing.T) {
	fsys := fstest.MapFS{"sql/1_init.up.sql": {Data: []byte("SELECT 1;")}}
	tests := map[string]string{
		"missing dir": "nonexistent",
		"empty dir":   "empty",
	}
	for name, dir := range tests {
		t.Run(name, func(t *testing.T) {
			if err := Run(fsys, dir, "postgres://u:p@h:5432/db"); err == nil {
				t.Fatalf("Run(dir=%q) = nil, want error", dir)
			}
		})
	}
}

func TestPgxURL(t *testing.T) {
	tests := map[string]string{
		"postgres://u:p@h:5432/db?sslmode=require":   "pgx5://u:p@h:5432/db?sslmode=require",
		"postgresql://u:p@h:5432/db?sslmode=disable": "pgx5://u:p@h:5432/db?sslmode=disable",
		"pgx5://u:p@h:5432/db":                       "pgx5://u:p@h:5432/db",
		"mysql://u:p@h:3306/db":                      "mysql://u:p@h:3306/db",
	}
	for in, want := range tests {
		if got := pgxURL(in); got != want {
			t.Errorf("pgxURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// A valid source with an unusable DSN must surface the driver error from
// migrate itself — the branch after iofs.New succeeds.
func TestRun_UnknownDriverSchemeFails(t *testing.T) {
	fsys := fstest.MapFS{
		"sql/0001_init.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
	}
	if err := Run(fsys, "sql", "nosuchdriver://localhost/db"); err == nil {
		t.Fatal("Run succeeded with an unregistered database scheme")
	}
}

func TestPostgresURL(t *testing.T) {
	tests := map[string]string{
		"pgx5://u:p@h:5432/db":     "postgres://u:p@h:5432/db",
		"postgres://u:p@h:5432/db": "postgres://u:p@h:5432/db",
	}
	for in, want := range tests {
		if got := postgresURL(in); got != want {
			t.Errorf("postgresURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// WithSetRole("") must fail before anything is opened: an unset environment
// variable must not silently run migrations as the login itself.
func TestRun_WithSetRoleRejectsEmptyRole(t *testing.T) {
	fsys := fstest.MapFS{"sql/0001_init.up.sql": {Data: []byte("SELECT 1;")}}
	err := Run(fsys, "sql", "postgres://u:p@h:5432/db", WithSetRole(""))
	if err == nil || !strings.Contains(err.Error(), "empty role") {
		t.Fatalf("Run(WithSetRole(\"\")) = %v, want empty role error", err)
	}
}

// The WithSetRole path parses the DSN itself, so a malformed one fails there.
func TestRun_WithSetRoleBadDSN(t *testing.T) {
	fsys := fstest.MapFS{"sql/0001_init.up.sql": {Data: []byte("SELECT 1;")}}
	for _, dsn := range []string{
		"postgres://u:p@h:notaport/db", // pgx.ParseConfig rejects it
		"postgres://u:p@h:5432/db%zz",  // url.Parse rejects it
	} {
		err := Run(fsys, "sql", dsn, WithSetRole("owner"))
		if err == nil || !strings.Contains(err.Error(), "parse dsn") {
			t.Fatalf("Run(%q) = %v, want parse dsn error", dsn, err)
		}
	}
}

// An unreachable database fails in the driver's ping, and Run returns it.
func TestRun_WithSetRoleUnreachable(t *testing.T) {
	fsys := fstest.MapFS{"sql/0001_init.up.sql": {Data: []byte("SELECT 1;")}}
	err := Run(fsys, "sql", "postgres://u:p@127.0.0.1:1/db?connect_timeout=1", WithSetRole("owner"))
	if err == nil {
		t.Fatal("Run succeeded against an unreachable database")
	}
}

// golang-migrate "x-" parameters would reach PostgreSQL as unknown startup
// parameters on the WithSetRole path, so they are rejected up front.
func TestRun_WithSetRoleRejectsCustomQuery(t *testing.T) {
	fsys := fstest.MapFS{"sql/0001_init.up.sql": {Data: []byte("SELECT 1;")}}
	err := Run(fsys, "sql", "postgres://u:p@h:5432/db?x-migrations-table=m", WithSetRole("owner"))
	if err == nil || !strings.Contains(err.Error(), "x-migrations-table") {
		t.Fatalf("Run(x- param) = %v, want unsupported parameter error", err)
	}
}

// A nil Option is ignored rather than panicking.
func TestRun_NilOptionIgnored(t *testing.T) {
	fsys := fstest.MapFS{"sql/0001_init.up.sql": {Data: []byte("SELECT 1;")}}
	if err := Run(fsys, "nonexistent", "postgres://u:p@h:5432/db", nil); err == nil {
		t.Fatal("Run(nil option, missing dir) = nil, want source error")
	}
}

// A malformed DSN must not echo its password into the error, which callers
// log.
func TestRun_WithSetRoleParseErrorRedactsPassword(t *testing.T) {
	fsys := fstest.MapFS{"sql/0001_init.up.sql": {Data: []byte("SELECT 1;")}}
	err := Run(fsys, "sql", "postgres://u:s3cr3t@h:5432/db%zz", WithSetRole("owner"))
	if err == nil {
		t.Fatal("Run(bad dsn) = nil, want error")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("error leaks the password: %v", err)
	}
}
