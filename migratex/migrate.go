// Package migratex runs embedded SQL schema migrations with golang-migrate.
//
// Each service embeds its own migration files via embed.FS and calls Run at
// startup (typically from a `migrate` subcommand executed in an init container
// against the DIRECT database host, never a transaction pooler — DDL is unsafe
// through PgBouncer/PgDog).
package migratex

import (
	"errors"
	"io/fs"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	// pgx/v5 database driver: registers the "pgx5" URL scheme.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Run applies all pending up-migrations from the embedded SQL files in fsys
// (rooted at dir, e.g. "sql") to the database at dsn. dsn is a standard
// postgres://user:pass@host:port/db?sslmode=... URL — Run rewrites it to the
// golang-migrate pgx/v5 ("pgx5") scheme.
//
// It is a no-op when the schema is already current. NOTE: a failed migration
// leaves golang-migrate's version marked "dirty"; recover by fixing the data
// and forcing the version before retrying.
func Run(fsys fs.FS, dir, dsn string) error {
	src, err := iofs.New(fsys, dir)
	if err != nil {
		return err
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, pgxURL(dsn))
	if err != nil {
		return err
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// pgxURL normalises a postgres DSN to the "pgx5" scheme that the golang-migrate
// pgx/v5 driver registers; other schemes pass through unchanged.
func pgxURL(dsn string) string {
	for _, p := range []string{"postgres://", "postgresql://"} {
		if after, ok := strings.CutPrefix(dsn, p); ok {
			return "pgx5://" + after
		}
	}
	return dsn
}
