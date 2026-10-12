# Run viewer: `bide-view`

`bide-view` serves a read-only web page for one run of a bide journal. It shows what the journal
recorded: an agent run's model turns, tool calls, attempts, approvals and halts; a plan flow's
declared topology with the path the run took; and, when you give it a signed tree head or an
evidence package, whether the journal verifies.

It never writes to the store: it opens a SQLite file read-only (`sqlite.OpenReadOnly`) or reads
exported journals. It listens on a loopback address only (`127.0.0.1` or `::1`) and answers only
requests addressed to one, so the page is not reachable from another machine. The page needs no
network: the binary serves its HTML, CSS and graph, with no CDN and no JavaScript.

![bide-view on a plan flow halted mid-node: END STATE "halted: outcome unknown", the halt with its ResolveHalt snippet, and the topology with the path taken](img/run-viewer-flow-halted.png)

## Install

`bide-view` lives in the `store/sqlite` module, since it reads SQLite files:

```sh
go install github.com/bide-ai/bide/store/sqlite/cmd/bide-view@v0.13.0
```

## Run it

On a SQLite journal (the file `sqlite.Open` writes):

```sh
bide-view -db run.db                  # the run list
bide-view -db run.db -run refund-42   # prints the URL of that run's page
```

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
| `-db <file>` | SQLite journal to read, opened read-only. Give `-db` or `-journal`, not both. |
| `-journal <file>` | An exported journal (`audit.JournalExport` JSON). Repeat it for several runs. |
| `-run <id>` | The run whose URL to print. Without it, the printed URL is the run list. |
| `-topology <file>` | The declared topology of a plan flow, as JSON of `flow.Topology()`, to draw a flow run against. |
| `-digest <hex>` | `flow.Digest()` of that flow, compared with the digest the run journaled. Needs `-topology`. |
| `-sth <file>` | A signed tree head (`audit.SignedTreeHead` JSON) to verify the journal against. Needs `-pubkey`. |
| `-evidence <file>` | An evidence package (`audit.EvidencePackage` JSON, as `audit.Evidence` writes it) to verify. Needs `-pubkey`. |
| `-pubkey <key>` | The log's public key: `<scheme>:<hex>` (`ed25519`, `ml-dsa-65`, `ed25519+ml-dsa-65`), bare ed25519 hex, or a file holding either. |
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

**CONFORMANCE** (plan flows given `-topology`). Whether the run followed the declared topology:
the run is of the topology's flow, every node it recorded is declared, every Switch choice is a
declared arm, and, with `-digest`, the journaled digest equals `flow.Digest()`. From the command line
this check is structural. `plan.Conform` also replays routing, loops and completion; a Go program
that embeds the viewer with the `*plan.Flow` gets that check instead.

**Flow** (plan flows given `-topology`). The declared topology, with each node marked ran, halted,
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
go run . evidence -db a.db -out ev.json     # prints the log public key
bide-view -db a.db -run refund-42 -evidence ev.json -pubkey <log public key>
```

![bide-view on the same run after ResolveHalt: completed, with the evidence package verifying](img/run-viewer-agent-verified.png)

For a plan flow, `examples/plan` crashes in its reserve step and writes its topology with
`-topology-out`:

```sh
cd examples/plan
go run . -db f.db -crash during-reserve -topology-out topo.json   # prints the flow digest
go run . -db f.db                                                 # the resume halts at reserve
bide-view -db f.db -run triage-demo -topology topo.json -digest <flow digest>
```

## What it cannot show

- **A flow's topology is not in the journal.** A flow run journals its flow's name and topology
  digest, not the topology, so you pass the topology with `-topology`. The digest commits to more
  than `Topology` holds (block and predicate names, package paths), so it cannot be recomputed from
  the topology file; pass `-digest` to compare it.
- **Postgres is not supported yet.** `bide-view` reads SQLite files and exported journals. To view a
  run in Postgres, export it with `audit.ExportJournal` and pass `-journal`.
- **Proof needs `-sth` or `-evidence`.** An `audit.AuditedStore` publishes its signed heads to an
  external anchor, not to the store, so the viewer verifies only a head or package you give it.
- **Plan flows have no approval gates.** The `plan` runtime refuses a gated node at `Build`, so a
  flow run has none to show; gate the agent tool a node calls instead.
- **A halted call's `Safety` is not journaled.** Only a result records it. A halted call has an
  attempt marker, which the engine writes only for a side effect, so the page shows it as one.
- **A paused run journaled by an earlier release** has no `approval-pending:` record, so its gate is
  listed only once a decision is recorded.
