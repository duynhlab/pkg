# AGENTS.md

Agent guide for `pkg`, the shared Go library for the duynhlab platform.

## Contribution workflow

Commit messages:

- **No attribution trailers.** Never add `Co-authored-by`, `Generated-by`,
  `Signed-off-by`, `Assisted-by`, or any AI/tool attribution.
- Subject: ≤ 50 chars, capitalised, imperative mood, no trailing period
  (`Add deadline interceptor`, not `Added` / `Adds` / `.`).
- Body (only if non-trivial): explain *what* and *why*, wrap at 72 chars,
  one blank line after the subject.
- No issue references (`Fixes #123`) and no `@`-mentions. Put those in the PR.

Branch and PR:

- **Never push to `main`.** Branch first: `<type>/<desc>` where `<type>` ∈
  `feat` `fix` `chore` `docs` `refactor` `ci` (e.g. `feat/grpc-mtls`).
- Open a PR against `main`; **squash-merge**.
- CI (`.github/workflows/check.yml`) must be green: `go-check`, `buf`, `sonar`.

## Code quality

- Idiomatic Go: small interfaces, accept `context.Context` first, return errors
  (don't panic in library code).
- Wrap errors with context (`fmt.Errorf("...: %w", err)`); never swallow them.
- **Fail closed** in security-critical paths (see `authmw`).
- Never log secrets, tokens, or bearer headers.
- Table-driven tests for every exported function; keep `go test ./...` green.
- Doc comment on every exported symbol; match existing package style.

## Project overview

`pkg` is the **shared Go library** for the duynhlab microservices platform —
common gRPC, auth, observability, logging, and protobuf code so services don't
copy-paste it.

- Module: `github.com/duynhlab/pkg`
- Consumed by `auth`, `user`, `product`, `cart`, `order`, `review`,
  `notification`, `shipping` services.

## Repository layout

| Path | What it provides |
|------|------------------|
| **`grpcx/`** | Shared gRPC server/client helpers for east-west calls. `NewServer` (otelgrpc stats handler + gRPC health service + server reflection); `Dial` (otelgrpc + client-side `round_robin` over `dns:///` headless Services + default per-RPC deadline `DefaultCallTimeout` = 5s). Transport is currently plaintext (mTLS is a later phase). |
| **`authmw/`** | Single fail-closed gin JWT middleware. Verifies RS256 bearer tokens locally against a cached, background-refreshed JWKS (`NewVerifier(jwksURL, iss, aud)` + `MiddlewareJWT(verifier)`), pinning issuer/audience and the RS256 alg. Missing header / invalid token → 401; JWKS never loaded → 503 (still denies); nil verifier → 503. Sets `user_id` / `username` / `email` on the gin context. JWT-only — the opaque-token `GetMe` fallback was removed in RFC-0009 Phase 5. |
| **`obsx/`** | `SetupObservability` — the single OTel SDK wiring point (call once in `main`, defer `Shutdown`). Builds per-signal providers from `Config`/`ConfigFromEnv`: traces (OTLP/HTTP, ParentBased sampler + W3C propagator), metrics (OTLP/HTTP PeriodicReader with SLO-preserving Views + Go runtime instrumentation; on by default since the RFC-0014 P3 cutover), logs (OTLP/HTTP via the `otelzap` bridge, opt-in). Installs the enabled providers as the OTel globals so contrib instrumentation (otelgin, otelgrpc, Temporal SDK) exports with no extra wiring — there is **no Prometheus exporter, no `/metrics` endpoint, and no default-registry bridge** (the scrape-era `SetupMetrics` was removed at P3). `TraceIDFromContext` returns the active span's trace ID for log↔trace correlation. |
| **`temporalx/`** | Temporal bootstrap helpers mirroring `grpcx`/`obsx`. `Dial(Config{HostPort, Namespace})` connects to the frontend with the OpenTelemetry tracing interceptor registered, so workflow/activity spans join the trace of the request that started them; `NewWorker(client, taskQueue)` builds a worker that inherits that interceptor. Plaintext transport (mTLS is a later phase). Used by the order-fulfillment saga. |
| **`logger/zerolog/`** | `rs/zerolog` logger: `Setup(level)`, context helpers with trace-ID injection. |
| **`logger/clog/`** | `log/slog` + `chainguard-dev/clog` logger: `TracingHandler`, `Setup(level)`, `*Context` helpers. |
| **`proto/<svc>/v1/`** | Versioned `.proto` contracts + **committed** generated stubs (`*.pb.go`, `*_grpc.pb.go`) for `notification`, `product`, `review`, `shipping`. `product` (`ReserveStock`/`ReleaseStock`) and `shipping` (`CreateShipment`/`CancelShipment`) are the order-fulfillment saga contracts. (`auth/v1` was removed in RFC-0009 Phase 5 — services verify JWTs locally, no GetMe RPC.) |

## Build, test, lint

```bash
# Build, vet, test (Go 1.25; auto-fetch the toolchain)
GOTOOLCHAIN=auto go build ./... && go vet ./... && go test ./...

# Race + coverage (matches CI)
go test -race -coverprofile=coverage.out ./...

# Lint
golangci-lint run

# Protobuf: lint, regenerate stubs, breaking-change check
buf lint
buf generate
buf breaking --against 'https://github.com/duynhlab/pkg.git#branch=main'
```

Generated `*.pb.go` stubs are **committed** — regenerate with `buf generate`
after editing a `.proto`, then commit the result.

## Conventions

- **Versioned releases.** Consumers pin a tag:
  `go get github.com/duynhlab/pkg@vX.Y.Z` — check `git tag --sort=-v:refname` for the latest.
- Protobuf package is `<svc>.v1`; contracts live in `proto/<svc>/v1/*.proto`.
- `buf` drives codegen (`buf.gen.yaml`: `protoc-gen-go` + `protoc-gen-go-grpc`,
  `paths=source_relative`) and lint/breaking checks (`buf.yaml`).
- Local development against a service: add a `replace` directive in the
  service's `go.mod` (`replace github.com/duynhlab/pkg => ../pkg`).

## Gotchas

- **Don't hand-edit generated stubs.** `*.pb.go` / `*_grpc.pb.go` are committed
  but regenerated from `.proto` via `buf generate`; edits will be overwritten.
- **A new version needs a tag.** Pushing to `main` does not update consumers —
  cut a `vX.Y.Z` tag so services can `go get` the change.
- **`buf breaking` runs in CI** against `main`; a backward-incompatible proto
  change fails the PR. Bump the package version / add new fields instead of
  changing existing ones.
