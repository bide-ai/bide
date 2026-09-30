# Project Context

Bide is a Go library for building durable AI agents. It centers on one
append-only journal that provides at-most-once side effects, many concurrent
durable runs in a single process, a cryptographically verifiable audit trail,
and provably convergent shared state.

The core module lives at the repository root; adapters and backends (`trace`,
`mcp`, `codec/gcf`, the `store/*` backends, `govern` and its `govern/*` event-log
backends), the example modules that import them, and the test-only `integration`
module are separate modules stitched together by a `go.work` for local
development. See
[docs/reference/module-structure.md](reference/module-structure.md) for the
layout and [README.md](../README.md) for the guarantees and their scope.
- **mofn-approval**: completed 2026-09-28, 3 waves, 7 agents
  - IMPL doc: docs/IMPL/complete/IMPL-mofn-approval.yaml
