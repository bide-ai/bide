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

This is a multi-module workspace. The core is one Go module at the repository root; adapters and backends are their own modules (`trace`, `mcp`, `store/sqlite`, `store/postgres`, `govern`, `govern/postgreslog`, `govern/redislog`, `govern/sqlitelog`, `codec/gcf`, the self-contained example modules `examples/approval`, `examples/plan`, `examples/mcp`, `examples/observability` and `examples/govern`, and the test-only `integration` module, which holds the core's tests that need govern). A `go.work` stitches them together for local development, and CI builds and tests every module listed in `MODULES` in `.github/workflows/ci.yml` (it fails if a module is missing from that list or from `go.work`).

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

## Module layout

See [docs/reference/module-structure.md](docs/reference/module-structure.md). The rule the architecture test enforces: the core imports no adapter and no infrastructure. A user who does not import `trace` gets no OpenTelemetry in their binary; the same holds for every adapter.

## Examples

Each example is its own `main.go` with a package-doc header that states what it shows and the command to run it. Prefer a small inline deterministic model for examples that demonstrate a durable mechanic (so they run with no API key); use a live OpenAI-compatible endpoint for model-centric examples. Do not leave a compiled binary in the tree (`go build ./examples/x/` drops one in the working directory; build with `go vet` or clean it up).

## Writing style

- Do not use em dashes in prose, comments, or docs. Use colons, commas, semicolons, periods, or parentheses.
- State claims plainly and scope them precisely. Match the surrounding code's comment density and idiom.
- Keep the guarantee language exact: at-most-once, tamper-evident, offline-verifiable, provably convergent. Do not overstate (for example, the cryptographic guarantees cover integrity and authenticity, not confidentiality; see [docs/guides/security-model.md](docs/guides/security-model.md)).

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
      tags each `<dir>/vX.Y.Z` at that commit, pushes the tags and waits for the proxy;
   3. does the same for the governed-event logs, which also require `govern` at `vX.Y.Z`;
   4. checks each module with `go list -m <module>@vX.Y.Z` against the proxy, and builds a scratch
      consumer module that `go get`s every published module at `vX.Y.Z` with an empty module cache.
4. The core tag runs the Release workflow (the `bide-audit` binaries and the GitHub Release, with
   `docs/releases/vX.Y.Z.md` as its notes); each nested tag runs the Release modules workflow,
   which fails if the tagged `go.mod` still requires `v0.0.0` or replaces a bide module, or does
   not build with `GOWORK=off`. Check both, the release assets and the Homebrew formula.

If a `--push` run stops partway, re-run the same command: the core tag and any nested tags already
on the remote are reused (each is only checked against the proxy) and the rest are made. Never move
or delete a pushed tag (the proxy and the checksum database keep the first version they saw); fix
forward with a patch release. `--ref <commit>` dry-runs a commit other than HEAD, `--no-test` skips
the module tests, and `--keep` keeps the scratch work tree for inspection.

## Before opening a change

- `GOWORK=off go build ./...`, `GOWORK=off go test ./...`, `GOWORK=off go vet ./...`, and `gofmt -l .` are clean (run `gofmt` from the go1.27 toolchain via `export PATH="$(go env GOROOT)/bin:$PATH"`, or use `go fmt ./...`; the base gofmt predates Go 1.27 generic methods and reports false errors).
- New exported symbols have doc comments that start with their name, and every package has a package comment; CI checks this with `go run ./internal/tools/doccheck -root . -allow .doccheck-allow` from the root, and a pull request may not add entries to `.doccheck-allow`.
- `CHANGELOG.md` has an entry under Unreleased, or the change is not user-facing.
- New docs are linked from the [docs index](docs/README.md) and honor the style above.
