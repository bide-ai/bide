# Contributing

## How contributions work

bide is maintained by Dayna Blackwell (bide-ai), and the core is kept under tight authorial control on
purpose. The project's value is correctness you can rely on: at-most-once side effects,
machine-checked convergence, and an offline-verifiable audit trail. Those guarantees require the
maintainers to own the core end to end, the same reason SQLite keeps a closed core. Concretely:

- **Bug reports and security reports are the most valuable thing you can send.** A clear,
  reproducible report is worth more to this project than a patch. Please report security
  vulnerabilities privately to the maintainers rather than in a public issue.
- **Design discussion and questions are welcome** as issues.
- **Unsolicited code pull requests are generally not accepted.** This is deliberate and not a
  judgment of your work: the correctness and audit guarantees depend on tight control of the core.
- **Code changes happen by invitation.** If a change is wanted, it gets discussed in an issue
  first; an invited contribution then follows the DCO below.

## Developer Certificate of Origin

An invited or accepted contribution is made under the [Developer Certificate of Origin](DCO). Sign off
each commit with `git commit -s`, which appends a `Signed-off-by` line certifying you
have the right to submit the work under the project's license (Apache-2.0). Commits
without a sign-off fail the DCO check.

## Build and test

This is a multi-module workspace. The core is one Go module at the repository root; adapters and backends are their own modules (`trace`, `mcp`, `store/sqlite`, `store/postgres`, `govern`, `govern/postgreslog`, `govern/redislog`, `govern/sqlitelog`, `codec/gcf`, the self-contained example modules `examples/approval`, `examples/plan`, `examples/mcp`, `examples/observability` and `examples/govern`, and the test-only `integration` module, which holds the core's tests that need govern). A `go.work` stitches them together for local development, and CI builds and tests every module listed in `MODULES` in `.github/workflows/ci.yml` (it fails if a module is missing from that list or from `go.work`). On Linux the tests run in parallel shards, one job each (`TEST_SHARDS` in the same file): a shard names modules (`mcp`) or single packages of a module (`.:agent`, the core's `agent` package), and a module entry covers the module's packages no package entry names. A new module goes on exactly one shard's line, or CI fails and says where (`.github/scripts/shards.sh check-modules`); a new package of the core needs nothing, since the `core` shard covers it.

Build and test with the versions pinned in each `go.mod` (rather than the workspace) by setting `GOWORK=off`:

```
GOWORK=off go build ./...
GOWORK=off go test ./...
GOWORK=off go vet ./...
export PATH="$(go env GOROOT)/bin:$PATH"   # use the go1.27 toolchain gofmt
gofmt -l .                                 # must print nothing
```

The homebrew/base `gofmt` predates Go 1.27 generic methods and reports false errors on this repo (for example "method must have no type parameters"); the `PATH` export above puts the go1.27 toolchain `gofmt` first, or run `go fmt ./...` instead.

The example modules that import separate modules (`examples/approval`, `examples/plan`, `examples/mcp`, `examples/observability`, `examples/govern`) build from their own directory:

```
cd examples/observability && GOWORK=off go run .
```

Some tests need external infrastructure and skip without it: `store/postgres` and `govern/postgreslog` look for `PG_DSN`; `govern/redislog` looks for `REDIS_ADDR`; the external convergence oracle cross-check looks for `GSM_AST_CHECKER`. The default suite is green with none of them set.

The fault-schedule explorations (the tests with `Explore` in their name, in `agent` and `plan`) run at a bound that fits CI by default. `BIDE_EXPLORE=1` runs the full bound (`BIDE_EXPLORE=1 GOWORK=off go test -count=1 -timeout 85m -run Explore ./agent ./plan`, about nine minutes on a development machine, without `-race`), and `BIDE_EXPLORE_SIGS=<file>` appends one line per explored concurrent schedule to that file. The Explore workflow (`.github/workflows/explore.yml`) runs the full bound nightly and on demand; it is not a required check. A new test with a `BIDE_EXPLORE` mode goes in the core module and has `Explore` in its name, or the nightly job does not run it (the job fails on a `BIDE_EXPLORE` test outside the core module).

## Module layout

See [docs/reference/module-structure.md](docs/reference/module-structure.md). The rule the architecture test enforces: the core imports no adapter and no infrastructure. A user who does not import `trace` gets no OpenTelemetry in their binary; the same holds for every adapter.

## Examples

