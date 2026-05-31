# pkg

Shared Go library for monitoring platform microservices.

## Features

- **Logging**: Zerolog implementation with structured JSON output
- **Tracing**: OpenTelemetry span helpers
- **gRPC (`grpcx`)**: shared server/client helpers for internal east-west calls — OpenTelemetry instrumentation, the gRPC health protocol, server reflection, and client-side round-robin load balancing.
- **Protobuf contracts (`proto/`)**: versioned `.proto` definitions and generated Go stubs (e.g. `proto/shipping/v1`). Phase 0 of the gRPC migration — see `homelab/docs/api/grpc-internal-comms.md`.
- **Common utilities**: Shared code across services

## Installation

```bash
go get github.com/duynhne/pkg
```

## gRPC / Protobuf

Contracts live in `proto/<service>/v1/*.proto` and are compiled with [buf](https://buf.build) using local plugins (`protoc-gen-go`, `protoc-gen-go-grpc`).

```bash
# one-time: install codegen tools
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# regenerate stubs after editing a .proto
buf lint
buf generate
```

`buf lint` and `buf breaking` (against `main`) run in CI; generated `*.pb.go` files are committed. Build a server / dial a peer with the `grpcx` helpers:

```go
import "github.com/duynhne/pkg/grpcx"

srv, health := grpcx.NewServer()            // otel + health + reflection
conn, _ := grpcx.Dial("dns:///shipping-grpc.shipping.svc.cluster.local:9090") // otel + round_robin
```

## Usage

```go
import "github.com/duynhne/pkg/logger/zerolog"

func main() {
    zerolog.Setup("info")
    zerolog.Info("Application started")
}
```

## License

MIT
