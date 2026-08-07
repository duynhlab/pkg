# AGENTS.md

Guidance for AI coding assistants working in `duynhlab/pkg`. Read this file before making changes.

## Contribution workflow for AI agents

- **No attribution trailers.** Never add `Co-authored-by`, `Generated-by`, `Signed-off-by`, `Assisted-by`, or any AI/tool attribution to commits.
- **Commit message format:** Subject in imperative mood ("Add feature X" instead of "Adding feature X"), capitalized, no trailing period, ≤50 characters. Prefix with the module when the change is scoped to one: `obsx: Add metric exporter`. Body wrapped at 72 columns, explaining what and why. No `@mentions` or `#123` issue references in the commit — put those in the PR description.
- **Trim verbiage:** in PR descriptions, commit messages, and code comments. No marketing prose, no restating the diff, no emojis.
- **Rebase, don't merge:** Never merge `main` into the feature branch; rebase onto the latest `main` and push with `--force-with-lease`.
- **Branch names:** `<type>/<desc>` where `<type>` ∈ `feat` `fix` `chore` `docs` `refactor` `ci`. Never push to `main`; PRs are squash-merged.
- **Pre-PR gate:** `make test-<module>` must pass for every module you touched. It runs tidy, fmt, vet, lint and tests in one shot. Run `make tidy` to tidy all affected modules.
- **Backward compatibility is mandatory.** These modules are consumed by all platform services. Breaking changes to exported APIs, function signatures, or behavior will be rejected. Design additive changes.
- **Tests:** New features, improvements and fixes must have test coverage. Follow existing patterns in the module you're modifying. Run tests locally before pushing.

## Code quality

Before submitting code, review your changes for the following:

- **No unchecked I/O.** Close HTTP response bodies, `sql.Rows`, and file handles in `defer` statements. Check and propagate errors from I/O operations.
- **Context everywhere.** Every exported function that performs I/O takes `ctx context.Context` as its first parameter. Never store a context in a struct field.
- **Error handling.** Wrap errors with `%w` for chain inspection. Do not swallow errors silently. Return errors that help callers diagnose the issue without leaking credentials, tokens, or connection strings.
- **No panics.** Never use `panic` in library code. Return errors and let callers decide. The explicit `Must*` variants (`flagx.MustEnum`, `temporalx.MustVersioningFromEnv`) exist for fail-fast startup and must stay the only exception.
- **No hardcoded defaults for security settings.** TLS verification stays enabled by default. `authmw` must fail closed — an unparseable or absent token is a rejection, never a pass-through; a JWKS that never loaded is a 503, not a bypass.
- **No secrets in telemetry.** Never put tokens, passwords, DSNs, or request bodies into span attributes, metric labels, or log fields. `dbx` deliberately never enables otelpgx query-parameter capture — bind parameters are PII/secrets.
- **Bounded cardinality.** Metric labels and span attributes must never contain user IDs, request IDs, full URLs with path params, or any unbounded value. Use route patterns (`/orders/{id}`), not concrete paths. `flagx` values are bounded by construction so they are safe as metric labels — keep that property.
- **Resource cleanup.** Every constructor that owns a goroutine, pool, or connection returns a `Close`/`Shutdown` function. Use `defer` and `t.TempDir()` in tests.
- **Thread safety.** These packages run in concurrent request handlers and Temporal workers. Do not introduce shared mutable state without synchronization.
- **Minimal surface.** Every exported type, function, and method is a backward-compatibility commitment consumed by multiple services. Minimize new exports. Prefer unexported struct fields with functional options over exported config structs.

## Project overview

`duynhlab/pkg` is the shared Go SDK for the platform's microservices (`auth`, `user`, `product`, `cart`, `order`, `review`, `shipping`, `notification`, `payment`, `checkout` — all built as `web → logic → core` layered services). It is a **multi-module monorepo**: the top-level `go.mod` is a deprecated, package-less placeholder (see below); all code lives in the 13 per-directory modules, each independently versioned and tagged (e.g. `httpx/v0.36.0`, `obsx/v0.36.0`, `logger/zapx/v0.36.0`). Services import specific modules from this repo.