Each example is its own `main.go` with a package-doc header that states what it shows and the command to run it. Prefer a small inline deterministic model for examples that demonstrate a durable mechanic (so they run with no API key); use a live OpenAI-compatible endpoint for model-centric examples. Do not leave a compiled binary in the tree (`go build ./examples/x/` drops one in the working directory; build with `go vet` or clean it up).

## Writing style

- Do not use em dashes in prose, comments, or docs. Use colons, commas, semicolons, periods, or parentheses.
- State claims plainly and scope them precisely. Match the surrounding code's comment density and idiom.
- Keep the guarantee language exact: at-most-once, tamper-evident, offline-verifiable, provably convergent. Do not overstate (for example, the cryptographic guarantees cover integrity and authenticity, not confidentiality; see [docs/guides/security-model.md](docs/guides/security-model.md)).

## Go code in the docs

Every ` ```go ` block in `README.md` and under `docs/` (the translations included) must compile
against the current code. The Lint job checks it on every pull request, documentation-only ones
included; run the same check locally from the repository root:

```
go run ./internal/tools/docsnip        # -v lists every block and how it was compiled
```

A block that starts with a `package` clause must compile as written. Any other block is compiled
as top-level declarations, as statements inside a function, or as declarations followed by
statements; a package it uses without importing (`agent`, `audit`, `fmt`, ...) is imported
automatically when the name is unambiguous, and unused variables are allowed. The elisions
`{ ... }` (a function body), `T{...}` (a composite literal) and a line holding only `...` compile.
An HTML comment directly above a block annotates it; it does not show when the markdown is
rendered:

```
<!-- docsnip: setup ctx context.Context; a *agent.Agent; runID, input string -->
<!-- docsnip: setup type Order struct{}; func classify(Order) (bool, error); returns error -->
<!-- docsnip: api agent -->
<!-- docsnip: skip pseudo-code: the loop, not the API -->
```

- **setup** declares the identifiers a block uses but does not declare. Items are separated by
  semicolons or newlines: `name[, name] Type` (a variable), a `type`, `func`, `var` or `const`
  declaration, `import "path"` (for a package whose name is ambiguous or not in the workspace, such
  as `gsm` or the OpenTelemetry API), and `returns T` for a statement block that returns.
- **api** marks a listing of a package's declarations (functions and methods without bodies,
  types, vars, consts). Each must match the package: signatures identical, interfaces identical, a
  struct's listed fields present with identical types. Names in the block resolve to the package's.
- **skip** is for pseudo-code only, with the reason. A skip on a block that compiles is reported,
  so delete the skip when the block becomes real code.

When a change breaks a block, fix the block and the text around it in the same pull request. The
README and its translations share their blocks, so annotate every copy identically. Release notes
and design records describe the API of their time: when a change breaks one of their blocks, skip
it with the version it describes instead of rewriting history.

## Changelog

Every user-facing change adds an entry to [CHANGELOG.md](CHANGELOG.md) under `## [Unreleased]`, in
the same pull request. Put it under Added, Changed, Deprecated, Removed, Fixed or Security; keep it
to one line that names the public identifiers affected and links the PR. Mark breaking changes
with a leading **Breaking:** under Changed or Removed. Describe a fix by the corrected behavior,
plainly. Internal-only changes (CI, tests, refactors with no API or behavior change) need no entry.

## Releasing

The core module and the library modules are released together under one version. Published
modules, tagged `<dir>/vX.Y.Z` at every release: `govern`, `store/sqlite`, `store/postgres`, `mcp`,
`trace`, `codec/gcf`, `govern/sqlitelog`, `govern/redislog`, `govern/postgreslog`. Repo-only
modules, never tagged: `examples/*`, `integration`, `benchmarks`. The lists live in
`scripts/release.sh`, and the Lint job fails if a module is in neither.

On main every nested module requires the core at the placeholder `v0.0.0` through a `replace`
directive, which a consumer cannot resolve. `scripts/release.sh` therefore tags the core on main,
then records, in commits reachable only from the release tags, the `go.mod` and `go.sum` each
published module needs: the core (and `govern`, for the log backends) required at the release
version, the `replace` directives dropped, and `go.sum` resolved through the module proxy. Main
keeps its `replace` directives, so the workspace and `GOWORK=off` builds keep using the code in the
tree between releases.

1. Open a release pull request: move the `## [Unreleased]` entries of `CHANGELOG.md` under
   `## [X.Y.Z] - <date>` and add `docs/releases/vX.Y.Z.md`. Merge it and wait for CI on main to
   pass for that commit.
