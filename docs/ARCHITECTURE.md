# Architecture

How the runtime is put together is in the [design doc](design/design.md); the vocabulary is in
[Concepts](CONCEPTS.md); the module split is in [module structure](reference/module-structure.md).
This page is the map of the source.

## Reading the code

Test files sit next to the code they test, as Go convention has it, in the same directory with a
`_test.go` suffix, named for the area they cover. The lists below name the source files to open first,
in reading order.

### The core path

A run is a sequence of journaled steps. Read in this order: the journal, the step that memoizes one
operation, the claim that keeps a side effect at most once, the lease that keeps two drivers off one
run, the loop that drives it, the approval gate and the check before a gated call executes, the
notifier that tells a person, and the audit log that proves afterwards what happened.

1. **Journal and step**: [journal.go](../agent/journal.go) (the `Store` port and the `Journal` the engine writes through), [record.go](../agent/record.go) (what one journal entry is), [step.go](../agent/step.go) (`Journal.Step`, named-step memoization), [keys.go](../agent/keys.go) (the rules that keep journal keys apart).
2. **Claims and leases**: [attempt.go](../agent/attempt.go) (the attempt marker: an exclusive claim before a side effect), [lease.go](../agent/lease.go) (run leasing, so one node drives a run), [halt.go](../agent/halt.go) (a non-idempotent call whose outcome is unknown halts; `ResolveHalt` settles it).
3. **The loop**: [loop.go](../agent/loop.go) (the single loop behind `Run` and `Stream`), [runapi.go](../agent/runapi.go) (`Run`, `Resume`, `Stream`, `RunTyped`), [recovery.go](../agent/recovery.go) (`Recover` and `RecoverLoop` re-drive unfinished runs), [pause.go](../agent/pause.go) (`Sleep`, `Interrupt`, `Signal`/`Await`, the durable pauses).
4. **Approvals and the exec gate**: [approval.go](../agent/approval.go) (the m-of-n gate's data model and policy check), [toolexec.go](../agent/toolexec.go) (tallying decisions and calling the tool), [exec_gate.go](../agent/exec_gate.go) (the re-check, immediately before the call, that the gate still holds), [deadline.go](../agent/deadline.go) (approval timeouts).
5. **The notifier**: [notify.go](../agent/notify.go) (public surface), [notify_engine.go](../agent/notify_engine.go) (delivery, claims on events), [notify_record.go](../agent/notify_record.go) (the record vocabulary of the trail), [notify_route.go](../agent/notify_route.go) (routing and escalation), then the webhook sender in [notify/webhook.go](../notify/webhook.go).
6. **Audit log and proofs**: [audit/audit.go](../audit/audit.go) (the hash-chain head and signing), [audit/merkle.go](../audit/merkle.go) (RFC 6962 trees over the journal), [audit/sth.go](../audit/sth.go) (signed tree heads), [audit/proof.go](../audit/proof.go) (`ProofBundle`), [audit/anchor.go](../audit/anchor.go) (what turns integrity into tamper-evidence).

### agent

The core package: the engine, the Run API, the approval gate and the notifier. Open first:

- [agent.go](../agent/agent.go): the package doc and the `Agent` type.
- [options.go](../agent/options.go): `New` and every option.
- [journal.go](../agent/journal.go), [record.go](../agent/record.go), [step.go](../agent/step.go): the journal and `Step`, as above.
- [loop.go](../agent/loop.go) and [runapi.go](../agent/runapi.go): the loop and the entry points.
- [tool.go](../agent/tool.go) and [toolexec.go](../agent/toolexec.go): the `Tool` interface and how a call executes.
- [approval.go](../agent/approval.go) and [exec_gate.go](../agent/exec_gate.go): the gate.
- [lease.go](../agent/lease.go) and [recovery.go](../agent/recovery.go): ownership and crash recovery.
- [memstore.go](../agent/memstore.go): the in-memory `Store`, the smallest complete implementation of the port.

Then, by topic: [compensate.go](../agent/compensate.go) and [saga.go](../agent/saga.go) (undo), [parallel.go](../agent/parallel.go) (durable fan-out), [session.go](../agent/session.go) (multi-turn), [subagent.go](../agent/subagent.go), [replay.go](../agent/replay.go), [identity.go](../agent/identity.go) (who acted, for whom), and the `approval_*.go`, `gate_*.go` and `notify_*.go` families. The [agenttest](../agent/agenttest) and [storetest](../agent/storetest) directories hold test helpers and the conformance suite a `Store` must pass.

### audit

Turns the journal into evidence a third party can check offline. Stdlib only. Open first:

- [audit.go](../audit/audit.go): the package doc, `Head`, `Sign`, `VerifySignature`.
- [signing.go](../audit/signing.go): signature schemes (Ed25519, ML-DSA-65, hybrid; P-256 is in [signing_p256.go](../audit/signing_p256.go)).
- [merkle.go](../audit/merkle.go), [consistency.go](../audit/consistency.go), [sth.go](../audit/sth.go): trees, consistency proofs, signed heads.
- [proof.go](../audit/proof.go) and [step.go](../audit/step.go): `ProofBundle` and `ProveStep`.
- [absence.go](../audit/absence.go): proofs that something did not happen.
- [anchor.go](../audit/anchor.go), [audited_store.go](../audit/audited_store.go): publishing heads and continuous anchoring.
- [evidence.go](../audit/evidence.go) and [runcert.go](../audit/runcert.go): the `EvidencePackage` and the proof-carrying `RunCertificate`.
- [receipt.go](../audit/receipt.go): decision receipts.

Then: [grant.go](../audit/grant.go) and [delegate.go](../audit/delegate.go) (signed grants, delegation), [earned.go](../audit/earned.go), the `identity_*.go` and `cert_*.go` files (certificate-bound approvers), the `notify_*.go` files (verifying the notification trail), [eventstore.go](../audit/eventstore.go) (the event-trail port), and the subpackages [verify](../audit/verify), [crlrefresh](../audit/crlrefresh) and [rfc3161](../audit/rfc3161).

### plan

Handle-based flow builder that lowers to the same journal. Open first:

- [doc.go](../plan/doc.go): the package doc.
- [builder.go](../plan/builder.go), [handle.go](../plan/handle.go), [wiring.go](../plan/wiring.go): declaring nodes and wiring them.
- [build.go](../plan/build.go) and [flow.go](../plan/flow.go): freezing and running a `Flow`.
- [gate.go](../plan/gate.go): approval on a node.
- [conformance.go](../plan/conformance.go): `Conform`, proving a run followed the declared graph.
- [config.go](../plan/config.go), [registry.go](../plan/registry.go), [inspect.go](../plan/inspect.go): YAML and JSON flow configs.
- [assert.go](../plan/assert.go): node safety and policy assertions (the risk map).
- [topology.go](../plan/topology.go) and [render.go](../plan/render.go): the declared graph and its Mermaid form.

### approvals

The intake an application serves so approvers can decide on gated calls. [approvals.go](../approvals/approvals.go) (the handler and its package doc), [limiter.go](../approvals/limiter.go) (rate limiting), [page.go](../approvals/page.go) (the static approve page; its sources are in [page](../approvals/page)).

### notify

Webhook notifier. [webhook.go](../notify/webhook.go) (the sender and package doc), [payload.go](../notify/payload.go) (the v1 payload), [sign.go](../notify/sign.go) (request signing headers), [tlsconf.go](../notify/tlsconf.go) (client certificate reload), [probe.go](../notify/probe.go) (testing a receiver), and the [relay](../notify/relay) subpackage ([relay.go](../notify/relay/relay.go), [slack.go](../notify/relay/slack.go), [pagerduty.go](../notify/relay/pagerduty.go)).

### store

Each store is its own module and implements `agent.Store` (and the event-store port from `audit`).

- [store/sqlite](../store/sqlite): [sqlite.go](../store/sqlite/sqlite.go) (the default single-file journal), [eventstore.go](../store/sqlite/eventstore.go) (event trail), [readonly.go](../store/sqlite/readonly.go) (read-only open for viewers and auditors).
- [store/postgres](../store/postgres): [postgres.go](../store/postgres/postgres.go) (the journal), [migrate.go](../store/postgres/migrate.go) (schema creation and checking), [sqlcheck.go](../store/postgres/sqlcheck.go) (the statement check), [definitions.go](../store/postgres/definitions.go) (the schema the statements depend on), [eventstore.go](../store/postgres/eventstore.go), [readonly.go](../store/postgres/readonly.go).

### govern

Governed multi-agent state on gsm. [govern.go](../govern/govern.go) (the package doc and entry points), [quorum.go](../govern/quorum.go) (k-of-n voting), [federated.go](../govern/federated.go) (federated events), [confluence.go](../govern/confluence.go) (the convergence certificate). Event logs for it: [sqlitelog](../govern/sqlitelog), [postgreslog](../govern/postgreslog), [redislog](../govern/redislog), each one file plus its module files.

### flowedit

The engine behind `bide edit`. [engine.go](../flowedit/engine.go) (the package doc and edit engine), [ops.go](../flowedit/ops.go) (the closed set of edit operations), [ops_structure.go](../flowedit/ops_structure.go) (add, remove, rename and move a node, the entry, wiring order, the flow's name and types), [engine_swap.go](../flowedit/engine_swap.go) (exchange two neighbouring list items, and check it), [ops_wiring.go](../flowedit/ops_wiring.go) (edges, switches, arms and else), [ops_join.go](../flowedit/ops_join.go) (joins and loop bounds), [verify.go](../flowedit/verify.go) (checks an edit changed only what it should), [baseline.go](../flowedit/baseline.go) (does an edit weaken the flow), [save.go](../flowedit/save.go) (preview and atomic save), [server.go](../flowedit/server.go) (the local HTTP page), [view.go](../flowedit/view.go) and [render.go](../flowedit/render.go).

### anchor

Where signed heads are published so the journal's operator cannot rewrite them. The SDK-free part is [audit/anchorobj](../audit/anchorobj) ([anchorobj.go](../audit/anchorobj/anchorobj.go): the object layout, body and the checks a Reader makes; [loaddir.go](../audit/anchorobj/loaddir.go): reading a downloaded copy; [suspect.go](../audit/anchorobj/suspect.go): the tripwire for run IDs that look like personal data). Two modules build on it, each its own module so its cloud SDK stays out of the core: [anchor/awss3](../anchor/awss3) (S3 Object Lock: [anchor.go](../anchor/awss3/anchor.go) publishes, [reader.go](../anchor/awss3/reader.go) reads back and audits the bucket's configuration, [kmskey.go](../anchor/awss3/kmskey.go) checks a KMS key reference, [iam](../anchor/awss3/iam) holds the policies) and [anchor/gcs](../anchor/gcs) (Cloud Storage with a locked retention policy: [anchor.go](../anchor/gcs/anchor.go), [reader.go](../anchor/gcs/reader.go), [store.go](../anchor/gcs/store.go) over the client in [sdk.go](../anchor/gcs/sdk.go)). The `bide-anchor` command, [cmd/bide-anchor](../cmd/bide-anchor) ([main.go](../cmd/bide-anchor/main.go) for `verify`, `export` and `check`; [backend.go](../cmd/bide-anchor/backend.go) and [backend_gcs.go](../cmd/bide-anchor/backend_gcs.go) for `-backend s3|gcs`), reads either.

### internal/runview

The run viewer, the diff and config pages, and the pages `bide check -diff` serves; `cmd/bide` calls it as `bide view`. [cli.go](../internal/runview/cli.go) (flags and startup), [server.go](../internal/runview/server.go) (the HTTP handlers and JSON endpoints), [graph.go](../internal/runview/graph.go) and [svg.go](../internal/runview/svg.go) (the server-drawn flow graph, shared with the editor), [diff.go](../internal/runview/diff.go) and [config.go](../internal/runview/config.go) (the diff and config pages), [attention.go](../internal/runview/attention.go) (the needs-attention list), [live.go](../internal/runview/live.go) (live mode), and [assets](../internal/runview/assets) (the page's HTML, CSS and JavaScript, embedded). The stylesheet's colours, fonts and spacing come from [internal/theme](../internal/theme) through [themefs.go](../internal/runview/themefs.go).

### internal/theme

The theme layer of the visual pages. [theme.go](../internal/theme/theme.go) (the theme file: parsing, strict keys, a shipped base plus overrides), [check.go](../internal/theme/check.go) with [color.go](../internal/theme/color.go) (contrast, colour distance, colour-blindness and polarity checks), [css.go](../internal/theme/css.go) (checked themes to the one generated stylesheet), [fonts.go](../internal/theme/fonts.go) (the fixed font list), [cmd.go](../internal/theme/cmd.go) (`bide theme check`) and [themes](../internal/theme/themes) (the shipped `light`, `dark` and `high-contrast` JSON). The rules and the format are in [Themes](guides/themes.md) and its [design note](design/themes.md).

### cmd/bide

The `bide` command. [main.go](../cmd/bide/main.go) (dispatch), [full.go](../cmd/bide/full.go) (commands that need a store driver; the `bide_audit_only` build uses [auditonly.go](../cmd/bide/auditonly.go) instead), [view.go](../cmd/bide/view.go) (the run viewer). Each subcommand's implementation is a package under [internal](../cmd/bide/internal), for example [approvecmd](../cmd/bide/internal/approvecmd), [editcmd](../cmd/bide/internal/editcmd), [migratecmd](../cmd/bide/internal/migratecmd) and [notifycmd](../cmd/bide/internal/notifycmd). The reference for every command is [reference/cli.md](reference/cli.md).
