# Platform semantic-convention registry

The names the platform owns — attributes, metrics and events beyond the upstream
OpenTelemetry semantic conventions — declared once, checked in CI, generated into
the shared package and checked against live telemetry (ADR-076).

| | |
|---|---|
| **Registry** | `registry/` — `manifest.yaml` (depends on semantic conventions **v1.41.0**, the version `obsx` pins) + `model/*.yaml` |
| **Policies** | `policies/registry.rego` — registered namespaces, no upstream redefinition, a UCUM unit on every metric, stability on every definition |
| **Templates** | `templates/registry/go/` — the constants `logger/slogx` and `temporalx` import (`*/semconv_gen.go`); `templates/registry/markdown/` — the event catalog table (`docs/event-catalog.md`) homelab's `docs/api/logs.md` embeds. Generated, never edited |
| **Tooling** | `otel/weaver:v0.26.1` through `make semconv-*` (docker, no install) |
| **Live check** | homelab `local-stack/compose.weaver.yaml` copies the gate's OTLP to `weaver registry live-check` against this registry |

## Layout

```
semconv/
  registry/
    manifest.yaml          name, schema_url, dependency on upstream v1.41.0
    model/
      upstream.yaml        imports of upstream metrics + refs to the upstream attributes the fleet writes
      attributes.yaml      platform attributes, one group per namespace
      attributes-bare.yaml single-segment keys (bounded labels, diagnostic fields, Temporal SDK keys)
      events.yaml          the frozen event catalog (19 names)
      metrics.yaml         the business instruments, unit = what the code passes to WithUnit
      metrics-vendor.yaml  instruments libraries emit under their own names (Temporal SDK, pgxpool)
  policies/registry.rego
  templates/registry/go/          slogx.go.j2, temporalx.go.j2 → */semconv_gen.go
  templates/registry/markdown/    event-catalog.md.j2 → docs/event-catalog.md
  docs/event-catalog.md           generated
```

The registry root is `registry/`, not this directory: Weaver reads every YAML under
the path it is given, so the templates' `weaver.yaml` has to live outside it.

## Rules the policies enforce

- **Registered namespaces.** A dotted attribute defined here must start with a
  namespace on the list in `registry.rego`, each named with an owner. Adding one is
  a change to that list, never a call-site decision. Single-segment keys are not
  namespaced (owner decision 2026-09-24); they only have to be declared.
- **Upstream by reference.** `http.*`, `rpc.*`, `db.*`, `error.*`, `exception.*`,
  `service.*`, `k8s.*`, `code.*`, `url.*`… are referenced from the dependency, never
  redefined here.
- **A UCUM unit on every metric.** Counters use annotation units (`{order}`,
  `{attempt}`), which the Prometheus name translation drops, so series names do not
  change. Instruments a library emits (`annotations: origin: vendor`) keep the unit
  the library chose.
- **Stability on every definition.** Everything is `development` today.

## Commands

```bash
make semconv-check            # resolve + policies (CI)
make semconv-generate         # regenerate logger/slogx/semconv_gen.go and temporalx/semconv_gen.go
make semconv-generated-check  # fail when a generated file is stale or hand-edited (CI)
make semconv-diff BASE=main   # renames/removals vs the registry at a git ref (ADR-072: a breaking release)
make semconv-lockstep         # the registry's upstream version equals the semconv version obsx imports (CI)
```

Deprecated upstream keys and unit or instrument mismatches are **violations** in
live-check even when the registry references them, which is why `error.message`
became `exception.message` (slogx v0.3.0) and every instrument carries a unit.

## Adding a name

1. Declare it in `registry/model/` (a new namespace also goes on the list in
   `registry.rego`, with its owner).
2. `make semconv-check semconv-generate`; commit the model and the regenerated
   constants together.
3. Emit it from code only after the registry change merged. The local-stack gate's
   live-check fails a name the registry does not know.
