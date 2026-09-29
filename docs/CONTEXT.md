# Project Context

Bide is a Go library for building durable AI agents. It centers on one
append-only journal that provides at-most-once side effects, many concurrent
durable runs in a single process, a cryptographically verifiable audit trail,
and provably convergent shared state.

The core module lives at the repository root; adapters and backends (`trace`,
`mcp`, the `store/*` and `govern/*` backends, and the `examples/*` modules) are
separate modules stitched together by a `go.work` for local development. See
[docs/reference/module-structure.md](reference/module-structure.md) for the
layout and [README.md](../README.md) for the guarantees and their scope.
- **mofn-approval**: completed 2026-09-28, 3 waves, 7 agents
  - IMPL doc: docs/IMPL/complete/IMPL-mofn-approval.yaml
