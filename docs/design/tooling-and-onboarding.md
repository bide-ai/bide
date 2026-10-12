# Design: one `bide` CLI and the onboarding path

Status: accepted (2026-10-12); being implemented as the PRs in section 7. Target: v0.14.0.

bide has no users yet, so this note asks how the tooling and onboarding should have looked from
the start, and plans the move there in one release without compatibility shims.

## Decisions

The maintainer decided, after review of this note:

1. **The core becomes stdlib-only.** The core module drops `golang.org/x/sync` (its one use, an
   errgroup, is replaced by a small stdlib equivalent) and `golang.org/x/text`, and the
   architecture guard becomes an allowlist: the standard library and bide's own packages only.
   Removing `x/text` needs a further decision, because the standard library has no Unicode
   normalization or full case folding to replace `approverFoldKey`; it is pending, and the claim
   and the guard change only once it is settled.
2. **The CLI is built on the standard library `flag` package**, not cobra (section 2, "CLI
   framework").
3. **The `bide_audit_only` build tag and its guard test** are in the plan (section 3).
4. **The reproducible-release fixes** are in the plan: `-trimpath`, `mod_timestamp`, a pinned Go
   toolchain, and the rebuild-and-compare check (section 3).
5. **Every open-question recommendation in section 8 is accepted**, including keeping the YAML
   dependency (question 9).

## The problem

A newcomer meets the library (`go get github.com/bide-ai/bide/agent`) and three tools, each
installed a different way and explained on a different page:

| Tool | Where it lives today | How it is installed | Documented in |
|---|---|---|---|
| `bide-audit` | `cmd/bide-audit`, package `main` in the **root** module | `brew install bide-ai/tap/bide-audit` (hand-maintained formula, binaries from goreleaser) | README, `docs/guides/audit.md`, the launch post |
| `bide-view` | `cmd/bide-view`, its own module (needs both store drivers); the logic is `internal/runview` in the root module | `go install github.com/bide-ai/bide/cmd/bide-view@v0.13.0` (tag `cmd/bide-view/v0.13.0`) | README, `docs/guides/run-viewer.md` |
| `bide-demo` | `cmd/bide-demo`, its own module (needs `store/sqlite`) | `go run github.com/bide-ai/bide/cmd/bide-demo@latest` (never tagged: the line does not resolve today) | README hero, `docs/getting-started.md`, `docs/guides/run-viewer.md`, ROADMAP |

Three names, three install mechanisms, and the one a non-Go user can install (Homebrew) is the
one that cannot show them a run.

## 1. Principles

1. **One name everywhere: `bide`.** The binary, the formula, the module path's last element, the
   usage text and every doc line.
2. **One install per audience.** Builders `go get` the library. Everyone else (an evaluator, an
   auditor, a reviewer reading a flow in CI) installs one CLI, by Homebrew or `go install`.
3. **The path mirrors the product: see, build, inspect, prove.** `bide demo`, `bide init`,
   `bide view` / `bide check`, `bide audit`.
4. **Tooling never weighs down the core.** The CLI is its own module; the core module's
   dependency set does not grow.

**Correction to principle 4 as stated in the brief.** The core is not stdlib-only: the root
`go.mod` requires `golang.org/x/sync` (`agent/loop.go`, errgroup) and `golang.org/x/text`
(`agent/approval.go`, approver-id normalization), and `docs/reference/module-structure.md` says so.
The accurate principle today is "the core module requires only the standard library, `x/sync`
and `x/text`"; decision 1 removes both so that the stdlib-only claim becomes true. The guard enforcing it is also weaker than "enforced" suggests:
`agent/architecture_test.go` and `plan/architecture_test.go` are **denylists** (no
`bide-ai/bide/store/`, no `modernc.org/sqlite`, no `jackc/pgx`, ...), and
`TestCoreModuleHasNoGSM` checks one name. A new dependency that is not on a list passes. This note
proposes an allowlist guard (section 3) and uses it for both the core module and the audit command.

## 2. One CLI module, `cmd/bide`

### Shape

`cmd/bide` is a nested module, `github.com/bide-ai/bide/cmd/bide`, published and tagged like
`cmd/bide-view` is today. It has to be a nested module: `bide view` and `bide demo` need the
SQLite and Postgres drivers, which the root module must not carry.

The logic stays where the viewer's already is, in the **root** module's `internal/` tree, so the
core module's tests and guards cover it and the command module is a thin dispatcher:

```
internal/auditcli   today's cmd/bide-audit package, moved and exported as Main(args, stdout, stderr) int
internal/runview    unchanged: Main(args, stdout, stderr, Stores) int
internal/democli    today's cmd/bide-demo logic; the journal store is injected (like runview.Stores)
internal/initcli    the scaffolder and its embedded template (embed.FS), no new dependencies
cmd/bide/main.go    dispatch on os.Args[1]; wires store/sqlite, store/postgres and the YAML decoder
```

Go allows `github.com/bide-ai/bide/cmd/bide` to import `github.com/bide-ai/bide/internal/...`
(the importer is inside the tree rooted at the parent of `internal`), which is how
`cmd/bide-view` imports `internal/runview` today.

### Command and flag map

Every existing flag and verb has a place; none is dropped and none changes meaning.

| Today | New | Notes |
|---|---|---|
| `bide-demo` | `bide demo` | |
| `bide-demo -no-browser` / `-fast` / `-addr` / `-keep` | `bide demo -no-browser` / `-fast` / `-addr` / `-keep` | `-keep` prints the `bide view ...` command that reopens the run |
| (child process: re-execs itself with `BIDE_DEMO_CHILD` set and no arguments) | re-execs `bide demo` with `BIDE_DEMO_CHILD` set | The child must be reached through the `demo` subcommand, not a no-argument `bide`, so a stray `BIDE_DEMO_CHILD` cannot turn every invocation into a demo child. `BIDE_DEMO_RECORD` is unchanged. |
| `bide-view -db` / `-postgres` / `-journal` (repeatable) / `-run` / `-addr` | `bide view -db` / `-postgres` / `-journal` / `-run` / `-addr` | |
| `bide-view -topology` / `-digest` | `bide view -topology` / `-digest` | |
| `bide-view -sth` / `-evidence` / `-pubkey` / `-approver-keys` / `-need` / `-approvers` | `bide view` with the same flags | |
| `bide-view -config flow.yaml` (serve the graph) | `bide view -config flow.yaml` | Stays on `view`: it serves a page |
| `bide-view -config flow.yaml -check` | `bide check flow.yaml` | Positional file; several files allowed; output stays `file:node: message`, exit 1 on an error |
| `bide-view -config ... -check -strict` | `bide check -strict flow.yaml` | Exit 1 on a warning too |
| (none) | `bide check` evaluates `assert:` blocks | **Not built yet**: see "Assertions do not exist yet" below |
| `bide-audit -version [-json]` | `bide version [-json]` and `bide audit -version [-json]` | `bide version` prints the CLI version and every artifact format `audit` reads; the `audit -version` form stays so the audit surface is unchanged |
| `bide-audit [-json] <verb> [flags]` | `bide audit [-json] <verb> [flags]` | Top-level `-json` stays before the verb |
| `prove`, `verify`, `verify-governance`, `verify-governed-action`, `verify-convergence`, `verify-quorum`, `verify-run`, `verify-evidence`, `verify-approvals`, `prove-absent`, `verify-absent` | `bide audit <same verb>` | All verb flags unchanged, including `-max-input-bytes`, `-max-clock-skew`, `-json`, `-checker` |
| `bide-audit` exit statuses 0 to 4 | `bide audit` exit statuses 0 to 4 | Unchanged: only 0 means verified |
| (none) | `bide init <dir>` | New, below |
| (none) | `bide help [command]`, `bide <command> -h` | |

### CLI framework: the standard library `flag` package (decided)

Maintainer decision: the CLI is built on the standard library `flag` package, one `FlagSet` per
command and a small dispatch table, not cobra or another framework. The reasons:

- **No framework dependency.** Parsing, help and dispatch add nothing to the module graph, which
  keeps the audit trust argument (section 3) about bide's code and the store drivers, not a CLI
  library as well. Auditors run `bide audit verify`; every package in that binary is one more
  thing to trust.
- **The command set is small.** Six commands (`init`, `demo`, `view`, `check`, `audit`,
  `version`) plus `help`, and `audit`'s eleven verbs, which already dispatch through a map
  (`verbs` in `cmd/bide-audit/exit.go`). A framework's nesting, aliases and plugins buy nothing here.
- **Flag syntax stays the same.** `bide-audit` and `bide-view` already use single-dash Go flags
  (`-db`, `-run`, `-evidence`, `-pubkey`), so every command line in the docs keeps its flags and
  only the program name changes. cobra's POSIX-style flags would have changed all of them.

The dispatcher, in outline:

```go
// command is one bide subcommand: its name, one-line summary, and the function that parses its
// own FlagSet and returns the exit status.
type command struct {
	name, summary string
	flags         func() *flag.FlagSet // the command's flags, for help and the reference generator
	run           func(args []string, stdout, stderr io.Writer) int
}

var commands = []command{ /* init, demo, view, check, audit, version */ }
```

`audit` keeps its own second-level table: `bide audit [-json] <verb> [flags]` is today's
`bide-audit` command line with the program name changed, parsed by the moved code unchanged.

**Help and usage errors, the same everywhere.**

- `bide`, `bide help` and `bide -h` print the command list (name and summary) and exit 0.
- `bide help <command>` and `bide <command> -h` print that command's usage line, summary and
  flags (`FlagSet.PrintDefaults`), exit 0; `bide help audit <verb>` does the same for a verb.
- A usage error (an unknown command, an unknown flag, a missing required flag, a stray argument)
  prints `bide <command>: <message>` and the command's usage line to stderr and exits 2. Every
  message starts with `bide <command>:` (`bide audit:` and `bide view:` replace `bide-audit:` and
  `bide-view:`; the ten mentions of `bide-audit` in `cmd/bide-audit/*_test.go` move with it).
- One deliberate exception stays: `bide audit <verb> -h` exits 2, as `bide-audit <verb> -h` does
  today, because `audit` reserves 0 for "verified" and a help request verifies nothing (see `parse`
  in `cmd/bide-audit/exit.go`). `bide help audit <verb>` is the help path that exits 0.

**Exit-status conventions.**

| Status | Meaning for every command | `bide audit` (unchanged) |
|---|---|---|
| 0 | success | verified (for `prove`, `prove-absent`: the artifact was written) |
| 1 | failure: an input cannot be read, the server fails, or `check` found an error | read and understood, and not verified |
| 2 | usage error; nothing was read | usage error; nothing was read |
| 3 | (not used) | no verdict: the `-checker` gave none, or an internal error |
| 4 | (not used) | an input is unreadable or unusable |

`bide view`, `bide check`, `bide init` and `bide demo` use 0, 1 and 2 (the viewer's statuses
today). `bide audit` keeps its five-status contract, which `docs/guides/audit.md` publishes and
scripts depend on; folding it into 0/1/2 would turn "not verified" and "unreadable" into one
status. The reference page states each command's table.

**A generated reference page.** `docs/reference/cli.md` is generated, not written: a small
generator (`go run ./cmd/bide/internal/gendoc` or a `bide` test helper) walks `commands`, calls
each `flags()` and, for `audit`, each verb's `FlagSet`, and writes every command, verb, flag
(name, type, default, usage string) and exit table. CI regenerates it and fails on a diff (the same
pattern as the module and shard checks), so a new flag cannot ship undocumented and a removed one
cannot linger. The long prose in today's usage texts (`cmd/bide-audit/main.go`,
`internal/runview/cli.go`) moves into each command's summary and the page's hand-written intro,
which the generator includes verbatim.

