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

This is a multi-module workspace. The core is one Go module at the repository root; adapters and backends are their own modules (`trace`, `mcptools`, `store/sqlite`, `store/postgres`, `govern`, `govern/postgreslog`, `govern/redislog`, `govern/sqlitelog`, `codec/gcf`, the `bide` command `cmd/bide`, the `bide-hsm` command `cmd/bide/bide-hsm` (its own module under `cmd/bide`, so that it can import that module's internal approval code; it needs cgo for PKCS#11 keys), and the deprecated `cmd/bide-view`, the self-contained example modules `examples/approval`, `examples/plan`, `examples/mcp`, `examples/observability`, `examples/stripe-refund` and `examples/govern`, the test-only `integration` module, which holds the core's tests that need govern, the test-only `cmd/bide/bide-hsm/emulatortest` and `cmd/bide/bide-hsm/azuretest`, which run bide-hsm's `gcpkms:` references against an in-process Cloud KMS emulator and its `azurekv:` references against the Azure SDK's fake Key Vault server, and the test-only `signer/awskms/emulatortest`, which runs the AWS KMS signer against an in-process emulator and keeps the emulator out of the signer's module graph). A `go.work` stitches them together for local development, and CI builds and tests every module listed in `MODULES` in `.github/workflows/ci.yml` (it fails if a module is missing from that list or from `go.work`). On Linux the tests run in parallel shards, one job each (`TEST_SHARDS` in the same file): a shard names modules (`mcptools`) or single packages of a module (`.:agent`, the core's `agent` package), and a module entry covers the module's packages no package entry names. A new module goes on exactly one shard's line, or CI fails and says where (`.github/scripts/shards.sh check-modules`); a new package of the core needs nothing, since the `core` shard covers it.

Build and test a module with the versions pinned in its `go.mod` (rather than the workspace) by setting `GOWORK=off` in its directory (at the root, `./...` covers only the core module):

```
GOWORK=off go build ./...
GOWORK=off go test ./...
GOWORK=off go vet ./...
export PATH="$(go env GOROOT)/bin:$PATH"   # use the go1.27 toolchain gofmt
gofmt -l .                                 # must print nothing
```

The homebrew/base `gofmt` predates Go 1.27 generic methods and reports false errors on this repo (for example "method must have no type parameters"); the `PATH` export above puts the go1.27 toolchain `gofmt` first, or run `go fmt ./...` instead.

The example modules that import separate modules (`examples/approval`, `examples/plan`, `examples/mcp`, `examples/observability`, `examples/stripe-refund`, `examples/govern`) build from their own directory:

```
cd examples/observability && GOWORK=off go run .
```

The approve page's script tests (`approvals/page`, run by `go test ./approvals`) need Node 20 or later on PATH (Ed25519 in WebCrypto) and skip without it locally; with `CI` set they fail instead, and the workflow's test jobs install Node. Regenerate the golden vectors the script is tested against with `GOWORK=off go test ./agent -run TestCanonicalVectors -update` after a deliberate change to the canonical arguments or the decision bytes.

Some tests need external infrastructure and skip without it: `store/postgres` and `govern/postgreslog` look for `PG_DSN`; `govern/redislog` looks for `REDIS_ADDR`; the external convergence oracle cross-check looks for `GSM_AST_CHECKER`. The default suite is green with none of them set. The AWS KMS signer needs no setup: `signer/awskms/emulatortest` starts an in-process AWS KMS emulator ([aws-kms-emulator](https://github.com/blackwell-systems/aws-kms-emulator)) in each test, so `cd signer/awskms/emulatortest && go test ./...` runs everything.

Coverage of the published modules is reported to Codecov by the Coverage workflow (`.github/workflows/coverage.yml`) on pushes to main, and on demand for a branch (see "What CI runs, and when"). It is information only: it is not a required check, an upload failure never fails it, and `codecov.yml` sets the project and patch statuses to informational. Run it locally with `scripts/coverage.sh` (it writes `coverage.out`, one merged profile, and prints the total; `go tool cover -html=coverage.out` shows it by line). Examples, benchmarks and the integration module are not measured.

The fault-schedule explorations (the tests with `Explore` in their name, in `agent` and `plan`) run at a bound that fits CI by default. `BIDE_EXPLORE=1` runs the full bound (`BIDE_EXPLORE=1 GOWORK=off go test -count=1 -timeout 85m -run Explore ./agent ./plan`, about nine minutes on a development machine, without `-race`), and `BIDE_EXPLORE_SIGS=<file>` appends one line per explored concurrent schedule to that file. The Explore workflow (`.github/workflows/explore.yml`) runs the full bound nightly and on demand; it is not a required check. A new test with a `BIDE_EXPLORE` mode goes in the core module and has `Explore` in its name, or the nightly job does not run it (the job fails on a `BIDE_EXPLORE` test outside the core module).

## What CI runs, and when

GitHub Free allows 20 jobs to run at once across the repository, and one pull request's full run is about 20, so the full matrix runs once per merge queue batch (up to five pull requests), not once per pull request push. A pull request runs a fast subset; the rest runs when the pull request enters the merge queue, and a failure there ejects it.

Every required check reports on every pull request. A check that does not run on a pull request is skipped, which satisfies the requirement; do not read a green skipped check as having run.

| Required check | On a pull request | In the merge queue and on main |
|---|---|---|
| `Lint` | modelsync, docs snippets and removed names always; gofmt, vet, doccheck and govulncheck when code changed | also the reproducible-build check (all skipped but the first three for a documentation-only batch) |
| `Test (ubuntu-latest)` | `agent`, `core` and `rest` shards under `-race`, when code changed (only `rest` for a change to `examples/` alone) | also the run without `encoding/json/v2` |
| `Test (macos-latest)` | skipped | one job, every module, without `-race` |
| `Test (windows-latest)` | skipped | one job, every module, without `-race` |
| `Integration (Postgres, Redis)` | skipped | the Postgres and Redis suites against service containers, twice each |
| `Models` | the TLA+ shards holding a model whose directory under `spec/tla` the change touches (the batch's changes in the queue) | the same in the queue; every model on main and nightly |
| `gsm machine gate` | when the change can alter what a bide program makes (code, less CI scripts, other workflows and tool configuration) | always |
| `Changelog fragment`, `dco` | always | always |

"Documentation only" means the change touches only: Markdown files at the top level or under `docs/`, `changes/` and `.github/`; images and other documents under `docs/` and `assets/` (`png`, `jpg`, `gif`, `svg`, `webp`, `ico`, `txt`, `mp4`, `webm`, `pdf`, `cast`); `docs-site/`; `spec/tla/`; `.github/ISSUE_TEMPLATE/`; `LICENSE`, `NOTICE`, `DCO`; and the `docs`, `bench`, `models` and `explore` workflows. The Go jobs' steps are skipped for it, in the merge queue (the batch's diff against the queue's base) as well as on a pull request. These count as code wherever they are, so they never skip anything: any `.go` file (the `gofmt`, vet and doccheck steps read `assets/` and `docs/` too), `go.mod`, `go.sum`, `go.work` and `go.work.sum`, anything under a `testdata` directory, a Markdown file inside a package or a directory such as `benchmarks/` (a test or `go:embed` may read it), any other file under `docs/` or `assets/` (a script, a program, JSON), and `docs/reference/cli.md`, `docs/reference/bide-hsm.md`, `docs/reference/api/`, `docs/trust/stability.md` and `docs/releases/`, which Go tests and `doccheck` compare with the code. "Examples only" means every code path lies under `examples/` (no other module imports an example module). Both are decided by `.github/scripts/changed-code.sh`; any other change, a failed diff or an unexpected event counts as a code change. The Coverage workflow (not required) no longer runs on pull requests: it runs on main, and from the Actions tab on a branch (`workflow_dispatch`) when a patch's coverage is wanted.

To run the full matrix on a pull request, for a risky change to the journal, leases, sagas, storage, anything OS-specific or the CI itself, add the `full-ci` label and re-run the CI, Models and gsm machine gate workflows from the Actions tab (or push a commit). The label is read when the jobs run, so adding it does not start a run by itself. With the label, the macOS, Windows and Integration jobs, the json/v2 run, the reproducibility check, every model and the gsm gate run on the pull request, as they do in the queue.

The test jobs reuse the Go test cache. The Linux, macOS and Windows jobs restore the build and test cache that the last push to main saved (`.github/actions/go-cache`) and run `go test` without `-count=1`, so a package whose test binary and inputs are unchanged reuses its result; a pull request and a merge queue batch run only what changed. Three things keep that sound:
- A push to main starts from an empty test cache (`go clean -testcache`), runs every test, and saves the cache for the next runs. Only main saves, since the caches of other runs are not readable by other runs.
- A change to any `go.mod`, `go.sum`, `go.work` or `go.work.sum`, or to the Go version, clears the cached test results (`.github/scripts/gocache.sh`): the architecture tests read those files through `go list` and `go mod edit`, which `go test` cannot see.
- A test that starts another process can depend on something `go test` does not see (another package it builds or lists, a script over files of the tree, a program named by the environment). Every test file that calls `exec.Command`, and every non-test file that does in a package with tests (or imported by tests), is therefore either in a package of `.github/uncached-tests.txt`, which always runs with `-count=1`, or in `.github/cache-sound-tests.txt` with the reason its result depends only on the test binary and the module files (it re-runs the test binary, runs git in a temporary directory, or lists its own package). Lint fails for a test file in neither (`.github/scripts/check-uncached.sh`), and for a file in the second list that lists packages with `./...`. Add a new process-starting test to one of the lists in the same pull request; when in doubt, use the first.

Still uncached by design: Integration (`-count=2` against the services), the `bide_audit_only` build's tests and `TestProtocolVocabulary` in Lint. A test that must not be cached for another reason (it reads the clock, the network or a service) belongs in `.github/uncached-tests.txt` too.

Pushing a new commit to a pull request cancels the superseded run of the same workflow; pushes to main, merge queue batches and the nightly are never cancelled. The Linux test shards and the Models shards are balanced by measured time; the longest single package is `agent` on Linux (about four minutes under `-race`). When a shard becomes the slowest check, move modules between shard lines (`TEST_SHARDS`, `MODEL_SHARDS`); the Changes and plan jobs fail unless every module and configuration is on exactly one line. Prefer fewer jobs over a shorter critical path: each job costs a runner start and its setup.

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

Every user-facing change adds a changelog fragment in the same pull request: one small file,
`changes/unreleased/<slug>.md`, instead of an edit to [CHANGELOG.md](CHANGELOG.md). Pull requests
therefore do not conflict on the changelog. Do not edit CHANGELOG.md in a pull request; the release
pull request assembles the fragments into it.

A fragment is a `Section:` line, a blank line, and the entry as one paragraph (no blank lines, no
leading `- `):

```
Section: Fixed

`Store.Resolve` no longer returns `ErrNotFound` for a run whose first record is still being written ([#240]).
```

- The section is one of `Added`, `Changed`, `Fixed`, `Removed` or `Security`. A release lists them
  in that order.
- The entry names the public identifiers affected. Mark breaking changes with a leading
  **Breaking:** under Changed or Removed. Describe a fix by the corrected behavior, plainly.
- Name the file for the change (`resolve-not-found.md`); entries of one section are listed in file
  name order. One fragment holds one entry: a change with several entries adds several files.
- A reference like `[#240]` needs no link definition: assembling adds `[#240]: .../pull/240` to the
  end of the changelog when it is missing.
- Put anything shaped like an HTML tag in backticks: a placeholder such as `<time>`, a generic
  such as `List<T>`. The docs site (VitePress) builds from the changelog and reads a bare `<word>`
  as an HTML element, which fails its build; `changelog lint` refuses one outside a code span
  (autolinks such as `<https://...>` and comparisons such as `a < b` are fine).
- Internal-only changes (CI, tests, docs, examples, refactors with no API or behavior change) need
  no fragment.

The Changelog workflow fails a pull request that changes code a published module ships (anything
outside documentation, tests, `testdata`, CI, `examples/`, `integration/`, `benchmarks/`,
`scripts/`, `internal/tools/` and the `go.mod` and `go.sum` files) and adds no fragment. A
maintainer can give the pull request the `no-changelog` label when the change has no user-facing
effect; the check runs again when a label changes. It also parses every fragment, so a malformed
one fails. Run the same checks locally:

```
go run ./internal/tools/changelog lint
go run ./internal/tools/changelog check -base origin/main -head HEAD
```

## Releasing

The core module and the library modules are released together under one version. Published
modules, tagged `<dir>/vX.Y.Z` at every release: `govern`, `store/sqlite`, `store/postgres`, `mcptools`,
`trace`, `codec/gcf`, `govern/sqlitelog`, `govern/redislog`, `govern/postgreslog`, `signer/pkcs11`, `signer/awskms`, `signer/azurekv`, `signer/gcpkms`, and the commands
`cmd/bide`, `cmd/bide/bide-hsm` (installed from source with `go install`, never built into the release archives: it needs cgo) and `cmd/bide-view` (the latter one last time, at v0.14.0). Repo-only
modules, never tagged: `examples/*`, `integration`, `benchmarks` and the test-only emulator modules (`signer/awskms/emulatortest`, `signer/gcpkms/emulatortest`, `cmd/bide/bide-hsm/emulatortest`, `cmd/bide/bide-hsm/azuretest`). The lists live in
`scripts/release.sh`, and the Lint job fails if a module is in neither.

On main every nested module requires the core at the placeholder `v0.0.0` through a `replace`
directive, which a consumer cannot resolve. `scripts/release.sh` therefore tags the core on main,
then records, in commits reachable only from the release tags, the `go.mod` and `go.sum` each
published module needs: the core (and `govern`, for the log backends) required at the release
version, the `replace` directives dropped, and `go.sum` resolved through the module proxy. Main
keeps its `replace` directives, so the workspace and `GOWORK=off` builds keep using the code in the
tree between releases.

1. Open a release pull request: assemble the fragments into `CHANGELOG.md` with
   `scripts/release.sh --assemble-changelog vX.Y.Z <date>` (it adds the `## [X.Y.Z] - <date>`
   section, grouped Added, Changed, Fixed, Removed, Security, moves the `[Unreleased]` compare link,
   adds the `[X.Y.Z]` one, and deletes the fragments), review the result, and add
   `docs/releases/vX.Y.Z.md`. The notes become the GitHub Release body, where a relative path
   does not resolve, so every link and image in them is absolute, an image pinned to the release
   tag (`https://raw.githubusercontent.com/bide-ai/bide/vX.Y.Z/docs/guides/img/...`); `doccheck`
   enforces it. Merge it and wait for CI on main to pass for that commit. The release script
   refuses a commit that still has fragments.
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

   "CI passed" for the dry run and the preflight means a completed `ci.yml` push run of the release
   commit on main succeeded and none failed, or, while no push run has completed, a successful
   `ci.yml` merge queue run (event `merge_group`) of a commit with the same tree (compared by tree
   SHA through the API, not by commit). That lets the dry run overlap main's CI. The queue's run
   is weaker than main's (it reuses the test cache and skips the Go jobs for a documentation-only
   batch), so before a `--push` release pushes the root tag, which cannot be undone, it checks main's
   push runs of the commit again, whatever the preflight found: it polls every 30 seconds for up to
   an hour (`BIDE_CI_POLL_SECS`, `BIDE_CI_WAIT_SECS`) while any is still going (a manual re-run
   included, even after an earlier run passed), tags only if all passed, and otherwise stops with
   nothing tagged (run it again when they have passed). A failed or cancelled push run stops a
   release whatever the queue found; a queue run of another tree, or one that failed, never counts.
3. Release, on the maintainer's go only:

   ```
   scripts/release.sh vX.Y.Z --push
   ```

   The script
   1. tags the core `vX.Y.Z` at HEAD, pushes the tag and waits until `proxy.golang.org` serves it (see below for how it waits);
   2. for the modules that need only the core (`govern`, the stores, `mcptools`, `trace`, `codec/gcf`, and `cmd/bide-view`, which needs none),
      sets their `require` on the core to `vX.Y.Z`, drops the `replace`, runs `go mod tidy`
      against the proxy, builds, vets and tests each one with `GOWORK=off`, commits (signed off),
      tags each `<dir>/vX.Y.Z` at that commit, pushes the tags at most three per push and waits
      for the proxy the same way;
   3. does the same for the governed-event logs, which also require `govern` at `vX.Y.Z`, and for
      the `bide` command, which requires the stores;
   3a. then for `bide-hsm`, which requires `bide`, the stores and the two signers; it is installed
      from the proxy and `bide-hsm version` must report the release;
   4. checks each module with `go list -m <module>@vX.Y.Z` against the proxy, and builds a scratch
      consumer module that `go get`s every published module at `vX.Y.Z` with an empty module cache,
      then installs the released `bide` and checks that the project `bide init` writes builds and
      passes its test.
4. The core tag runs the Release workflow: the `bide` binaries and the GitHub Release (with
   `docs/releases/vX.Y.Z.md` as its notes), the Homebrew formula (`bide.rb`, attached to the
   release, and pushed to bide-ai/homebrew-tap when the `HOMEBREW_TAP_TOKEN` secret is set; see
   [The Homebrew tap by hand](#the-homebrew-tap-by-hand)), and the `repro` job, which rebuilds every binary
   from the tag on macOS and fails unless the bytes match (`scripts/repro.sh`). The Release workflow
   also builds the approve page bundle from the tag (`go run ./approvals/cmd/approvepage`), signs its
   `SHA256SUMS` keylessly and attaches it to the release as `approve-page.SHA256SUMS` (with its
   `.sigstore.json` bundle), because Subresource Integrity cannot pin the page's entry document; the
   `verify-signatures` job verifies that signature, and the `repro` job rebuilds the bundle on macOS
   and fails unless its sums are the published ones. Its `module-sbom` job waits for the nested
   tags below to reach the Go proxy, then publishes the signed SBOM of the published module set
   (`scripts/module-sbom.sh`), which the `verify-module-sbom` job then verifies; every SBOM is signed like the archives. Each nested tag runs the Release modules workflow,
   which fails if the tagged `go.mod` still requires `v0.0.0` or replaces a bide module, or does
   not build with `GOWORK=off`. Check both, the release assets and the Homebrew formula: every
   nested tag has its own Release modules run.

   **How the script waits for the proxy.** `proxy.golang.org` caches a miss for 30 to 60 minutes, and
   asking again keeps the cached miss fresh; the v0.18.0 release lost hours to it. So after a push the
   script first checks each tag with `GOPROXY=direct` (git, with the checksum database off, so neither
   the proxy nor the database hears of the version), does not ask the proxy until five minutes after
   the push (or its own start, when it resumes), then asks with waits of 5, 10, then 15 minutes, for
   at most 90 minutes in all for one stage's modules, printing what it waits for. If it gives up, run the same command again once the proxy serves the tag
   (`go list -m <module>@vX.Y.Z` shows it); do not poll sooner. `PW_SETTLE`, `PW_STEP` and `PW_MAX`
   (seconds) change the delays (`scripts/proxy-wait.sh`); `--self-test` checks the rules on a fake
   clock. The `module-sbom` job waits the same way, for up to four hours.

   GitHub creates no push event, so runs no workflow, for the tags of a push that carries more
   than three tags. The script therefore never pushes more than three at once (its `--self-test`,
   run in CI, fails otherwise). If a nested tag has no Release modules run (it was pushed some
   other way, or the run was lost), run the check by hand for that tag; the workflow checks out
   the tag and runs the same check:

   ```
   gh workflow run release-modules.yml -R bide-ai/bide --ref main -f tag=govern/vX.Y.Z
   ```

### The Homebrew tap by hand

Without `HOMEBREW_TAP_TOKEN`, the Release run attaches the formula to the release and leaves the
tap alone (its log ends with a notice saying so). Update the tap from a clone of
bide-ai/homebrew-tap:

```
gh release download vX.Y.Z -R bide-ai/bide -p bide.rb -O Formula/bide.rb
git rm -q --ignore-unmatch Formula/bide-audit.rb          # the first time only: bide replaces it
printf '{\n  "bide-audit": "bide"\n}\n' > formula_renames.json   # brew upgrade moves bide-audit users
git add Formula/bide.rb formula_renames.json
git commit -s -m "bide vX.Y.Z" && git push
brew update && brew install bide-ai/tap/bide && brew test bide   # check it
```

The formula pins each archive's sha256 from the release's `checksums.txt`
(`scripts/brew-formula.sh`), so it needs no edits.

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
invariant (`spec/tla/claims/ClaimsInductive.tla`) that proves, for two drivers over two processes, `AtMostOnce` and `NotStartedExclusive` on one call (attempts 0..3, 8 claim ids) and all four of `AtMostOnce`, `NotStartedExclusive`, `NoLiveOverride` and `AtMostOncePerIntent` with halt resolution and the caller's second call (attempts 0..3, 6 claim ids), without the approval gate, and, under the lease check, assuming no plain run holds the live attempt at the check (`PlainRunIdleAtCheck`), at any depth and for any number and mix of faults within the run's 8 (6) claim ids and attempts 0..3 (claim ids are never reused, so this bounds the number of claims).
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
- A changelog fragment is in `changes/unreleased/`, or the change is not user-facing (see [Changelog](#changelog)).
- New docs are linked from the [docs index](docs/README.md) and honor the style above.