The repository provides: generated gRPC/protobuf contracts, structured logging, OpenTelemetry bootstrap, HTTP and gRPC transport helpers, authentication middleware, database access, migrations, startup flags, idempotency handling, and Temporal client/worker helpers.

## Repository layout

The top-level `go.mod` contains **no Go packages** — it exists only so the frozen single-module line (`github.com/duynhlab/pkg`, last real version `v0.35.0`) can be vacated with a final placeholder tag. Never add code to it. Each of the 13 real modules has its own `go.mod`:

- `proto/` — one module holding the versioned gRPC contracts for all services (`cart`, `inventory`, `notification`, `order`, `payment`, `product`, `review`, `shipping`, each under `<svc>/v1/`). Source `.proto` files and the **committed** generated `.pb.go` stubs live together; `buf` drives codegen from the repo root.
- `logger/zapx/`, `logger/zerolog/`, `logger/clog/` — three independent logger modules, one per backend. `logger/zapx` (zap) is the production default — every service uses it and it pairs with `obsx.ZapCore` for OTLP log export. `logger/clog` (stdlib `log/slog` via chainguard-dev/clog) is the slog-based alternative; `logger/zerolog` wraps rs/zerolog. All inject the active trace ID into log lines.
- `flagx/` — startup-validated environment flags (`Enum`/`MustEnum`, `Percent`/`MustPercent`). Values are read and validated once at startup, fail fast, and are bounded by construction so they are safe as metric labels.
- `httpx/` — shared HTTP helpers on gin: consistent error responses (`RespondError`) and pagination (`ParsePage`, `NewPaginated`).
- `grpcx/` — gRPC server and client helpers for east-west calls: `NewServer` (otelgrpc stats handler, health service, reflection, panic recovery, access logging), `Dial` (otelgrpc, `round_robin` over `dns:///`, default per-RPC deadline), machine-readable error reasons (`reasons.go`), telemetry filters.
- `authmw/` — fail-closed gin JWT middleware. `NewVerifier(jwksURL, iss, aud)` + `MiddlewareJWT(verifier)` verify RS256 bearer tokens against a cached, background-refreshed JWKS; missing/invalid token → 401, JWKS never loaded or nil verifier → 503 (still denies). Sets `user_id`/`username`/`email` on the gin context.
- `idempotency/` — Stripe-style idempotency-key handling: `Record`, sentinel errors (`ErrConflict`, `ErrLocked`, `ErrNotFound`), and a Postgres-backed `Repository` (`Claim`/`Checkpoint`/`Release`/`Finish`/`Reap`) that takes a `*pgxpool.Pool` directly. The required DDL ships as a doc comment; this module owns no migrations.
- `dbx/` — Postgres pool construction pre-wired for OpenTelemetry: `NewPool(ctx, dsn, opts...)` applies transaction-mode-pooler-safe settings, an otelpgx tracer with safe defaults (bounded span names, no connection details, never query parameters), and pgxpool stat metrics. Uses the OTel **API** only; providers are injected via options and default to the globals.
- `migratex/` — embedded SQL schema migrations with golang-migrate: `Run(fsys, dir, dsn)`. Accepts a DSN, deliberately independent of `dbx`.
- `obsx/` — OpenTelemetry SDK bootstrap and the **only** module that links the OTel SDK: `SetupObservability(ctx, ConfigFromEnv())` builds traces + metrics + logs over OTLP and returns one `Shutdown`; `ZapCore` bridges zap into OTLP logs; `TraceContext`/`TraceIDFromContext` for log↔trace correlation; `SetupProfiling` (Pyroscope).
- `temporalx/` — Temporal client and worker construction with the OTel tracing interceptor wired in: `Dial(Config{HostPort, Namespace})`, `NewWorker(client, taskQueue, opts...)`, opt-in Worker Deployment Versioning from `TEMPORAL_WORKER_DEPLOYMENT_NAME`/`TEMPORAL_WORKER_BUILD_ID`.
- `.github/` — CI workflows. Not a module.

## Multi-module architecture

This is the most important thing to understand about this repo:

