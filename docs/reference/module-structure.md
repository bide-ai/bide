# Module structure

Bide is a **multi-module repository**: a dependency-light core module plus one module
per heavy adapter. This keeps a consumer's dependency/audit surface proportional to what they
actually import.

## Why

The core module requires only `golang.org/x/sync` and `golang.org/x/text`. The adapters, however, pull
large dependency trees: modernc pure-Go SQLite alone is 34 modules. As a single module, a
consumer who imported *only* the core still inherited the whole set in their `go.sum` /
`go mod graph` / SCA audit surface (Go's module-graph pruning keeps them from *compiling*
unused adapters, but they still appear to dependency scanners).

Measured before the split: a core-only consumer inherited **54 external modules**; they now
inherit **2** (`x/sync` and `x/text`). gsm is not among them: it belongs to the `govern` module.
Each heavy tree lives behind its own module and is pulled in
only when that adapter is imported.

## The modules

| Module | Path | Extra deps beyond core |
|---|---|---|
| **core** | `github.com/bide-ai/bide` | `x/sync`, `x/text` |
| govern | `…/govern` | blackwell-systems/gsm |
| mcp | `…/mcp` | modelcontextprotocol/go-sdk (+ jsonschema, segmentio, …) |
| trace | `…/trace` | go.opentelemetry.io/otel |
| sqlite store | `…/store/sqlite` | modernc.org/sqlite |
| postgres store | `…/store/postgres` | jackc/pgx |
| redis log | `…/govern/redislog` | redis/go-redis |
| sqlite log | `…/govern/sqlitelog` | modernc.org/sqlite |
| postgres log | `…/govern/postgreslog` | jackc/pgx |
| gcf codec | `…/codec/gcf` | blackwell-systems/gcf-go |

The core module keeps everything with no heavy deps: `agent` (loop), `schema`, `middleware`,
`model/anthropic`, `model/openai`, `model/gemini`, `plan`, `audit`, `eval`, `chaos`, the
`cmd` tools, and most of `examples`. The `architecture_test.go` guard
(core must not import an adapter) still holds, now enforced at the module boundary too, and
`TestCoreModuleHasNoGSM` checks that the core module's graph never names gsm.

`govern` is its own module so the core does not depend on gsm. It stays v0.x until gsm is stable,
while the core moves on its own schedule. It uses only the core's exported API (never a package
under `internal/`), which its `TestGovernImportsNoCoreInternal` enforces. The three governed-event
log modules (`govern/redislog`, `govern/sqlitelog`, `govern/postgreslog`) require it.

Seven more modules are not libraries you import; they exist so their dependencies stay out of the
core:

| Module | Path | Why it is separate |
|---|---|---|
| approval example | `…/examples/approval` | imports `store/sqlite` |
| plan example | `…/examples/plan` | imports `store/sqlite` |
| mcp example | `…/examples/mcp` | imports `mcp` and the MCP go-sdk |
| observability example | `…/examples/observability` | imports `trace` and the OpenTelemetry SDK |
| governance examples | `…/examples/govern` | nine programs (`authority`, `compliance`, `compose`, `coordination`, `delegation`, `earned-authority`, `mesh`, `proof-carrying-run`, `quorum`) that import `govern` and gsm |
| integration tests | `…/integration` | test-only: the core's tests that need `govern` and gsm (the many-agent convergence test, the run-certificate tests over real gsm policies, and the `bide-audit` CLI tests fed by quorum runs and convergence certificates) |
| benchmarks | `…/benchmarks` | the cross-SDK chaos comparison: eino, langchaingo, adk-go, trpc-agent-go |

The example and integration modules are in `go.work`; `benchmarks` is not, so run it with `GOWORK=off` from its
directory.

## Working in the repo

A root `go.work` ties the modules together for local development, so `go build`/tests resolve
cross-module references without published versions:

```
go work sync
cd trace && go test ./...     # or any module
```

On main, each nested module's `go.mod` requires the core (and `govern`, for the log backends) at
the placeholder `v0.0.0` and carries a `replace github.com/bide-ai/bide => <rel>`, so it builds
standalone against the code in the tree, with or without the workspace (`GOWORK=off`). CI builds and tests every module in its own directory (see
`.github/workflows/ci.yml`, `MODULES`), except `benchmarks`, which CI does not run. The Lint job
fails if a module in the tree is missing from `MODULES` or from `go.work`, and the Changes job if a
module in `MODULES` is not in exactly one of the Linux test shards (`TEST_SHARDS`); the core is
split there, its `agent` package in a shard of its own (`.:agent`) and the rest of its packages in
another, and Lint fails unless each of the core's packages is in exactly one.

## Releases

The core and the library modules are released together, with one version. `scripts/release.sh`
tags the core `vX.Y.Z` on main, then makes a commit, reachable only from the release tags, that
sets each library module's `require` on the core (and on `govern`, for the log backends) to
`vX.Y.Z`, drops the `replace` directives, and records a `go.sum` resolved through the module proxy.
It tags each library module `<dir>/vX.Y.Z` at that commit, so a consumer's `go get` resolves every
module from the proxy. Main keeps its `replace` directives, so development is unchanged.

Published (tagged at every release): `govern`, `store/sqlite`, `store/postgres`, `mcp`, `trace`,
`codec/gcf`, `govern/sqlitelog`, `govern/redislog`, `govern/postgreslog`.

Repo-only (never tagged; they keep their `replace` directives): `examples/approval`,
`examples/plan`, `examples/mcp`, `examples/observability`, `examples/govern`, `integration`,
`benchmarks`.

The procedure is in [CONTRIBUTING.md](../../CONTRIBUTING.md#releasing). The Lint job fails if a
module is in neither list, and a pushed nested tag fails its check if the tagged `go.mod` still
requires `v0.0.0` or replaces a bide module.
