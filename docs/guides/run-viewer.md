# Run viewer: `bide view`

`bide view` serves a read-only web page for one run of a bide journal. It shows what the journal
recorded: an agent run's model turns, tool calls, attempts, approvals and halts; a plan flow's
declared topology with the path the run took; and, when you give it a signed tree head or an
evidence package, whether the journal verifies.

It never writes to the store: it opens a SQLite file read-only (`sqlite.OpenReadOnly`), reads a
Postgres journal in sessions the server holds read-only (`postgres.OpenReadOnly`), or reads
exported journals. It listens on a loopback address only (`127.0.0.1` or `::1`) and answers only
requests addressed to one, so the page is not reachable from another machine. The page needs no
network: the binary serves its HTML, CSS and graph, with no CDN. The page is complete without
JavaScript: the two small scripts embedded in the binary only add interaction (hover and click on the
graph, pan and zoom, copy buttons) and, on the page of a run in progress, the live updates (see
[Follow a run in progress](#follow-a-run-in-progress)). The fonts are the system's and there is no
request to any other host.

![bide view on a plan flow halted mid-node: END STATE "halted: outcome unknown", the halt with its ResolveHalt snippet, and the topology with the path taken](img/run-viewer-flow-halted-dark.png)

The quickest way to see the page is the crash demo, which needs no API key and no journal of your
own. It kills an agent mid-charge, resolves the halt, and opens this page on the run, with an
evidence package and the approvers' keys, so the audit proof and the gate's signatures show as
verified:

```sh
bide demo            # -no-browser prints the URL instead; -keep keeps the journal
```

With `-keep` it keeps its temp directory and prints the `bide view` command that reopens the run.

## Install

`bide view` is a command of `bide`, whose module (`github.com/bide-ai/bide/cmd/bide`) carries the
SQLite and Postgres drivers the core module does not:

```sh
brew install bide-ai/tap/bide                         # or:
go install github.com/bide-ai/bide/cmd/bide@latest
```

Every flag of `bide view` and `bide check` is listed in the [CLI reference](../reference/cli.md).

## Run it

On a SQLite journal (the file `sqlite.Open` writes):

```sh
bide view -db run.db                  # the run list
bide view -db run.db -run refund-42   # prints the URL of that run's page
```

On a Postgres journal (the database `postgres.Open` writes), given as a pgx DSN:

```sh
bide view -postgres 'postgres://user:pass@host:5432/db' -run refund-42
```

Every session it opens has `default_transaction_read_only` on, so the server refuses any write.

On exported journals (`audit.ExportJournal(ctx, journal, runID)`, marshaled to JSON, the input
`bide audit prove` reads), one file per run:

```sh
bide view -journal refund-42.json -journal refund-43.json
```

It prints the URL it serves (by default `http://127.0.0.1:8484/`) and runs until Ctrl-C; it does not
open a browser (unlike `bide demo`, it has no `-no-browser` flag). A run's
page is at `/run?id=<run>`, and the same view as JSON at `/run.json?id=<run>`.
A run in progress read from a store also has `/run/live?id=<run>&after=<cursor>`, which its page
polls (see [Follow a run in progress](#follow-a-run-in-progress)).

## The run list

With no `-run`, the page opens on the journal's runs, so you can find the one that halted or is
waiting without knowing its ID:

![bide view's run list over a journal of ten runs: completed, halted, waiting for approval, in progress, aborted and cancelled agent and flow runs, newest first, with state counts and filters](img/run-viewer-run-list-dark.png)

Each row is a run: its ID (a link to its page), its kind (`agent`, `flow` with the flow's name, or
`session turn`), its state, when it started, and its record count. The state is the
one the run's page shows (see END STATE below), so the list and the page never disagree. The state
filters above the table count the runs in each state: completed, **halted: outcome unknown**,
**waiting for approval**, **in progress** (started, not ended, not halted and not waiting), and
**failed or cancelled** (`aborted`, a saga that rolled back, or `cancelled`), and **not started**
for a run whose journal holds nothing but its header. A run made of `Journal.Step` calls alone has no
`run:start`, so the journal does not say what it is: it is listed as **steps only (no run:start)**,
with a state read from what the journal shows. It is **halted: outcome unknown** while a step's
attempt marker is live with no result, **resolved** once `agent.ResolveHalt` has recorded the
outcome of one, and otherwise **steps done**: every step shown has its result. It has no end marker,
so the journal cannot say whether more steps will follow; the done and resolved runs are counted
with the completed ones. Its page has the same Halts panel and ResolveHalt snippet as any run's.
The kind filter and a
run ID filter (a case-insensitive substring) narrow the list further; the filters are in the URL
(`/?state=halted&kind=flow&q=refund`), so a filtered list can be bookmarked. The same list is JSON
at `/runs.json`, with the same query.

**Started** is the time the run's `run:start` records (`RunStart.StartedAt`): when its first drive
began, by that drive's clock, shown in UTC. The list is ordered by it, newest first. A run journaled
by an earlier version, which recorded no start time, shows **none** and is listed after the others,
by run ID. The journal records no end time, so the list shows none.

The list shows 50 runs and a **Show more** link for the next 50. To classify a run the viewer reads
its journal, so one load reads at most 5,000 runs (the first by run ID among those the run ID filter
admits) and says so when there are more; narrow the run ID filter to reach the others. An exported
journal holds one run, so `-journal` lists as many runs as files it is given, and the page says so
when it is one.

To make the journal in the screenshot (ten runs, every state, one journaled with no start time, no
API key), run the test that
writes it and open it:

```sh
BIDE_RUNLIST_DEMO=$PWD/runs.db go test ./cmd/bide -run TestRunListDemo
bide view -db runs.db
```

## Flags

| Flag | Meaning |
|---|---|
| `-db <file>` | SQLite journal to read, opened read-only. Give one of `-db`, `-postgres` and `-journal`. |
| `-postgres <dsn>` | Postgres journal to read, a pgx DSN; read in read-only sessions. |
| `-journal <file>` | An exported journal (`audit.JournalExport` JSON). Repeat it for several runs. |
| `-run <id>` | The run whose URL to print. Without it, the printed URL is the [run list](#the-run-list). |
| `-topology <file>` | A declared topology, as JSON of `flow.Topology()`, to draw a flow run against instead of the topology the run journaled: for a run started before `flow:topology` was journaled, or to check a run against another flow. |
| `-digest <hex>` | `flow.Digest()` of the `-topology` flow, compared with the digest the run journaled. Needs `-topology`. By default it is computed from the `-topology` file when the file carries the digest's inputs (a topology exported by this version does). |
| `-sth <file>` | A signed tree head (`audit.SignedTreeHead` JSON) to verify the journal against. Needs `-pubkey`. |
| `-evidence <file>` | An evidence package (`audit.EvidencePackage` JSON, as `audit.Evidence` writes it) to verify. Needs `-pubkey`. |
| `-pubkey <key>` | The log's public key: `<scheme>:<hex>` (`ed25519`, `ml-dsa-65`, `ed25519+ml-dsa-65`), bare ed25519 hex, or a file holding either. |
| `-approver-keys <file>` | The approvers' public keys, the file `bide audit verify-approvals` reads: a JSON object of approver id to key text, such as `{"ops":"ed25519:ab12..."}`. With it, each m-of-n gate's signatures are verified. Needs `-sth` or `-evidence`. |
| `-need <k>`, `-approvers <a,b,c>` | The policy you expect (k of these approvers, in policy order). With them the signatures are verified against this policy, as `bide audit verify-approvals -need -approvers` does, and a gate whose recorded policy differs fails. Need `-approver-keys`. |
| `-config <file>` | A flow config (`.json`, or `.yaml`/`.yml`) to show instead of runs; see [Check a flow config](#check-a-flow-config). Takes none of the run flags. |
| `-check` | With `-config`: serve nothing, print each problem as `file:node: message`, and exit 1 if any is an error. `bide check <file>...` is the same check, for several files. |
| `-strict` | With `-check`: exit 1 on a warning too (`bide check -strict`). |
| `-diff <old>` | With `-config`: compare it with an earlier version, a file or a git revision; see [Compare two versions of a flow](#compare-two-versions-of-a-flow). With `-check`, print the changes instead of serving the page. |
| `-fail-on-weaken` | With `-diff -check`: exit 1 if an approval gate was removed or weakened. |
| `-addr <host:port>` | Where to listen: a loopback IP or `localhost`, with a port. Default `127.0.0.1:8484`. Any other host is refused. |
| `-color <when>` | `auto` (the default), `always` or `never`: whether the text output (the address to open, and `-check`'s lines) is colored. `auto` colors a terminal only, unless `NO_COLOR` is set to any value or `TERM` is `dumb`; see [Color](#color). |

Input files are read strictly, as `bide audit` reads them (`audit.UnmarshalStrict`): a duplicate or
unknown field is an error. `bide view -h` prints the same list. Exit status: 0 after a clean stop or
`-h`, 1 if an input cannot be read or the server fails, 2 for a usage error.

## The page

Every page opens with a **verdict**: one state in large type, with a glyph and a line of why (so the
state is never carried by color alone), and three or four key numbers under it. On a run page:

| Verdict | When |
|---|---|
| `VERIFIED` | the run completed, and the audit proof verifies (`-sth` or `-evidence`) with no approval gate's signatures failing |
| `COMPLETED` | the run completed and no proof was given, so nothing is verified |
| `HALTED` | an operation's outcome is unknown: it needs `agent.ResolveHalt` |
| `WAITING FOR APPROVAL` | the run is paused at an approval gate |
| `FAILED` | the run aborted or was cancelled, or it completed but its proof or an approval gate's signatures do not verify |
| `IN PROGRESS` | the run has started and has no end marker yet |

The numbers are the journal records, how many of them the signed head covers, the first approval
gate's approvals (`2/3` with its signature verdict), the operations whose outcome is unknown, the
flow nodes reached, and the tool results. A config page opens with `N POLICY VIOLATIONS` (the
violated assertions), `N CONFIG ERRORS`, `N ASSERTIONS UNDECIDED`, `ALL ASSERTIONS HOLD` or `NO
ASSERTIONS`, in that order. A diff page opens with `APPROVAL GATE WEAKENED`, with each weakened gate
under it, whenever one was weakened, and otherwise with the number of changes. The run list opens
with what needs a person: `N HALTED`, `N WAITING FOR APPROVAL`, `N FAILED OR CANCELLED` or `ALL
CLEAR`. The cards below the verdict are short: a label, a value and a line, with the detail in a
`details` element. The JSON views do not carry the verdict.

![bide view on an agent run halted after a crash mid-refund, with its 2-of-3 approval gate](img/run-viewer-agent-halted-dark.png)

**END STATE.** The run's state as `agent.Status` reads it from the journal, with two refinements:

- **halted: outcome unknown**: the run has a live attempt marker with no result (a side effect
  started and its outcome was lost, for example to a crash). A resume returns
  `*agent.OutcomeUnknown` until the outcome is recorded. See Halts below.
- **waiting for approval**: the run paused at an approval gate (it journaled `approval-pending:`
  for the call) and the gate has not decided.
- **started**: the run is in progress, as far as the journal shows.
- **completed**, **aborted**, **cancelled**: the run's first end marker, with the reason a
  cancellation or abort recorded.

The card also gives the record count and the last plan node the run reached; the run's terminal reason
and the model's last text are under `details`.

**APPROVAL GATE** (when the run has one). Each gate in a line: the gated call or flow node and where
it stands (`refund: approved 2 of 3`), the policy, who approved or denied, who is pending, and
whether the signatures verified (see the Approval gates table below for the full detail).

**AUDIT PROOF.** Given `-sth` or `-evidence` and `-pubkey`, whether the journal verifies, checked
with the audit package's own verifiers, as `bide audit` does:

- with `-sth`: the head's signature and timestamp, and an inclusion proof of the last record it
  covers, built from the journal shown (`audit.ProveRecord`, `ProofBundle.Verify`). A head that does
  not commit to this journal does not verify.
- with `-evidence`: `EvidencePackage.Verify` (the seal, the key, the run binding, and every proof in
  the package, listed one per line), and the same check of its head against the journal shown.

The card says how many records the head covers: records written after it are listed as not yet
covered. Without `-sth` or `-evidence` the card says so.

With `-approver-keys` as well, the card gives a line per m-of-n approval gate whose tally is
journaled: whether its approvals verify, from `audit.VerifyApprovals`, the check
`bide audit verify-approvals` runs. It verifies the proofs of the request, every decision the gate
read, the tally and the result under the log key; checks each decision's signature with the
approver's key against the exact call; recounts with the gate's rule; and requires at least k. With
`-evidence` it checks the package's actions; with `-sth` it builds the same evidence from the journal
under that head (`audit.ApprovalEvidence`). Without `-need` and `-approvers` it checks against the
policy the gate journaled in its tally, and says `(against the recorded policy)`. With them it
checks against the policy you expect, says `(against the expected policy)`, and a gate that
recorded another policy fails with, for example,
`recorded policy 1 of 3 (ops, finance, risk) differs from expected 2 of 3 (ops, finance, risk)`.

**CONFORMANCE** (plan flows). Whether the run followed the declared topology: the run is of the
topology's flow, every node it recorded is declared, every Switch choice is a declared arm, and the
journaled digest equals the topology's. The card says where the topology came from:

- `Topology from journal (digest verified)`: the topology the run's first drive journaled under
  `flow:topology`, drawn only once `plan.JournaledTopology` has checked that its digest
  (`plan.TopologyDigest`) is the run's `flow:digest`. A record whose digest differs is not drawn,
  and the page says why.
- `Topology from -topology`: the file you gave, which overrides the journaled one; `(digest computed from it)`
  when no `-digest` was given and the file carries the digest's inputs.

From the command line this check is structural. `plan.Conform` also replays routing, loops and completion; a Go program
that embeds the viewer with the `*plan.Flow` gets that check instead.

**Flow** (plan flows). The declared topology, drawn on the server as a layered SVG (the layout is
the viewer's own, with no library): each node a rounded box with a glyph for its state (ran, halted,
attempted, waiting for approval, not reached), the last node outlined, and the path the run took
drawn as a flowing dash, which stops under `prefers-reduced-motion`. A gated node wears a badge on its
corner (`2/3`) and shows its gate in its box: `waiting, 1 of 2 approvals` and who is pending, or
`approved 2 of 3` and `signatures verified` (or `signatures NOT verified`), and its tooltip the full
status. The graph is complete without JavaScript. With it, hovering or focusing a node highlights
every path through it, clicking a node (or pressing Enter on it) shows its details under the graph,
and the buttons at the graph's corner zoom and reset it (ctrl or cmd and the wheel zoom too; drag
pans a zoomed graph). The Mermaid text of the same graph is below it.

**Halts.** Each operation whose attempt marker is live with no result: the tool call or step, its
marker, when it was attempted, and the `agent.ResolveHalt` call that records its outcome once you
have confirmed it (from the provider's record, say):

<!-- docsnip: skip illustrative: the page fills in the run and the operation -->
```go
agent.ResolveHalt(ctx, journal,
	agent.HaltRef{RunID: "refund-42", Op: agent.OpRef{Kind: agent.OpTool, ID: "refund-1", ToolName: "refund"}, Cause: agent.HaltCrashed},
	agent.Outcome{Result: "$120 to order 42", Evidence: map[string]string{"provider_refund": "re_42"}})
```

**Approval gates.** One row per gated tool call or plan flow node (named by its key, such as
`node:refund`, with its input in the timeline's `approval requested` row): the gate (k of n and the approvers, or one
decision), who approved, who denied, who is pending, and a status such as
`approved (2 of 3 approvers; 2 needed)`, `denied (...)` or `pending (0 of 3 approvers; 2 needed; ...)`.
Once the gate decides, the counts are the tally it journaled; before that they are the signed
decisions recorded so far, whose signatures the gate checks when the run resumes.

The **Signatures** column says whether the approvals were verified (see AUDIT PROOF):

- `2 of 3: signatures verified (ops, finance) (against the recorded policy)`: `audit.VerifyApprovals`
  held with the keys given.
- `signatures NOT verified: ...`: it did not, with every reason: a decision that did not count and
  why (a signature that does not verify under the approver's key, an approver with no key), the
  count below k, or evidence that is inconsistent (a decision the gate read and the evidence omits, a
  recount that disagrees with the tally).
- `included in the signed log; signatures not checked (pass -approver-keys)`: the journal verifies
  under `-sth` or `-evidence`, but no approver keys were given.
- `signatures not checked (pass -approver-keys, with -sth or -evidence)`: no proof was given.
- `signatures not checked: the gate has not journaled its tally yet`: the gate is still open.
- A one-decision gate (`agent.SingleApproval`, recorded with `agent.Approve`) is not signed, so
  there is nothing to verify.

**Approvers.** For a gate with several approvers, one card each: initials, the approver's id,
whether they approved, denied or have not decided, and, for a decision, whether its signature
verified (`signature verified`; `signature NOT verified` with the reason; `in the signed log,
signature not checked` when a proof was given without approver keys; `signature not checked`
otherwise), the fingerprint of their key (`ed25519:3b6a27bc`, the first 8 hex digits of the SHA-256
of the public key, with `-approver-keys`) and when the head that covers the decision was signed. The
journal records no time for a decision, so the card gives the time of the signed head it is under,
which is a bound: the decision was journaled no later. A one-decision gate is not signed and has no
cards.

**Proof path.** With a signed head (`-sth`) or an evidence package (`-evidence`), a small diagram
of the proof the audit package verified for the last record the head covers: the record's leaf
hash at the foot, the sibling hash at each level of the RFC 6962 audit path with the node it makes,
and the signed root at the top, with its algorithm and signing time. Hashes are shown as their
first 8 hex digits, and the full ones are in each box's tooltip. The page recomputes the climb to
draw the intermediate nodes, and draws nothing if it does not end at the root.

**Timeline.** Every record in journal order, each with a glyph for its type: the header, `run:start`
(kind, input, tools), model turns (text, tool calls with arguments, finish reason, model), attempt
markers and re-attempts, tool results with the `Safety` they ran under (read-only, idempotent, side
effect) and whether `ResolveHalt` reconciled them (with the evidence), approval decisions, the
approval-pending record and tally, plan nodes and Switch choices, and the end marker.

- Each row has an anchor, `#record-N`, with a link glyph on hover, so a record can be linked.
- Each row's **payload** is the record as the journal holds it, indented, in a collapsed `details`
  with its JSON highlighted (a payload over 8 KiB is cut, and says how much); with JavaScript a
  **Copy** button copies it.
- The journal timestamps only the run's start and its attempt markers, so only those rows show a
  time: an offset from the start (`+3.2s`) with the absolute UTC time on hover, and a bar spanning
  from that time to the next timestamped record's (an upper bound on how long the attempt had been
  open, not the operation's duration: the journal records no end time).

## Theme, keyboard and sharing

- **Light or dark.** The pages follow `prefers-color-scheme`. The sun button in the header (or `t`)
  switches, and the choice is remembered in the browser's local storage; where storage is blocked it
  applies to the page only.
- **Keyboard.** `j` and `k` move to the next and previous record (or run, on the list), `Enter`
  opens the highlighted run, `/` goes to the filter (the timeline's, which narrows the rows as you
  type, or the list's run ID), `t` switches the theme, `s` shares, `?` shows this list and `Esc`
  closes it. The shortcuts are off while you type in a field.
- **Tab icon and title.** Each page's title is its verdict and what it is about (`VERIFIED ·
  refund-42 · bide view`), and its tab icon is a square in the verdict's color holding its glyph, so
  a strip of tabs shows which runs need a person.
- **Share this run.** On a run page, the button in the verdict (or `s`) saves a 1200 by 630 PNG of
  the verdict, the key numbers, and the flow graph (or the proof path, with the approvers; a run with
  neither gets its audit proof line instead). The card ends with the project's address. It is drawn in your browser: the page becomes an SVG with its styles written in, the SVG is
  drawn on a canvas, and the canvas is saved. Nothing is sent anywhere, and the server is not asked
  for anything.

  ![The image Share this run saves for a verified run: the VERIFIED verdict, four key numbers, the proof path from the last record to the signed root and the three approvers, at 1200 by 630](img/run-viewer-share-dark.png)

- **Empty and loading states.** A list with no run, a filter that matches nothing and a timeline
  filter with no match each say so. While a live page fetches new records a thin bar runs along the
  top of the window.

## Follow a run in progress

Open the page of a run that is still going and it follows the run: the timeline rows appear as the
records land, and END STATE, the approval gate and its table, and the flow graph change with them
(`started`, then `waiting for approval`, then `completed`), with no reload, so the scroll position
and any open details stay. A **live** mark next to the state says the page is following the run, and
the rows the last update added are highlighted. Once the run journals its end marker
(`run:complete`, `run:aborted` or `run:cancelled`) the page updates a last time and stops: the mark
goes away. A halted or waiting run is still followed, since a resume or `ResolveHalt` adds to it.

![bide view following a gated refund run: started, waiting for approval with none and then one of the two approvals it needs, started again once the refund ran, and completed](img/run-viewer-live-dark.gif)

How it works, and what it costs the store:

- The page's script asks `/run/live?id=<run>&after=<cursor>` about once a second, where the cursor is
  the store sequence (`agent.Entry.Seq`) of the last record the page shows. The viewer answers with
  one `Load` of the run's entries after the cursor, stopped at the first: no entry, no other read.
  Only when there is one does it read the run again and answer with the timeline rows of the new
  records only, the run's state, the next cursor and whether to keep polling (`"live": false` once
  the run has ended). The page then fetches itself and swaps in its content.
- It reads only, as the rest of the viewer does, from SQLite and Postgres alike: the store is opened
  read-only, and the endpoint answers only GET and HEAD on a loopback address.
- An exported journal (`-journal`) does not grow, so its page has no live mark and no live script, and
  `/run/live` is not found.
- Without JavaScript the page is the same, complete as of its load, with a link to reload it while
  the run is in progress. A browser tab in the background does not poll.

To watch one, hold each model turn of the approval example for a few seconds (`-turn-delay`) and
open the page while it runs:

```sh
cd examples/approval
go run . run -db a.db -turn-delay 6s &      # in progress for 6 s, then waiting for approval
bide view -db a.db -run refund-42           # open the URL it prints; the page follows the run
go run . approve -db a.db -as ops           # the gate: 1 of 2 approvals
go run . approve -db a.db -as finance
go run . run -db a.db -turn-delay 6s        # the refund runs; 6 s later the run completes
```

`assets/demo/live.sh` drives these steps and records the GIF above.

## Demo: crash, resolve, verify

`examples/approval` refunds an order behind a 2-of-3 approval gate, against a SQLite journal. Crash
it mid-refund, view the halt, resolve it, and view it again with its evidence verifying:

```sh
cd examples/approval
go run . run -db a.db                       # the gate pauses: waiting for approval
bide view -db a.db -run refund-42           # the gate listed, 0 of 3 decided
go run . approve -db a.db -as ops
go run . approve -db a.db -as finance
go run . run -db a.db -crash                # the refund fires; the process dies before its result is journaled
go run . run -db a.db                       # the resume halts: outcome unknown
bide view -db a.db -run refund-42           # the halt, its HaltRef, and the gate approved 2 of 3
go run . resolve -db a.db                   # ResolveHalt from the provider's record, then the run finishes
go run . evidence -db a.db -out ev.json     # prints the log public key; writes approver-keys.json
bide view -db a.db -run refund-42 -evidence ev.json -pubkey <log public key> -approver-keys approver-keys.json
```

![bide view on the same run after ResolveHalt: completed, with the evidence package verifying](img/run-viewer-agent-verified-dark.png)

For a gated plan flow node, `examples/approval/flow` loads a refund flow whose `refund` node needs 2
of 3 approvers:

```sh
cd examples/approval
go run ./flow run -db f.db                  # the flow pauses before refund
go run ./flow approve -db f.db -as ops
go run ./flow run -db f.db                  # still paused: 1 of 2 approvals
bide view -db f.db -run refund-flow-42      # refund waiting at its 2 of 3 gate
go run ./flow approve -db f.db -as risk
go run ./flow run -db f.db                  # the refund runs once; the flow finishes
go run ./flow evidence -db f.db -out fev.json   # prints the log public key; writes approver-keys.json
bide view -db f.db -run refund-flow-42 -evidence fev.json -pubkey <log public key> -approver-keys approver-keys.json
```

<img src="./img/flow-gate-waiting-dark.png" alt="bide view on a plan flow paused at its refund node's 2 of 3 approval gate: 1 of 2 approvals, pending finance and risk">

<img src="./img/flow-gate-approved-dark.png" alt="bide view on the same flow after a second approval: completed, the refund node approved 2 of 3 with its signatures verified, and the evidence package verifying">

For a plan flow, `examples/plan` crashes in its reserve step. The run journals its topology, so no
flag is needed to draw it:

```sh
cd examples/plan
go run . -db f.db -crash during-reserve
go run . -db f.db                                                 # the resume halts at reserve
bide view -db f.db -run triage-demo
```

## Check a flow config

`bide view -config` shows a flow definition (a `plan` config, `.json` or `.yaml`) without running
anything: the declared graph, its Switch arms and loops, each node's approval gate written as
`2 of 3: ops, finance, risk`, and every problem `plan.InspectConfig` finds, marked on the node, edge
or arm it belongs to, with the full message in the Problems table and in each mark's tooltip. An
edge to a name no node declares draws that name as a dashed "not declared" node.

```sh
cd examples/plan
bide view -config flows/refund.yaml                 # the page; no problems
bide check flows/refund-typo.yaml                   # for CI: no server, one line per problem
```

The screenshot below is `examples/plan/flows/refund-typo.yaml`, whose last edge names `reciept`.

<img src="./img/run-viewer-config-problem-dark.png" alt="bide view -config on a refund flow whose edge names an undeclared node, marked on the edge">

### Risk map

Each node is colored by what a crash does to it, the property that decides whether a resume is
safe:

| Color | Class | On a crash mid-node |
|---|---|---|
| orange | side effect: must not run twice | the resume halts (`*agent.OutcomeUnknown`) and asks for the outcome |
| green | read-only: no external effect | the resume re-runs it |
| blue | idempotent: safe to repeat | the resume re-runs it |
| dashed | safety unknown | not known |

Each class has its own glyph as well as its color (a bolt, an eye, a loop, a question mark). A node with an approval gate has a purple border, a `2/3` badge on its corner and a `gated: 2 of 3` line. Each node's tooltip
says its class, what a crash does, and, for a side effect, whether every path to it passes an
approval gate. The Side effects card sums it up: "1 side-effect step, 1 gated, 0 ungated", where a
gated side effect is one every path from the entry to which passes a gate (its own or an earlier
node's). From the command line a class is known only where the config states `safety`, since the
block's registered `Safety` is Go code; the card then says how many nodes are "safety unknown
without the Registry" and counts only the known side effects. With your Registry (`plan/planview`)
every class is exact, a config's lowering included: a Model node, for example, is read-only unless its config says `safety: side_effect`.

### Policy assertions

A config's `assert` list (see [Policy assertions](plan-flows.md#policy-assertions) for the grammar)
is decided on the graph. The Assertions card lists each verdict (holds, violated, undecided or
invalid), and a violated assertion's counterexample, a shortest path from the entry, is drawn in red
on the graph. `examples/plan/flows/refund-policy.yaml` routes a large refund through `review`, a 2-of-3
gate, to `charge`, and all four of its assertions hold:

<img src="./img/run-viewer-config-risk-dark.png" alt="bide view -config on a refund flow: nodes colored by crash behavior, a 2-of-3 gate on review, and four holding assertions">

`refund-policy-bypass.yaml` adds a `vip` arm from `lookup` straight to `charge`. The assertion
`every path to charge passes approval >= 2 of 3` is violated, and its counterexample,
`validate -> lookup -> charge`, is drawn in red:

<img src="./img/run-viewer-config-assert-dark.png" alt="bide view -config on the same flow with a vip arm that bypasses the gate: the violated assertion and its counterexample path drawn in red">

`bide check` fails on it and prints it as a line, then counts the verdicts:

```text
flows/refund-policy-bypass.yaml:charge: assert[0] "every path to charge passes approval >= 2 of 3" is violated: a path reaches "charge" without passing an approval gate of at least 2 of 3 (counterexample: validate -> lookup -> charge)
flows/refund-policy-bypass.yaml: 4 assertions: 3 hold, 1 violated, 0 undecided or invalid
```

On a terminal the violation is split over lines that fit the terminal: a red ✗ and the assertion,
then the reason, dimmed, then the counterexample path in red with the node it reaches in bold; a
file whose assertions all hold gets a green ✓ (see [Color](#color)). Piped or redirected, it is the
one line above:

<img src="./img/cli-check-violation-dark.png" alt="bide check on a terminal: the violated assertion marked with a red cross, its counterexample validate -> lookup -> charge in red with charge in bold, and the holding file marked with a green check">

An undecided assertion (a node of unknown safety after X, in `no side effect after X`), an
assertion outside the grammar, and one naming an undeclared node are errors as well.

`bide check` (or `bide view -config <file> -check`) prints `file:node: message` for a node's problem, `file:from->to: message` for an
edge's or an arm's, and `file: message` for the config as a whole, and exits 1 if any is an error
(0 otherwise; a warning is printed as `warning:` and does not fail the check unless you add
`-strict`; an unreadable file exits 1, a usage error 2). A violated, undecided or invalid assertion
is an error. `bide check` takes several files and
exits 1 if any of them fails.

**Structural, from the command line.** The command cannot import your Go types, blocks or tools,
so it checks what needs no Go code: the config reads as `plan.Load` reads it (strictly, version 1);
node names are unique; each wiring element is one edge, switch or join; every edge, arm, `else`,
join input and the entry name a declared node; every `safety` is known; and every approval block is
a valid policy (a valid one is drawn on its node). A switch with no `else` is a warning: a value no arm
matches fails the run. The page says "structural check", and shows no digest, which commits to the
blocks' types.

**Full, from your program.** Package `plan/planview` serves the same page with your
`*plan.Registry`, so the check is `plan.Load`'s own: unregistered blocks, predicates and merges (with
near-miss suggestions), edge and arm types, and the whole-graph checks, with In and Out taken from
the entry and terminal nodes. The page then shows the digest a run of the flow journals.

<!-- docsnip: skip illustrative: config and reg are the program's config bytes and plan.Registry -->
```go
ln, err := planview.Listen("127.0.0.1:8485")
if err != nil {
	log.Fatal(err)
}
log.Fatal(http.Serve(ln, planview.Handler("refund.json", config, reg)))
// or, in CI: if planview.Check(os.Stdout, "refund.json", config, reg) { os.Exit(1) }
```

## Compare two versions of a flow

`bide check <file> -diff <old>` compares a flow config with an earlier version of it, for review
before a change ships. It serves a read-only page (as `bide view` does, on `-addr`) that draws the
new flow with added nodes and edges in green, removed ones as dashed red ghosts where they were,
and changed nodes in amber with what changed: the block, the `safety`, or the approval gate
(`approval 1 of 3 -> 2 of 3`, strengthened or weakened). A summary card counts the changes and
gives the digest before and after, and a weakened approval policy is the verdict, `APPROVAL GATE
WEAKENED`, above everything else. `bide view -config <file> -diff <old>` is the same page.

```sh
cd examples/plan
bide check flows/refund-v2.yaml -diff flows/refund-v1.yaml            # the page
bide check flows/refund-v2.yaml -diff main                            # against the file on main
bide check flows/refund-v2.yaml -diff HEAD~1:examples/plan/flows/refund-v2.yaml
bide check flows/refund-v1.yaml -diff flows/refund-v2.yaml -check -fail-on-weaken   # for CI
```

`examples/plan/flows/refund-v2.yaml` sends small refunds to a `review` step instead of skipping them
and raises the refund approval from 1 of 3 to 2 of 3:

<img src="./img/flow-diff-strengthened-dark.png" alt="bide check -diff: review added in green, skip removed as a dashed red ghost, charge amber with approval 1 of 3 -> 2 of 3 strengthened">

The reverse comparison lowers the gate, so the page leads with the weakened policy:

<img src="./img/flow-diff-weakened-dark.png" alt="bide check -diff with the approval gate lowered from 2 of 3 to 1 of 3, called out as approval policy weakened">

**Where `-diff` reads the old version.** When a file has that name, it is the file. Otherwise the
argument is a git revision, read with `git show` from the working directory: `<rev>:<path>` is
git's own form (the path from the top of the repository, or from the working directory when it
starts with `./`), and a bare `<rev>` (`main`, `HEAD~1`) means the config's own path at that
revision. The extension of the path picks YAML or JSON. A file named like a revision wins; write
`./main` or `main:<path>` to be explicit.

**Text, for CI.** With `-check` it serves nothing and prints one line per change, a line per
weakened gate and a summary, each prefixed with the new file's name:

```text
flows/refund-v1.yaml: changed node charge: approval 2 of 3 -> 1 of 3 (weakened)
flows/refund-v1.yaml: added node skip: block skip
flows/refund-v1.yaml: removed node review: block review
flows/refund-v1.yaml: added edge validate -> skip: else
flows/refund-v1.yaml: removed edge validate -> review: else
flows/refund-v1.yaml: approval policy weakened: charge: approval 2 of 3 -> 1 of 3
flows/refund-v1.yaml: 5 changes from flows/refund-v2.yaml (nodes: 1 added, 1 removed, 1 changed; edges: 1 added, 1 removed, 0 changed); digest not computed: the check is structural (the digest commits to the blocks' Go types; see plan/planview)
```

On a terminal the lines are colored as the page is (added green, removed red, changed amber), the
weakened gate moves to the top, in bold red, and the summary's counts and digest go on lines of
their own:

<img src="./img/cli-diff-weakened-dark.png" alt="bide check -diff -check on a terminal: approval policy weakened called out first in bold red, then the changes with added in green, removed in red and changed in amber">

A diff is not a failure: it exits 0, unless `-fail-on-weaken` finds a weakened gate (exit 1). An
old or new version that cannot be read as a flow config exits 1; a version with config errors is
compared as far as it reads, with a note (`bide check <file>` lists the errors). `-diff` compares;
it does not check, so `-strict` is a usage error with it.

**What counts as weakened.** A gate is weakened when some set of approvals the new gate accepts
would not have passed the old one: the gate was removed (a removed gated node counts, so renaming a
gated node reads as removing its gate), its need was lowered, an approver was added without raising
the need to match, or an approver was replaced. Precisely, need k of approvers A becomes need k' of
approvers B, and the change is weakened unless k' minus the number of approvers in B and not in A
is at least k. The reverse is strengthened; the same approvers and need in another policy order is
reordered. The check compares each node's gate; it does not reason about paths, so a new arm that
routes around a gated node shows as an added edge, not as a weakened policy. Read the graph for
that.

**The digest.** From the command line the comparison is structural, as `bide check` is, so it
shows no digest. Package `plan/planview` serves the same page (`DiffHandler`) and text
(`CheckDiff`) with a `plan.Registry` for each version, and then shows the digest each version's
runs journal: a changed digest means a run paused under the old flow will not resume under the new
one. `plan.Load` refuses a block registered and never used, so each version takes the Registry the
program had at that version.

<!-- docsnip: skip illustrative: old, cfg, oldReg and reg are the program's two config versions and Registries -->
```go
weakened, err := planview.CheckDiff(os.Stdout,
	planview.Version{Name: "main:refund.json", Data: old, Registry: oldReg},
	planview.Version{Name: "refund.json", Data: cfg, Registry: reg})
if err != nil || weakened {
	os.Exit(1)
}
```

A GitHub Action that posts the `-check` text as a pull request comment is a planned follow-up; until
then, a workflow step can run the command and post its output.

## Color

`bide check`, `bide view`, `bide demo`, `bide init` and `bide audit` color their text output when it
goes to a terminal: green and a ✓ for what passed, red and a ✗ for what failed, amber for a warning,
a halt or an approval gate, bold cyan for an address or a command to run next, and dim for detail
such as a digest. The words are the same with or without color, so nothing is told by color alone.

Output to a file or a pipe, as in CI, is the plain text shown above, byte for byte. Each of these
commands takes `-color=auto|always|never` (default `auto`). `auto` colors only a terminal, and not
when `NO_COLOR` is set to any value or `TERM` is `dumb`; on Windows it colors only a terminal known
to read ANSI escapes (Windows Terminal, ConEmu, ANSICON, VS Code, or a `TERM` such as
`xterm-256color`). `-color=always` colors even a pipe (for a log viewer that renders ANSI), and
overrides `NO_COLOR`. Help (`-h`) is plain.

## What it cannot show

- **A flow run started before `flow:topology` was journaled** has only its digest, so pass its
  topology with `-topology` (JSON of `flow.Topology()`, which `examples/plan -topology-out` writes)
  to draw it.
- **Proof needs `-sth` or `-evidence`.** An `audit.AuditedStore` publishes its signed heads to an
  external anchor, not to the store, so the viewer verifies only a head or package you give it.
- **A halted call's `Safety` is not journaled.** Only a result records it. A halted call has an
  attempt marker, which the engine writes only for a side effect, so the page shows it as one.
- **A paused run journaled by an earlier release** has no `approval-pending:` record, so its gate is
  listed only once a decision is recorded.