- **Every directory with a `go.mod` is an independent module.** There are 13 taggable modules.
- **Each module gets its own git tag** in the form `<module-path>/v<semver>` (e.g. `httpx/v0.36.0`, `logger/zapx/v0.36.0`). Module tags continue the pre-split numbering — the last single-module tag was `v0.35.0`, so per-module history starts at `v0.36.0`.
- **External consumers** import specific tagged versions: `go get github.com/duynhlab/pkg/httpx@v0.36.0`.
- **The root module is a tombstone.** `github.com/duynhlab/pkg` is a deprecated placeholder with no packages. Tagging it `v0.36.0` vacates the old package paths, so a consumer whose graph mixes the old `pkg` require with new per-module requires (an `ambiguous import` build error otherwise) can resolve it by bumping the root line. Never put Go files at the repo root.
- **There are currently no cross-module dependencies** inside this repo — every module builds standalone. Keep it that way when you can. If a genuine internal dependency ever appears, the dependent module carries a real published version in `require` **plus** a permanent sibling `replace` (e.g. `replace github.com/duynhlab/pkg/logger/zapx => ../logger/zapx`) for local development; `replace` in a non-main module is ignored by external consumers, so the `require` version must always be real.
- **Changing one module may require updating dependents in the services.** A change to `obsx.ZapCore`'s contract, for example, affects every service's `middleware/logging.go`, which pairs it with `logger/zapx`.
- **Do not use `go.work`.** With no cross-module imports there is nothing for a workspace to resolve, and a workspace file would mask module-boundary errors that CI will catch.

## Dependency rules

Modules are organised in strict layers. **A module may only import modules from a lower layer.** Same-layer imports are forbidden even when they would not create a cycle, because they create hidden tag-ordering constraints.

**Layer 0 — foundation.** Zero internal dependencies.

- `proto`, `logger/zapx`, `logger/zerolog`, `logger/clog`, `flagx`

**Layer 1 — building blocks.** May import Layer 0 only.

- `httpx`, `grpcx`, `authmw`, `idempotency`

**Layer 2 — terminal.** Heavy third-party SDKs. **No module in this repository may import a Layer 2 module.**

- `obsx`, `dbx`, `migratex`, `temporalx`

Today no module imports another at all; the layers say what is *allowed* if one ever must.

Concrete rules that follow from this:

- **OpenTelemetry API vs SDK.** `logger/*`, `grpcx`, and `dbx` may import `go.opentelemetry.io/otel` (the API), `otel/trace`, `otel/metric`, and contrib instrumentation (`otelgrpc`, `otelpgx`). They must **never** import `go.opentelemetry.io/otel/sdk` or `duynhlab/pkg/obsx`. The SDK lives in `obsx` and nowhere else. This is what lets a service use `grpcx` without being forced to link the OpenTelemetry SDK. (Tests are exempt: in-memory exporters need the SDK.)
- **`obsx`, `temporalx`, and `migratex` are wired in `main()` only.** Service business packages must not import them. `dbx` is exempt from this rule — repository/store code may use it — but no module *in this repo* may.
- **`migratex` does not import `dbx`.** It accepts a DSN string. Keeping them independent means a migration job binary does not link the pool implementation and vice versa.
- **`grpcx` does not import `proto`.** Interceptors are generic. Anything that needs a concrete message type belongs in the service.
- **`httpx` and `authmw` do not import each other.** Both produce gin middleware/helpers. Composition happens in the service.
- **`idempotency` does not import `dbx`.** Both bind to `*pgxpool.Pool` directly, which keeps them independent siblings — but they must stay on compatible pgx major versions.
- **Stdlib and shared-ecosystem types at the boundary.** Types appearing in an exported signature become a mandatory dependency for every consumer; types used only inside a function body do not. Prefer `context.Context`, `error`, `*zap.Logger`, `fs.FS`, `*pgxpool.Pool`, or a narrow interface declared locally. Never put a type from another `duynhlab/pkg` module into an exported signature.
- **Consumer-side interfaces.** When a lower-layer module needs a capability from a higher layer, it declares the interface itself and lets `main()` inject the implementation, or accepts the OTel API's provider interfaces (see `dbx.WithTracerProvider`/`WithMeterProvider`).
- **Nested modules are the escape hatch.** When an implementation genuinely needs a Layer 2 dependency, put it in a nested module rather than raising the parent's layer — its own `go.mod`, its own tag (the `logger/*` modules already follow this shape). A hypothetical Postgres-backed store for a Layer 1 module would live in `<module>/postgres/` and may import `dbx`; the parent stays Layer 1.

