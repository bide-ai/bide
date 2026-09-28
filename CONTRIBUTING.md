# Contributing

## Developer Certificate of Origin

Contributions are accepted under the [Developer Certificate of Origin](DCO). Sign off
each commit with `git commit -s`, which appends a `Signed-off-by` line certifying you
have the right to submit the work under the project's license (Apache-2.0). Commits
without a sign-off fail the DCO check.

## Build and test

This is a multi-module workspace. The core is one Go module at the repository root; adapters and backends are their own modules (`trace`, `mcp`, `store/sqlite`, `store/postgres`, `govern/postgreslog`, `govern/redislog`, `govern/sqlitelog`, and the self-contained `examples/mcp` and `examples/observability`). A `go.work` stitches them together for local development.

Build and test with the versions pinned in each `go.mod` (rather than the workspace) by setting `GOWORK=off`:

```
GOWORK=off go build ./...
GOWORK=off go test ./...
GOWORK=off go vet .
export PATH="$(go env GOROOT)/bin:$PATH"   # use the go1.27 toolchain gofmt
gofmt -l .                                 # must print nothing
```

The homebrew/base `gofmt` predates Go 1.27 generic methods and reports false errors on this repo (for example "method must have no type parameters"); the `PATH` export above puts the go1.27 toolchain `gofmt` first, or run `go fmt ./...` instead.

The two example modules that import separate modules (`examples/mcp`, `examples/observability`) build from their own directory:

```
cd examples/observability && GOWORK=off go run .
```

Some tests need external infrastructure and skip without it: `store/postgres` and `govern/postgreslog` look for `PG_DSN`; the external convergence oracle cross-check looks for `GSM_AST_CHECKER`. The default suite is green with none of them set.

## Module layout

See [docs/reference/module-structure.md](docs/reference/module-structure.md). The rule the architecture test enforces: the core imports no adapter and no infrastructure. A user who does not import `trace` gets no OpenTelemetry in their binary; the same holds for every adapter.

## Examples

Each example is its own `main.go` with a package-doc header that states what it shows and the command to run it. Prefer a small inline deterministic model for examples that demonstrate a durable mechanic (so they run with no API key); use a live OpenAI-compatible endpoint for model-centric examples. Do not leave a compiled binary in the tree (`go build ./examples/x/` drops one in the working directory; build with `go vet` or clean it up).

## Writing style

- Do not use em dashes in prose, comments, or docs. Use colons, commas, semicolons, periods, or parentheses.
- State claims plainly and scope them precisely. Match the surrounding code's comment density and idiom.
- Keep the guarantee language exact: at-most-once, tamper-evident, offline-verifiable, provably convergent. Do not overstate (for example, the cryptographic guarantees cover integrity and authenticity, not confidentiality; see [docs/guides/security-model.md](docs/guides/security-model.md)).

## Before opening a change

- `GOWORK=off go build ./...`, `GOWORK=off go test ./...`, and `gofmt -l .` are clean (run `gofmt` from the go1.27 toolchain via `export PATH="$(go env GOROOT)/bin:$PATH"`, or use `go fmt ./...`; the base gofmt predates Go 1.27 generic methods and reports false errors).
- New exported symbols have doc comments.
- New docs are linked from the [docs index](docs/README.md) and honor the style above.
