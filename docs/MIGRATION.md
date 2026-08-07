# Migrating services to the per-module pkg

As of the multi-module split, `github.com/duynhlab/pkg` no longer publishes a
single version. Each module is tagged separately (`httpx/v0.36.0`,
`logger/zapx/v0.36.0`, …) and services require only the modules they import.

**Nothing is urgent.** The old single-module tags (`v0.35.0` and earlier)
resolve forever — a service keeps building until it chooses to migrate.
Migrate one service per PR.

## What changes and what doesn't

- **Go import paths do not change.** `import "github.com/duynhlab/pkg/httpx"`
  stays exactly as-is in every `.go` file.
- **Only `go.mod` changes.** One `require github.com/duynhlab/pkg v0.35.0`
  becomes N per-module requires.
- New code lands only in per-module tags (`v0.36.0` onward); the single-module
  line is frozen at `v0.35.0` and will never publish another version.

## Per-service module map

Which modules each service actually imports today (verify with the grep below
before editing):

| Service | Modules used |
|---|---|
| auth | dbx, httpx, migratex, obsx, logger/zapx |
| user | dbx, httpx, migratex, obsx, logger/zapx, authmw |
| cart | dbx, httpx, migratex, obsx, logger/zapx, authmw, grpcx, proto |
| checkout | dbx, httpx, migratex, obsx, logger/zapx, authmw, grpcx, proto, idempotency, temporalx |
| notification | dbx, httpx, migratex, obsx, logger/zapx, authmw, grpcx, proto |
| order | dbx, httpx, migratex, obsx, logger/zapx, authmw, grpcx, proto, temporalx, flagx |
| payment | dbx, httpx, migratex, obsx, logger/zapx, authmw, grpcx, proto, idempotency |
| product | dbx, httpx, migratex, obsx, logger/zapx, grpcx, proto |
| review | dbx, httpx, migratex, obsx, logger/zapx, authmw, grpcx, proto |
| shipping | dbx, httpx, migratex, obsx, logger/zapx, grpcx, proto |

## Steps (per service)

1. List what the service really imports:

   ```sh
   grep -rho 'github.com/duynhlab/pkg/[a-z/]*' --include='*.go' . | sort -u
   ```

   Map each hit to its module: `proto/<svc>/v1` → module `proto`;
   `logger/zapx` → module `logger/zapx`; everything else is its own module.

2. In `go.mod`, drop the old require and add one per module:

   ```diff
   -require github.com/duynhlab/pkg v0.35.0
   +require (
   +	github.com/duynhlab/pkg/dbx v0.36.0
   +	github.com/duynhlab/pkg/httpx v0.36.0
   +	github.com/duynhlab/pkg/logger/zapx v0.36.0
   +	github.com/duynhlab/pkg/migratex v0.36.0
   +	github.com/duynhlab/pkg/obsx v0.36.0
   +)
   ```

3. Update the commented local-dev replace block the same way:

   ```diff
   -// replace github.com/duynhlab/pkg => ../pkg
   +// replace github.com/duynhlab/pkg/obsx => ../pkg/obsx
   +// replace github.com/duynhlab/pkg/logger/zapx => ../pkg/logger/zapx
   ```

   (one line per module the service uses — uncomment only what you're
   developing against locally).

4. `go mod tidy`, build, run the service's test gate. Expect the `go.sum` to
   shrink noticeably: modules the service doesn't use (temporal, moby/
   testcontainers, pyroscope) no longer appear.

5. Commit as a single `chore` PR; no `.go` file should change.

## Pitfalls

- **`logger/zapx` and `obsx` version together.** Every service's
  `middleware/logging.go` pairs `zapx.New` with `obsx.ZapCore` — when bumping
  one, bump the other to the tag cut from the same `main`.
- **`dbx` and `idempotency` share pgx.** Both expose `*pgxpool.Pool` in their
  APIs; keep them on tags with the same pgx major version.
- **Don't mix the old and new lines.** Requiring both
  `github.com/duynhlab/pkg@v0.35.0` (old) and
  `github.com/duynhlab/pkg/obsx@v0.36.0` (new) does **not** build: both
  modules provide the same package path and Go fails immediately with
  `ambiguous import: found package ... in multiple modules`. Remove the old
  require entirely in the same PR. No newer root version will ever exist, so
  if a *transitive* dependency still pins the old `pkg`, migrate that
  dependency first.
- **Tags without a `v` prefix don't exist.** The module tag is
  `obsx/v0.36.0`; the `go get` spelling is
  `go get github.com/duynhlab/pkg/obsx@v0.36.0`.
