# Migrating a service from `logger/zapx` to `logger/slogx`

One facade for application logging (`logger/slogx`), replacing `logger/zapx`
plus `obsx.ZapCore` plus `obsx.TraceContext`. The envelope on stdout is
unchanged, so dashboards, stored queries and alerts that read `timestamp`,
`level`, `message`, `caller`, `trace_id` or `span_id` keep working. One field
does change shape — the error, see the table — and the rest is the call site.

## TL;DR

| | Before | After |
|---|---|---|
| Construction | `zapx.New(os.Getenv("LOG_LEVEL"))` + `obsx.ZapCore` for the OTLP side | `slogx.New(slogx.Config{Level: os.Getenv("LOG_LEVEL"), Flush: obs.ForceFlush})` — both sinks |
| Call | `log.Info("msg", zap.String("k", v))` | `log.Info(ctx, "msg", slog.String("k", v))` |
| Errors | `zap.Error(err)` → `"error": "<text>"` | `slogx.Err(err)` → `"error.type"` + `"error.message"` |
| Trace correlation | `obsx.TraceContext(ctx)` passed as a field | automatic — the context is the first argument |
| Redaction | per-adapter, partial | mandatory, before both sinks |
| Named records | none | `log.Event(ctx, level, "order.confirmed", "…")` |

The envelope keys (`timestamp`, `level`, `message`, `caller`, `trace_id`,
`span_id`) are byte-for-byte what `zapx` emitted. A service is migrated when
`grep -r 'go.uber.org/zap' internal/ cmd/` is empty.

**The one breaking query change:** `zap.Error(err)` wrote a single string
field named `error`. `slogx.Err(err)` writes two flat fields, `error.type`
(the Go type, a stable low-cardinality label) and `error.message` (the text,
redacted and bounded). Anything matching `error` as a string — a VictoriaLogs
filter, a Grafana panel, an alert expression — must move to `error.message`
for the text or, better, to `error.type` for the classification. Grep the
dashboards and rules in `homelab` before the first service cuts over.

## Before you start

```bash
go get github.com/duynhlab/pkg/logger/slogx@v0.1.0
```

Move `obsx` to **v0.45.0** in the same change: it removes `ZapCore` and
`TraceContext` (so the old bootstrap stops compiling, which is the point — a
service cannot end up half-migrated) and adds `ForceFlush`. `slogx` reads the
logger provider from the OTel global obsx installs, so `main()` never hands it
one.

The same change bumps the transport and worker adapters to their slog
releases — `httpmw` v0.2.0, `grpcx` v0.37.0, `temporalx` v0.40.0 (see
[Transport and worker adapters](#transport-and-worker-adapters)); the older
tags need the zap bridge v0.45.0 removed. A service that has not migrated stays
on obsx v0.44.x, which remains the patch line until the fleet has cut over.

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
log := slogx.New(slogx.Config{
    Level: os.Getenv("LOG_LEVEL"),
    Flush: obs.ForceFlush, // a FATAL record leaves the process before it exits
})
slogx.SetDefault(log)
```

Ordering: call `Fatal` **before** `obs.Shutdown()`. Shutdown stops the
provider, and a record emitted after it is dropped rather than exported.

`Config.Flush` is what lets a FATAL record leave the process: the OTLP side is
batched and nothing runs after `Fatal`, so without it a FATAL reaches stdout
only (Vector still ships it to VictoriaLogs, but it is absent from the 90-day
store). `obs.ForceFlush` exports every provider's buffer without stopping it,
which is why it must run before `obs.Shutdown`, never after.

## Transport and worker adapters

`httpmw`, `grpcx` and `temporalx` take a `*slog.Logger`; hand them the
facade's `Slog()`. Bump them in the same change as the facade — the old tags
take a `*zap.Logger` and will not compile against it:

| Module | Minimum | Wiring |
|---|---|---|
| `httpmw` | v0.2.0 | `r := gin.New()`; `r.Use(httpmw.Tracing(name), httpmw.Logging(log.Slog()), httpmw.Recovery(log.Slog()))` |
| `grpcx` | v0.37.0 | `grpcx.NewServer(log.Slog(), opts...)` |
| `temporalx` | v0.40.0 | `temporalx.Dial(cfg, temporalx.WithLogger(log.Slog()))` |

What changes for readers of the access records:

- **HTTP** records carry `http.request.method` (non-standard methods become
  `_OTHER`), `http.route` (omitted when nothing matched), and
  `http.response.status_code`, plus `error.type` for a 5xx. `method`, `path`,
  `status`, `duration`, `client_ip` and `user_agent` are gone.
- **gRPC** records carry `rpc.system.name`, `rpc.method` (no leading slash)
  and `rpc.response.status_code` in the spec spelling (`NOT_FOUND`), plus
  `error.type` for the codes the span marks as Error. `method`, `code`,
  `duration`, `peer` and the bound `trace_id` field are gone.
- **Correlation** comes from the context on both: the envelope's `trace_id`
  and `span_id` are the span's. `httpmw.LoggerFrom(c)` is bound to the
  request, so a handler's `LoggerFrom(c).Info("…")` keeps the ids too.
- **Panics** are one structured record (`error.type=panic`, bounded
  `exception.message` naming the panic's type, bounded
  `exception.stacktrace`), never gin's or grpc-go's text output; the HTTP
  access summary for that request is Error with `error.type=panic`, even
  when the handler had already written a 2xx before panicking. Build the
  router with `gin.New()`: `gin.Default()` installs gin's own logger and
  recovery, which print the raw path and client address past the facade.
- **Temporal**: the tracing interceptor's `TraceID`/`SpanID` attributes
  become the record's span context, so workflow and activity lines carry the
  canonical `trace_id`/`span_id` instead of a second pair of fields.

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
goimports -w internal cmd   # gofmt -r rewrites calls, not imports
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
accepts `trace|debug|info|warn|warning|error`. `Fatal` writes the record and ends the
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

`error.type` is only as useful as the errors behind it: every `errors.New`
sentinel reports `errors.errorString`. A domain whose failures should be
distinguishable on a dashboard declares error types (`type NotFound struct{…}`)
rather than sentinel values.

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
