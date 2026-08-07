# Migrating services to the per-module pkg (v0.36.0)

Runbook for moving each service off the frozen single-module
`github.com/duynhlab/pkg v0.35.0` onto the per-module tags. One service =
one PR. **A migration PR changes `go.mod`, `go.sum`, and
`.github/dependabot.yml` — never a `.go` file.**

**Nothing is urgent.** The old tags (`v0.35.0` and earlier) resolve forever;
a service keeps building and shipping until it migrates.

## TL;DR — what changes and what doesn't

- **Go import paths do not change.** `import "github.com/duynhlab/pkg/obsx"`
  stays exactly as-is in every `.go` file.
- **Only `go.mod` changes**: one `require github.com/duynhlab/pkg v0.35.0`
  becomes N per-module requires at `v0.36.0`.
- **There is no plain `v0.36.0` tag.** The single-module line ends at
  `v0.35.0`. The old require must be **deleted**, never version-edited —
  editing it in place points at a tag that does not exist.

### Why telemetry does not change

The entire code delta between `v0.35.0` and the `v0.36.0` module tags is:

1. `grpcx/logging.go` computes the access-log `trace_id` with a private
   helper instead of calling `obsx.TraceIDFromContext`. The logic is
   byte-identical (`trace.SpanFromContext(ctx).SpanContext()`, empty string
   when no span); a test pins the output. No service calls this path
   directly.
2. Every module's `go` directive moved from `1.25.8` to `1.26.0`. All
   services already build with Go 1.26.x.

Everything a service actually touches is **unchanged**:

| Surface (unchanged v0.35.0 → v0.36.0) | Used by |
|---|---|
| `obsx.ConfigFromEnv`, `obsx.SetupObservability`, `obsx.SetupProfiling` | all 10 (`cmd/main.go`) |
| `obs.ZapCore(service, level)` method — the OTLP log tee (`zapcore.NewTee`) | all 10 |
| `obsx.TraceIDFromContext` | all 10 (`middleware/logging.go`) |
| `obsx.TraceContext` | 9 (auth never used it — pre-existing, not a regression) |
| `obsx.DurationBuckets` | auth, checkout, notification, payment (custom metrics) |
| `zapx.New`, `dbx.NewPool` (+`WithMaxConns`, notification also `WithPasswordFile`), `migratex.Run`, `grpcx.NewServer/Dial`, grpcx error reasons, `authmw.*`, `idempotency.*`, `temporalx.*`, `flagx.MustEnum`, all `proto/*/v1` types | per the map below |

If the service compiles, the API contract held; the runtime behavior of all
of the above is the same code as `v0.35.0`.

## Before you start (once)

- [ ] Confirm the module tags resolve (spot-check a few):
  ```sh
  GOPROXY=https://proxy.golang.org go list -m github.com/duynhlab/pkg/obsx@v0.36.0
  GOPROXY=https://proxy.golang.org go list -m github.com/duynhlab/pkg/logger/zapx@v0.36.0
  GOPROXY=https://proxy.golang.org go list -m github.com/duynhlab/pkg/proto@v0.36.0
  ```
- [ ] Docker daemon running (the integration tests use testcontainers).
- [ ] Read the Dependabot section below — every service's Dependabot runs
  **daily** with no ignore/group rules, so land each migration PR promptly
  and include the dependabot.yml edit in the same PR.

## Per-service module map

Verified against each repo's `main` on 2026-08-07. The **source of truth is
the code** — re-derive before editing, the table may age:

```sh
grep -rho 'github.com/duynhlab/pkg/[a-z/]*' --include='*.go' . | sort -u
# proto/<svc>/v1 → module "proto"; logger/zapx → module "logger/zapx";
# everything else maps 1:1 to a module.
```

All ten need the floor: `dbx`, `httpx`, `logger/zapx`, `migratex`, `obsx`.