**Shell completion is deferred.** cobra would have generated it; `flag` does not. It is not needed
for v0.14.0 and can be written by hand later (a short bash/zsh script over the command and flag
names, or generated by the same walker) if users ask.

**The CLI module's dependencies.** `cmd/bide`'s `go.mod` requires bide's own modules and the
libraries `bide view` and `bide demo` need, and nothing for the CLI itself. The full list, which is
the union of today's `cmd/bide-view` and `cmd/bide-demo` `go.mod` files:

| Requirement | Why |
|---|---|
| `github.com/bide-ai/bide` | the core; `internal/auditcli`, `internal/runview`, `internal/democli`, `internal/initcli` |
| `github.com/bide-ai/bide/store/sqlite` | `bide view -db`, `bide demo` |
| `github.com/bide-ai/bide/store/postgres` | `bide view -postgres` |
| `go.yaml.in/yaml/v3` | `bide view -config` and `bide check` on `.yaml`/`.yml` flow configs |
| indirect, via the core: `golang.org/x/sync`, `golang.org/x/text` | |
| indirect, via `store/sqlite`: `modernc.org/sqlite`, `modernc.org/libc`, `modernc.org/mathutil`, `modernc.org/memory`, `github.com/dustin/go-humanize`, `github.com/google/uuid`, `github.com/mattn/go-isatty`, `github.com/ncruces/go-strftime`, `github.com/remyoudompheng/bigfft`, `golang.org/x/sys` | |
| indirect, via `store/postgres`: `github.com/jackc/pgx/v5`, `github.com/jackc/pgpassfile`, `github.com/jackc/pgservicefile`, `github.com/jackc/puddle/v2` | |

