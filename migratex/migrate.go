// Package migratex runs embedded SQL schema migrations with golang-migrate.
//
// Each service embeds its own migration files via embed.FS and calls Run at
// startup (typically from a `migrate` subcommand executed in an init container
// against the DIRECT database host, never a transaction pooler — DDL is unsafe
// through PgBouncer/PgDog).
package migratex

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	// pgx/v5 database driver: registers the "pgx5" URL scheme.
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// sourceName is the name golang-migrate reports for the embedded source.
const sourceName = "iofs"

// Option configures Run.
type Option func(*options)

type options struct {
	role    string
	roleSet bool
}

// WithSetRole runs `SET ROLE <role>` on every connection before any migration
// executes, so the objects the migrations create (and the schema_migrations
// table) are owned by role rather than by the login in the DSN.
//
// This is how a migrator login that is a NOINHERIT member of the schema owner
// acts as that owner. A denied SET ROLE fails Run; it never falls back to the
// login's own identity. An empty role is rejected, so pass the value
// unconditionally (WithSetRole(os.Getenv("DB_MIGRATION_ROLE"))) and a missing
// variable fails the run instead of skipping the switch.
//
// The role is session state: dsn must reach PostgreSQL directly (or through a
// session-mode pool), and a migration file must not run RESET ROLE, SET ROLE,
// SET SESSION AUTHORIZATION or DISCARD, which would end the switch for every
// later statement. golang-migrate "x-" query parameters are not supported on
// this path and are rejected.
func WithSetRole(role string) Option {
	return func(o *options) {
		o.role = role
		o.roleSet = true
	}
}

// Run applies all pending up-migrations from the embedded SQL files in fsys
// (rooted at dir, e.g. "sql") to the database at dsn. dsn is a standard
// postgres://user:pass@host:port/db?sslmode=... URL — Run rewrites it to the
// golang-migrate pgx/v5 ("pgx5") scheme.
//
// It is a no-op when the schema is already current. NOTE: a failed migration
// leaves golang-migrate's version marked "dirty"; recover by fixing the data
// and forcing the version before retrying.
func Run(fsys fs.FS, dir, dsn string, opts ...Option) error {
	var o options
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.roleSet && o.role == "" {
		return errors.New("migratex: WithSetRole: empty role")
	}

	src, err := iofs.New(fsys, dir)
	if err != nil {
		return err
	}

	var m *migrate.Migrate
	if o.roleSet {
		m, err = newWithRole(src, dsn, o.role)
	} else {
		m, err = migrate.NewWithSourceInstance(sourceName, src, pgxURL(dsn))
	}
	if err != nil {
		return err
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// newWithRole opens the database through database/sql so every pooled
// connection runs SET ROLE before golang-migrate uses it.
func newWithRole(src source.Driver, dsn, role string) (*migrate.Migrate, error) {
	pgDSN := postgresURL(dsn)
	u, err := url.Parse(pgDSN)
	if err != nil {
		return nil, fmt.Errorf("migratex: parse dsn: %w", err)
	}
	for k := range u.Query() {
		if strings.HasPrefix(k, "x-") {
			return nil, fmt.Errorf("migratex: WithSetRole does not support the %q dsn parameter", k)
		}
	}
	cfg, err := pgx.ParseConfig(pgDSN)
	if err != nil {
		return nil, fmt.Errorf("migratex: parse dsn: %w", err)
	}
	setRole := "SET ROLE " + pgx.Identifier{role}.Sanitize()
	db := stdlib.OpenDB(*cfg, stdlib.OptionAfterConnect(func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, setRole); err != nil {
			// stdlib does not close a connection whose AfterConnect fails.
			_ = conn.Close(ctx)
			return fmt.Errorf("migratex: SET ROLE %s: %w", role, err)
		}
		return nil
	}))

	// DatabaseName as the URL path, exactly as the driver's Open sets it, so
	// both paths derive the same advisory-lock id and serialise against each
	// other.
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{DatabaseName: u.Path})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	m, err := migrate.NewWithInstance(sourceName, src, "pgx5", driver)
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	return m, nil
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

// postgresURL is the inverse of pgxURL for pgx.ParseConfig, which does not
// know the "pgx5" scheme.
func postgresURL(dsn string) string {
	if after, ok := strings.CutPrefix(dsn, "pgx5://"); ok {
		return "postgres://" + after
	}
	return dsn
}