2. From a clean checkout of main, dry-run the release. Nothing leaves the machine: the script
   works in a scratch clone and pushes to a scratch repository that stands in for GitHub, and the
   go command resolves the bide modules from it.

   ```
   git switch main && git pull
   scripts/release.sh vX.Y.Z
   ```

   It checks that HEAD is `origin/main`, that CI passed for it, that the release notes and the
   changelog section exist, and that no tag exists yet (a dry run reports these as warnings), then
   runs every step below and ends with `dry run complete`.
3. Release, on the maintainer's go only:

   ```
   scripts/release.sh vX.Y.Z --push
   ```

   The script
   1. tags the core `vX.Y.Z` at HEAD, pushes the tag and waits until `proxy.golang.org` serves it;
   2. for the modules that need only the core (`govern`, the stores, `mcp`, `trace`, `codec/gcf`),
      sets their `require` on the core to `vX.Y.Z`, drops the `replace`, runs `go mod tidy`
      against the proxy, builds, vets and tests each one with `GOWORK=off`, commits (signed off),
      tags each `<dir>/vX.Y.Z` at that commit, pushes the tags at most three per push and waits
      for the proxy;
   3. does the same for the governed-event logs, which also require `govern` at `vX.Y.Z`;
   4. checks each module with `go list -m <module>@vX.Y.Z` against the proxy, and builds a scratch
      consumer module that `go get`s every published module at `vX.Y.Z` with an empty module cache.
4. The core tag runs the Release workflow (the `bide-audit` binaries and the GitHub Release, with
   `docs/releases/vX.Y.Z.md` as its notes); each nested tag runs the Release modules workflow,
   which fails if the tagged `go.mod` still requires `v0.0.0` or replaces a bide module, or does
   not build with `GOWORK=off`. Check both, the release assets and the Homebrew formula: every
   nested tag has its own Release modules run.

   GitHub creates no push event, so runs no workflow, for the tags of a push that carries more
   than three tags. The script therefore never pushes more than three at once (its `--self-test`,
   run in CI, fails otherwise). If a nested tag has no Release modules run (it was pushed some
   other way, or the run was lost), run the check by hand for that tag; the workflow checks out
   the tag and runs the same check:

   ```
   gh workflow run release-modules.yml -R bide-ai/bide --ref main -f tag=govern/vX.Y.Z
   ```

If a `--push` run stops partway, re-run the same command: the core tag and any nested tags already
on the remote are reused (each is only checked against the proxy) and the rest are made. Never move
or delete a pushed tag (the proxy and the checksum database keep the first version they saw); fix
forward with a patch release. `--ref <commit>` dry-runs a commit other than HEAD, `--no-test` skips
the module tests, and `--keep` keeps the scratch work tree for inspection.

## Formal models

The claim protocol (attempt markers, not-started records, numbered retries, pendingClaims, the
resume gate, the Step flight and halt resolution) is modelled in TLA+ under `spec/tla/`, and the
Models workflow checks it with TLC on every pull request. A change to those rules changes the model
in the same pull request: edit the PlusCal in `spec/tla/claims/Claims.tla`, re-translate it with
`spec/tla/check.sh translate` (CI fails on a stale translation), and run `spec/tla/check.sh`, which
needs Java 11 or later and downloads the pinned `tla2tools.jar` (checked against its SHA-256 in
`spec/tla/tools.lock`). A counterexample TLC finds in the current rules is a bug: reproduce it as a
deterministic Go test before fixing it, as for any bug. A rule replaced by the change becomes a
`Bug` value and a configuration in `spec/tla/claims/regress/` that must keep failing. The Models
workflow checks the configurations in parallel shards listed in `MODEL_SHARDS` in
`.github/workflows/models.yml`, each a set of configuration directories: a new model, or a model's
first `regress`, `findings` or `limits` directory, goes on one shard's line in the same pull
request. Until it does, the Models check fails with the directory to add and the shard line to add
it to (`.github/scripts/shards.sh check-models`). The same holds for the other models under
`spec/tla/`; in particular, a change to the run's end markers, to `Lease`, `Recover` or
`RecoverLoop`, or to `Cancel` changes the run lifecycle model, `spec/tla/lifecycle/Lifecycle.tla`, and a
change to `audit.AttenuatingSubAgent` (its grants, `BindRollback`, `CallGuard`), the sub-agent
tool, programmatic sub-runs (`SubRunFor`, their links) or the saga's rollback walk changes the
delegation model, `spec/tla/delegation/Delegation.tla`, and a change to `Session` (its journal's
`start/`, `from/` and `turn/` records, `Send`, `SendOnce`, `reload`, `appendTurn`) or to the
session run IDs changes the sessions model, `spec/tla/sessions/Sessions.tla`.
See [spec/tla/README.md](spec/tla/README.md).