**This does not match "only bide's modules plus the store drivers" in one place:** YAML. The
viewer decodes YAML flow configs with `go.yaml.in/yaml/v3`, injected from `cmd/bide-view` because
`plan` reads JSON only. Keeping YAML support keeps this one requirement; dropping it means
`bide view -config` and `bide check` read JSON only, which loses a capability every flow config in
the docs and `examples/plan/flows` uses. See open question 9.

A test in `cmd/bide` pins the direct requirement set (`go mod edit -json`, `.Require` without
`// indirect`) to exactly the four rows above, so a framework or other library cannot creep in
unreviewed. That `bide audit` compiles from core-only packages is shown in section 3.

### `bide init <dir>`

Scaffolds a project that builds, runs with no API key and no network, and shows each part of the
product once. It refuses a non-empty `<dir>` (no `-force` in the first version).

```
<dir>/
  go.mod          module <name from dir>; requires github.com/bide-ai/bide and
                  github.com/bide-ai/bide/store/sqlite at the CLI's own version, go.yaml.in/yaml/v3
  main.go         the agent and the flow, about 120 lines
  flow.yaml       the flow config
  main_test.go    one test: run, crash-free resume, the gate, the assertion
  README.md       what you have and next steps
  .gitignore      *.db, evidence.json, keys/
```

`main.go`:

