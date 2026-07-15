# pkg

Shared Go library for the **duynhlab** microservices platform — the common gRPC,
auth, observability, database, logging, database-migration and protobuf code so
the services (`auth`, `user`, `product`, `cart`, `order`, `review`, `shipping`,
`notification`, `payment`, `checkout`) don't reimplement it.

```bash
go get github.com/duynhlab/pkg
```

Consumers pin a tag (`github.com/duynhlab/pkg@vX.Y.Z`); Renovate keeps services
up to date.

## Packages

| Package | What it provides |
|---------|------------------|
| `grpcx` | gRPC server/client helpers for east-west calls — `NewServer` (otelgrpc + health + reflection), `Dial` (otelgrpc + `round_robin` over `dns:///` + default per-RPC timeout). Plaintext transport (mTLS later). |
| `authmw` | Fail-closed Gin JWT middleware — verifies RS256 bearer tokens locally against a cached JWKS (issuer/audience pinned, RS256-only); missing/invalid → 401, JWKS unavailable → 503 (still denies); sets `user_id`/`username`/`email` on the context. |
| `obsx` | OpenTelemetry bootstrap — the single SDK wiring point. `SetupObservability(ctx, ConfigFromEnv())` builds traces + metrics + logs over OTLP (no scrape endpoint since RFC-0014 P3) and returns one `Shutdown`; `ZapCore` tees zap logs into the OTLP pipeline; `TraceContext(ctx)` binds a span to a log so the bridge stamps native trace_id/span_id; `SetupProfiling` (Pyroscope), `TracerProviderWithProfiles` (traces↔profiles), `TraceIDFromContext` (trace-id string). |
| `dbx` | Postgres pool pre-wired with OpenTelemetry — `NewPool(ctx, dsn, opts...)` builds a `pgxpool` with otelpgx query tracing (bounded span names, no bind-parameter/connection PII), `pgxpool.*` pool-stat metrics, and the transaction-mode-pooler-safe settings (simple protocol, caches off). The one place the fleet configures DB instrumentation. |
| `temporalx` | Temporal client/worker bootstrap with the OTel tracing interceptor wired in — `Dial(Config{HostPort, Namespace})`, `NewWorker(client, taskQueue)`. Used by the order-fulfillment saga. |
| `migratex` | Runs embedded SQL schema migrations with golang-migrate — `Run(fsys, dir, dsn)`. |
| `httpx` | Shared HTTP helpers — consistent error responses (`RespondError`) and pagination (`ParsePage`, `NewPaginated`). |
| `logger/zerolog`, `logger/clog`, `logger/zapx` | Structured loggers (`Setup(level)` + context helpers) with trace-ID injection. |
| `proto/<svc>/v1` | Versioned `.proto` contracts + **committed** generated stubs for `notification`, `product`, `review`, `shipping`. |

> Authoritative per-package detail lives in [`AGENTS.md`](AGENTS.md).

## Usage

```go
import (
	"github.com/duynhlab/pkg/dbx"
	"github.com/duynhlab/pkg/grpcx"
	"github.com/duynhlab/pkg/obsx"
	"go.uber.org/zap"
)

// One-call OTel SDK wiring — traces + metrics + logs over OTLP. Defer Shutdown.
obs, _ := obsx.SetupObservability(ctx, obsx.ConfigFromEnv())
defer obs.Shutdown(ctx)

// Continuous profiling (gated by PROFILING_ENABLED; reads PYROSCOPE_ENDPOINT).
if stopProfiling, err := obsx.SetupProfiling(); err == nil {
	defer stopProfiling(ctx)
}

// Postgres pool with query tracing + pool-stat metrics baked in.
pool, _ := dbx.NewPool(ctx, cfg.Database.BuildDSN(), dbx.WithMaxConns(cfg.Database.MaxConnections))
defer pool.Close()

// gRPC server + client.
srv, health := grpcx.NewServer()            // otel + health + reflection
conn, _ := grpcx.Dial("dns:///shipping.shipping.svc.cluster.local:9090") // otel + round_robin

// Native log↔trace correlation from a repo/handler.
log.Error("query failed", zap.Error(err), obsx.TraceContext(ctx))
```

## gRPC / Protobuf

Contracts live in `proto/<service>/v1/*.proto`, compiled with
[buf](https://buf.build) (`protoc-gen-go`, `protoc-gen-go-grpc`). Generated
`*.pb.go` stubs are **committed** — regenerate after editing a `.proto`:

```bash
# one-time: install codegen tools
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

buf lint
buf generate   # then commit the regenerated stubs
```

`buf lint` + `buf breaking` (against `main`) run in CI.

## Development

```bash
go test -race ./...     # race + coverage (matches CI)
golangci-lint run
```

CI (`.github/workflows/check.yml`) gates on `go-check` (test + lint), `buf`, and
SonarCloud. See [`AGENTS.md`](AGENTS.md) for contribution conventions (branch
prefixes, ≤50-char commit subjects, no attribution trailers).

## License

MIT
