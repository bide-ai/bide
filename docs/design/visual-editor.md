# Design: a visual editor for flow configs

Status: accepted (the maintainer decided all 12 decisions as recommended on 2026-10-15). Phase 1 is built (`bide edit`, v0.19.0 and v0.20.0; see the [flow editor guide](../guides/flow-editor.md) and the phased plan below); phase 2 is built (v0.22.0: add, remove, rename and move nodes, the entry, the flow's name and types, edges, switches, arms and `else`, joins and loop bounds, undo and redo tested over every one of them, and drag to connect), phase 3 (typed mode) is built (v0.22.0: the read-only Registry description `Registry.Describe`, `plan.InspectConfigDescribed`, `plan.AllowUnusedBlocks`, the `-blocks` flag of `bide check` and `bide view -config`, and the typed editor, `bide edit -blocks` or `flowedit.WithRegistry` and `WithBlocks`: typed checks, type-refusing operations, the typed-plus-structural weakened verdict, and on the page the palette, the digest, `(types differ)` hints and the NOT CERTIFIED banner; as built, see "Phase 3 as built" below); phase 4 (compare with a run) is built (see "Phase 4 design" under the phased plan: the run as a baseline, its path on the canvas, and the list of unfinished runs a change would refuse), and the names in the unbuilt parts are a
sketch. Written against `main` at 759cf547 and, where it says so, against the open pull request
#312 (approval deadlines).

The note covers who edits and who reviews a flow, where the editor runs, how it edits a config
without damaging the file, how it presents approval gates and shows a weakened one, how edits are
saved, reviewed and signed, the security posture, accessibility, the technology, the tests and the
phases. The decisions it needs from the maintainer are at the end.

## Summary

- **A lowering front-end, and only that.** The editor reads and writes the config `plan.Load`
  reads. It adds no executor, no store and no state of its own. The run is the run `plan` already
  makes: at-most-once, halted on ambiguity, journaled, provable.
- **Local, not hosted.** A `bide edit flow.yaml` command serves a page on a loopback address, as
  `bide view` does, and writes only the files it was given. A hosted editor is not recommended.
- **The Go side is the authority.** The page holds the text and draws; every edit is an operation
  sent to Go, which applies it to the file's text, runs `plan.InspectConfig` and the same
  classifier `bide check -diff` uses, and returns the new text and the report. The browser has no
  second validator.
- **Edits are small splices of the file's text**, not a re-render of it, so comments, ordering,
  flow-style mappings and blank lines survive. Where the engine cannot splice safely it refuses and
  says so.
- **Nothing weakens silently.** The editor shows every weakening against a baseline, as a banner
  that stays until the edit is undone or accepted, and saving a weakened config needs an explicit
  confirmation. Checking this against the code found four ways a config can get weaker that
  `bide check -diff` does not report today (G1 to G4 below). Three are classifier gaps to close
  before the editor ships; the fourth is inherent to the structural check and the editor marks it.
- **Smallest useful first release:** edit a node's approval gate, safety and asserts in a
  config, see the verdict and the diff live, and save. Structure editing, a typed block palette and
  run comparison follow.

## The gate this passes

bide's rule for any authoring surface is that it may add a way to author and may not add a way to
execute, and that the runtime stays the headline. Two questions decide whether a surface is built:

1. **Does it lower to the core?** Yes. The editor's output is a flow config. `plan.Load` builds the
   same `*Flow` from it that it builds from a hand-written file, and `Run` is unchanged. There is
   no editor-only key: the config is read strictly, so a key `Load` does not know is a parse error,
   which also means the editor cannot smuggle layout or any other metadata into the file.
2. **Does the substrate stay the headline?** This is a presentation matter and the note holds
   itself to it: the editor is documented as a tool in the flow guide and the run viewer guide,
   not as a README feature, and its page leads with the gate and the verdict, the same words the
   viewer uses, not with a canvas.

The expression-surfaces note this principle comes from is kept in the go-agents repository and is
not in this one; the two questions are restated here so the note stands alone.

## Goals and non-goals

Goals:

1. Edit a flow config's structure and its accountability settings (approval gates, safety, bounded
   loops, assertions) without hand-writing YAML.
2. Show, while editing, what `bide check` and `bide view -config` would show: problems on the node
   or edge they belong to, the risk map, the assertion verdicts with counterexample paths.
3. Make a weaker flow impossible to produce without the author seeing that it is weaker.
4. Leave a hand-written file recognizable: the diff of an edit is the edit.
5. Fit a bank's change control: the editor produces a reviewable diff of a text file in git, and
   nothing else.
6. Usable by keyboard and screen reader, not only by pointer.

Non-goals:

- **Writing Go.** A config places and wires nodes; step bodies and predicates stay registered Go.
  The editor never writes or edits Go, and a predicate is chosen by name.
- **A new executor, a debugger, or a way to run a flow.** The editor does not start runs. The run
  viewer already shows runs; the editor links to it and does not duplicate it.
- **Free-form positioning.** The layout is derived (see "No layout in the file"), so there are no stored coordinates
  and no dragging a node to a place.
- **Accounts, sharing, comments, or real-time collaboration.** Git is the collaboration layer.
- **Signing configs.** "Collaboration, review and signing" says what bide does and does not offer.
- **Replacing text editing.** The JSON Schema already gives completion in an editor. Text stays a
  first-class way to author, and the editor must keep working on files written that way.
- **A hosted service**, and for now **a desktop app** (see "Where it runs").

## Users

| Who | What they do | What they need from the editor |
|---|---|---|
| Flow author (an engineer, often the same person who registered the blocks) | Creates and changes flows | Fast structure edits; type-compatible connections when the Registry is known; a file they still recognize |
| Policy owner (risk, compliance, an approver-set owner) | Changes who must approve what, deadlines, assertions; rarely touches structure | A form for the gate, not YAML; the verdict on what the change does to the flow's policy |
| Reviewer (a second engineer or a change-control board) | Reads a diff, decides to merge | Not the editor. The text diff, `bide check -diff` and a rendering of the change; the editor must make their job easier by producing small diffs |
| Auditor | Reads what a flow was, after the fact | Not the editor. The run viewer and `bide audit` |

The reviewer is the user the design is built around. An editor is judged by the diff it leaves.

## What exists, checked against the code

| Piece | Where | What the editor uses it for |
|---|---|---|
| Strict config reader, version 1, snake_case keys | `plan/config.go` (`parseConfig`, `config`, `configNode`, `configApproval`, `configWire`) | The definition of a valid document; unknown keys are errors |
| JSON Schema, Draft 2020-12 | `plan/schema/flow.schema.json`, kept equal to the structs by `plan/schema_test.go` | Editor-side shape checks in a text editor; a test oracle here |
| `plan.InspectConfig(data, reg)` | `plan/inspect.go` | The report: problems by node, edge or arm; `Topology`; `Digest` (with a Registry only); `Safety`; `Assertions`; `Gated` (every path to a node passes a gate) |
| `Topology()`, `RenderMermaid`, `TopologyDigest` | `plan/topology.go`, `plan/render.go`, `plan/digest.go` | The declared graph; the digest commits to node, edge and arm order |
| Layered layout and SVG, server-side | `internal/runview/layout.go`, `svg.go` | The canvas. Nodes already carry `data-id`, `tabindex="0"` and `aria-label` |
| Config page, risk map, assertion counterexamples | `internal/runview` (`BuildConfig`), `docs/guides/run-viewer.md` | The read-only half of the editor |
| `bide check -diff`, the gate classifier | `internal/runview/diff.go` (`BuildDiff`, `gateChange`, `accepts`), `cmd/bide/full.go` | The weakening verdict, reused as a library |
| YAML to JSON conversion | `cmd/bide/view.go` (`yamlToJSON`), `go.yaml.in/yaml/v3` in `cmd/bide/go.mod` | YAML is read in the `cmd/bide` module; the root module requires nothing |
| Loopback listener and Host check | `internal/runview/server.go` (`Listen`, `guard`) | The same posture for the editor's server |
| Approve page build | `approvals/page.go` (`//go:embed`, `PageFiles`, `PageSHA256`), `approvals/page/*.js` | Hand-written JS, embedded, no bundler |
| Browser test job | `.github/workflows/page-mtls.yml`, `approvals/page/test/chrome/package.json` (puppeteer-core, pinned by lockfile) | The model for the editor's Chrome job |

Facts that shape the design:

- **The root module requires no dependency at all** (`go.mod` has no `require`). `plan.Load` reads
  JSON; YAML is decoded by `yamlToJSON` in the `cmd/bide` module, which is also where the one YAML
  dependency lives. Whatever edits YAML therefore cannot live in the root module.
- **`bide check` and `bide view -config` are structural.** Block, predicate and merge names, the
  types and the digest are checked only with a `plan.Registry`, which a Go program supplies through
  `plan/planview`. From the command line a gate declared only in Go reads as missing
  (`docs/guides/plan-flows.md`, "Policy assertions").
- **A `plan.Registry` cannot be listed.** `plan/registry.go` has `Register*` methods and no
  accessor that returns the registered blocks, their types, safety or gates. A typed palette needs a
  new read-only method (see "Registry description" under the phased plan).
- **`assert` lines are not enforced at run time.** `Load` ignores them and they are not in the
  digest (`config.Assert`). They are checked by `bide check`, `bide view -config` and
  `InspectConfig`. An editor that adds an assertion adds a check that CI can fail on, not a runtime
  guard, and must say so.
- **Budget caps and obligations are not config keys.** The only budget in the repository is the
  per-run token budget `agent.WithTokenBudget` (`agent/budget_tree.go`), set in Go on an agent. The
  word "obligation" in the repository means gsm's delivery obligations in a convergence
  certificate (`govern/confluence.go`) and, in `docs/design/design-mofn-approval.md`, the idea of
  "reach k approvals by T", which is what the approval deadline of #312 (`timeout` in the
  `approval` block) implements. There is no flow-level budget or obligation key to lower to. See
  decision 6.
- **The approval `timeout` key is not on `main`.** It is added by #312 (`configApproval.Timeout`,
  and a pattern in the schema). This note treats it as present once #312 merges.

### Gaps in `bide check -diff` found while checking

