buf lint
# pkg

[![godev](https://img.shields.io/static/v1?label=godev&message=reference&color=00add8)](https://pkg.go.dev/github.com/duynhlab/pkg)
[![build](https://github.com/duynhlab/pkg/actions/workflows/check.yml/badge.svg)](https://github.com/duynhlab/pkg/actions)

Shared Go library for the duynhlab microservices platform — common gRPC,
auth, observability, database, logging, migration and protobuf helpers so
services don't reimplement them.

## Packages

### Observability & Instrumentation
- **[github.com/duynhlab/pkg/obsx](./obsx)** - OpenTelemetry SDK wiring, traces, metrics and log↔trace bridging

### gRPC & Transport
- **[github.com/duynhlab/pkg/grpcx](./grpcx)** - gRPC server/client helpers (otelgrpc, health, reflection, Dial)

### Authentication & Middleware
- **[github.com/duynhlab/pkg/authmw](./authmw)** - Fail-closed Gin JWT middleware (RS256 + JWKS)

### Database
- **[github.com/duynhlab/pkg/dbx](./dbx)** - Postgres `pgxpool` builder with otelpgx query tracing and pool metrics

### Temporal
- **[github.com/duynhlab/pkg/temporalx](./temporalx)** - Temporal client/worker bootstrap with tracing and versioning helpers

### Migrations
- **[github.com/duynhlab/pkg/migratex](./migratex)** - Embedded SQL migrations runner (golang-migrate)

### HTTP & Utilities
- **[github.com/duynhlab/pkg/httpx](./httpx)** - HTTP helpers: error responses and pagination
- **[github.com/duynhlab/pkg/idempotency](./idempotency)** - Idempotency helpers and repository abstractions

### Logging
- **[github.com/duynhlab/pkg/logger/zerolog](./logger/zerolog)** - zerolog helpers
- **[github.com/duynhlab/pkg/logger/clog](./logger/clog)** - slog + clog bridge
- **[github.com/duynhlab/pkg/logger/zapx](./logger/zapx)** - zap helpers and zap↔OTLP wiring

### Protobuf / Contracts
- **[proto/*/v1](./proto)** - Versioned `.proto` contracts and committed generated `*.pb.go` stubs

For authoritative per-package guidance see [AGENTS.md](AGENTS.md).

## Usage

Typical wiring (observability, DB pool, gRPC):

```go
import (
	"context"
	"github.com/duynhlab/pkg/dbx"
	"github.com/duynhlab/pkg/grpcx"
	"github.com/duynhlab/pkg/obsx"
)

ctx := context.Background()
obs, _ := obsx.SetupObservability(ctx, obsx.ConfigFromEnv())
defer obs.Shutdown(ctx)

pool, _ := dbx.NewPool(ctx, "postgres://user:pass@localhost/db")
defer pool.Close()

srv, _ := grpcx.NewServer()
_ = srv
```

## gRPC / Protobuf

Contracts live in `proto/<service>/v1/*.proto` and are compiled with `buf`.
Generated `*.pb.go` files are committed; regenerate with `buf generate` after
editing a `.proto`.

Prereqs (one-time):

```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
buf lint
buf generate
```

## Development

Run tests and linters locally:

```bash
go test -race ./...
golangci-lint run
```

CI runs `go-check`, `buf` and SonarCloud. See [AGENTS.md](AGENTS.md) for
contribution conventions (branch naming, commit message style).

## License

MIT