The claim model is also checked with Apalache, nightly: bounded symbolic checks and an inductive
invariant (`spec/tla/claims/ClaimsInductive.tla`) that proves `AtMostOnce`, `NotStartedExclusive`, `NoLiveOverride` and `AtMostOncePerIntent` for two drivers over two processes, on one call (attempts 0..3, 8 claim ids) or with halt resolution and the caller's second call (attempts 0..3, 6 claim ids), without the approval gate, and, under the lease check, assuming no plain run holds the live attempt at the check (`PlainRunIdleAtCheck`), at any depth and for any number and mix of faults within the run's 8 (6) claim ids and attempts 0..3 (claim ids are never reused, so this bounds the number of claims).
A change to `Claims.tla` should keep both passing; run them locally with
`spec/tla/check.sh apalache claims`, which needs Java 17 or later and downloads the pinned Apalache
release (about 190 MB, checked against its SHA-256 in `spec/tla/tools.lock`). If a rule change
breaks the induction step, Apalache prints a counterexample to induction: a state satisfying the
invariant and one step out of it. Either the rule broke the property (reproduce it as a bounded
counterexample and a Go test) or the invariant needs a new conjunct; see
[Apalache](spec/tla/README.md#apalache).

The Go code the models describe (the claim protocol, the approval gate, flows, spend accounting, the tool-call state machine, the run lifecycle, delegation and sub-run authority, sessions)
is wrapped in region markers, `// protocol:<model> begin <Action> ...` and `// protocol:<model> end`,
that name the model and the model actions the region implements. The Lint job runs
`go run ./internal/tools/modelsync`, which fails a pull request that:

- touches a marked region and changes nothing under `spec/tla/<model>/`. A change that leaves the
  modelled behavior as it is (a rename, a comment, an error message) says so with a line in the
  pull request's description or a commit message: `Protocol-Impact: none (<reason>)`, or
  `Protocol-Impact: <model>[,<model>] none (<reason>)` for some models only. The override is printed
  as a warning on the pull request, and the reviewer judges the reason. Re-run the Lint job after
  editing the description.
- leaves a marker, the model-to-code map in `spec/tla/README.md` and the spec disagreeing on an
  action's name: every marker names an action the spec defines and the map lists, and every mapped
  action is marked unless the map lists it as having no Go code.

Move or add the markers when you move or add modelled code; a new model lands with its markers
(see [Keeping the code and the models in step](spec/tla/README.md#keeping-the-code-and-the-models-in-step)).

The tool-call state machine (`spec/tla/toolcall/`) covers `agent/toolexec.go`'s base handler
and call states, the tool call in `agent/loop.go`, `agent/tool_middleware.go`, `rollbackRun` in
`agent/saga.go`, and `internal/toolhook`.

## Before opening a change

- `GOWORK=off go build ./...`, `GOWORK=off go test ./...`, `GOWORK=off go vet ./...`, and `gofmt -l .` are clean (run `gofmt` from the go1.27 toolchain via `export PATH="$(go env GOROOT)/bin:$PATH"`, or use `go fmt ./...`; the base gofmt predates Go 1.27 generic methods and reports false errors).
- New exported symbols have doc comments that start with their name, and every package has a package comment; CI checks this with `go run ./internal/tools/doccheck -root . -allow .doccheck-allow` from the root, and a pull request may not add entries to `.doccheck-allow`.
- `go run ./internal/tools/docsnip` is clean: the Go blocks of the docs compile (see [Go code in the docs](#go-code-in-the-docs)).
- `spec/tla/check.sh` passes when the change touches a modelled protocol (the claim protocol, the tool-call state machine, delegation and sub-runs, sessions), and `go run ./internal/tools/modelsync -base origin/main` passes (see [Formal models](#formal-models)).
- `CHANGELOG.md` has an entry under Unreleased, or the change is not user-facing.
- New docs are linked from the [docs index](docs/README.md) and honor the style above.
