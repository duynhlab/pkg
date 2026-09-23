# Migrating a service from `logger/zapx` to `logger/slogx`

One facade for application logging (`logger/slogx`), replacing `logger/zapx`
plus `obsx.ZapCore` plus `obsx.TraceContext`. The record shape on stdout does
not change, so dashboards, stored queries and alerts keep working; what
changes is the call site.

## TL;DR

| | Before | After |
|---|---|---|
| Construction | `zapx.New(os.Getenv("LOG_LEVEL"))` + `obsx.ZapCore` for the OTLP side | `slogx.New(slogx.Config{Level: os.Getenv("LOG_LEVEL")})` — both sinks |
| Call | `log.Info("msg", zap.String("k", v))` | `log.Info(ctx, "msg", slog.String("k", v))` |
| Errors | `zap.Error(err)` | `slogx.Err(err)` |
| Trace correlation | `obsx.TraceContext(ctx)` passed as a field | automatic — the context is the first argument |
| Redaction | per-adapter, partial | mandatory, before both sinks |
| Named records | none | `log.Event(ctx, level, "order.confirmed", "…")` |

The envelope keys (`timestamp`, `level`, `message`, `caller`, `trace_id`,
`span_id`) are byte-for-byte what `zapx` emitted. A service is migrated when
`grep -r 'go.uber.org/zap' internal/ cmd/` is empty.

## Before you start

```bash
go get github.com/duynhlab/pkg/logger/slogx@v0.1.0
```

`obsx` must be at the floor that installs the logger provider in the OTel
global (v0.40.0 or later). Nothing else moves: `slogx` reads the provider from
the global, so `main()` never hands it one.

## The bootstrap

```go
// before
logger, err := zapx.New(os.Getenv("LOG_LEVEL"))
if err != nil { … }
defer logger.Sync()
core := obs.ZapCore("service", zapcore.InfoLevel)
logger = logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
    return zapcore.NewTee(c, core)
}))

// after
log := slogx.New(slogx.Config{Level: os.Getenv("LOG_LEVEL")})
slogx.SetDefault(log)
```

Ordering: call `Fatal` **before** `obs.Shutdown()`. Shutdown stops the
provider, and a record emitted after it is dropped rather than exported.

`Config.Flush` is the seam that lets a FATAL record leave the process: the
OTLP side is batched and nothing runs after `Fatal`, so without it a FATAL
reaches stdout only (Vector still ships it to VictoriaLogs; it will be absent
from the 90-day store). `obsx` does not expose a log force-flush yet — wire
the field when it does. Nothing else about Fatal changes.

## Call sites

`gofmt -r` rewrites the mechanical half; the context argument is the part a
human adds.

```bash
# field constructors
gofmt -r 'zap.String(a, b) -> slog.String(a, b)' -w internal cmd
gofmt -r 'zap.Int(a, b) -> slog.Int(a, b)' -w internal cmd
gofmt -r 'zap.Int64(a, b) -> slog.Int64(a, b)' -w internal cmd
gofmt -r 'zap.Bool(a, b) -> slog.Bool(a, b)' -w internal cmd
gofmt -r 'zap.Duration(a, b) -> slog.Duration(a, b)' -w internal cmd
gofmt -r 'zap.Any(a, b) -> slog.Any(a, b)' -w internal cmd
gofmt -r 'zap.Error(a) -> slogx.Err(a)' -w internal cmd
```

Then the signatures and the context:

| zap | slogx |
|---|---|
| `*zap.Logger` field or parameter | `*slogx.Logger` |
| `log.Debug/Info/Warn/Error(msg, fields…)` | `log.Debug/Info/Warn/Error(ctx, msg, attrs…)` |
| `log.With(fields…)` | `log.With(attrs…)` (unchanged shape) |
| `zapx.WithContext` / `zapx.FromContext` | `slogx.WithContext` / `slogx.FromContext` |
| `obsx.TraceContext(ctx)` as a field | delete it — the ids come from `ctx` |
| `zap.Float64`, `zap.Time`, `zap.Uint64` | `slog.Float64`, `slog.Time`, `slog.Uint64` |
| `zap.Strings`, `zap.Ints` (slices) | `slog.Any` |
| `zap.Namespace("x")` | `slog.Group("x", …)` |
| `logger.Sync()` | nothing — stdout is unbuffered |
| an SDK that wants a `*slog.Logger` | `log.Slog()` |

A method that has no context in scope takes one: thread it from the caller
rather than reaching for `context.Background()`, or the record loses its trace
ids and the whole point of the change with it.

## Levels

`trace` (-8) and `fatal` (+12) join the four standard levels, and `LOG_LEVEL`
accepts `trace|debug|info|warn|error`. `Fatal` writes the record and ends the
process; nothing else may call `os.Exit`. On the OTLP side the severity
NUMBER is the RFC table (trace 1 … fatal 21) and is the field to query — the
bridge writes severity TEXT with the standard library's spelling, which has no
name for those two levels.

## What the redactor does to your records

Redaction is mandatory and runs before both sinks. Expect these, and fix the
call site rather than reaching for an exception:

- Keys on the platform deny list are replaced wherever they appear, including
  inside a struct, a map or a `LogValuer`: `authorization`, `cookie`,
  `password*`, `token*`, `secret*`, `api_key*`, `client_ip`, `user_agent`,
  `headers`, `body`, `payload`, connection strings, card fields.
- Values are scanned too: a Bearer/Basic header, a JWT, a `key=value` pair
  whose key is denied, URL userinfo and a Luhn-valid card number are replaced
  inside the text, so a raw error string or a body echo is safe but noisy.
- Bounds: 64 attributes per record (`_slogx.dropped` counts the rest), depth
  4, 4 KiB per value, 1 KiB per message.
- The six envelope keys are reserved at the top level. An attribute named
  `trace_id`, `level` or `message` is dropped: rename it
  (`upstream_trace_id`, `event_timestamp`).

A service may only tighten the policy — `Config.Redact.ExtraDenyKeys` adds
keys, the bounds shrink. There is no way to turn it off.

## Named records

`Event` is for the reviewed catalog, not for every line:

```go
log.Event(ctx, slog.LevelInfo, "order.confirmed", "order confirmed",
    slog.String("order_id", id), slog.String("outcome", "ok"))
```

The name is `[a-z][a-z0-9_]*` segments joined by dots, at least two, at most
64 bytes. An invalid name still emits the record, with `event.invalid` instead
of `event`, so the mistake shows up in the stream instead of vanishing.

## Verification

Per service, before the PR:

```bash
grep -rn 'go.uber.org/zap' internal/ cmd/   # empty
go build ./... && go test -race ./...
golangci-lint run
```

Then on the running stack: a request produces one JSON line per record with
the same envelope keys as before, `trace_id` matching the span, and the same
record in ClickHouse under `ScopeName = github.com/duynhlab/pkg/logger/slogx`.

## Gotchas

- **A helper that wraps `Info`** becomes the `caller` on every line it writes,
  exactly as it did with zap and no `AddCallerSkip`. There is no skip option;
  inline the helper or accept the location.
- **`Slog()` is for SDK bridges** (the Temporal structured logger, a router's
  recovery hook), not for business code: its calls take no context, so those
  records have no trace ids.
- **Group state travels with the logger**, so `log.Slog().WithGroup("req")`
  nests everything you add afterwards — the trace ids stay in the envelope.
- **An OTLP export failure is silent** by design: a log line must not fail the
  operation that wrote it. Watch the collector's own metrics for that.