Enforcement lives in `.golangci.yml` via `depguard` (terminal-module imports and the OTel SDK outside `obsx` are denied). If you need to add an internal import that the linter rejects, the import is wrong — not the linter. Escalate to a human before editing the depguard rules.

## Build, test, lint

All targets in the root `Makefile`. Module paths use `:` as separator in make targets (`logger/zapx` → `logger:zapx`).

- `make modules` — list the modules the Makefile discovered. Run this after adding a module to confirm it was picked up.
- `make all` — runs `tidy`, `fmt`, `vet`, `lint` for all modules.
- `make test` — runs the full gate for ALL modules. `make test TAGS=integration` additionally runs the testcontainers-backed integration tests (needs a Docker daemon).
- `make test-<module>` — runs tidy, fmt, vet, lint, then `go test ./... -race -coverprofile coverage.out` for a single module. Examples: `make test-obsx`, `make test-logger:zapx`.
- `make tidy` / `make tidy-<module>` — `go mod tidy` for all or one module.
- `make lint` / `make lint-<module>` — `golangci-lint` (pinned, via `go run`) with the root `.golangci.yml`.
- `make coverage` — merge per-module coverage profiles into the root `coverage.out` for SonarCloud.
- `make generate-proto` — `buf generate` and `buf lint`.
- `make proto-breaking` — `buf breaking` against `main`. Required on every PR touching `proto`.
- `make release-<module> VER=x.y.z` — tags and pushes `<module>/vx.y.z`. Verifies the module exists and the tree is clean before tagging.

Targets fan out via `$(MAKE)` rather than a shell loop, so `make -j8 test` parallelises across modules.

To run a single test function, `cd` into the module directory and run `go test ./... -run TestName -v`.

## Codegen and generated files

Only the `proto` module has codegen. `buf.yaml` and `buf.gen.yaml` live at the repo root and point at `proto/`. After changing any `.proto` file:

```sh
make generate-proto
```

Generated files (never hand-edit):

- `proto/**/*.pb.go`
- `proto/**/*_grpc.pb.go`

`proto` has the highest fan-in in the repo. Protobuf changes must be **additive only**: add fields and messages, never renumber or reuse field numbers, never rename or remove existing fields, never change a field's type. `make proto-breaking` enforces this and must pass before merge. Keep each `option go_package` at `github.com/duynhlab/pkg/proto/<svc>/v1` — it is baked into the generated descriptors and is part of the module's import path.

## Conventions

- Standard `gofmt`. All exported names need doc comments. Match the style of the module you're editing.
- **Naming.** Module directories are domain nouns. Do not create `common`, `utils`, `shared`, `core`, `helpers`, or `internal` top-level modules — they have no admission criteria and become dependency magnets. If you cannot name a module after what it does, it should not be a module yet.
- **Functional options.** Constructors take `New(ctx, required..., opts ...Option)` (see `dbx.NewPool`, `temporalx.NewWorker`). Adding an option is additive; adding a field to an exported config struct is not always.
- **Logging.** Library modules accept a `*zap.Logger` (or nothing); `logger/zapx` produces one. Never construct a logger inside a library function and never log to a package-level default. Never log secrets, tokens, or bearer headers.
- **Configuration.** Read standard environment variables rather than inventing names. `obsx.ConfigFromEnv` uses `OTEL_SERVICE_NAME` (with `SERVICE_NAME` fallback), `OTEL_COLLECTOR_ENDPOINT`, `OTEL_SAMPLE_RATE`, `OTEL_METRICS_ENABLED`, `OTEL_LOGS_ENABLED`, `OTEL_RESOURCE_ATTRIBUTES`, `TRACING_ENABLED`, `PROFILING_ENABLED`, `PYROSCOPE_ENDPOINT`; `temporalx` uses `TEMPORAL_WORKER_DEPLOYMENT_NAME`/`TEMPORAL_WORKER_BUILD_ID`. Do not hardcode samplers or endpoints in Go.
- **Semantic conventions.** The OTel `semconv` version is pinned where imported (currently `obsx`). Bumping it renames attributes and breaks existing dashboards and alert rules — treat it as a coordinated change, not a routine dependency update.
- **Shutdown.** Anything that buffers telemetry or holds connections returns a shutdown function. `obsx.SetupObservability` returns one; failing to call it drops the final batch of spans when a pod receives SIGTERM.
- **Cross-module test helpers.** If a test in module A needs a helper from module B, duplicate the small helper. Do not add a production `require` on another module to satisfy a test.

