# pkg

[![godev](https://img.shields.io/static/v1?label=godev&message=reference&color=00add8)](https://pkg.go.dev/github.com/duynhlab/pkg)
[![build](https://github.com/duynhlab/pkg/actions/workflows/check.yml/badge.svg)](https://github.com/duynhlab/pkg/actions)

Shared Go SDK for the duynhlab microservices platform — common gRPC, auth,
observability, database, logging, migration and protobuf code so services
don't reimplement it.

This is a **multi-module monorepo**: there is no top-level `go.mod` (the
single-module line is frozen at `v0.35.0`). Each
module below has its own `go.mod` and is versioned and tagged independently
(`<module>/vX.Y.Z`), so a service pulls in only what it imports:

```bash
go get github.com/duynhlab/pkg/httpx@v0.36.0        # tag: httpx/v0.36.0
go get github.com/duynhlab/pkg/logger/zapx@v0.36.0  # tag: logger/zapx/v0.36.0
```

Module tags continue the pre-split numbering (the last single-module tag was
`v0.35.0`). Migrating a service from the single-module `pkg`? See
[docs/MIGRATION.md](docs/MIGRATION.md) — import paths don't change, only
`go.mod` does.

## Modules

Modules are layered; lower layers never import higher ones (see
[AGENTS.md](AGENTS.md) for the full dependency rules).

| Module | Layer | What it provides |
|--------|-------|------------------|
| [`proto`](./proto) | 0 | Versioned gRPC contracts for all services (`<svc>/v1/*.proto`) with **committed** generated stubs. |
| [`logger/zapx`](./logger/zapx) | 0 | zap logger construction with trace-ID injection — the production default, pairs with `obsx.ZapCore`. |
| [`logger/clog`](./logger/clog) | 0 | `log/slog` + chainguard-dev/clog logger with trace-context correlation. |
| [`logger/zerolog`](./logger/zerolog) | 0 | rs/zerolog logger with trace-ID injection. |
| [`flagx`](./flagx) | 0 | Startup-validated environment flags (`Enum`, `Percent` + `Must*`) — fail fast, bounded values safe for metric labels. |
| [`httpx`](./httpx) | 1 | HTTP helpers on gin: consistent error responses and pagination. |
| [`grpcx`](./grpcx) | 1 | gRPC server/client for east-west calls: otelgrpc, health, reflection, panic recovery, access logs, error reasons. |
| [`authmw`](./authmw) | 1 | Fail-closed gin JWT middleware (RS256 + cached JWKS, issuer/audience pinned). |
| [`idempotency`](./idempotency) | 1 | Stripe-style idempotency keys: `Record`, sentinel errors, Postgres `Repository` over `*pgxpool.Pool`. |
| [`obsx`](./obsx) | 2 | OpenTelemetry SDK bootstrap — traces + metrics + logs over OTLP, zap bridge, Pyroscope profiling. The only module linking the OTel SDK. |
| [`dbx`](./dbx) | 2 | Postgres `pgxpool` builder with otelpgx tracing and pool metrics, pooler-safe settings, no PII in telemetry. |
| [`migratex`](./migratex) | 2 | Embedded SQL migrations runner (golang-migrate). |
| [`temporalx`](./temporalx) | 2 | Temporal client/worker bootstrap with OTel tracing and Worker Deployment Versioning. |

Authoritative per-module detail and contribution rules live in
[AGENTS.md](AGENTS.md).

## Usage

Typical `main()` wiring (observability, DB pool, gRPC):

```go
import (
	"context"

	"github.com/duynhlab/pkg/dbx"
	"github.com/duynhlab/pkg/grpcx"
	"github.com/duynhlab/pkg/logger/zapx"
	"github.com/duynhlab/pkg/obsx"
)

ctx := context.Background()

log, _ := zapx.New("info")

// One-call OTel SDK wiring — traces + metrics + logs over OTLP.
obs, _ := obsx.SetupObservability(ctx, obsx.ConfigFromEnv())
defer obs.Shutdown(ctx)

// Postgres pool with query tracing + pool-stat metrics baked in.
pool, _ := dbx.NewPool(ctx, "postgres://user:pass@localhost/db")
defer pool.Close()

// gRPC server (otel + health + reflection + access logs) and client.
srv, health := grpcx.NewServer(log)
conn, _ := grpcx.Dial("dns:///shipping.shipping.svc.cluster.local:9090")
_, _, _ = srv, health, conn
```

## Development

All workflows go through the root `Makefile` — `go test ./...` at the repo
root checks nothing, because there is no root module:

```bash
make modules                  # list discovered modules
make test                     # tidy+fmt+vet+lint+test for every module
make test-obsx                # one module ("/" becomes ":" — make test-logger:zapx)
make test TAGS=integration    # include testcontainers tests (needs Docker)
make generate-proto           # buf generate + buf lint after editing a .proto
make release-obsx VER=0.36.0  # tag and push obsx/v0.36.0
```

CI (`.github/workflows/check.yml`) gates on the same `make test`, `buf`
lint/breaking, and SonarCloud; CodeQL analyzes Go and workflow files, and
repo labels are declaratively synced from `.github/labels.yaml`.

## Releasing

Each module is tagged separately, `<module>/vX.Y.Z`. A pushed tag is cached
by the Go module proxy immediately and cannot be fixed, only superseded by a
new patch version. `make release-<module>` guards the common mistakes: it
refuses a dirty tree, a HEAD not on `origin/main`, a non-semver `VER`, and an
unknown module.

**Step 0 — prepare** (release always cuts from pushed `main`):

```bash
git checkout main
git pull --ff-only origin main   # HEAD must already be on origin/main
git status                       # working tree must be clean
make modules                     # confirm all modules are discovered
```

**Step 1 — tag each module you're releasing.** Every command tags
`<module>/v<VER>` and pushes it immediately; nested modules use `:` instead
of `/`. `VER` carries no `v` prefix. Releasing everything at once:

```bash
# Layer 0
make release-proto          VER=0.36.0
make release-flagx          VER=0.36.0
make release-logger:zapx    VER=0.36.0
make release-logger:zerolog VER=0.36.0
make release-logger:clog    VER=0.36.0

# Layer 1
make release-httpx          VER=0.36.0
make release-grpcx          VER=0.36.0
make release-authmw         VER=0.36.0
make release-idempotency    VER=0.36.0

# Layer 2
make release-obsx           VER=0.36.0
make release-dbx            VER=0.36.0
make release-migratex       VER=0.36.0
make release-temporalx      VER=0.36.0
```

Order does not matter today (no cross-module dependencies); the layer
grouping is habit-forming — the moment one module requires another, its
dependency must be tagged first (Layer 0 → 1 → 2). Each pushed tag triggers
the `release` workflow, which publishes a GitHub Release with generated
notes.

**Step 2 — verify:**

```bash
git tag --sort=-creatordate | head -13

# The proxy must resolve the new versions (spot-check a few):
GOPROXY=https://proxy.golang.org go list -m github.com/duynhlab/pkg/httpx@v0.36.0
GOPROXY=https://proxy.golang.org go list -m github.com/duynhlab/pkg/logger/zapx@v0.36.0
GOPROXY=https://proxy.golang.org go list -m github.com/duynhlab/pkg/obsx@v0.36.0
```

If the repo is private, go through git instead of the public proxy:
`GOPRIVATE=github.com/duynhlab go list -m ...`.

## License

MIT
