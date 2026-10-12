# Run viewer: `bide-view`

`bide-view` serves a read-only web page for one run of a bide journal. It shows what the journal
recorded: an agent run's model turns, tool calls, attempts, approvals and halts; a plan flow's
declared topology with the path the run took; and, when you give it a signed tree head or an
evidence package, whether the journal verifies.

It never writes to the store: it opens a SQLite file read-only (`sqlite.OpenReadOnly`), reads a
Postgres journal in sessions the server holds read-only (`postgres.OpenReadOnly`), or reads
exported journals. It listens on a loopback address only (`127.0.0.1` or `::1`) and answers only
requests addressed to one, so the page is not reachable from another machine. The page needs no
network: the binary serves its HTML, CSS and graph, with no CDN and no JavaScript.

![bide-view on a plan flow halted mid-node: END STATE "halted: outcome unknown", the halt with its ResolveHalt snippet, and the topology with the path taken](img/run-viewer-flow-halted.png)

## Install

`bide-view` is its own module, `github.com/bide-ai/bide/cmd/bide-view`, which carries the SQLite
and Postgres drivers the core module does not:

```sh
go install github.com/bide-ai/bide/cmd/bide-view@v0.13.0
```

## Run it

On a SQLite journal (the file `sqlite.Open` writes):

```sh
bide-view -db run.db                  # the run list
bide-view -db run.db -run refund-42   # prints the URL of that run's page
```

On a Postgres journal (the database `postgres.Open` writes), given as a pgx DSN:

```sh
bide-view -postgres 'postgres://user:pass@host:5432/db' -run refund-42
```

Every session it opens has `default_transaction_read_only` on, so the server refuses any write.

On exported journals (`audit.ExportJournal(ctx, journal, runID)`, marshaled to JSON, the input
`bide-audit prove` reads), one file per run:

```sh
bide-view -journal refund-42.json -journal refund-43.json
```

It prints the URL it serves (by default `http://127.0.0.1:8484/`) and runs until Ctrl-C. A run's
page is at `/run?id=<run>`, and the same view as JSON at `/run.json?id=<run>`.

## Flags