- **Model**: `agenttest`'s scripted model, with a comment block showing the two-line swap to a real
  adapter (`model/anthropic` or the OpenAI-compatible one) and the environment variable it reads.
- **A side-effect tool**: `send_refund`, default Safety (a side effect, so at most once and a halt on
  an ambiguous crash), writing to a local file that stands in for a payment provider.
- **A 2-of-3 approval gate** on that tool (`agent.ApprovalPolicy{Need: 2, Approvers: ops, finance,
  risk}`), with per-approver Ed25519 keys generated on first run into `keys/` and two approvals
  signed by the program, as `cmd/bide-demo` does.
- **A SQLite journal** (`store/sqlite`) in `run.db`, wrapped in the audited store so `bide audit`
  and `bide view` have a signed tree head and an evidence package to read.
- **The flow**: `flow.yaml` loaded with `plan.Load` through a `plan.Registry` holding the tool and
  a model node. YAML is decoded with `go.yaml.in/yaml/v3` exactly as `examples/plan` and
  `cmd/bide-view` do (`plan.Load` reads JSON). The gate is declared in the config's `approval`
  block on the refund node (landed in #195).

`flow.yaml` carries `version: 1`, the nodes, the `approval` block and an `assert:` block such as
"every path to `send_refund` passes the approval gate".

`README.md` lists next steps in product order: `go run .`, `bide view -db run.db`,
`bide check flow.yaml`, `bide audit verify-evidence -evidence evidence.json -pubkey $(cat keys/log.pub)`,
then "swap in a real model" and links to the guides.

**Version pinning.** The template's `go.mod` names the CLI's own release version, read from
`runtime/debug.ReadBuildInfo` (or the `-X main.version` stamp). A development build (`(devel)`)
writes `v0.0.0` plus `replace` directives only when `-dev <path to a bide clone>` is passed, and
otherwise refuses with a message, so a scaffold never names a version that does not exist.

**How the template stays tested.**

1. `internal/initcli` test (core module, every CI run): scaffold into `t.TempDir()`, `go mod edit
   -replace` both bide modules to the checkout, `GOFLAGS=-mod=mod go vet ./... && go test ./...`
   in the scaffold. The scaffold's own `main_test.go` runs the agent, kills nothing (the crash path
   is the demo's job) and asserts the gate paused, two approvals resumed it, the tool ran once and
   the evidence package verifies. The test is skipped when the `go` command is absent, as the
   architecture guards are.
2. The same test runs `bide check flow.yaml` on the scaffold (through `runview.Check`), so a
   template config that drifts from the schema fails CI.
3. `scripts/release.sh` stage 3 (the consumer check) gains one step: run the released
   `cmd/bide@vX.Y.Z` as `bide init`, then build and test the scaffold with `GOWORK=off` against the
   proxy, so the published template resolves against published modules.
4. The docs CI that checks README snippets covers the README's `bide init` lines.

### Assertions do not exist yet

`plan` has no `assert:` block today; ROADMAP lists "policy assertions in the config (`assert:`),
decided on the graph" under Next. The brief's `bide check` "plus assertions" and the template's
assert block therefore depend on a feature that is not built. v0.14.0 can ship the CLI either way:

- If assertions land first, `bide check` evaluates them and the template uses one.
- If not, `bide check` ships with `-strict` only, and the template omits `assert:` (the config
  reader refuses unknown keys, so an aspirational block would break the scaffold).

Recommendation in open question 1.

## 3. No separate verifier binary

`bide audit` is the verifier. There is no `bide-verify`. The argument has two parts, and it holds
only partly as the brief states it.

### What holds: the audit code is core-only, and a test keeps it so

`cmd/bide-audit` today imports `agent` and `audit` and the standard library; `go list -deps`
shows nothing else beyond `x/sync` and `x/text` (checked on main at `8dde59ad`). Moved to
`internal/auditcli` in the root module, it cannot gain a driver without the root `go.mod`
gaining one. The guard turns that into a test. Sketch:

```go
// TestAuditCLIDepsAreCore: the audit command's import graph is the standard library, packages of
// the core module, and the core module's two requirements. An allowlist, not a denylist.
func TestAuditCLIDepsAreCore(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f",
		"{{if not .Standard}}{{.ImportPath}}{{end}}", "github.com/bide-ai/bide/internal/auditcli").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, p := range strings.Fields(string(out)) {
		switch {
		case p == "github.com/bide-ai/bide" || strings.HasPrefix(p, "github.com/bide-ai/bide/"):
			if mod := moduleOf(t, p); mod != "github.com/bide-ai/bide" {
				t.Errorf("%s is in module %s, not the core module", p, mod)
			}
		case strings.HasPrefix(p, "golang.org/x/sync/"), strings.HasPrefix(p, "golang.org/x/text/"):
		default:
			t.Errorf("internal/auditcli depends on %s", p)
		}
	}
}
```

The same allowlist test, pointed at `github.com/bide-ai/bide/agent` and `.../plan`, replaces the
string denylists in `agent/architecture_test.go` and `plan/architecture_test.go`; a third test
pins the root `go.mod` require set to exactly `x/sync` and `x/text`.

### What does not hold: the shipped binary is not core-only

The `bide` binary also links `bide view` and `bide demo`, so it contains `jackc/pgx` and its
dependencies, `modernc.org/sqlite` and `modernc.org/libc`, `go.yaml.in/yaml/v3`, `google/uuid`,
`dustin/go-humanize`, `mattn/go-isatty` and others (the `cmd/bide-view` dependency list). A
package-level guard on `internal/auditcli` says nothing about them, because **every linked
package's `init` runs in every invocation**, including `bide audit verify`. Concretely: a
compromised dependency whose `init` calls `os.Exit(0)` makes every `bide audit` verb exit 0, and
0 is the status that means "verified". Today's Homebrew `bide-audit` binary does not have this
exposure; folding the verifier into the full CLI widens its trusted base from the core to the
core plus the store drivers' trees.

Mitigation, without a second product:

- **A build tag that drops the non-core commands.** The files of `cmd/bide` that import a store or
  YAML carry `//go:build !bide_audit_only`. `go build -tags bide_audit_only
  github.com/bide-ai/bide/cmd/bide@vX.Y.Z` (or `go install` with the tag) yields a `bide` whose
  only working subcommands are `audit` and `version`, and the allowlist test above runs against
  `go list -deps -tags bide_audit_only ./cmd/bide` as well. Same name, same module, same release;
  the security model page tells a high-assurance verifier to build it this way.
- **The library path already exists.** `audit/verify` is a stdlib-only verifier package
  (`docs/guides/audit.md`, `docs/guides/security-model.md`); a party who will not run our binary
  at all can verify inclusion with it.

### Reproducible releases

The trust argument for a downloaded binary is: anyone can rebuild it from the tag and get the same
bytes, so trusting the binary reduces to trusting the source at the tag plus the Go toolchain.
Today's release config does not deliver that, and the note proposes the changes:

| Gap today | Fix |
|---|---|
| `.goreleaser.yaml` builds without `-trimpath`, so the runner's paths are in the binary | `flags: [-trimpath]` |
| No `mod_timestamp`, so archive entries carry build-time mtimes | `mod_timestamp: "{{ .CommitTimestamp }}"` on builds and archives |
| `release.yml` installs Go `1.27` (floating patch), so a rebuild months later uses another compiler | `go-version-file: go.mod` with the exact `toolchain` line, and the nested `cmd/bide/go.mod` gains the same `toolchain` line |
| Nothing ever rebuilds and compares | A `repro` job after the release, on a different runner OS (macOS), rebuilds the six targets from the tag and diffs against `checksums.txt`; `scripts/repro.sh vX.Y.Z` does the same for anyone |

What gets built: goreleaser runs on the **root** tag `vX.Y.Z` (as now), in `cmd/bide` with
`GOWORK=off`, so the `replace` directives point at the tagged tree: the binary is built from
exactly the source at the root tag. `go install github.com/bide-ai/bide/cmd/bide@vX.Y.Z` builds
the same source through the release commit release.sh makes, but its embedded build info names
module versions instead of `(devel)` replacements, so it is **not** byte-identical to the release
archive. The reproducibility claim is therefore stated for the documented rebuild recipe
(`scripts/repro.sh`), not for `go install`. Making `go install` reproduce the archive would mean
building the archive from the nested tag's commit; goreleaser's open-source edition does not take
`cmd/bide/vX.Y.Z` as a version (monorepo tag prefixes are a Pro feature). See open question 4.

### Residual risk, stated

- The Go toolchain and the GitHub runner are trusted; reproducibility moves trust from our runner
  to the toolchain, it does not remove it.
- Linked third-party `init` code in the default build (above); closed by the `bide_audit_only`
  build, not by the default binary.
- The Homebrew tap is a separate repository; a compromised tap can point at another archive. The
  formula's sha256 comes from `checksums.txt`, and the release notes print the checksums, so a
  user can compare.
- Reproducibility helps only if someone rebuilds. The `repro` job is that someone, on every
  release.

## 4. Naming: package `plan`, feature "plan flows"

The package stays `plan`. The feature is "plan flows" (a "flow" in running text once the context
is set; "flow config" for the YAML/JSON file, which is a plan flow's config). The pages that use
another name for the feature, from a grep on main:

| File | Lines | Says |
|---|---|---|
| `docs/guides/flows.md` | 1, 3, 301 | "Flows (the `plan` builder)", "typed flow builder", "declared flow" |
| `docs/CONCEPTS.md` | 53 | "typed flow" |
| `docs/README.md` | 20 | "Flows", "typed flow" |
| `README.md` | 199, 289, 941, 998 | "flow graph", "flow config", "typed flow" |
| `docs/guides/run-viewer.md` | 78, 241, 243 | "flow config", "flow definition" |
| `docs/ROADMAP.md` | 87 | "flow definition" |
| `docs/i18n/README.{ar,hi,ru,zh-CN}.md` | the translations of the README lines above (ru: 616) | "typed flow" and translations |
| `docs/design/api-v1.md`, `docs/design/design-mofn-approval.md` | throughout | mix "plan flow" and bare "flow" (design records; leave as written) |
| `internal/runview/cli.go` usage text | `-config` help | "a flow config" |

"Workflow" in `README.md` (97, 152), `docs/formal-verification.md` (211 to 215),
`docs/testing/verification.md` (156 to 181), `docs/guides/durable-steps.md` (112) and
`docs/reference/module-structure.md` (75) refers to workflow engines in comparisons, not to the
feature, and stays.

Proposed: rename `docs/guides/flows.md` to `docs/guides/plan-flows.md` with the title "Plan flows",
and use "plan flow" at first mention on each page.

## 5. Homebrew ships the full CLI

**The SQLite driver is pure Go and cross-compiles.** `store/sqlite` and `govern/sqlitelog` require
`modernc.org/sqlite v1.59.0` (C translated to Go, no cgo). Checked: `cmd/bide-view` (SQLite,
Postgres via `pgx`, YAML) builds with `CGO_ENABLED=0 GOWORK=off go build -trimpath` for
linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64 and windows/arm64, the six
targets `.goreleaser.yaml` builds. No fix is needed. `modernc.org/libc` supports a fixed list of
targets; adding one later (say linux/riscv64) needs the same check.

**The formula.** `bide-ai/homebrew-tap` holds one hand-maintained formula, `Formula/bide-audit.rb`
(v0.13.0, four archives: darwin and linux, amd64 and arm64). The `.goreleaser.yaml` comment
explains why: goreleaser deprecated `brews` (formulae) for `homebrew_casks`, and casks would drop
Linux. Also, `release.yml` passes `HOMEBREW_TAP_TOKEN` and its comment mentions `skip_upload`, but
`.goreleaser.yaml` has no Homebrew section: the comment is stale.

Plan:

- Add `Formula/bide.rb` (binary `bide`, same four archives, named `bide_<version>_<os>_<arch>`).
- Generate it instead of editing it: `scripts/brew-formula.sh vX.Y.Z` renders the formula from the
  release's `checksums.txt`, with a `--self-test`; `release.yml` runs it after goreleaser and pushes
  to the tap with `HOMEBREW_TAP_TOKEN`. This keeps formulae (and Linux) without depending on the
  deprecated `brews` key.
- Remove `Formula/bide-audit.rb` and add `formula_renames.json` with `{"bide-audit": "bide"}`, so
  `brew upgrade` moves anyone who followed the launch post. A rename table is Homebrew metadata,
  not a code shim.
- The formula's `test do` runs `bide version` and `bide audit -version` and checks the exit status,
  instead of grepping usage text.

## 6. Docs information architecture

### The README becomes the pitch

1. One-paragraph pitch (who it is for, what it guarantees), the hero GIF.
2. **Install**: `brew install bide-ai/tap/bide` or `go install github.com/bide-ai/bide/cmd/bide@latest`;
   for builders, `go get github.com/bide-ai/bide/agent`.
3. **Start in four steps**:
   `bide demo` (see it), `bide init refund && cd refund && go run .` (build),
   `bide view -db run.db` (inspect), `bide audit verify-evidence ...` (prove). The brief lists
   `bide audit verify`; the scaffold produces an evidence package, so `verify-evidence` is the verb
   that works on it without first running `prove`.
4. Links: Start here, Concepts, Guides, Reference, Proofs.

The README today is about 1,030 lines (the four guarantees, graphs, ambient runs, quickstart,
streaming, typed output, sampling, caching, sessions, auditability, RAG, resume safety, HITL,
errors, middleware, modules, architecture, federated governance, guides). Those sections move to
the pages below; the README keeps the four guarantees as a short list with links. The i18n
READMEs follow the new README.

### The docs map

`docs/README.md` already groups pages into Start here, Guides, Reference, Design notes, Testing.
The new map, with every existing page placed:

| Section | Pages |
|---|---|
| **Start here** | `getting-started.md` (rewritten around install, `bide demo`, `bide init`), `CONCEPTS.md`, `GUARANTEE.md`, `KNOWN-LIMITATIONS.md` |
| **Concepts** | `CONCEPTS.md` (the vocabulary), `CONTEXT.md` (today unlisted in the map; place it here or fold it into CONCEPTS), `design/design.md` (architecture overview) |
| **Guides: build** | `guides/reliability.md`, `guides/durable-steps.md`, `guides/plan-flows.md` (renamed from `flows.md`), `guides/signals.md`, `guides/models.md`, `guides/mcp.md`, `guides/messaging.md`, `guides/rag-memory.md`, `guides/observability.md`, `guides/debugging.md` |
| **Guides: inspect** | `guides/run-viewer.md` (now `bide view` and `bide check`) |
| **Guides: prove and govern** | `guides/audit.md`, `guides/approval.md`, `guides/hitl-approval.md`, `guides/quorum.md`, `guides/delegation.md`, `guides/governance.md`, `guides/security-model.md` |
| **Reference** | **new** `reference/cli.md` (every command, flag and exit status), **new** `reference/flow-config.md` (the config schema: `version`, nodes, edges, `approval`, `assert` when it exists), `reference/extension-points.md`, `reference/module-structure.md`, the API (pkg.go.dev links per package) |
| **Proofs** | `formal-verification.md`, `testing/verification.md`, `testing/testing.md`, `../spec/tla/README.md`, `design/formal-models.md` |
| **Project** | `ROADMAP.md`, `../CHANGELOG.md`, `releases/*`, `blog/index.md`, `blog/introducing-bide.md`, `../CONTRIBUTING.md`, `../SECURITY.md` |
| **Design notes** | `design/api-v1.md`, `design/chaos-benchmark.md`, `design/compaction.md`, `design/design-durable-signals.md`, `design/design-mofn-approval.md`, `design/probes.md`, `design/protocol.md`, this note |
| **Internal** | `IMPL/complete/IMPL-mofn-approval.yaml` (planning record; not in the map) |
| **Translations** | `i18n/README.{ar,hi,ru,zh-CN}.md` |

`guides/approval.md` and `guides/hitl-approval.md` overlap by title; check whether one should
fold into the other while moving them (open question 6).

`reference/cli.md` is generated from the commands' `FlagSet`s and checked in CI (section 2, "CLI
framework"), so the reference cannot drift.

## 7. Migration plan

Ordered PRs, all merged before the v0.14.0 release PR. Each keeps main green.

1. **Guards first.** The allowlist dependency tests for the core module, `agent` and `plan`
   (section 3); no behaviour change. Lands alone so a later move that breaks it fails loudly.
2. **Move the logic into the root `internal/` tree.** `cmd/bide-audit` to `internal/auditcli`
   (exported `Main`), `cmd/bide-demo` logic to `internal/democli` with the store injected. The old
   `main` packages become three-line wrappers for one PR so CI proves the move changed nothing.
3. **Add `cmd/bide`.** New module: `go.mod` (requires root, `store/sqlite`, `store/postgres`,
   `go.yaml.in/yaml/v3`; `replace` directives; `toolchain go1.27.0`), dispatcher, the
   `bide_audit_only` build tag and its guard test, the requirement-set test, `bide version`,
   `bide help`, `bide check`, and the `docs/reference/cli.md` generator with its CI diff check. Then:
   - `go.work`: add `./cmd/bide`.
   - `.github/workflows/ci.yml`: `MODULES` and the `rest` shard add `cmd/bide`; the Postgres loop
     at line 360 adds `cmd/bide`. `.github/scripts/check-modules.sh` and `shards.sh check-modules`
     fail until both are updated, which is the point.
4. **`bide init`.** `internal/initcli`, the template, its test (section 2), the scaffold step in
   `scripts/release.sh` stage 3.
5. **Remove the old commands.** Delete `cmd/bide-audit`, `cmd/bide-demo`; reduce `cmd/bide-view`
   to the deprecation stub below. Update:
   - `scripts/release.sh`: `PUBLISHED` drops `cmd/bide-demo`, adds `cmd/bide`; `COMMANDS="cmd/bide
     cmd/bide-view"` for v0.14.0 only; the header comment's stage description; the self-test lines
     that name `cmd/bide-view` and `cmd/bide-demo` (`in_list ... $(libraries)`) name `cmd/bide`.
   - `.goreleaser.yaml`: build `id: bide`, `dir: cmd/bide`, `binary: bide`, `-trimpath`,
     `mod_timestamp`, archives `bide_{{ .Version }}_{{ .Os }}_{{ .Arch }}`, the header comment.
   - `.github/workflows/release.yml`: exact Go version, the formula step, the `repro` job, the
     header comment ("builds the bide-audit binaries").
   - `.github/workflows/ci.yml`: `MODULES` and shards drop `cmd/bide-view` (if fully removed) and
     `cmd/bide-demo`.
   - `go.work`: drop `./cmd/bide-demo` (and `./cmd/bide-view` once removed).
   - `internal/runview/architecture_test.go` comment and usage text (`bide-view` to `bide view`).
   - `assets/demo/record.sh` (builds `cmd/bide`, calls `bide demo` and `bide view`) and
     `assets/demo/crash-demo.tape` (the `go()` alias and the typed line).
6. **Docs.** Every install and command line, then the README restructure:
   - Install lines: `README.md` (19, 136, 635, 776, 974, 1011, 1016), `docs/getting-started.md`
     (11 and the install section), `docs/guides/run-viewer.md` (22, 33, 54, 76, 77, 122, and every
     `bide-view` example), `docs/guides/audit.md` (401, 448, 496, 616, 639, 681, 748, 749, 825 and
     the rest of its `bide-audit` lines), `docs/ROADMAP.md` (11), `docs/blog/introducing-bide.md`
     (141, 151, 196: a post records what was true when published; add a dated note pointing to
     `bide` rather than rewriting it), `docs/i18n/README.*.md` (the same lines, e.g. ru 19, 76,
     411, 412, 532, 634, 663), `examples/approval/main.go` (23, 31, 32) and
     `examples/approval/flow/main.go` (20, 26) doc comments.
   - The README to the pitch shape (section 6), the i18n READMEs in the same PR or a follow-up
     the release waits for.
   - `docs/README.md` to the new map; `reference/cli.md`, `reference/flow-config.md`; the
     `flows.md` rename and the naming fixes in section 4; `docs/reference/module-structure.md`
     (the module table gains `cmd/bide`).
   - The LinkedIn/launch post install lines (the post is drafted outside this repository): the
     single line becomes `brew install bide-ai/tap/bide` then `bide demo`.
7. **Hero GIF re-record.** It types the command, so it waits for the final spelling:
   `crash-demo.tape` types `bide demo` (the viewer half already comes from `record.sh`). Re-record
   after PR 5 and before the release PR.
8. **Release PR v0.14.0** (CHANGELOG with the moves marked breaking, `docs/releases/v0.14.0.md`),
   then `scripts/release.sh v0.14.0 --push` on the maintainer's go, then the tap update.

### Old module paths and tags

Published tags cannot be deleted from the module proxy, and none should be.

- `github.com/bide-ai/bide/cmd/bide-audit` was a package of the root module. At v0.13.0 and
  earlier it stays installable by version. At v0.14.0 the package is gone, so
  `go install .../cmd/bide-audit@latest` fails with "module does not contain package": a clear
  failure, nothing to do.
- `github.com/bide-ai/bide/cmd/bide-demo` was never tagged. Delete it.
- `github.com/bide-ai/bide/cmd/bide-view` has one tag, `cmd/bide-view/v0.13.0`, so `@latest` would
  keep resolving to v0.13.0 forever after the directory is deleted. Plan: v0.14.0 tags it one last
  time with a `// Deprecated: use github.com/bide-ai/bide/cmd/bide (bide view).` comment on its
  `module` line, which `go get` and `go list -m -u` print, and a `main` that prints the replacement
  command and exits 2. It runs nothing, so it is a signpost rather than a compatibility shim. The
  directory, its `PUBLISHED`/`COMMANDS` entries, `go.work` and CI lines are deleted in the first
  PR after v0.14.0. See open question 3.

## 8. Open questions (all decided: each recommendation was accepted)

1. **Assertions before or after v0.14.0?** `bide check` "plus assertions" and the template's
   `assert:` block need a plan feature that does not exist yet. *Recommendation:* do not hold the
   CLI for it. Ship v0.14.0 with `bide check -strict`, a template without `assert:`, and add both in
   the release that adds assertions (the template test then enforces the block).
2. **Ship the `bide_audit_only` build as a release artifact?** *Recommendation:* no second artifact;
   document the one-line build in the security model and audit guide, and keep the allowlist test
   on the tagged build. Revisit if an auditor asks for a prebuilt minimal binary.
3. **Final `cmd/bide-view` signpost tag, or nothing?** *Recommendation:* the signpost (Deprecated
   comment plus a stub `main`), one release only. It is a few lines and stops `@latest` silently
   installing the v0.13.0 viewer, which the v0.13.0 release notes and README told people to install.
4. **Should the release archive equal `go install @vX.Y.Z` byte for byte?** It would need the
   archive built from the nested tag's release commit, which goreleaser's open-source edition does
   not support as a version. *Recommendation:* no for v0.14.0. State reproducibility for the
   `scripts/repro.sh` recipe and prove it with the `repro` job.
5. **Homebrew cask or formula?** *Recommendation:* formula, generated by our script, because a cask
   drops Linux. Revisit if goreleaser's formula support returns or casks gain Linux.
6. **Fold `guides/approval.md` and `guides/hitl-approval.md`?** *Recommendation:* read both during
   the docs PR; fold if they cover the same gate, keep both with distinct titles if not.
7. **`bide init` flow config in YAML or JSON?** `plan.Load` reads JSON; YAML costs the scaffold a
   `go.yaml.in/yaml/v3` requirement. *Recommendation:* YAML, matching `examples/plan` and every
   flow config in the docs; the dependency is in the user's module, not the core.
8. **`bide demo` without installing?** `go run github.com/bide-ai/bide/cmd/bide@latest demo`
   works for Go users once `cmd/bide` is tagged. *Recommendation:* the README leads with the
   install line and `bide demo`; getting-started mentions the `go run` form as the no-install path.
9. **Keep YAML in `bide view` and `bide check`?** It is the one CLI requirement that is neither a
   bide module nor a store driver (`go.yaml.in/yaml/v3`, no further dependencies). *Recommendation:*
   keep it. Every flow config in the docs and examples is YAML, and the package is not linked into
   the `bide_audit_only` build, so it does not touch the audit trust base.
