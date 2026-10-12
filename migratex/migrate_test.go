package migratex

import (
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