| Service | + on top of the floor | Total | go directive | old require at | dead `// replace` trailer |
|---|---|---|---|---|---|
| auth | — | **5** | 1.26.2 | go.mod:6 | yes |
| user | authmw | **6** | 1.26.2 | go.mod:6 | yes |
| product | grpcx, proto | **7** | 1.26.2 | **go.mod:7** | yes |
| shipping | grpcx, proto | **7** | 1.26.2 | go.mod:6 | yes |
| cart | authmw, grpcx, proto | **8** | 1.26.2 | go.mod:6 | yes |
| notification | authmw, grpcx, proto | **8** | 1.26.2 | go.mod:6 | yes |
| review | authmw, grpcx, proto | **8** | 1.26.2 | go.mod:6 | yes |
| payment | authmw, grpcx, proto, idempotency | **9** | 1.26.2 | go.mod:6 | none |
| order | authmw, grpcx, proto, temporalx, flagx | **10** | 1.26.2 | go.mod:6 | yes |
| checkout | authmw, grpcx, proto, idempotency, temporalx | **10** | 1.26.1 | go.mod:6 | none |

Never add `logger/zerolog` or `logger/clog` — zero services import them
(`go mod tidy` would strip them, but don't create the churn).

## Migration steps (per service, one PR)

- [ ] **1. Branch** from the service's latest `main`: `chore/pkg-v0.36.0`.

- [ ] **2. Edit `go.mod`** — *delete* the old line, *add* the module block.
  Do not match by line number (product's require sits one line lower than
  the rest):

  ```diff
  -	github.com/duynhlab/pkg v0.35.0
  +	github.com/duynhlab/pkg/authmw v0.36.0
  +	github.com/duynhlab/pkg/dbx v0.36.0
  +	github.com/duynhlab/pkg/grpcx v0.36.0
  +	github.com/duynhlab/pkg/httpx v0.36.0
  +	github.com/duynhlab/pkg/logger/zapx v0.36.0
  +	github.com/duynhlab/pkg/migratex v0.36.0
  +	github.com/duynhlab/pkg/obsx v0.36.0
  +	github.com/duynhlab/pkg/proto v0.36.0
  ```
  (example shows cart/notification/review's 8; use the map above.)

- [ ] **3. Fix the local-dev replace trailer.** Eight services carry this
  dead comment at the end of go.mod (pkg's root has no `go.mod` anymore, so
  it can never be uncommented successfully):

  ```
  // For local development with pkg
  // replace github.com/duynhlab/pkg => ../pkg
  ```

  Replace it with per-module lines matching the service's modules, e.g.:

  ```
  // For local development with pkg (uncomment only what you develop against)
  // replace github.com/duynhlab/pkg/obsx => ../pkg/obsx
  // replace github.com/duynhlab/pkg/logger/zapx => ../pkg/logger/zapx
  ```

  checkout and payment have no trailer — add one or skip, either is fine.

- [ ] **4. Edit `.github/dependabot.yml`** (same PR): group the pkg modules
  so a future bump is one PR, not up to 13 against the 10-PR limit:

  ```yaml
  # under the gomod update entry:
      groups:
        duynhlab-pkg:
          patterns:
            - "github.com/duynhlab/pkg/*"
  ```

- [ ] **5. Tidy and verify locally:**

  ```sh
  go mod tidy
  grep 'duynhlab/pkg v0' go.mod go.sum && echo "OLD LINE STILL PRESENT" || echo OK
  go build ./...
  go test -race ./...
  go test -tags=integration -race ./internal/core/repository/...   # needs Docker
  ```

  Expected: `go.sum` shrinks (the pkg root hashes and unused transitives
  drop out) and gains one hash pair per pkg module. `git diff --stat` must
  show **only** `go.mod`, `go.sum`, `.github/dependabot.yml`.

- [ ] **6. PR and CI.** All go-check jobs (Test, Integration Test, Lint,
  sonar) must be green. The Lint job's `go mod download` is where a stale
  `go.sum` surfaces first.

- [ ] **7. Merge, deploy to staging, run the telemetry checklist below,
  soak, then promote.**

## Telemetry verification (post-deploy, staging first)

Compile success already proves the API surface; this checklist proves the
runtime signals. Compare each item against a pre-migration baseline
(same query, before the deploy).

- [ ] **stdout logs carry `trace_id`.** Hit any authenticated endpoint, then:
  ```sh
  kubectl logs deploy/<svc> --since=2m | grep '"trace_id":"' | head
  ```
  Non-empty 32-hex values for traced requests (empty only for requests
  outside a span — same as before).
- [ ] **OTLP logs still arrive with native trace/span IDs.** In the logs
  backend, query the service for the last 5 minutes and confirm records
  carry `trace_id`/`span_id` attributes. *(Known pre-existing exception:
  auth-service never bound `obsx.TraceContext`, so its OTLP records have
  only the string field — do not treat that as a migration regression.)*
- [ ] **Traces flow.** Search traces by `service.name=<svc>` with a
  time range after the deploy; confirm new spans, and for gin/gRPC servers
  confirm server spans still parent DB client spans (`db.client.*`).
- [ ] **East-west trace continuity** (services with grpcx clients —
  checkout, order, product): one user request produces a single trace
  spanning caller and callee, and the callee's gRPC access-log line carries
  the same `trace_id`.
- [ ] **Metrics continue.**
  - `db.client.operation.duration` (all services): new points after deploy,
    same bucket boundaries as before.
  - Custom histograms built with `obsx.DurationBuckets` (auth, checkout,
    notification, payment): series continue, bucket layout unchanged.
  - Go runtime metrics still reported.
- [ ] **Profiling continues** (`PROFILING_ENABLED` services): new profiles
  for the service appear after the deploy.
- [ ] **Shutdown flush intact:** roll one pod and confirm no burst of
  dropped-span/exporter errors during termination (`tp.Shutdown` is wired
  unchanged in every `main.go`).

Any failure here → rollback (below) and investigate; do not migrate the
next service on a red checklist.

## Rollback

Revert the migration commit (`go.mod` + `go.sum` + dependabot.yml) and
redeploy — `v0.35.0` is immutable and resolves forever. Never partially
roll back (mixing the old root require with per-module requires fails to
build with `ambiguous import`).

## Suggested order

Canary with the smallest surface, finish with the saga services:

1. **auth** (5 modules — floor only) → soak one day
2. **user** (6)
3. **product**, **shipping** (7)
4. **cart**, **notification**, **review** (8)
5. **payment** (9 — note: authmw + grpcx + proto + idempotency, more than
   older docs claimed)
6. **order**, **checkout** (10 — Temporal workers; verify the worker gets
   the OTLP-teed logger and workflow spans keep flowing)

## Gotchas

- **No `github.com/duynhlab/pkg@v0.36.0` exists.** Delete the old require;
  never bump it in place. A graph that mixes the old require with any new
  per-module require fails immediately with `ambiguous import` — and no
  future root version will exist to resolve it.
- **Dependabot runs daily in every service repo** with no pkg ignore rule:
  it cannot bump the retired root line (nothing newer than v0.35.0 exists
  there), but until the migration lands it may open noise PRs. Land the
  migration PR the same day you open it, and ship the `groups:` edit with it.
- **product-service's require is on line 7** (miniredis sorts first). Never
  script the edit by line number.
- **Version skew is benign:** payment pins `google.golang.org/grpc v1.82.0`
  while the pkg modules require `v1.81.1`; MVS keeps payment on 1.82.0.
  Expect no downgrade anywhere.
- **`logger/zapx` and `obsx` version together.** Every service's
  `middleware/logging.go` + `main.go` pair `zapx.New` with `obs.ZapCore`;
  when bumping one later, bump the other to the tag cut from the same main.
- **`dbx` and `idempotency` share pgx** (checkout, payment): keep them on
  tags with the same pgx major version.
- **The `Test (stable)` CI job runs on Go `stable`,** ignoring go.mod — if
  it fails, that is toolchain drift, not this migration.
- **Go directives need no change:** modules declare `go 1.26.0`; services
  are on 1.26.1+ already.
