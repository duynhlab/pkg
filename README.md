# pkg

Shared Go library for the **duynhlab** microservices platform — the common gRPC,
auth, observability, logging, database-migration and protobuf code so the
services (`auth`, `user`, `product`, `cart`, `order`, `review`, `shipping`,
`notification`) don't reimplement it.

```bash
go get github.com/duynhlab/pkg
```

Consumers pin a tag (`github.com/duynhlab/pkg@vX.Y.Z`); Renovate keeps services
up to date.

## Packages

| Package | What it provides |
|---------|------------------|
| `grpcx` | gRPC server/client helpers for east-west calls — `NewServer` (otelgrpc + health + reflection), `Dial` (otelgrpc + `round_robin` over `dns:///` + default per-RPC timeout), `WithAuthToken`/`TokenFromContext` for `authorization` metadata. Plaintext transport (mTLS later). |
| `authmw` | Fail-closed Gin JWT middleware — validates the bearer token via auth `GetMe` over gRPC; missing/invalid → 401, auth unreachable → 503 (still denies); sets `user_id`/`username`/`email` on the context. |
| `obsx` | Observability bootstrap: `SetupMetrics` (gRPC RED metrics on the existing `/metrics`), `SetupProfiling` (Pyroscope continuous profiling), `TracerProviderWithProfiles` (traces↔profiles), `TraceIDFromContext` (log↔trace correlation). |
| `temporalx` | Temporal client/worker bootstrap with the OTel tracing interceptor wired in — `Dial(Config{HostPort, Namespace})`, `NewWorker(client, taskQueue)`. Used by the order-fulfillment saga. |
| `migratex` | Runs embedded SQL schema migrations with golang-migrate — `Run(fsys, dir, dsn)`. |
| `httpx` | Shared HTTP helpers — consistent error responses (`RespondError`) and pagination (`ParsePage`, `NewPaginated`). |
| `logger/zerolog`, `logger/clog`, `logger/zapx` | Structured loggers (`Setup(level)` + context helpers) with trace-ID injection. |
| `proto/<svc>/v1` | Versioned `.proto` contracts + **committed** generated stubs for `auth`, `notification`, `product`, `review`, `shipping`. |

> Authoritative per-package detail lives in [`AGENTS.md`](AGENTS.md).

## Usage

```go
import (
	"github.com/duynhlab/pkg/grpcx"
	"github.com/duynhlab/pkg/obsx"
	"github.com/duynhlab/pkg/logger/zerolog"
)

zerolog.Setup("info")

// gRPC RED metrics on the existing /metrics endpoint.
stopMetrics, _ := obsx.SetupMetrics()
defer stopMetrics(ctx)

// Continuous profiling (gated by your own PROFILING_ENABLED; reads PYROSCOPE_ENDPOINT).
stopProfiling, err := obsx.SetupProfiling()
if err == nil {
	defer stopProfiling(ctx)
}

// gRPC server + client.
srv, health := grpcx.NewServer()            // otel + health + reflection
conn, _ := grpcx.Dial("dns:///shipping-grpc.shipping.svc.cluster.local:9090") // otel + round_robin
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