| Flag | Meaning |
|---|---|
| `-db <file>` | SQLite journal to read, opened read-only. Give one of `-db`, `-postgres` and `-journal`. |
| `-postgres <dsn>` | Postgres journal to read, a pgx DSN; read in read-only sessions. |
| `-journal <file>` | An exported journal (`audit.JournalExport` JSON). Repeat it for several runs. |
| `-run <id>` | The run whose URL to print. Without it, the printed URL is the run list. |
| `-topology <file>` | A declared topology, as JSON of `flow.Topology()`, to draw a flow run against instead of the topology the run journaled: for a run started before `flow:topology` was journaled, or to check a run against another flow. |
| `-digest <hex>` | `flow.Digest()` of the `-topology` flow, compared with the digest the run journaled. Needs `-topology`. By default it is computed from the `-topology` file when the file carries the digest's inputs (a topology exported by this version does). |
| `-sth <file>` | A signed tree head (`audit.SignedTreeHead` JSON) to verify the journal against. Needs `-pubkey`. |
| `-evidence <file>` | An evidence package (`audit.EvidencePackage` JSON, as `audit.Evidence` writes it) to verify. Needs `-pubkey`. |
| `-pubkey <key>` | The log's public key: `<scheme>:<hex>` (`ed25519`, `ml-dsa-65`, `ed25519+ml-dsa-65`), bare ed25519 hex, or a file holding either. |
| `-approver-keys <file>` | The approvers' public keys, the file `bide-audit verify-approvals` reads: a JSON object of approver id to key text, such as `{"ops":"ed25519:ab12..."}`. With it, each m-of-n gate's signatures are verified. Needs `-sth` or `-evidence`. |
| `-need <k>`, `-approvers <a,b,c>` | The policy you expect (k of these approvers, in policy order). With them the signatures are verified against this policy, as `bide-audit verify-approvals -need -approvers` does, and a gate whose recorded policy differs fails. Need `-approver-keys`. |
| `-config <file>` | A flow config (`.json`, or `.yaml`/`.yml`) to show instead of runs; see [Check a flow config](#check-a-flow-config). Takes none of the run flags. |
| `-check` | With `-config`: serve nothing, print each problem as `file:node: message`, and exit 1 if any is an error. |
| `-strict` | With `-check`: exit 1 on a warning too. |
| `-addr <host:port>` | Where to listen: a loopback IP or `localhost`, with a port. Default `127.0.0.1:8484`. Any other host is refused. |

Input files are read strictly, as `bide-audit` reads them (`audit.UnmarshalStrict`): a duplicate or
unknown field is an error. `bide-view -h` prints the same list. Exit status: 0 after a clean stop or
`-h`, 1 if an input cannot be read or the server fails, 2 for a usage error.

## The page

![bide-view on an agent run halted after a crash mid-refund, with its 2-of-3 approval gate](img/run-viewer-agent-halted.png)

**END STATE.** The run's state as `agent.Status` reads it from the journal, with two refinements:

- **halted: outcome unknown**: the run has a live attempt marker with no result (a side effect
  started and its outcome was lost, for example to a crash). A resume returns
  `*agent.OutcomeUnknown` until the outcome is recorded. See Halts below.
- **waiting for approval**: the run paused at an approval gate (it journaled `approval-pending:`
  for the call) and the gate has not decided.
- **started**: the run is in progress, as far as the journal shows.
- **completed**, **aborted**, **cancelled**: the run's first end marker, with the reason a
  cancellation or abort recorded.

The card also gives the record count, the last plan node the run reached, and the model's last text.

**AUDIT PROOF.** Given `-sth` or `-evidence` and `-pubkey`, whether the journal verifies, checked
with the audit package's own verifiers, as `bide-audit` does:

- with `-sth`: the head's signature and timestamp, and an inclusion proof of the last record it
  covers, built from the journal shown (`audit.ProveRecord`, `ProofBundle.Verify`). A head that does
  not commit to this journal does not verify.
- with `-evidence`: `EvidencePackage.Verify` (the seal, the key, the run binding, and every proof in
  the package, listed one per line), and the same check of its head against the journal shown.

The card says how many records the head covers: records written after it are listed as not yet
covered. Without `-sth` or `-evidence` the card says so.

With `-approver-keys` as well, the card gives a line per m-of-n approval gate whose tally is
journaled: whether its approvals verify, from `audit.VerifyApprovals`, the check
`bide-audit verify-approvals` runs. It verifies the proofs of the request, every decision the gate
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

**Flow** (plan flows). The declared topology, with each node marked ran, halted,
attempted or not reached, the last node outlined, and the edges and Switch arms the run took drawn
solid. The Mermaid text of the same graph is below it.

**Halts.** Each operation whose attempt marker is live with no result: the tool call or step, its
marker, when it was attempted, and the `agent.ResolveHalt` call that records its outcome once you
have confirmed it (from the provider's record, say):

<!-- docsnip: skip illustrative: the page fills in the run and the operation -->
```go
agent.ResolveHalt(ctx, journal,
	agent.HaltRef{RunID: "refund-42", Op: agent.OpRef{Kind: agent.OpTool, ID: "refund-1", ToolName: "refund"}, Cause: agent.HaltCrashed},
	agent.Outcome{Result: "$120 to order 42", Evidence: map[string]string{"provider_refund": "re_42"}})
```

**Approval gates.** One row per gated tool call: the gate (k of n and the approvers, or one
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

**Timeline.** Every record in journal order: the header, `run:start` (kind, input, tools), model
turns (text, tool calls with arguments, finish reason, model), attempt markers and re-attempts, tool
results with the `Safety` they ran under (read-only, idempotent, side effect) and whether
`ResolveHalt` reconciled them (with the evidence), approval decisions, the approval-pending record and
tally, plan nodes and Switch choices, and the end marker.

## Demo: crash, resolve, verify

`examples/approval` refunds an order behind a 2-of-3 approval gate, against a SQLite journal. Crash
it mid-refund, view the halt, resolve it, and view it again with its evidence verifying:

```sh
cd examples/approval
go run . run -db a.db                       # the gate pauses: waiting for approval
bide-view -db a.db -run refund-42           # the gate listed, 0 of 3 decided
go run . approve -db a.db -as ops
go run . approve -db a.db -as finance
go run . run -db a.db -crash                # the refund fires; the process dies before its result is journaled
go run . run -db a.db                       # the resume halts: outcome unknown
bide-view -db a.db -run refund-42           # the halt, its HaltRef, and the gate approved 2 of 3
go run . resolve -db a.db                   # ResolveHalt from the provider's record, then the run finishes
go run . evidence -db a.db -out ev.json     # prints the log public key; writes approver-keys.json
bide-view -db a.db -run refund-42 -evidence ev.json -pubkey <log public key> -approver-keys approver-keys.json
```

![bide-view on the same run after ResolveHalt: completed, with the evidence package verifying](img/run-viewer-agent-verified.png)

For a plan flow, `examples/plan` crashes in its reserve step. The run journals its topology, so no
flag is needed to draw it:

```sh
cd examples/plan
go run . -db f.db -crash during-reserve
go run . -db f.db                                                 # the resume halts at reserve
bide-view -db f.db -run triage-demo
```

## Check a flow config

`bide-view -config` shows a flow definition (a `plan` config, `.json` or `.yaml`) without running
anything: the declared graph, its Switch arms and loops, each node's approval gate written as
`2 of 3: ops, finance, risk`, and every problem `plan.InspectConfig` finds, marked on the node, edge
or arm it belongs to, with the full message in the Problems table and in each mark's tooltip. An
edge to a name no node declares draws that name as a dashed "not declared" node.

```sh
cd examples/plan
bide-view -config flows/refund.yaml                 # the page; no problems
bide-view -config flows/refund-typo.yaml -check     # for CI: no server, one line per problem
```

The screenshot below is `examples/plan/flows/refund-typo.yaml`, whose last edge names `reciept`.

<picture><source media="(prefers-color-scheme: dark)" srcset="img/run-viewer-config-problem-dark.png"><img src="img/run-viewer-config-problem-light.png" alt="bide-view -config on a refund flow whose edge names an undeclared node, marked on the edge"></picture>

With `-check` it prints `file:node: message` for a node's problem, `file:from->to: message` for an
edge's or an arm's, and `file: message` for the config as a whole, and exits 1 if any is an error
(0 otherwise; a warning is printed as `warning:` and does not fail the check unless you add
`-strict`; an unreadable file exits 1, a usage error 2).

**Structural, from the command line.** The command cannot import your Go types, blocks or tools,
so it checks what needs no Go code: the config reads as `plan.Load` reads it (strictly, version 1);
node names are unique; each wiring element is one edge, switch or join; every edge, arm, `else`,
join input and the entry name a declared node; every `safety` is known; and every approval block is
a valid policy. An approval block is still reported as an error, since `plan.Load` refuses it: plan
flows do not enforce approval gates yet. A switch with no `else` is a warning: a value no arm
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

## What it cannot show

- **A flow run started before `flow:topology` was journaled** has only its digest, so pass its
  topology with `-topology` (JSON of `flow.Topology()`, which `examples/plan -topology-out` writes)
  to draw it.
- **Proof needs `-sth` or `-evidence`.** An `audit.AuditedStore` publishes its signed heads to an
  external anchor, not to the store, so the viewer verifies only a head or package you give it.
- **Plan flows have no approval gates.** The `plan` runtime refuses a gated node at `Build`, so a
  flow run has none to show; gate the agent tool a node calls instead.
- **A halted call's `Safety` is not journaled.** Only a result records it. A halted call has an
  attempt marker, which the engine writes only for a side effect, so the page shows it as one.
- **A paused run journaled by an earlier release** has no `approval-pending:` record, so its gate is
  listed only once a decision is recorded.
