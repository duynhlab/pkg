# pkg

[![godev](https://img.shields.io/static/v1?label=godev&message=reference&color=00add8)](https://pkg.go.dev/github.com/duynhlab/pkg)
[![build](https://github.com/duynhlab/pkg/actions/workflows/check.yml/badge.svg)](https://github.com/duynhlab/pkg/actions)

Shared Go SDK for the duynhlab microservices platform — common gRPC, auth,
observability, database, logging, migration and protobuf code so services
don't reimplement it.

This is a **multi-module monorepo**: the top-level `go.mod` is a deprecated,
package-less placeholder (the single-module line is frozen at `v0.35.0`). Each
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
root checks nothing, because the root module contains no packages:

```bash
make modules                  # list discovered modules
make test                     # tidy+fmt+vet+lint+test for every module
make test-obsx                # one module ("/" becomes ":" — make test-logger:zapx)
make test TAGS=integration    # include testcontainers tests (needs Docker)
make generate-proto           # buf generate + buf lint after editing a .proto
make release-obsx VER=0.36.0  # tag and push obsx/v0.36.0
```

CI (`.github/workflows/check.yml`) gates on the same `make test`, `buf`
lint/breaking, and SonarCloud.

## Releasing

Each module is tagged separately, `<module>/vX.Y.Z`. Cut a tag only from
`main`, after the content is verified — a pushed tag is cached by the Go
module proxy immediately and cannot be fixed, only superseded:

```bash
make release-httpx VER=0.36.1
```

If modules ever depend on each other, tag dependencies first (Layer 0 → 1 → 2).

## License

MIT
