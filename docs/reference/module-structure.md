# Module structure

go-agents is a **multi-module repository**: a dependency-light core module plus one module
per heavy adapter. This keeps a consumer's dependency/audit surface proportional to what they
actually import.

## Why

The core agent loop imports only `golang.org/x/sync` (plus gsm). The adapters, however, pull
large dependency trees: modernc pure-Go SQLite alone is 34 modules. As a single module, a
consumer who imported *only* the core still inherited the whole set in their `go.sum` /
`go mod graph` / SCA audit surface (Go's module-graph pruning keeps them from *compiling*
unused adapters, but they still appear to dependency scanners).

Measured before the split: a core-only consumer inherited **54 external modules**; they now
inherit **2** (gsm + x/sync). Each heavy tree lives behind its own module and is pulled in
only when that adapter is imported.

## The modules

| Module | Path | Extra deps beyond core |
|---|---|---|
| **core** | `github.com/dayna/go-agents` | gsm, `x/sync` |
| mcp | `…/mcp` | modelcontextprotocol/go-sdk (+ jsonschema, segmentio, …) |
| trace | `…/trace` | go.opentelemetry.io/otel |
| sqlite store | `…/store/sqlite` | modernc.org/sqlite |
| postgres store | `…/store/postgres` | jackc/pgx |
| redis log | `…/govern/redislog` | redis/go-redis |
| sqlite log | `…/govern/sqlitelog` | modernc.org/sqlite |

The core module keeps everything with no heavy deps: `agent` (loop), `schema`, `middleware`,
`model/anthropic`, `model/openai`, `govern`, and `examples`. The `architecture_test.go` guard
(core must not import an adapter) still holds, now enforced at the module boundary too.

## Working in the repo

A root `go.work` ties the modules together for local development, so `go build`/tests resolve
cross-module references without published versions:

```
go work sync
cd trace && go test ./...     # or any module
```

Each adapter module's `go.mod` also carries a `replace github.com/dayna/go-agents => <rel>`
so it builds standalone in CI. CI builds and tests every module in its own directory (see
`.github/workflows/ci.yml`, `MODULES`).

## Interim state (pre-1.0)

The module path is still the working-codename placeholder and the core is unpublished, so the
adapter modules resolve the core via `replace` directives rather than a pinned version. At the
**v1.0 / real-name** milestone: rename all module paths, tag the core, and replace the
`replace` directives with version pins: one coordinated event. Until then there are no
external consumers, so the interim resolution is invisible.