The editor's "never weakens silently" rule leans on the diff classifier. Read against the code, the
classifier covers less than the rule needs. Each of these is a way to make a flow weaker that
`bide check -diff -check -fail-on-weaken` does not flag today:

| # | Edit | Why it is not reported |
|---|---|---|
| G1 | Delete an `assert` line, or change one so it holds more easily | `internal/runview/diff.go` has no handling of assertions; `Change` has no kind for them |
| G2 | Remove or lengthen an approval `timeout` (once #312 lands) | `gateEqual` compares `Single`, `Need`, `Approvers` and `ExcludePrincipal`; #312 changes `TopologyGate` and `configGate` but not `diff.go`, so a changed deadline reads as no change |
| G3 | Add an arm or edge that routes around a gated node | The guide says it plainly: the check compares each node's gate and does not reason about paths. The path-level answer exists (`ConfigReport.Gated`, and the `every path to X passes approval` assertions) but the diff does not compare it between versions |
| G4 | Change a node's `block` to one whose Go registration declares no gate | From the command line only gates the config states are known, so the gate the old block declared in Go is invisible on both sides. With a Registry on both sides the gates are known |

G1 to G3 are classifier gaps and are prerequisites for the editor (phase 1 needs them, see the phased plan). Status: G1 and G2 landed; G3 is the broad rule below (any node a gate guarded on every path before and a path now reaches without one, whatever its safety label, plus a lowered safety).
G4 is inherent to the structural mode and the editor must say so on the node (see "Structural mode and Go-declared gates").

## Where it runs

**Recommendation: a local command, `bide edit <file>`.** It serves a page on a loopback address,
exactly as `bide view` and `bide check -diff` do (`runview.Listen` refuses a non-loopback address),
and reads and writes only the file named on the command line.

| | Local `bide edit` | Wails desktop app | Hosted app |
|---|---|---|---|
| Where the config lives | The user's working tree, in git | The same | A service's storage, or a git integration with credentials |
| Shares `plan.Load`, `InspectConfig` and the diff classifier | Yes, in process (Go imports) | Yes, in process | Only on the server, behind an API |
| Trust path | The `bide` binary the user already runs | A second signed desktop program | A server in the path of a policy document, with its own auth, storage, logs and operators |
| Change control | The edit is a file change; the bank's existing review applies | The same | A second system to reconcile with git |
| Registry for the typed palette | A Go program can embed the editor beside its Registry | The same, but the program becomes a desktop app | Cannot see it (the Registry is Go code) |
| Offline, air-gapped | Works | Works | Does not |
| File access | Only the files named at startup | Native dialogs: any file the user picks | Server-side |
| Build and release | Pure Go; cross-compiles; fits the reproducible-build check | Per-OS builds, a webview toolkit (cgo on macOS and Linux, none on Windows; checked against Wails v2.14.0 and the v3 beta documentation on 2026-10-15), signing and notarization, an update path | Hosting and operations |
| Rendering engines to test | The user's browser; CI tests Chrome | WKWebView, WebView2 and WebKitGTK | The user's browser |
| Endpoint approval at a bank | `bide` is already on the machine | A new desktop product to approve, per OS | A new vendor-style service to approve |
| New dependencies | None beyond the YAML library in the nested module | A large dependency tree in its own module | Server stack |

A bank's change control wants edits reviewed, diffed and signed in the systems it already audits.
A hosted editor would be one more such system with a copy of the policy in it. It buys sharing and
no install, neither of which bide's users have asked for, and costs the property that makes the
review credible: the editor holds nothing the repository does not.

**Wails** (a Go program in a window, using the operating system's webview) was weighed on its merits.
Its real advantages are in-process Go, offline use and native file dialogs. The first two
`bide edit` has too: the page is served by the same Go binary that imports `plan`, `InspectConfig`
and the classifier, so there is no second implementation to drift. The native dialog is the one
advantage that is not matched, and it cuts the other way for this editor: the safest property of
the write path is that it can write only the files named on the command line, and an "open any
file" dialog widens that. Against it: a bank has to approve and endpoint-manage a new desktop
product on each operating system; the build stops being pure Go (Wails needs cgo on macOS and Linux,
and none on Windows; the latest stable release is v2.14.0 and v3 is still in beta, v3.0.0-beta.30 on
2026-10-15; so macOS and Linux builds need a native toolchain and WebKit development packages and
no longer cross-compile in one step, and the repository's
rebuild-and-compare reproducibility check would not cover them as it stands); macOS notarization, Windows code signing and an
update path become bide's job; three webview engines have to be tested where today the user's
browser is; and a large dependency tree enters the project, though in its own module. None of these
is a reason it cannot be done. Together they are a cost to pay when users ask for a window and not
before. Wails is therefore **deferred, not rejected**, and the design keeps the door open
(next paragraph).

**A host-agnostic front end.** The page talks to Go only through the `/api` requests in the table
below and uses relative URLs, so any host that can serve the embedded files and route `/api` to the
handler can show it: the loopback server today, a Wails window's asset handler later. To keep that
true: no feature may depend on a TCP port or on the URL (the session token lives in a header the
host supplies, and a desktop host that has no network listener needs none); no use of `window.open`,
downloads or clipboard permissions that differ across webviews; saving goes through `/api/save`, not
a browser file API; and the handler is an `http.Handler`, not tied to `net.Listener`. A Wails
wrapper would then be a small separate module that embeds the same handler, with its own file
choice policy (which would need its own security review, since it is a wider write surface).

Two ways to start the same editor:

1. `bide edit flow.yaml` (the `cmd/bide` binary). **Structural mode**: it knows block names only
   from the config itself, cannot check types, and shows no digest.
2. A Go program embeds the editor beside its Registry, as `planview` embeds the config page
   today. **Typed mode**: a palette of registered blocks with their types, type-compatible
   connections, the digest, and Go-declared gates and safety visible. A `bide edit -blocks
   blocks.json` form (a description a program exports) gives typed mode without embedding.

`bide edit` is not part of a `bide_audit_only` build (`cmd/bide/auditonly.go` leaves
`fullCommands()` empty and links no YAML decoder). The audit-only binary exists so an auditor runs
a small program that cannot write files; it stays that way.

## The editing model

### The document lives in the browser; Go is a pure function plus a write gate

The page holds the file's current text and a short undo history of earlier texts. The server holds
no document: each request carries the text, and the response carries the new text and what Go
computed from it. Config files are kilobytes, so sending the whole text is cheap and removes every
question of server-side state, sessions and cache invalidation.

| Request | Carries | Returns |
|---|---|---|
| `POST /api/doc` | nothing | The file's text, its hash, the baseline text and name |
| `POST /api/apply` | text, one operation | New text, or a refusal naming why; the report; the graph |
| `POST /api/report` | text (after a source edit) | The report and graph for the text |
| `POST /api/save` | text, the hash of the file as read | `200` after an atomic write, or `409` if the file on disk changed |

Every request is a `POST` of JSON (see "Security"), so even reading the file is a `POST`. The report is `plan.InspectConfig` (with the Registry in typed mode), the graph is the viewer's
`BuildConfig` output, and the weakening verdict is `runview.BuildDiff` of the text against the
baseline. The page renders them and computes nothing about the flow itself. There is one validator,
the one `bide check` runs; the JSON Schema is not run in the browser, because a second validator
would drift (the schema test exists precisely because the schema and the code can disagree).

### Three panes, one selection

- **Canvas.** The layered SVG the viewer draws, with a node's gate shown as the same corner badge
  (`2/3`) and its text (`2 of 3: ops, finance, risk`), problems marked on the node or edge they
  belong to, and the risk map colors with their glyphs. Selecting a node selects it everywhere.
- **Outline.** The nodes, wiring and assertions as a list in config order. This is not a
  convenience: node and wiring order are part of the digest (`TopologyDigest` commits to nodes,
  edges and arms in declared order), so order is data, and the outline is where a reorder is made
  and seen. It is also the accessible representation (see Accessibility).
- **Inspector.** The selected node, edge, arm, join or assertion as a form. Node: name, block
  (a text field with the names used in the file; a picker in typed mode), safety (only the values
  `Load` accepts for it, which in typed mode is exactly the lowering-only rule), and the
  **approval** section (below). Arm: predicate, target, `loop_max`. Join: inputs, merge block.
- **Source.** A read-write text view of the file, as an escape hatch. A change typed there goes
  through `/api/report`, so the other panes update and the engine's rules about what it can splice
  apply to the next operation.

### No layout in the file

The layout is a function of the topology (`internal/runview/layout.go` is deterministic and breaks
ties on the declared order). The editor stores no coordinates, because the config format has no key
for them and the loader would refuse one, and because a picture whose positions the author chose
could say something the topology does not. The cost is that a user cannot arrange the canvas. The
benefit is that the canvas is the same picture `bide view -config` draws for the same file, and a
reviewer who opens the diff page sees what the author saw.

### Operations

Every edit is an operation with a small, closed vocabulary. Go applies it to the text and the
result is checked (below).

- Nodes: add (name, block), remove, rename, set block, set safety, move (reorder).
- Gate: set or clear the `approval` block; set `need`, add or remove an approver, set
  `exclude_principal`, set or clear `timeout`.
- Wiring: add or remove an edge; add a switch, add or remove an arm, set the `else`; set an arm's
  `loop_max`; add or remove a join and its inputs and merge.
- Config: set the entry, the flow name, `in` and `out`.
- Assertions: add, edit or remove a line, from the grammar's six forms as templates.

Rename rewrites the node's name in every wiring element and the entry, and in assertion lines
only through the assertion grammar `plan` parses (the editor does not regex over free text). A
rename that cannot be applied everywhere is refused with the places it could not reach. Remove
reports what it will also remove (the edges and arms that name the node, the comments attached to
it, and the `assert` lines that name it) before it applies.

**What the editor does not offer, because `Load` would refuse it:** raising a node's `safety`
above what Go declares (typed mode knows the ceiling; structural mode shows the lowering rule and
lets `Load` be the check, which the banner says), replacing a block's own gate, an `else` and an
edge from the same node, and the other structural rules in `docs/guides/plan-flows.md`
("Load-time validation"). The report still catches them if they arrive through the source pane.

### Validation, live

After each operation the report is recomputed and shown: errors and warnings on their nodes,
edges and arms, the assertion verdicts with the counterexample path drawn, the risk map, and, in
typed mode, the digest. The page labels the mode, in the verdict area, in the words the viewer
uses: "structural check only: blocks and types are checked when a program loads the flow". A file
can be saved with errors (an editor must be able to hold a half-built flow), and the page says it
has errors every time it shows the save control.

Because the check runs where `bide check` runs, there is nothing for the editor to disagree with.
CI runs `bide check` on the saved file and gets the same answer.

### Round-tripping a hand-written file

This is the hard part and the one that decides whether anyone keeps using the editor, so the note
says what was checked and what is claimed.

**What a YAML library does on its own.** Parsing the example flow into `go.yaml.in/yaml/v3`'s
`yaml.Node` and writing it back (checked with the version in `cmd/bide/go.mod`) keeps comments
(head, line and foot), key order and flow-style mappings (`{name: validate, block: validate}`), but
drops blank lines, normalizes the space before a trailing comment, and applies its own indent. A
re-emit therefore gives a diff that touches lines the author did not change. `yaml.Node` has `Line`
and `Column` for a node's start and no end position, so it cannot say which bytes a node occupies.

**The approach: splice, then verify.**

1. Parse the text to a `yaml.Node` tree and index each node by its path and start position.
2. Compute the byte range an operation changes, from node start positions and the following
   sibling's start (or the end of the parent), pulled back over the comment lines and blank lines
   that precede that sibling.
3. Render only the new fragment, in the style of its siblings: a node added to a list of
   flow-style mappings is a flow-style mapping, one added to block-style items is a block item,
   with the file's indent, line ending and quote habit.
4. Splice the fragment into the original bytes. Everything outside the range is byte-identical by
   construction.
5. **Verify before returning.** Re-parse the result and check that it equals the model the
   operation should produce (the decoded JSON of the old text with the operation applied to the
   data), and that the bytes outside the changed range are unchanged. If either check fails, the
   operation is refused; the engine never returns text it has not verified.

Comment attachment follows one rule that the page states: a comment block directly above a node
(no blank line between) and a trailing comment on its line belong to the node, and are removed
with it; a comment separated by a blank line belongs to the file. The pre-save diff shows any
comment an operation removes.

**What the engine refuses to splice, and says so:** anchors, aliases and merge keys (`yamlToJSON`
resolves them, so a file may legally use them, but a splice inside an alias is ambiguous); more
than one document; tags; a node the end-of-range computation cannot place unambiguously; a file
with a byte-order mark or mixed line endings it cannot reproduce. The affected node is shown
read-only with the reason ("edit this in the source pane"), and every other node stays editable.
The refusal is a feature: a refused edit costs the user a text edit, a wrong splice costs a
reviewer's trust.

**JSON configs** go through the same engine, because JSON is YAML flow style: the phase 0 spike
(`flowedit`, pull request #317) splices JSON files, tab-indented ones included, and writes new text
as JSON (quoted keys and strings), one entry to a line when the collection it joins is
pretty-printed. A JSON file has no comments, so the comment-ownership checks have nothing to do
there; the data, key-order and line checks still run on every result.

**Where YAML becomes JSON today.** `plan.Load` reads strict JSON only. A `.yaml` or `.yml` file is
converted by `yamlToJSON` in `cmd/bide/view.go` (for `bide view -config` and `bide check`), and the
`bide init` project template carries its own copy for its embedded `flow.yaml`. Both call
`yaml.Unmarshal`, which reads the first document and ignores any later one without a word (issue
#320). The editor refuses a file of more than one document, and counts it as a miss in the
measurement; the loader should refuse it too.

**If the spike fails.** The fallback is the `yaml.Node` re-emit, with the cost shown in
the editor: the first save of a file would normalize blank lines and indent, and the editor would
offer that as a separate "normalize formatting" step (its own diff, its own commit) before any
semantic edit, so the semantic diff stays small. Phase 0 (see the phased plan) decides between the two with
numbers from a corpus, before any interface is built.

## Gates and the accountability blocks

### What is a "block" in the editor

A block is a form over config keys that exist. The editor presents:

| Block | Config it writes | Lowers to |
|---|---|---|
| **Approval gate** | `approval: {need, approvers, exclude_principal, timeout}` on a node or join | `agent.ApprovalPolicy` on the node's gate; journaled request, decisions and tally; checked offline by `audit.VerifyApprovals` |
| **Separation of duties** | `exclude_principal: true` inside the gate | The gate never counts the run's requester |
| **Deadline** | `timeout` inside the gate (after #312) | The gate expires; the node is never run; `plan.ErrApprovalExpired` |
| **Safety class** | `safety: readonly | idempotent | side_effect` | The node's behavior on an ambiguous crash: halt or re-run |
| **Bounded loop** | an arm's `loop_max` | A finite back-edge |
| **Policy assertion** | a line in `assert` | A check by `bide check` (not enforced at run time) |

Each of these is a form that writes keys `Load` already reads. The inspector's gate section shows
`need` as "k of n" beside the approver list, rejects what `configGate` rejects (no approvers,
`need` outside `1..n`, a repeated or case-colliding id, an id that is not printable ASCII) as the
user types, and explains separation of duties in one line: "never counts the person who started
the run". It cannot check that an approver has an enrolled key; the page says approvers' keys are
supplied at load time (`WithLoadedApproverVerifiers`).

**Budget caps and obligations are not blocks in this design.** There is no config key for either
and nothing for an editor to lower them to. Showing a "budget" block would draw something the
runtime does not enforce, which is the fork the gate above forbids. If the maintainer wants them,
the order is a design of the key and its runtime meaning first (in `plan`, with the journal and
proof story), then a form. Until then the closest real things are the deadline, the bounded loop
and the assertions, and the page uses those names. See decision 6.

### Weakening is shown in the editor, by the same code as in CI

The baseline is the config the author starts from, and the editor always has one:

- With a git working tree, the file at `HEAD` by default, or at the revision `-base <rev>` names
  (the resolution `bide check -diff` already does with `git show`). The baseline is the committed
  policy, not the session, so weakening in two saves is still weakening against `HEAD`.
- Without git, the file as it was when the editor opened it.

After each operation the editor runs `runview.BuildDiff(baseline, text)` and shows the result (with the baseline read as if the session's renames had been made in it: see the build-time decision 1 of phase 2). A
weakened gate is a banner at the top of the page in the viewer's verdict style: the glyph, the
words `APPROVAL GATE WEAKENED`, and each item (`charge: approval 2 of 3 -> 1 of 3`). It stays until
the edit is undone or the text no longer weakens. It is not dismissible, because a banner that can
be closed is a warning that can be missed.

**Saving a weakened config needs one explicit confirmation** that lists each weakened item. The
confirmation is not written into the file, because the format has nowhere to carry it and must not
grow one for this: the control that stops a weakened policy merging is the pull request, where
`bide check -diff -check -fail-on-weaken` fails and a policy owner must approve (see "Collaboration, review and signing"). The
editor's confirmation exists so the author never saves a weaker policy by accident.

**One classifier.** The editor does not reimplement `gateChange` or `accepts`. A test saves the
edited text and compares the editor's verdict with `CheckDiff` on the same two files; they must
agree on every case in a table of edits. If the classifier gains a category, the editor shows it
because it shows what the classifier returns.

**The classifier first needs to cover G1 to G3** (above), as a change to `internal/runview` that
CI and the editor both get:

- G1: a removed or changed `assert` line is reported, and so is an assertion that held at the
  baseline and is violated or undecided now.
- G2: a gate whose `timeout` was removed or lengthened is weakened; shortened is strengthened. This
  needs a rule decision (decision 5), because a shorter deadline also makes a pending run expire
  sooner, which is stricter about time and not about who approves.
- G3 (broad): for each node, compare `ConfigReport.Gated` between the two versions; a node that every
  path reached through a gate before, and some path now reaches without one, is reported as
  "reachable without an approval gate" with its safety and the shortest path (the counterexample
  machinery for assertions exists), **whatever its safety label**. The label is the new config's own
  claim, so trusting it would let one edit relabel a node read-only and route around its gate; the
  old label counts for the effect checks, and a safety moved toward read-only is itself a weakening.
  The classifier says plainly that this is the path-level check the per-node gate comparison lacks.

**Structural mode and Go-declared gates (G4).** When a node's `block` changes and the editor does
not have the Registry, it marks the node: "gate not checked: a block can declare a gate in Go". The
mark is part of the weakening verdict (an unknown, not a pass), and the save confirmation lists it.
Typed mode removes the unknown, because the gates are then known on both sides.

### Declared next to what ran

The viewer already draws a flow run's declared topology, taken from the journal after its digest
is checked against `flow:digest`, with the path the run took and a conformance card
(`docs/guides/run-viewer.md`, "CONFORMANCE"). The editor adds the question the author has: how does
the file I am editing differ from the flow a run actually used? In a later phase, `bide edit -run
<id> -db ...` loads the run's journaled topology as a second baseline and shows the same diff the
config diff page shows (added, removed and changed nodes, weakened gates), with the run's path
drawn on the current canvas. That tells an author, before merging, which in-flight runs a change
would affect: a changed digest means a paused run under the old flow will not resume under the new
one (`docs/guides/run-viewer.md`, "The digest").

What it claims: the file's topology differs from, or equals, the topology the run journaled. What
it does not: that the run followed the file. That is conformance, and `Conform` and the viewer's
card do it. Conformance sees which steps ran and which Switch arm was taken, not what a step did
inside; the editor does not change that.

## Saving

- **Explicit save.** The file is written only when the user presses Save. No autosave: a policy
  file should not change on a keystroke.
- **Atomic and checked.** Write to a temporary file in the same directory and rename over the
  target, keeping the mode. `POST /api/save` carries the hash of the file as it was read; if the
  file on disk has changed since, the answer is `409` and the page shows the on-disk diff and
  offers to reload, never to overwrite. On Windows the rename-over-existing is done with the
  platform's replace call, and a test covers it.
- **Only the named files.** The set of writable paths is fixed at startup from the command line,
  resolved once, and re-checked at write time. There is no "open another file" endpoint.
- **No server-side state.** The server keeps the allowed paths and the session token in memory and
  nothing else. Close the tab and the unsaved edits are gone, and the page warns before it closes
  with unsaved edits.
- **The user commits.** The editor does not run `git add`, `commit` or `push`. It shows the diff of
  the bytes it would write (a plain unified diff) before save, so formatting churn and removed
  comments are visible. It reads git only to resolve the baseline, with the same `git show` call
  `-diff` makes.

## Collaboration, review and signing

**Git is the collaboration layer.** Two people editing the same flow merge their branches; the
editor's compare-and-swap on save only stops it from overwriting a change made behind its back. No
real-time editing, no comments, no shared session: each would be state outside git.

**Review uses artifacts that already exist.** The reviewer reads the text diff (small, because the
edit was a splice), and opens `bide check <file> -diff <rev>` for the graph with added nodes in
green, removed ones as ghosts and weakened gates called out, or reads its `-check` text in CI. The
planned GitHub Action that posts that text on a pull request (ROADMAP, "The visual builder") is the
reviewer's view; the editor needs nothing in it.

**What a bank's change control can build on, without bide building anything for it:**

1. Signed commits and branch protection, which are git's.
2. A code-owners rule that requires a policy owner to approve any change under the flows directory
   that touches an `approval` block, and `bide check -diff -check -fail-on-weaken` as a required
   status (after G1 to G3, a stronger one).
3. The topology digest: a run journals `flow:digest`, so a run is bound to the exact topology it
   ran under, and the digest of the merged config (from a program with the Registry) can be
   recorded in the change ticket and compared with what the journal shows. The editor shows the
   digest in typed mode so an author can paste it into the ticket.

**Signing edits.** The editor does not sign. Two distinct things could be meant:

- Signing the *commit* is git's, and recommended; nothing to build.
- Signing the *config file* with a bide signer (the HSM and KMS signers exist for journals) and
  having `Load` refuse an unsigned config would put a signature check in the runtime's load path.
  That is a runtime and protocol change with its own design, and an editor feature that reached it
  would be an executor change in disguise. It is out of scope here; decision 8 records it.

The editor never holds a signing key and signs nothing, which is also why it can be a local page
with a simple security posture.

## Security

The editor is a web server that can write files, which the viewer deliberately is not. The posture
is the viewer's plus what a write endpoint adds.

**Inherited from the viewer** (`internal/runview/server.go`):

- Listens only on a loopback IP or `localhost` (`Listen` refuses anything else); the default is
  a loopback address with a port of its own.
- Answers only requests whose `Host` is a loopback address, which defeats DNS rebinding.
- `Content-Security-Policy` that allows only same-origin resources, `X-Content-Type-Options:
  nosniff`. The editor's policy is the stricter set the approve page uses: `default-src 'none';
  script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none';
  form-action 'none'; frame-ancestors 'none'`, plus `Referrer-Policy: no-referrer` and
  `Cache-Control: no-store`. No inline script, no inline style, no event-handler attributes.
- **No remote code.** Every script, style and icon is embedded in the binary (`go:embed`). No CDN,
  no web font, no analytics, no outbound request from the page or the server.

**Added for writes:**

- A per-process random token (128 bits) printed in the URL fragment the command opens, so it never
  appears in a `Referer`, and sent by the page in a request header on every `/api` call. Compared in
  constant time. A request without it is refused.
- Every `/api` request is `POST` with `Content-Type: application/json` and a body limit (1 MiB), so
  a cross-origin form post cannot make one, and the `Origin` must equal the server's own origin
  (absent or foreign is refused).
- Writes only to the paths fixed at startup; the path is re-resolved at write time and a target
  that has become a symlink outside the original directory is refused.
- Config text is untrusted data: node names, block names, comments and assertion lines are put in
  the page with `textContent` and attribute setters, never `innerHTML`. The graph SVG, which the
  server generates with names escaped, is parsed with `DOMParser` and adopted, not assigned as
  markup.

**What this does and does not defend against.** It defends against another web page or local
process writing through the editor and against a hostile config file attacking the person who opens
it. It does not defend against a person who controls the machine, who can edit the file directly.
It also does not make the editor the control: a compromised or buggy editor could write a weaker
policy, so the controls that matter are downstream and independent of it, the diff, `bide check`
and the pull request. That is why the weakening verdict is computed in Go from the bytes that will
be written, shown in the editor, and enforced again in CI from the committed bytes. The approve
page's central problem (who serves the script an approver signs with) does not arise, because the
editor signs nothing and its output is a text file a person reviews.

## Accessibility

A graph editor is the kind of tool that is easy to build only for a mouse. Requirements:

- **Every operation is reachable without a pointer and without dragging.** Connecting two nodes is
  an inspector action ("add an edge from this node to [a node]") and an outline action; dragging
  from a port is an extra, not the way. Reordering is "move up" and "move down" in the outline.
- **The outline is the primary accessible view**: a list in config order, with each node's gate and
  problems as text, navigable with arrow keys, and every change announced through an `aria-live`
  region (the verdict and the first new problem). The canvas is a second, visual view of the same
  selection; its nodes are already focusable and labelled (`svg.go`).
- **State is never carried by color alone.** The viewer already pairs each verdict and risk class
  with a glyph and words; the editor keeps that, including for the weakened banner.
- **Reduced motion and contrast.** The flowing-dash animation stops under `prefers-reduced-motion`
  as it does in the viewer. The page draws from the checked themes of [Themes](themes.md) (`light`,
  `dark`, `high-contrast`, `-theme` for your own): text is 4.5:1 against the backgrounds it is drawn on; control
  edges and the focus ring (which is the selection ring's colour) are checked at 3:1 in every theme, a custom one included, against
  the page, a panel, a nested panel and the graph canvas (the save dialog's edge is checked against the
  page, not the dimmed page behind an open dialog), and the focus ring also on a node's fill and the selected item's tint (check C2;
  the lowest in the shipped themes is 3.60:1, a control edge on `surface2` in `light`);
  `prefers-contrast: more` gets `high-contrast`, and forced colors get the system's. The header button steps through the themes, names the next one and announces
  it; a host that locks the editor to one theme hides it. The selected node's ring is 7 px, heavier
  than any state stroke, so selection is not carried by colour; the save dialog dims the page with a
  shadow of the theme's scrim (not under forced colours, where it keeps its border).
- **Focus is managed.** After an operation the focus returns to the thing the user changed;
  dialogs trap focus and return it; the weakened confirmation lists each item as text.
- **Forms are real forms**: labels bound to inputs, errors tied to their fields by
  `aria-describedby`, no placeholder-only labels.

Tested by a keyboard-only end-to-end run and an automated check (see Testing); an automated check
finds a fraction of problems, so the keyboard run and a manual screen-reader pass at each phase are
the gate.

## Technology

Keep the repository's stance: no heavy dependency, nothing to trust that is not reviewed here.

- **Server and engine, Go.** A new nested module, `github.com/bide-ai/bide/flowedit`, holds the
  edit engine (it needs `go.yaml.in/yaml/v3`, which the root module must not require), the HTTP
  handler and the embedded page. `cmd/bide` imports it for `bide edit`; a Go program imports it to
  embed typed mode. It imports the viewer's `internal/runview` for the graph and the classifier,
  which Go permits because the module's path is under the root's (`cmd/bide` does the same). The
  root module and its stdlib-only rule are untouched. The cost: one more nested module to tag in
  the release (the release script tags the root and nested modules together).
- **Page, plain JavaScript.** Hand-written ES modules served from `go:embed`, with no framework,
  no bundler, no transpiler and no `npm` in the build, the way `approvals/page/*.js` and the
  viewer's `assets/*.js` are. The approve page is about 42 KB of JS, CSS and HTML in all; the
  viewer's scripts are about 28 KB. The editor needs more, and the proposed budget is **150 KB
  uncompressed for all page files**, asserted by a test so growth is a visible decision. If the
  outline and inspector outgrow plain modules, the answer to consider is a small vendored library
  (a few KB, source in the repository), not a build chain (decision 4).
- **Deterministic by construction.** With no build step, the bytes served are the bytes in the
  repository; `go:embed` plus the Go build's own reproducibility (`-trimpath`, the release's
  rebuild-and-compare) are the determinism. A test pins the file list and sizes. The approve page
  publishes `approve-page.SHA256SUMS` because an organization hosts it on its own origin; the
  editor is served by the binary itself, so the binary's checksum and signature already cover it
  and a separate sums file adds nothing.
- **No node on the user's machine.** Nothing in `bide edit` runs node. Node appears only in tests
  (`node:test` for engine-independent page logic, as in `approvals/page/test`; puppeteer-core,
  pinned by a lockfile, for Chrome).
- **Types.** JS stays untyped, with JSDoc on the request and response shapes and a Go test that
  decodes the page's expected shapes from the Go structs, so the contract is checked from one side.

## Testing

Per the repository's rules: each behavior is specified by a failing test first, and the key tests
are mutation-checked after the commit.

**Round-trip and engine (the highest-risk code).**

- **Property tests** over a corpus (every file in `examples/plan/flows`, the configs the schema
  test and `plan/fuzz_config_test.go` use, and generated files in both flow and block styles, with
  comments and blank lines in every position the grammar allows). For each file and each
  operation: bytes outside the changed range are identical; the result re-parses; the decoded
  document equals the old document with the operation applied to the data; `Undo` restores the
  exact bytes; applying a no-op operation (setting a value to itself) returns identical bytes.
- **Fuzz target** `FuzzApply` over (text, operation) pairs: the engine must return either verified
  text or a refusal, never text that fails the check, and never panic.
- **Refusals are specified**: anchors, aliases, merge keys, tags, multiple documents, a byte-order
  mark, CRLF, tabs in odd places, each with a test that the file is read, shown, refused for the
  edit, and left byte-identical.
- **Formatting-preservation metric** for the spike: over the corpus, the fraction of operations
  whose diff touches only the intended lines, compared with the `yaml.Node` re-emit baseline.

**Schema and plan conformance.**

- Every text the engine produces validates under `plan/schema/flow.schema.json` (using the
  validator the schema test already has, moved to a shared internal package so both use one) and
  gives the same `InspectConfig` errors as the model the operation should have produced.
- The operation vocabulary is generated from the structs: a test fails when `config`, `configNode`,
  `configApproval` or `configWire` gains a key the editor has no operation or an explicit "not
  editable" entry for, so the editor cannot fall behind the format silently.
- `plan.Load` on the saved text builds a flow whose `Topology()` equals the editor's graph, in a
  typed-mode test.

**Weakening.**

- A table of edits, each with its expected verdict (removed gate, lowered need, added approver
  without raising the need, replaced approver, dropped `exclude_principal`, removed or lengthened
  `timeout`, removed or changed `assert`, a new arm around a gated node, a block change in
  structural mode, a rename of a gated node), run through the editor and through
  `CheckDiff -fail-on-weaken` on the saved files, which must agree. A case the classifier misses is
  a failing test that names the gap.
- The banner cannot be dismissed, and a weakened save without the confirmation is refused by the
  server and not only by the page.

**Server and security.** Table tests for each refusal: no token, wrong token, wrong `Host`, wrong
or absent `Origin`, wrong content type, oversized body, a path outside the allowed set, a symlink
swapped in after startup, a changed file (`409`), a read-only file, a full disk (the original is
untouched). A test that every response carries the headers above, as the approve page's test does.
Windows tests for the rename and for path handling, with `GOOS=windows go vet` per module.

**Browser tests.** A Chrome job modeled on `.github/workflows/page-mtls.yml` (puppeteer-core 25.14.0
from the lockfile, runs on pull requests that touch the editor and in the merge queue): open the
editor on a sample, change a gate, see the verdict and the weakened banner, save, and check the
file on disk; a keyboard-only run through the same steps; a check that no console error, no
inline-script CSP violation and no network request outside the origin occurred. An axe-core run is
an optional addition, pinned by lockfile.

## Phased plan

Each phase is one or more pull requests, each with its docs, changelog fragment and tests.

- **Phase 0, the engine spike.** The `flowedit` engine with the operations for a node's fields and
  a gate, against the corpus, with property tests and the formatting metric, and no interface.
  Exit: a go or no-go on splicing against the re-emit fallback, decided with numbers
  (decision 3). **Done:** go. Over 367 configs and 65,951 operations, 99.2 percent applied and
  touched only the intended lines (the bar is 95 percent) against 18.4 percent for the re-emit, and
  no unverified output in 4.5 million fuzz executions; the module is `flowedit`. In parallel,
  **prerequisite P1**: the classifier covers G1 to G3 in `internal/runview`, with the CI flag,
  tests and docs (this is useful to `bide check -diff` users on its own, with or without an
  editor). #315 and #316 cover G2, the removed or loosened `assert` line of G1, and G3 for nodes
  with a side effect; an assertion that held at the baseline and is violated now with its line
  unchanged is still not reported, and is closed before phase 1 ships.
- **Phase 1, the smallest useful editor (built).** Status: shipped across the pull requests that
  followed the spike (the read-only page, the baseline and weakened banner, the source pane and
  atomic save, the inspector's forms, the assertions panel, the accessibility pass), with the
  decisions of this note unchanged. Three things differ from the sketch below, each for a reason
  the tests found: every API call is a `POST` (including reading the file), a refused operation is
  a `200` with the reason and not an error status (so asking what an operation would do does not
  fill the browser console), and the assertion verdict regression of G1 was closed in its own pull
  request. The manual screen-reader pass of the phase gate is not done and is listed in
  `docs/KNOWN-LIMITATIONS.md`. The sketch: `bide edit flow.yaml`: the canvas and outline (read),
  the inspector for a node's block, safety and **approval gate** (need, approvers, separation of
  duties, deadline once #312 is in), the assertions panel with the six forms as templates and live
  verdicts, the source pane, the baseline and the weakened banner, the pre-save diff, atomic save.
  Structural mode only. This covers what a policy owner does, which is the part of editing that is
  most error-prone by hand and most important to review, without structure editing.
- **Phase 2, structure.** Add, remove, rename and reorder nodes; edges, switches, arms, `else`,
  `loop_max`, joins; drag to connect as an extra; undo and redo.

  *Built in steps; decisions made at build time (2026-10-19):*
  1. **A rename reads as weakened.** *(Superseded 2026-10-21: see the next paragraph.)* The classifier
     follows names, so renaming a gated node is a removed gate and a new ungated node. The editor takes
     the over-report and says why next to the verdict; the editor passes no hint to the classifier,
     ever, and classifier-side rename detection is out of scope for phase 2.

     *Superseded.* The classifier pairs a node renamed with the same block, kind and edges (so a pure
     rename was never reported), and gives up on a rename beside another change to that node. The editor
     knows a rename happened because it made it, so it keeps the session's renames (old to new, in order;
     an undo restores the list its text had) and replays them, with its own `rename_node`, on the baseline
     text before the comparison. The classifier is unchanged and gets no hint: it compares two texts
     whose names agree. A rename the baseline cannot take (no such node, or the new name is in use) is
     dropped and the nodes are matched by name. The saved file and `bide check -diff` are unchanged.
  2. **The implicit entry is never changed in silence.** An operation that would change the entry a
     config leaves implicit (the first node) is refused, with a reason that offers `set_entry`.
  3. **Removal cascades, and the verdict is the classifier's.** Removing a node also removes the
     edges and arms that name it; the dry-run list of what goes is shown and must be confirmed, and is
     separate from the weakening confirmation. A switch left with no arm is removed whole. *(Changed
     2026-10-21: the `assert` lines that name the node are removed with it, and listed in the same
     dry run; before, they stayed and the check reported them as naming a missing node. Removing an
     assertion is a weakening, which the verdict, the banner and the save confirmation show as they
     show any removed assertion.)*
  4. **The last arm of a switch is not removed one arm at a time.** `remove_arm` on the last arm is
     refused; `remove_switch` is the explicit operation.
  5. **Joins are wiring-element paths** (phase 2, joins step): a join has no entry in `nodes`, so its
     gate and safety are edited at the wiring element.
  6. **Undo and redo** keep their stack across a save, and the server recomputes the check, the
     banner and the diff for every step. Undo and redo themselves shipped in phase 1; the phase 2
     work on them is only that they cover the new operations.
  7. **The page budget** is reported per pull request, and raised only by the maintainer.
  8. **Skip links, not a bigger Tab budget.** The structure controls make a full Shift+Tab walk from
     the assertion form back to Save longer than phase 1's bound of 40. Rather than raise that bound,
     the page gets WCAG 2.4.1 skip links (Skip to toolbar, outline, inspector, assertions, at the top
     and at the start of the inspector and the assertions) and labelled landmarks. The standard the
     test holds is: from the assertion form to Save in at most 5 key presses by the skip link; the raw
     walk is bounded at 50 only as a regression guard. Lists are one Tab stop (a roving list) with one
     shared Move pair, and the flow settings are a closed disclosure. No keyboard shortcuts in phase 2.
- **Phase 3, typed mode.** A read-only `Registry` description (below), `-blocks`, embedding in a Go
  program, a palette, type-compatible connections, the digest, Go-declared gates and safety visible
  (which ends G4's "unknown").
- **Phase 4, compare with a run.** `-run` and the stores the viewer already opens: a run's
  journaled topology as a baseline, the run's path on the canvas, and which runs the change would
  affect.

**Registry description (phase 3).** A new exported, read-only method on `plan.Registry` that returns,
for each registered block: its name, kind (step, tool, model, join), input and output type names,
the safety it declares and the gate it declares; each predicate with its switched type; each merge
block with its arity and input types. It is a published API addition to the `plan` package, so it
needs the maintainer's approval as such (decision 7), with godoc, an example, and a test that it
matches what `Load` resolves.

**Phase 3 as built (differences from the plan above).**

- *Registry description.* `Registry.Describe()` returns `plan.RegistryDescription` (blocks, predicates, merges, and `Errors`, the registrations a Registry refused). Merges are a list of their own, with no `join` kind (a merge name and a block name are separate namespaces). A single-decision gate is described with `need` 0, as the topology carries it, and a gate `Validate` refuses is described as declared and named in `Errors`.
- *`-blocks` and the checker.* A description cannot rebuild a Registry (it holds type names, not types), so `plan.InspectConfigDescribed` builds an inert stand-in Registry whose types are `reflect.StructOf` stand-ins carrying the name, and runs the same code `InspectConfig` runs: one implementation. Types are told apart by name only, so two distinct types spelled alike are one (the digest already cannot tell them apart; see the known limitations), and the process keeps at most 20000 type names (about 25 MB). `RegistryDescription.Validate` refuses a description with registration errors, so a description of a program that does not load is not used.
- *Unused blocks.* A palette lists blocks the config has not placed, and `Load` refuses a registered block nothing uses, which would stop the check before it builds anything. `plan.AllowUnusedBlocks()` (the maintainer approved it) is an option of `InspectConfig` and `InspectConfigDescribed`; `Load` and `Validate` still refuse.
- *A description is only a file.* Every typed result that rests on one says so and, for `check`, names the file by its SHA-256. A diff is never less strict than without a Registry or description: a side with errors against its blocks makes the diff structural and not certified; a description for one side only is never compared with the side that has none; both sides described are compared typed against typed, from two files the CI job generated from the two program versions.
- *The editor's verdict.* The program the baseline was written for is not the one the editor knows, so the weakened verdict is the typed comparison together with the structural one, never less than the structural editor says, and a comparison that is not certified needs the same confirmation as a weakening.
- *Type-refusing operations.* The server refuses an operation that introduces a type problem the text did not have (an edge, a predicate, a join input, a terminal), with `plan`'s reason; the page's hints never decide.

### Phase 4 design

Confirmed by the maintainer and reviewed against `main` at a34e4c83; the pull requests that build it are
listed at the end of this section, and the sections below say what each does. Names are as designed;
the text under "as built" in the guide is what ships.

#### What is there today

- The editor's baseline is bytes: `baseline []byte` (git HEAD, `-base`, or the file as opened), fixed
  at startup, compared by `flowedit.compare` -> `runview.BuildDiff(Diff{Old, New Config})`, where each
  side is a config's bytes run through `plan.InspectConfig`. Renames of the session are replayed on
  the baseline bytes (`renameBaseline`).
- A run's journal holds a `plan.Topology` (record `flow:topology`) and `flow:digest`.
  `plan.JournaledTopology(recs)` decodes it and checks its digest; the viewer draws it only then.
- `diffChanges` takes two `plan.ConfigReport`s (Topology, Approvals, Safety, Gated, Assertions). A
  journaled topology is a Topology, not config bytes, so it has no assertions and no `safety` text,
  and no `Gated` map unless computed.

#### 1. Flags

`bide edit [existing flags] -run <id> (-db <file> | -postgres <dsn> | -postgres-env <VAR> | -journal <file>...) <flow>`

Exactly the viewer's store flags, with the viewer's help text and rules: `-db` (SQLite, opened with
`sqlite.OpenReadOnly`), `-postgres` / `-postgres-env` (`postgres.OpenReadOnly`; prefer the env form,
which keeps the password out of argv), `-journal` (repeatable `audit.ExportJournal` JSON). One store
at a time, as in the viewer. `-run` without a store, or a store without `-run`, is a usage error;
except that a store alone is allowed and means "no baseline run chosen, but the affected-runs list
(section 4) is on". Not carried over: `-sth`, `-evidence`, `-pubkey`, `-approver-keys`, `-stop-*`,
`-topology`, `-digest` (proofs and conformance are the viewer's; section 5).

The store flag parsing moves out of `runview/cli.go` into one shared helper that both commands call,
so the two cannot drift (new, internal to `runview`). The run ID and the store are fixed at startup,
like the file path: no endpoint opens another run or another store. Restart with another `-run`.

#### 2. The run as a second baseline

The run comparison answers one question: **would resume accept
this file for that run**, which is the digest. So it compares exactly what the digest covers, and
nothing the digest does not.

What the digest covers (`plan/digest.go`): flow name, entry, each node's name, block, kind, input and
output type and gate, every edge, every switch and its arms in order, and the **order** of nodes,
edges and arms. It excludes `safety` and `assert` lines.

Shared code: `diffChanges`, `diffGraph` and the verdict words are reused; `BuildDiff` is not, because
its fallback branches rebuild both sides from config bytes (`diff.go:268,281`) and would erase a side
that has only a Topology. A new `runview.DiffTopologies(old, new plan.ConfigReport, typed bool)`
calls the same functions on two reports, and `BuildDiff` is refactored to call it too (one path).
`Config.Topology` is dropped; the run side is a `ConfigReport` made by `plan.InspectTopology`.

Normalisation, applied to **both** sides before `diffChanges`, so nothing the digest ignores can show
and nothing a mode cannot know can read as a change:
- Both modes: `Safety` and `Assertions` nil. The page lists "safety and assert lines are not in a
  journal and not in the digest; not compared". The "safety lowered" and assertion categories can
  never appear. The count N is of digest-covered changes only.
- Structural mode (file has no Registry): also blank `Kind`, the input/output types and the canonical
  types, and the **gates**, on both sides. The file's `Kind` is empty and `renamedNodes` needs equal
  kinds, so without this a pure rename reads as a removed node plus an added one; and the journaled
  gate is the resolved policy (block-declared Go gates, Single) while the structural file shows only
  config gates, so it would read "gate removed (weakened)". The page says "gates are compared only in
  typed mode". Gate and path weakened verdicts against a run are therefore **typed-only**; structural
  mode reports structure (names, blocks, edges, arms, entry, flow name, order) and no weakened verdict.
- Typed mode: compare the digests directly first. Same digest: no changes listed, nothing else is
  needed. Different digest: list the digest-covered changes; if the list is empty, say "the digest
  differs in a way the diff does not itemise (order, or types)" rather than "no changes".

Order is compared explicitly (the diff matches by name): the node, edge and arm order of each side is
compared as lists; a difference is listed as "order changed: nodes", "order changed: edges", "order
changed: arms of S". Swapping two nodes (`move_node`, `move_wiring` can produce it) therefore reads
"differs", as resume treats it.

The run snapshot is cached and shared across requests, so `NormalizeForRun` works on a deep copy
(Gate pointers, slices, maps); a test asserts normalising leaves the cached report unchanged.
`InspectTopology` documents that its report does not alias `t` (it copies). Structural order
comparison skips the derived `Loops` slice and relies on the config-derived order equalling the built
flow's; a corpus test (every corpus config with joins, switch/else and loops) asserts the structural
topology's node, edge and arm order equals the typed (`Load`) topology's, so an unchanged file never
reads "order changed".

A new node with no gate, added since the run, is a change and is not judged as a weakening against the
run: a journal holds no safety and no reachability through gates that the digest covers, so the only
verdict made against a run is about a node's gate, in typed mode. The headline says what is known and no
more: SAME DIGEST (typed, equal digests), NO DIFFERENCE SEEN (structural, with the kinds, types and gates
it does not compare named), DIGEST DIFFERS (the digests differ and nothing itemises it), CANNOT COMPARE (a
typed file with errors), NO DIGEST TO COMPARE (a typed run whose digest cannot be computed), or the config
diff's words when changes are itemised. It never reads "no changes" or "the two configs declare the same
flow".

Session renames: `renameBaseline` is generalised to a Topology (`renameTopology`): a rename rewrites
the entry, every node name, edge endpoint, switch `over`, arm target, join input and loop target, with
a mutation test that fails when any one of those fields is skipped. A rename the topology cannot take
is skipped whole, as for the config baseline.

`plan.InspectTopology(t Topology) ConfigReport` contract (godoc): returns the report of a declared
Topology alone: `Topology` is t, `Digest` is `TopologyDigest(t)` when computable, `Approvals` from the
gates, `Gated` computed as `InspectConfig` computes it; `Safety` and `Assertions` are nil, `Typed` is
false, `Problems` are empty (it does not validate: a dangling edge or a duplicate name is tolerated
and the unreachable parts are simply not gated); it never panics on any input and is O(nodes + edges).
Tests: a corpus conformance test (for every corpus config, `InspectTopology(report.Topology).Gated`
equals `report.Gated`, and Approvals and Digest agree), and a fuzz target over hostile topologies
(dangling edges, duplicate names, self loops, huge fan-out).

**With `-base`: both, as two baselines, not one.** The git baseline (the committed policy) keeps its
job: it drives the WEAKENED banner and the save confirmation, unchanged. The run is a second,
informational comparison shown in its own panel ("Compared with run X"); it never gates a save and
never changes the banner. A file can be stronger than the committed policy and different from what
the run used; both facts matter, and merging them into one verdict would hide one. The page has a
selector (Committed policy | Run X) for which diff the canvas colours show; the other stays one click
away; both verdicts are always in the status line.

#### 3. The run's path on the current canvas

The path comes from the existing `flowGraph` overlay computed against the run's own journaled
topology (node status: done, halted, waiting, attempted; edges taken; last node; iteration counts).
It is projected onto the edited file:

- A node is matched only when its **name and block** agree (and, in typed mode, its kind). A name that
  matches with another block is a different node: it carries no run mark and is listed under the
  canvas as "the run ran `charge` with block `x`; this file's `charge` has block `y`".
- Renamed in this session: the run side is replayed through the session's renames (`renameTopology`),
  so the path follows the node. A rename from an earlier commit is matched only where the diff's own
  rename rule matches, labelled "matched by shape".
- Removed since the run: listed under the canvas in run order ("the run also ran: charge, not in this
  file"), never dropped.
- Taken edges and arms with no counterpart in the file (an endpoint removed, or the edge removed) are
  listed the same way, as text.
- Loop arms: the arm is matched by from/to; if its bound differs from the run's, the mark carries
  "loop bound was N in the run".
- Added since the run: drawn with no run mark; the legend says "not in the run's flow".
- Run topology refused (section 6): no path, no baseline, a banner says why.

Every run mark has text, not only colour.

#### 4. Which runs the change would affect

A button "Check unfinished runs" (not run at startup). Server side:

1. List with the recovery pass's filter, not "no `run:complete`" alone: `ExcludeHolding:
   run:complete, run:aborted, run:cancelled`. A cancelled run is refused by `Begin` regardless of the
   flow, so it is not "affected by the change"; an aborted saga is over. Reserved runs
   (`agent.IsReservedRun`) are skipped. Limit N = 5000; the query asks for N+1 so the page can say "at least N, the first N by run ID" when there are more. A store with no `agent.Lister`, and
   `-journal` exports (only the runs given), say so.
2. Per run, **one small point read**, `Journal.Get(run, "flow:digest")` (a narrow interface;
   `Source.History` reads a whole run and is not used). `run:start` is **not** read per run: it carries
   the flow input, which can be megabytes. Read errors count as "unreadable", by class. A digest
   that is not 64 hex characters makes the run "unreadable", not a group.
3. Group runs by distinct digest (at most 20 groups, see Bounds). The digest commits to the flow name
   (`writeField("flow", ...)`), so **one** representative run per group is enough: read its `run:start`
   once (at most 20 reads in all) to get the flow name, and keep the group only if that is the
   **baseline** flow name (the name in the file as the editor opened it), not the edited one. A flow
   renamed in the editor is itself refused by `holdToStart`, so those groups are still listed, with
   that reason.
4. A run with **no `flow:digest`** (including one that journaled `flow:topology` but crashed before
   `flow:digest`, a normal crash window) is not refused by the digest rule (resume records the new
   digest; `flow.go:181-195`). Without a digest it cannot be attributed to a flow without reading its
   `run:start`, so the choice is: **count them, do not read them**: "K unfinished runs record no
   digest yet (all flows, not attributed); resume records the digest of whichever flow resumes them".
   They are not in the affected total. A group under the v1 digest (`DigestV1`) **is** refused by resume (`flow.go:196-199`), so it is in
   the affected total, labelled "recorded under digest v1; resume refuses it": a fact about the run,
   not a change you made. Only runs with no digest are in the separate, not-refused count.
5. File with errors: when the file has errors its typed digest is empty (`flowedit/typed.go:148`), so
   the run panel and this list say "cannot compare: the file has errors (N)" and never "differs" or
   "affected". Typed mode, no errors: a group is affected iff its digest differs from the file's, and
   the line carries the basis, as `DiffView.BasisLine` does ("the digest rests on the program's
   Registry" or "rests on the description file, not checked against the program"). Structural mode:
   read `flow:topology` once per group (point read), check it, diff with `DiffTopologies` (section 2:
   any digest-covered difference or an order difference means refused). A group whose topology cannot
   be read or trusted is listed as "could not be compared".

The result is worded as "would be refused on its next drive (resume, recovery or restart)", never
"affected now": a live process keeps the flow it started with.

Bounds: at most 20 distinct digests are examined and the structural diffs share a total budget of
10 s; more are reported as "N more digests not compared". Counts are "at least":
`UnfinishedLimit` stops at the limit, so the page says "checked 5000 (there may be more)", never "of
M". Concurrency: one computation at a time (single-flight) per server; a second request while one runs
waits for and shares its answer. The shared computation has its own context, cancelled on server Close
or when no waiter remains, so the first caller disconnecting does not kill it for the others. The result is
cached until the file's text changes or "Reload run" is pressed, so repeated clicks or a script cannot
multiply store reads. Output: per digest, the run count and up to 20 run IDs shown as sanitised text.

The "restart with" command shown for a run is `bide edit -run '<id>' <store flags> <file>`, with the ID
quoted for a POSIX shell by the same quoting the Needs attention list's `bide approve` commands use; an
ID with a control character or one the quoter cannot make safe gets no command, just the ID as text.
The command carries no DSN, store path or credential: it says `<store flags>`.

#### 5. What the page claims, exact wording

Same digest (typed; the line ends with the basis, "The digest rests on the program's Registry" or "rests
on the description file `-blocks`, not checked against the program", as `DiffView.BasisLine` words it):
"This file has the topology digest `D`, the same digest run `R` journaled. This
does not say the run followed this file or what any step did; that is conformance (`bide view`)."

No difference seen (structural): "No difference in flow name, entry, node names, blocks, edges, switch
arms or their order between this file and the topology run `R` journaled. Kinds, types and gates are
not compared in this mode, so a change in them would not show. Load the program's blocks (`-blocks`)
to compare digests."

Different: "This file differs from the topology run `R` journaled (N changes the digest covers). The
digest is part of what resume checks, so a run paused under that topology will not resume under this
file." Safety and assert lines are never counted, because the digest ignores them, and order is
(see section 2). Sound in both modes because only digest-covered differences are counted; "same" is
sound only when typed. A typed digest difference with nothing to itemise says "the digest differs in
order or types".

Affected list heading: "Unfinished runs whose topology differs from this file: N of M checked". With
zero: "none of the at least M checked" plus the truncation note, and separately "K runs record no digest yet". Always followed by: "This lists unfinished runs
in the store you gave; runs in other stores are not seen."

Not claimed: conformance, what steps did, that a run "will" fail (it will not resume; that is the
digest rule in `run-viewer.md`, "The digest"), or anything about runs after saving. Nothing here is
executed or changed in any run; the store is read-only.

File has errors (typed mode, empty digest): "Cannot compare: the file has errors (N), so it has no digest.
Fix them to compare with run `R`." Never "differs", never "refused", never "same", on the run panel or
in the affected list.

#### 6. Security

- Loopback, Host check, `Origin` equality and session token on every `/api` call: unchanged. The run
  endpoints are `POST /api/run` and `POST /api/affected`, both behind `apiGuard`, JSON bodies capped
  by `MaxBodySize`. Responses never contain a DSN, a file path of the store or a credential; store
  errors are classed (`errClass`) as the viewer does.
- Read-only: only `OpenReadOnly` constructors (sqlite file read-only, postgres read-only sessions),
  and the code reaches the store through `UnfinishedHeads` and `Journal.Get`, over the opened
  `OpenReadOnly` store. A test hands the code the real opened store type and asserts `Insert` is
  rejected (no wrapper to test instead of the real thing).
- Hostile journal. The journal is data written by whoever ran the flow, possibly adversarial:
  names, block names, gate approver ids and run IDs are arbitrary strings: rendered with
  `textContent` and the existing server-side escapers/`printable` filters (control, bidi and
  non-printing characters), never HTML. Size: `flow:topology` is capped at `MaxFileSize` (1 MiB).
  The loader caps bytes only, so node and edge counts get **new explicit caps**: a run topology of
  more than 1000 nodes or 4000 edges and arms is refused with a note (measured: `diffChanges` takes
  0.66 s at 1000 nodes and 3.2 s at 4000 with disjoint names). The digest string must be 64 hex
  characters. The affected list is bounded as in section 4.
  The digest check proves the topology is the one the digest names (self-consistent), not that the
  run ran it or who wrote it; the page says "as the journal records it" and points at `bide view
  -sth` for the journal's own proof. Responses carry no record payload, no DSN, no store path and no
  credential (a test marshals every response of every endpoint against a store seeded with
  recognisable secrets and asserts none appears); reserved runs are refused by `-run`. A real digest mismatch (`JournaledTopology` err): the topology is
  not used for baseline, path or list; the page says "run R journaled a topology that does not match
  its digest; it is not shown". A run with `flow:topology` and **no** `flow:digest` is a normal crash
  window, not a mismatch: "this run has not recorded its digest yet, so there is nothing to compare;
  resume will record whichever flow resumes it", and no baseline or path is drawn. A run with no
  `flow:topology` (older run): "this run journaled no topology", no baseline; in the affected list it
  is a group by its digest when it has one, compared by topology only if it has one (structural mode
  needs it, section 4.5) and by digest alone in typed mode.
- No new write paths, no new file reads, no new network listeners. A store is not reachable from the
  page except through the fixed startup arguments.
- Live runs change while the page is open: a snapshot is taken on first request and re-read only on an
  explicit "Reload run" press, so a baseline never changes under an edit; the page shows when the
  snapshot was read.

#### 7. Accessibility

Same bar as the editor (design note, "Accessibility"; the keyboard run and screen-reader pass at the
phase gate). New items: the run panel is a labelled region with a heading; the baseline selector is a
real radio group; run marks have text and a glyph beside colour, and pass the contrast check in all
three themes plus forced-colors; the off-canvas path list is an ordered list in run order; the
affected list is a table with header cells and a caption; "Check unfinished runs" and "Reload run"
announce results through the existing `aria-live` region ("checked 120 runs, 3 differ"); focus moves
to the result heading, never stolen on load; the nodes already carry `aria-label`, extended with the
run mark ("charge, gate 2 of 3, ran 3 times"). Added to the existing browser a11y tests, themes test
and the keyboard-only run.

#### 8. New names, flags and JSON

Flags (bide edit): `-run`, `-db`, `-postgres`, `-postgres-env`, `-journal` (same text as `bide view`).

Exported (listed even though most are in internal packages; items marked P are published API):
- P `plan.InspectTopology(Topology) ConfigReport` (question 1).
- `runview.DiffTopologies` and `runview.NormalizeForRun` (internal; `BuildDiff` calls the former), `renameTopology`.
- `runview.StoreFlags` (shared flag registration and open; internal) and `runview.OpenSource`.
- `runview.RunPath` (path projected onto a topology) and `runview.UnfinishedHeads`
  (unfinished runs with flow name and digest, by two point reads each).
- No exported `flowedit` names (approved). The run's types (`runBaseline`, `affected`, `affectedGroup`)
  are unexported in `flowedit`; the wiring is in section 8a.

##### 8a. Wiring without public flowedit API

`flowedit.Option` is `func(*options)` with an unexported `options`, so `cmd/bide` cannot build an option
of its own. Decided: a registered constructor, following the repo's
`journalhook`/`planaccess` pattern, with zero exported names.
- New internal package `internal/editrun` (importable by `cmd/bide` and by `flowedit`, which already
  imports `internal/runview`) holds the interface `editrun.Run` (the opened source, the run ID, the
  point-read head reader) and `var NewOption func(Run) any`.
- `flowedit`'s `init` sets `editrun.NewOption` to a function that returns a `flowedit.Option` which puts
  the run into that `Server`'s own options.
- `editcmd` opens the store, then appends `editrun.NewOption(run).(flowedit.Option)` to its options
  before `flowedit.New`.
There is no mutable session global: the run is per-Server state, so two editors in one process each
have their own run, and tests run in parallel and reset nothing. The only package-level variable is the
constructor, written once at init. A test asserts `editrun.NewOption` is non-nil once `flowedit` is
linked (so a missing import fails loudly, not as a silently absent feature) and that the returned value
is a `flowedit.Option`. A library user of `flowedit` who does not use `editcmd` gets today's editor, so
embedding hosts have no run comparison.
Rejected: (1) a set-once global hook read by `New` (`editrun.Set`), for five reasons: (a) it holds
per-session data (a store, a run ID, a cancel scope) in a global, where the repo's precedents
(`planaccess.RegisterParse`, `journalhook.Begin`) are stateless functions set at init; (b) set-once
cannot serve a second editor in one process, and the cmd tests call `editcmd.run` repeatedly; (c) a
missed reset leaks the run into every later `flowedit.New` in the test binary, with no failure where
it starts; (d) Set then New is not atomic, so a concurrent `New` can read another session's store, and
it forbids `t.Parallel`; (e) the global keeps the store reachable after `editcmd` closes it.
(2) aliasing
`options` to an internal type: puts an internal type in `Option`'s godoc and API reference; (3) a front
handler in `editcmd`: it cannot reuse the unexported `apiGuard`, and `/api/doc` and the typed state are
`flowedit`'s; (4) an exported `flowedit.WithRun`: public API, not approved.

JSON, as built in the first pull request that serves it: `POST /api/run` request `{"text","renames","reload"}`,
response `{"configured":false}` without `-run`, and otherwise
`{"configured":true,"run":{"id","flow","state","state_text","digest","records","read_at"},"status","claim","basis","run_digest","file_digest","changes":[],"weakened":[],"strengthened":[],"renamed":[],"notes":[]}`
where `status` is `same`, `none_seen`, `differs`, `cannot`, or one of `no_topology`, `no_digest`, `mismatch`,
`too_large`, `unreadable`, `not_found`, and `claim` is the sentence the page shows. The page learns whether
there is a run from the first answer, so `/api/doc` and the exported `Doc` and `View` types gain no field
(a field on them would be a new exported name). The run's path (`path`, later) and `POST /api/affected`
(`{"text"}` in, `{"listed","checked","truncated","unreadable","can_list","groups":[{"digest","runs","ids","command"}],"differs","notes"}`
out) are added by the pull requests that build them; no existing field changes meaning.

#### 9. Cost and PR split

About 4 to 6 working days of agent time plus reviews; adversarial review gate applies (journal).

1. **PR 1, topology as a diff side** (`plan.InspectTopology` with its contract, corpus conformance
   test and fuzz; `runview.DiffTopologies` with the normalisation, order comparison and
   digest-covered-only counting; structural-mode kind/type/gate blanking; `renameTopology`
   with a mutation test per field; tests that safety and assert lines never show, that a node swap
   shows as "order changed", that a pure rename is a rename, and that `bide check -diff` output is
   unchanged; the 1000-node / 4000-edge cap). Plan export approved.
2. **PR 2, `-run` and the stores** (shared store flags, read-only open, `/api/run`: baseline panel,
   verdict wording, digest check, hostile-journal tests, `-help`, guide section). No path yet.
3. **PR 3, the path on the canvas** (projection, renames, off-canvas list, a11y, browser tests).
4. **PR 4, affected runs** (point reads, recovery filter, grouping, caps, single-flight and cache, page table, big-store test with a synthetic
   store that counts reads).
Docs, godoc, CHANGELOG fragment, i18n pointers and the design note's phase status update land in
each PR it touches.

#### 9a. Tests, by PR

PR 1: safety-only and assert-only differences never appear; node swap and edge swap read "order
changed" and "differs"; pure rename (structural) is a rename, not remove plus add; a resolved Go gate or
Single gate is not "gate removed" in structural mode; `renameTopology` mutation per field (Entry, Nodes,
Edges, Branches, Joins, Loops), including rename swaps (a<->b) and chains (a->b, b->c);
`InspectTopology` corpus conformance and fuzz (dangling edges, duplicate names, self loops); empty
typed digest reads "cannot compare"; the 1000/4000 cap; `bide check -diff` golden output unchanged;
API reference (`go test ./internal/tools/...` with -update), doccheck on every new exported name, and
the CHANGELOG fragment.
PR 2: `editrun.NewOption` is registered once flowedit is linked and returns a `flowedit.Option`; two
editors in one process keep separate runs; the guard test enumerates every `POST /api` route and
checks token, Origin and Host on each, including the new ones; the secrets-in-journal test (no payload,
DSN, path or credential in any response or error); reserved runs refused; a concurrent writer to the
store is not blocked by the read-only open; a digest-mismatch and a digest-less topology draw nothing;
oversized topology refused; the keyboard and screen-reader checks of the run panel (a11y test in PR 2).
PR 3: projection tests (block mismatch, removed node, missing taken edge, loop bound changed, session
rename, shape-matched rename), text for every mark, themes and forced-colors.
PR 4: a synthetic store that counts reads (two point reads per run, one topology read per distinct
digest, none for a finished run), the recovery filter (cancelled and aborted excluded), digest-less
runs counted apart, single-flight and cache (N concurrent requests, one scan), cancellation, the cap
of distinct digests, and unsafe run IDs getting no command.

##### 9b. More tests

Also: PR 3's block-mismatch case names the run's block and the file's block in text; PR 4 adds a
`-journal` export saying it cannot list the store, a v1 group counted as affected, and the no-digest
read count (zero `run:start` reads for them); every new function gets a mutation check, run with -v
and the PASS lines read (protocol).

PR 1 also: `go test ./internal/tools/...` (`TestAPIReference`), doccheck, a byte-identical golden check
of `bide check -diff` over the existing diff tests after the `BuildDiff` refactor, the order-corpus test
and the no-aliasing/normalise-copy tests of section 2.
PR 2 also: the run panel's browser a11y test (the panel ships here); hostile-journal cases: oversized
topology, 65+ char or non-hex digest, bidi and control names, shell-metacharacter run ID, topology
without digest, digest without topology, v1 digest, mismatched digest, `Topology.Flow` different from
the `run:start` flow; the file-with-errors "cannot compare" state; the basis label (Registry vs
description).
PR 4 also: cancelled/aborted exclusion, reserved-run skip, flow rename listing old-name runs, the
no-digest count, single-flight with the first caller disconnecting, cache invalidation on text change,
cross-store (SQLite file and the Integration Postgres job), a concurrent writer not blocked by a scan,
the 20-digest and 10 s budgets, and the guard test over every `POST /api` route.
Across PRs: one test per sentence of section 5 that fails if the sentence would be false (same digest,
no difference seen, differs, the three counts of the affected list).

#### 10. Decisions (all taken as recommended)

1. `plan.InspectTopology` is a published export; reachability through gates has one implementation.
2. The run is an informational panel; it never gates a save. The git baseline is the policy.
3. The run is a snapshot with an explicit reload, not a live follow: a baseline must not move under an edit.
4. The affected list covers unfinished runs only, capped at 5000, with no prefix flag in the first version.
5. A store without `-run` is allowed (the affected list alone).
6. In structural mode the page says "no difference seen" and names what is not compared, never "equal".
7. The page cannot switch runs; the run and the store are fixed at startup, and the list prints the restart command.

## Risks

1. **Round-trip fidelity.** The splice engine is the most delicate code in the project's tooling.
   Mitigations: verify-before-return, refuse rather than guess, the property and fuzz tests, and the
   phase 0 go or no-go. A wrong splice that passes verification would still be seen in the pre-save
   diff and the review.
2. **Users expect a free canvas.** A graph tool that will not let you move a node reads as
   unfinished. The deterministic layout is a decision, not an omission, and the guide says why (the
   picture equals the one the reviewer sees); it may still disappoint people. No mitigation is
   proposed beyond saying so.
3. **The editor becomes the pitch.** Visual flow tools are a crowded category and competing there
   gives up bide's position. Mitigation: the editor is documented as a tool in guides, not in the
   README's lead; its page leads with the gate and the verdict; the first release is the
   accountability editing, not general graph drawing.
4. **False assurance from assertions.** `assert` is checked by `bide check`, not enforced by the
   runtime. An editor that makes assertions easy to add can make a flow look more governed than it
   is. The assertion panel says "checked in CI, not at run time" beside the list, and the run
   viewer's assertion text should say the same where it does not.
5. **Structural mode passes what `Load` refuses.** A saved file can fail `Load` on a type or a
   block name. The page says "structural check only" wherever it shows a clean verdict, and typed
   mode is the answer for a team that wants more.
6. **A write-capable local server.** The surface is small and specified (see Security), but
   it is the first write endpoint in `bide view`'s family. It is a separate command from the viewer
   so the viewer stays read-only, and it is absent from the audit-only build.
7. **The classifier is the control, and it has gaps** (G1 to G4). If the editor shipped before P1,
   its banner would be weaker than the claim. P1 is a prerequisite for phase 1, not an
   improvement.
8. **Format drift.** A new config key not in the editor makes it a partial editor. The generated
   vocabulary test turns that into a failing test.
9. **YAML library maintenance.** The editor depends on `go.yaml.in/yaml/v3` in a nested module, as
   `cmd/bide` already does; a change of library would be a change to that module only.
10. **Scope creep toward Go.** Requests to edit predicates, step bodies or prompts in the page are
    requests for a different product. The answer is the registry and names.
11. **Accessibility debt.** A canvas can be accessible only if the outline is built first and kept
    complete; the phase gate includes the keyboard and screen-reader passes.
12. **Line endings, encodings and Windows.** CRLF and a byte-order mark must be reproduced exactly
    or refused; the rename-over-existing and path rules differ on Windows. Tested as described under Testing.

## Decisions

The maintainer decided all 12 as recommended on 2026-10-15. They are recorded as written below.

1. **Local only.** Build `bide edit` as a loopback command with a host-agnostic front end, and do
   not build a hosted editor. Recommend yes.
2. **Where the engine lives.** A new nested module `flowedit` (needs a YAML dependency; the root
   stays dependency-free; one more module to tag) rather than inside `cmd/bide`. Recommend the new
   module, because only it lets a Go program embed typed mode.
3. **Round-trip strategy.** Splice-and-verify, decided by a phase 0 spike with a stated bar
   (recommend: at least 95 percent of operations on the corpus touch only the intended lines, no
   unverified output ever), falling back to a `yaml.Node` re-emit with a separate "normalize
   formatting" step. Recommend approving the spike, not the full build.
4. **Frontend stack.** Plain ES modules, no framework or bundler, 150 KB budget, enforced by a
   test. Recommend yes; revisit a small vendored library only if the budget is exceeded.
5. **Weakening rule.** Save of a weakened config needs an explicit confirmation, against a baseline
   of git `HEAD` (or `-base`), and the classifier is extended first for G1 to G3 as its own pull
   request. Includes the rule for deadlines: removing or lengthening a `timeout` counts as
   weakened. Recommend yes to all three.
6. **Budget caps and obligations.** Do not show them in the editor until each has a config key and
   a runtime meaning designed in `plan` first. Recommend not promising them; if wanted, the next
   step is a design note for the key, not editor work.
7. **A read-only `Registry` description in `plan`.** A published API addition, needed for typed
   mode. Recommend yes, in phase 3, with the maintainer reviewing the signature.
8. **Config signing at load time.** Out of scope for the editor. Recommend using signed commits,
   branch protection and the required `-fail-on-weaken` check, and treating a signed-config
   `Load` as a separate runtime design if a bank asks for it.
9. **Derived layout only.** No stored coordinates, no dragging a node to a place. Recommend yes.
10. **Name and build.** The command is `bide edit`, present in the full `bide` binary and absent
    from the `bide_audit_only` build. Recommend yes.
11. **First release.** Phase 1 as described (the accountability editor, structural mode), after
    phase 0 and P1, rather than structure editing first. Recommend yes.
12. **Wails desktop app.** Defer it: start with `bide edit` and the host-agnostic front end, and
    add a Wails wrapper (a separate module reusing the same handler, with its own security review
    of file access) only if users ask for a window. Recommend deferring, not rejecting. The cost
    of waiting is that people who do not want a browser tab get no native file dialog; the cost
    of building now is per-OS builds, signing and notarization, three webview engines to test,
    bank endpoint approval of a new desktop product, and a larger dependency tree.