## Testing

- Tests use standard `go test ./... -race`. The Makefile orchestrates per-module.
- `obsx` and `grpcx` tests use the OTel SDK's in-memory exporters (`tracetest.NewInMemoryExporter`, `sdkmetric` readers) to assert on emitted telemetry. Do not assert against a live collector.
- `grpcx` access-log tests assert log↔trace correlation: a request with an active span must produce a log entry carrying that span's trace ID. This is the test that catches broken context propagation, which is otherwise invisible until production.
- `dbx` and `idempotency` integration tests use `testcontainers-go` with Postgres behind the `integration` build tag — plain `make test` skips them; `make test TAGS=integration` (or CI) runs them and needs a Docker daemon.
- `temporalx` tests do not require a running Temporal server.
- `authmw` failures are silent in tests that use a permissive stub. When adding a claims field, add a negative test asserting rejection, not only a positive test asserting acceptance.
- Match the module's existing test framework (stdlib `testing`, table-driven). Do not introduce a new assertion library.

## Gotchas and non-obvious rules

- **The root module contains no packages.** From the repo root, `go build ./...` warns "matched no packages" but exits 0 — it appears to pass while checking nothing — and `go test ./...` fails with "no packages to test". Neither touches the 13 real modules. Always work within a module directory or use `make test-<module>`.
- **Module versioning is independent.** Changing `logger/zapx` does not bump `httpx`. Tag each changed module separately at release time.
- **Tag order matters once modules depend on each other.** Tag dependencies before dependents (Layer 0 → 1 → 2), otherwise a `require` line points at a tag that does not exist yet and external `go get` fails even though local builds pass.
- **A pushed tag cannot be fixed.** The Go module proxy caches immediately. A wrong `obsx/v0.36.0` cannot be corrected — you must burn the version and publish `v0.36.1`. `make release-<module>` checks the module exists and the tree is clean, but it cannot check that the content is right.
- **Adding a new exported symbol is a cross-repo contract change.** All platform services depend on these modules. Renaming, removing, or changing the signature of any exported type or function breaks downstream consumers even if this repo's tests pass.
- **Adding a new module** requires: `go mod init github.com/duynhlab/pkg/<name>`, adding it to the Repository layout and Dependency rules sections above, and adding it to `README.md`. The Makefile discovers it automatically — confirm with `make modules`.
- **The Makefile computes `MODULES` dynamically** by scanning for `go.mod` files, bounded by `-maxdepth 4`. A module nested deeper than that bound disappears from every target with no error. Run `make modules` after adding one and confirm the count.
- **Colon encoding in make targets.** `logger/zapx` is targeted as `make test-logger:zapx`. The Makefile translates `:` back to `/` internally.
- **`flagx` is startup-time by design.** It reads and validates env vars once, at process start, and fails fast on invalid values. Do not use it for per-request or runtime-mutable flags — that is a different tool.
- **`logger/zapx` and `obsx` version together in practice.** Every service's logging middleware pairs `zapx.New` with `obsx.ZapCore`; when either side's contract moves, tag both and update services in one PR.
- **Naming is inconsistent by history.** Some modules carry an `x` suffix (`httpx`, `grpcx`, `dbx`, `obsx`, `flagx`, `migratex`, `temporalx`, `logger/zapx`) and some do not (`proto`, `authmw`, `idempotency`). Do not rename existing modules — the import path is a published contract. New modules follow the `x` suffix convention.
