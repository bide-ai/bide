# Audited erasure of journal content (redaction)

Status: approved by the lead on 2026-10-17 except decision 4. PR 1 (the store capability) is merged; PR 2a (the signed trail, `audit.RedactJournal`, `audit.VerifyRedactions` and `bide audit redactions` with name attestation) is built; the command (2b), legal hold (2c), and the view and docs sweep (2d) are not. Decision 4 (end markers) was approved by the user and is PR 3, not built. Additions agreed with the lead: the evidence-before-redaction rule is the first thing in the guide and the command must refuse, or require an explicit flag, when no signed head covers the run; the command's report names the copies it cannot reach (Postgres dead tuples until VACUUM, SQLite free pages and WAL, WAL archives, replicas, backups); the docs say plainly that personal data must not be put in govern events; follow-up: tombstones for event leaves in `bide_events`, so `-keep-event-copies` is not the only answer.

## Problem

The journal holds personal data: the run's input (`run:start`), model replies and tool-use arguments (`@llm/*`, model records), tool results (`tool:*`), signals, approval decisions, the final result (`run:complete`). A bank must be able to erase it on request or when retention ends, and afterwards the evidence that the run happened, and what it committed to, must still verify.

Today readers understand a tombstone (`{"redacted":{"leaf_hash":"<hex>","at_ms":<ms>}}`, `agent.Record.Redacted`, `audit.journalLeafHashes`, the `redacted (anchored)` receipt verdict, `NoteRedacted` in decision coverage), and `docs/design/api-v1.md` (A6, D2) reserved an optional `Redactor` capability. Nothing writes a tombstone. This note designs the writer, the guard rails, the accountability trail, and the verification.

What a tombstone can and cannot do follows from the hash. A leaf is `SHA-256(0x00 || tag || stored bytes)` and the stored bytes include a 32-byte random salt. The tombstone keeps that hash, so every other proof and every signed head still verifies. The record's content, and its salt, are gone for good. Because the salt is random and erased, the retained hash is not a handle for guessing the content. Evidence packages and receipts made earlier still carry whatever they disclosed (see Scope limits).

## 1. API

### Store capability (not in the core port)

`agent.Store` is unchanged, so A6 stays true for the engine: the engine never obtains a Redactor. A store opts in, found with `agent.Capability`, as `Lister` and `Leaser` do:

```go
// Redactor is the optional capability that replaces one entry's bytes with a tombstone.
type Redactor interface {
	// Redact replaces runID's entry name with tombstone, in one transaction, only if all hold:
	//  - the entry's current bytes equal current (compare-and-swap, so a changed row is refused);
	//  - SHA-256 leaf hash of current (JournalLeafHash) equals the tombstone's leaf_hash;
	//  - tombstone is a well-formed tombstone (agent.CheckRedaction);
	//  - name is not a protected kind (section 2);
	//  - the run holds an end marker (run:complete, run:aborted, run:cancelled), read in the same
	//    transaction, and no live lease on the run belongs to another holder.
	// It reports whether it replaced the entry; an entry that already holds a tombstone with the
	// same leaf hash is not an error and replaces nothing (idempotent, so a crashed pass resumes).
	Redact(ctx context.Context, runID, name string, current, tombstone []byte) (bool, error)
}
```

Redact never changes `seq`, `run_id` or `name`, so position and `Load` order are unchanged (A2, A4). The store re-derives the hash itself from its own bytes inside the transaction; a caller cannot write a wrong leaf hash, which would silently break every root over the run. The shared checks (`CheckRedaction`: tombstone shape, hash match, protected-name list, end marker present) live once in `agent` and each store calls them, with a conformance suite in `agent/storetest` (`storetest.Redactor`) that both stores and MemStore must pass. To keep the hash in one place, `audit.JournalLeafHash` delegates to a new `agent.LeafHash` and a test pins that they agree.

Store specifics:

- SQLite: `UPDATE steps SET data=? WHERE run_id=? AND name=? AND data=?` plus the end-marker `EXISTS`, in one `BEGIN IMMEDIATE` transaction through the same write gate as `Insert`.
- Postgres: one call of a `SECURITY DEFINER` function `bide_redact_v1(run, name, current, at_ms)` that the schema owner's migration creates (never replaced, search_path pinned, definition checked by `Open`), which repeats every check server-side and builds the tombstone itself. It takes no advisory lock: the replacement is one `UPDATE` of a row that keeps its `seq`, so it does not contend with inserts for a position, and it is safe because an end marker, once stored, stays, and the lease check is a read at the statement's snapshot. The application role has only `SELECT, INSERT` on `bide_steps` (`docs/guides/postgres.md`), so it cannot erase. The redaction tool uses a third role, `bide_redactor`, with `EXECUTE` on the function and no privilege on any table (a first design gave it `UPDATE (data)`, which let raw SQL rewrite any row; review found it): that is the first layer of separation of duties.
- MemStore: implements it for tests.

### Engine-side helper

`agent.Redact(ctx, j *agent.Journal, plan RedactionPlan) (RedactionResult, error)` runs a plan over a store that has the capability and returns `ErrConfig` wrapping "this store cannot redact" if it does not. `Plan` builds a plan without writing (the dry run). A plan is data: the run, and for each selected record its position (index in `Load` order), name, kind, stored length, and leaf hash. Its digest is `SHA-256` of a canonical encoding of that list plus the run ID, so "what was reviewed" is a single value (`plan_digest`).

### Selection

By run, and for each run one of:

- `-name <step key>` (repeatable): exactly those records;
- `-kind <model|tool_result|value|signal|approval|...>` and `-prefix <key prefix>`: records of a kind or key prefix (a prefix such as `tool:` or `@llm/`);
- `-payloads`: the preset "every record whose content is payload and that no verifier or later run needs" (section 2's redactable list). This is the whole-run erasure the bank usually wants.
- `-subruns`: also select the run's sub-runs (`<run>><call>`), found with `Lister` and the sub-run ID prefix. Off by default; with a flag the plan lists each sub-run separately.

Protected names are never selectable. A selector that matches one is not silently skipped: the plan lists it as `kept (protected: <why>)`, so the operator sees what remains.

### CLI

The new write command is `bide journal redact`, in the full build only. It is not a `bide audit` verb: `bide audit` is the offline, read-only verifier (it also ships in the `bide_audit_only` build, and `audit` and `agent` hold no os or net I/O per the gsm gate). Verification stays in `bide audit`, with one new read-only verb, `bide audit redactions` (section 4). Command layout follows `bide notify` (flag set per verb, `-db <sqlite file>` or `-postgres-env <VAR>` for the DSN, never an argv secret).

```
bide journal redact -db FILE|-postgres-env VAR -run ID (-name K ... | -kind K ... | -prefix P ... | -payloads) [-subruns]
    -reason TEXT -ticket ID           (required: both are recorded in the trail)
    -dry-run                          (lists the plan and its digest, writes nothing; the default)
    -execute -plan-digest HEX         (writes; the digest must equal the freshly computed plan's)
    -trail-db ... -sign-key ... -head-out FILE   (the accountability trail, section 3)
    -authorization FILE -authorizer-pubkey KEY   (optional, section 3)
    -json
```

`-dry-run` is the default: with neither flag the command prints the plan and exits 0. A write requires `-execute` and the `-plan-digest` of a plan the operator saw, which also makes the two-step flow (one person plans, another executes) work. The dry run prints position, name, kind, length, leaf hash, and the reason a record is kept, and never record content (the dry run itself must not become a copy of the data). Exit statuses: 0 done or nothing to do, 1 refused or failed (a typed refusal, below), 2 usage.

Refusals are explicit and typed (each wraps `agent.ErrConfig` or `ErrStorage` and the command prints the code): `not-terminal`, `live-lease`, `protected-record`, `hash-mismatch`, `changed-concurrently`, `no-selection`, `event-copies-exist`, `trail-unavailable`, `authorization-required`. A refusal writes nothing.

## 2. Safety

### Terminal runs only

A redaction is refused unless the run holds an end marker (`firstEnd`: `run:complete`, `run:aborted` or `run:cancelled`, by row name, checked in the redaction's own transaction) and no other holder has a live lease on it (`Leaser`; a store without leases skips that check and the doc says so). `Journal.open` already refuses to drive a run holding a tombstone (`ErrConfig`), which is the other half: a redacted run cannot restart. Two gaps are named, not hidden:

- A straggler driver of an ended run can still `Insert` a new name (for example `@spend-late/<id>`). A tombstoned run then gains a record; readers handle it, and the next drive is refused. Not a safety problem, noted in the docs.
- Sessions (`<id>>@journal`, `turn/*`, `start/*`, `from/*`) have no end marker, so they are never terminal. v1 refuses them (`not-terminal`) and the docs say so. A closed-session notion is a separate design.
- A saga run that ended `run:aborted` after compensation is terminal; a run with `run:cancel-requested` and no end marker is not.

### What reads which record (checked in code)

| Reader | Reads | Needs content of |
|---|---|---|
| `Journal.open`, Resume, Recover, RecoverLoop | all records; `Lister.ExcludeHolding` by row name | everything, for a live run; a terminal run is excluded by the end marker row name, which a tombstone keeps |
| `Status`, `Cancel`, `endedErr`, `firstEnd` | end markers by `Kind == StepValue` and name; the result of `run:complete` | end markers (a tombstone has no `Kind`, so `firstEnd` would not see it) |
| claims, attempt markers, `not_started`, `ResolveHalt` | `attempt:*`, `not-started:*`, `tool:*`, `@saga/*` | attempt and outcome of every call; only a live or halted run needs them |
| saga compensation | `@saga/args/<call>`, `tool:*` Safety | only while compensating (non-terminal) |
| approvals, tallies, exec holds, identity | `approval:*`, `approval-tally:*`, `approval-exec-hold:*`, `approval-identity:*`, `approval-status:*` | live gates; afterwards `verify-approvals`, `verify-decision-coverage`, receipts |
| notifier (`notify_pending`, `notify_route`, `notify_resolved`) | unfinished runs' journals (approval-pending, halts) | live runs only; terminal runs are skipped before reading |
| `audit` Root, Head, Prove, Consistency, Evidence | leaf hashes (tombstone-aware) | the record being proven |
| `audit` ProveAbsent, PoliciesUsed, run certificate, `EventLogFromJournal`, `PersistJournal` | every record's content | all records (they refuse `ErrRedacted`) |
| `bide view` / `internal/runview` | all records for display | the content shown |

The projections in the fifth row are the real constraint. A journal holding any tombstone cannot produce a new absence key set, used-policy set, run certificate or event stream (`audit.ErrRedacted`), because the content that would define the projection is gone. Heads and packages made earlier verify forever; new ones cannot be made. So: **make every evidence artifact you will need (absence head, run certificate, evidence packages, event trail) before redacting, and the command checks for it** (section 4: it warns, and with `-require-evidence-from` refuses, if no signed head covers the run's current size).

### Record kinds

Never redactable (a verifier, the engine, or a later reader needs the content or the structure):

| Kind / key | Why |
|---|---|
| `@journal` header (`StepHeader`) | `checkFirst` reads the format of every journal before any other record |
| `run:complete`, `run:aborted`, `run:cancelled` | `firstEnd` identifies the end by `Kind` and name; a tombstone has neither, so the run would read as running |
| `run:cancel-requested`, `run:limits:*` | cancellation and budget state |
| `attempt:*`, `attempt:not-started:*` (`StepAttempt`, `StepNotStarted`) | the at-most-once evidence; a verifier of "was it attempted" needs the marker |
| `StepSagaFail` records, `outcome_unknown` | the rollback and unknown-outcome evidence |
| `approval-tally:*`, `approval-identity:*`, `approval-status:*`, `approval-exec-hold:*`, `approval-deadline:*` | what an approval count and its revocation basis rest on (identity-bound gates); no free-form personal content |
| `audit:*` | the audit package's own leaves |
| `flow:*` | topology digest |

Redactable (payload bytes), in the `-payloads` preset:

| Kind / key | What it holds |
|---|---|
| `run:start` (the input `Message`, `Principal`, `Ext`) | the prompt; the person who started the run |
| `StepModel` records (`@llm/*` and model turns) | model replies, tool-use arguments |
| `StepToolResult` (`tool:*`) | tool results, `Evidence` |
| `StepValue` user steps (named steps, `node:*`, `@subrun/*`, `@retrieval/*`, `@spend*` only if payload) | user step results |
| `StepSignal` (`signal:*`, `chan:*`) | signal payloads |
| `approval:*` decision records | approver identity and decision; **opt in only** (`-include-approvals`, off by default): the decision is accountability evidence about staff, not customer data, and `verify-approvals` needs it. Receipts and decision coverage handle the tombstone. |

Two consequences to decide (see questions): `run:complete` holds the run's final result, which is often personal data, and must stay protected until readers learn that a tombstone named like an end marker is still an end. And the retained row names can contain identifiers (`approval:<call>:<approver>:<digest>`, developer-chosen step names, encoded tool-use IDs); names stay, and the docs say so.

### Atomic per record, verified before write

Per record, one transaction: re-read the row; confirm it equals the bytes the plan read (`changed-concurrently` otherwise); recompute the leaf hash of those bytes and compare with the tombstone's (`hash-mismatch`, a store bug or tampering signal); confirm the end marker; update. A run's records are redacted in position order, each its own transaction, so a crash leaves a prefix redacted and the rest intact; re-running the same plan finishes it (the plan digest is stable because it is computed from leaf hashes, and an already-redacted row is a no-op that still matches its planned leaf hash). Per-run atomicity across all records is not claimed, because a crash mid-run is recoverable and recorded (section 3), while one giant transaction would hold the run lock for as long as the run is large.

## 3. Accountability

Every redaction is recorded in a separate append-only trail, signed and anchored by the machinery that signs the notification trails, and never in the redacted run's journal (which would be a write to a run that is over, and itself redactable).

- **Trail.** One trail per deployment, `redact:@log` (a single trail so deleting an entry is a position gap under a signed head, like the notifier's open trail). Leaves are appended through `audit.EventStore` (the `bide_events` table of the same database, or another one given by `-trail-db`; a separate database is recommended so the redaction role cannot rewrite its own trail). `audit.AuditedEventStore` signs and anchors each growth with the log signer and `Anchor`, exactly as for `notify:*` trails, and `bide audit` verifies against the heads.
- **Record** (`bide.audit.redaction-record.v1`, new type, the leaf bytes stored verbatim): `phase` (`begin`/`end`), `plan_digest`, `run_id`, `records` as `[{position, name, kind, leaf_hash, length}]`, `reason`, `ticket`, `executor` (key ID of the signer plus the OS user name as reported, informational), `authorization` (optional, below), `at` (the tool's clock), the journal size at planning, `bide` version. For `end`: counts redacted, already redacted, refused, and an error string.
- **Order.** `begin` is appended first (and `-execute` fails with `trail-unavailable` if it cannot be, writing nothing), then the redactions, then `end`. A crash between leaves a `begin` with no `end`: that is reported by `bide audit redactions` as `INCOMPLETE` and finished by re-running the same plan. The reverse order (redact, then record) could leave an erasure with no record, which is the failure an auditor must never see.
- **Heads.** The tool signs and anchors the trail with `AuditedEventStore`. The CLI has no anchor service of its own (the in-memory `MemAnchorLog` is a reference); it writes each signed head to `-head-out` (append, one JSON per line) for the operator's anchor, and library callers pass an `Anchor`. Without a head the trail is `UNANCHORED` in the report, as for notification trails.
- **Separation of duties.** Three layers, in order of cost:
  1. *Database roles* (cheap, recommended always): the application cannot erase (no `UPDATE` on `bide_steps`); a distinct `bide_redactor` role can, and cannot touch the trail database; the trail's signer is not the application's.
  2. *Plan digest handoff* (free): one person runs the dry run and approves the digest, another executes it. Recorded as `executor` only; nothing enforces two people.
  3. *Signed authorization* (optional, `-authorization FILE`): a second person signs `{plan_digest, reason, ticket, not_after, authorizer key id}` with a key of their own (`bide.audit.redaction-authorization.v1`, any `audit.Signer`, so an HSM or KMS key works). `redact -execute` verifies it against `-authorizer-pubkey`, refuses an expired one, a different plan digest, or an authorizer key equal to the executor's signing key (`authorization-required`), and copies it into the `begin` record, so the verifier checks it later. A deployment sets "authorization required" in the config of the tool (or the flag `-require-authorization`); a bank would expect it on for any erasure of approval records or of a regulated run. It is optional in v1 so the first PR is not blocked on it; the record format has the field from the start so adding it later changes no format.

### Names are attested by the trail

A tombstone carries the record's name so that an export (bytes without row keys) can say which record it replaced. The name is outside the leaf hash, so nothing in the journal, an export or an evidence package authenticates it: it can be rewritten without breaking a root or a signature. Until the trail exists, every reader that acts on it labels it (decision coverage: "redacted, name not attested"; `RunStatus.StartRedacted`). The signed, anchored `redact:@log` trail records each redaction's run, position, name and leaf hash, so `bide audit redactions`, and the evidence path when given the trail and its head, check every tombstone's name against it: names are attested exactly when the trail verifies.

## 4. Verification afterwards

New read-only verb `bide audit redactions` (in `bide audit`, so also in the audit-only build): inputs are the trail export (`audit.ExportEventTrail`-style, as `bide.audit.notify-trail-export.v1` is), its signed heads and the public key, and the journals (`-db`, opened read-only, or `-journal` exports). It reports:

- the trail is complete under its heads (a deleted entry is `records were deleted`, as for notify trails);
- every `begin` has an `end`, or is `INCOMPLETE`;
- every tombstone found in the journals has a trail record naming its run, position and leaf hash (a tombstone with none is `UNACCOUNTED`, which finds a redaction made by hand with SQL; a tombstone's `at_ms` is compared with the record's time);
- every listed record is a tombstone with that leaf hash (a record listed and still holding content is `NOT ERASED`);
- each authorization verifies, and its signer differs from the executor.

Readers, and the test each gets (all new, all run against both stores where a store is involved):

| Reader | Handles a tombstone by | Test |
|---|---|---|
| `Journal.History/Get`, `DecodeStoredRecord` | `Record{Name, Redacted: true}` (exists) | per-kind round trip through each store |
| `Journal.open`, `Resume`, `Run` of a redacted run | refuses `ErrConfig` (exists) | redact then Run: refused, journal unchanged |
| `Recover`, `RecoverLoop` | skips: the end marker row is kept and excluded by name | redact a terminal run, Recover does not drive it |
| `Status`, `Cancel` | reports the end marker, which is protected | redact `-payloads`, Status unchanged |
| `audit.Root/Head/Prove/VerifyInclusion/Consistency` | tombstone leaf hash (exists) | redact, old signed head and consistency still verify; proof of a kept record verifies; proof of a redacted one is refused |
| `audit.Evidence` / `verify-evidence` | the package is independent of the database | package made before still PASSes |
| `ProveAbsent`, `PoliciesUsed`, run certificate, `EventLogFromJournal`, `PersistJournal` | refuse `ErrRedacted` (exists) | a test per entry point pinning the error and message |
| `ExportJournal` and `JournalExport.Journal()` | keep the tombstone; today `recordName` returns `""` for it | export a redacted run and re-read it; fix the lost name |
| `verify-decision-coverage`, `verify-receipts` | `NoteRedacted`, `redacted (anchored)` (exist) | end to end with `-include-approvals` |
| `bide audit verify-run`, `notifications` journal cross-check | must pass or report a defined verdict | redact a run with notifications, report unchanged for the kept records |
| `bide view` (`runview`: list, live, attention, server) | **to build**: render a record as `redacted <date>, leaf <short hash>` with no content, and keep the run listed with its end state | view tests in `internal/runview` and `cmd/bide` |
| `bide approve list/show` (reads `History`) | must not fail on a tombstone | test |
| `chaos`, `eval`, `examples` (read histories) | not user-facing; checked by grep and a test where they decode records | none unless they decode |

The acceptance test is: run the sample (`bide sample`), make evidence and signed heads, `bide journal redact -payloads -execute`, then the full `bide audit` suite (`verify-evidence`, `verify-run`, `verify-receipts`, `verify-decision-coverage`, `notifications`, `redactions`) on the redacted database: PASS (`redacted (anchored)` where a receipt names a redacted record), with `ErrRedacted` the documented result for new projections.

## 5. Scope limits (stated in the docs)

The command erases journal rows in the one database it is given. It cannot reach:

- **Exported journals and evidence packages** (`audit.ExportJournal` files, `bide.audit.evidence.v6` packages, proofs): they carry record bytes and salts verbatim. A package holds the bytes of the actions it names. Erasure of those copies is the operator's.
- **Approver receipts** (held by the approver, they include the call's arguments).
- **Backups, snapshots, replicas, WAL archives**, the SQLite `-wal` file and free pages until vacuumed (the tool runs `PRAGMA wal_checkpoint(TRUNCATE)` and states that `VACUUM` is the operator's; Postgres dead tuples remain until vacuum, and a `VACUUM FULL` is the operator's; a `pg_dump` is not touched).
- **The event store** (`bide_events`, `audit.PersistJournal`): a copy of event content, append-only, with no tombstone for event leaves. v1 refuses a run that has event leaves unless `-keep-event-copies` is given, which is recorded in the trail; erasing them is a separate design.
- **Notification trails** (`notify:*`): they hold event keys and run IDs, not payloads; the docs say what they hold.
- **Logs, traces, metrics** (`trace` with content capture), the application's own tables, the model provider's retention, and any tool's downstream system.
- **Govern logs** (`govern/sqlitelog`, `postgreslog`, `redislog`): each `EventLog` stores opaque caller-defined event strings per entity, appended and folded in order by the governor. bide defines no payload there, so what they hold is the application's; they have no tombstone and replacing an event changes the folded state, so they are out of scope and the docs say a deployment that puts personal data in governed events must not. Recommendation: no change.
- **Row names** stay (step keys, encoded tool-use IDs, approver names inside `approval:*` keys) and so do the retained leaf hash, `seq` and `at_ms`.
- A record erased here is gone from the journal's own proof: a later "prove this record" is impossible by design.

No sentence in the docs says the feature makes a deployment compliant with any law. It says what the command does and does not reach.

## 6. Formal models and protocol

- The store capability sits outside the engine's protocol-marked code. `Journal.open` (`protocol:claims Open`) already refuses a redacted run and is unchanged. If decision 4 (end markers) is accepted, `firstEnd` and `endedErr` change and those are protocol-marked, so the `lifecycle`/`claims` models need a check and likely a `Protocol-Impact` line with a stated reason.
- `docs/design/formal-models.md` records that invariant 3 (Insert first-writer) "holds by construction ... a future redaction action must not break it for a live run", and lists "redaction" under the deferred store-contract model 4. Recommendation: add a small `Redact` action to the lifecycle model with the invariant "a redaction happens only in a run with an end marker, and keeps position and leaf hash", checked by TLC (cheap, shardable). It would be the first model of the redaction rule and needs no engine change. Without it the claim rests on the conformance tests only.
- Protocol-Impact for PR 1: `none` for the engine if the model addition is declined, with the reason that no marked code changes.

## 7. Tests and evidence plan

Test first, per rule 4.

- `storetest.Redactor` conformance (MemStore, SQLite, Postgres): replaces and keeps seq/order; refuses non-terminal (no end marker), live lease of another holder, protected names, hash mismatch (corrupt tombstone), changed-concurrently (row differs from `current`), malformed tombstone; idempotent on repeat; unknown run/name error; read-only handle refused (`OpenReadOnly`).
- Concurrent append: a writer inserting new names while redaction runs: the redaction's transaction either sees the end marker or refuses; no row other than the target changes; a concurrent redaction of the same row has one winner. Both stores, `-race`.
- Crash mid-plan (kill after k of n): re-run completes, trail shows begin without end then a complete second pass.
- Acceptance: the full `bide audit` PASS above on the sample, both stores.
- Refusals through the CLI: live run, protected record named, `-payloads` keeps the protected list, wrong `-plan-digest`, missing `-reason`, trail unavailable, authorization missing/expired/self-signed.
- Reader tests from section 4.
- Mutation plan (after committing, rule 1): drop the end-marker check; drop the CAS (`data = ?`); drop the hash comparison; swap position order; remove the protected-name check; skip the `begin` append; drop the executor-differs check; each must fail a named test. Windows `go vet` per module; gsm gate; doccheck, docsnip, changelog lint.
- Break-it review on the store PR (A6, journal, leases): can a tombstone be forged to pass `CheckRedaction`; can a row be redacted that was not terminal at commit; can a plan digest be replayed against changed content.

## PR plan (rule 17, one at a time)

1. Store capability: `agent.Redactor`, `CheckRedaction`, `LeafHash`, `agent.Redact` and plan, MemStore, SQLite, Postgres, `storetest`, the `ExportJournal` name fix, the Postgres `bide_redactor` role in docs/tests; docs: security model, KNOWN-LIMITATIONS, retention guide, api-v1 status, apiref regenerated, changelog.
2. `bide journal redact` (dry run, execute, trail, heads, refusals), `bide audit redactions`, `bide view` rendering, CLI reference regenerated, README and i18n where they cover retention, recipe, getting-started.
3. Signed authorization (if kept optional in 1).
4. Model addition (if accepted) and, separately, end-marker redaction (if accepted).

## Planned follow-ups

- **Erasing a session transcript.** A session's journal and turn runs never end, so v1 refuses them (`ErrRedactSession`), and chat transcripts are where personal data concentrates. Needs a notion of a closed session (an explicit end record the session writes and every reader of a turn respects), then the same tombstone path over `turn/`, `start/`, `from/` records, which are protected today for that reason. Separate design and PR.
- **Tombstones for event leaves** in `bide_events`, so `-keep-event-copies` is not the only answer.
- **End-marker tombstones** (decision 4, approved): a tombstone of an end marker counts as an end, recognised by the tombstone's name, never by content. PR 3, with the lifecycle model.

## Legal hold

A bank must be able to freeze erasure while a matter is open. A hold is a promise the redaction path checks, so it is stored where a redaction cannot skip it and recorded where an auditor can see who placed and lifted it.

- **Scope.** A hold names a run (and, by default, its sub-run tree: the run and every run whose ID starts with `<run>>`), or a matter id that covers a set of runs listed in the hold. A hold has an id, a matter, a scope, a reason, who placed it and when.
- **Representation (recommended): a table, plus the trail.** A `bide_holds` table (SQLite and Postgres) with one row per active hold (`hold_id`, `matter`, `run_id`, `tree` flag), written only by the hold commands. The redaction statement (SQLite) and `bide_redact_v1` (Postgres) refuse, in the same statement that replaces the row, any run that has an active hold row for it or an ancestor (`NOT EXISTS`), so a hold placed concurrently with a redaction either lands first (the redaction is refused) or after (the redaction is already done and the hold command reports the run as already redacted at its position). Rejected: a protected record kind inside the run's journal, because holds placed on a run would be writes to a journal that may be over, would change the run's leaf set (and every root), and cannot cover a matter that names runs not yet created. The migration creates the table and the function checks it; a schema without the table has no holds, and `Redact` on it says to migrate, because a store that cannot check holds must not erase.
- **Accountability.** Placing and lifting a hold are records in the same signed, anchored `redact:@log` trail (kinds `hold_placed`, `hold_lifted`: hold id, matter, scope, reason, who, when, and for a lift the id of the placement). The table is the enforcement; the trail is the evidence. `bide audit redactions` checks that every active table row has a placement in the trail and that no lift is missing a placement, and reports the table and trail disagreeing as a finding.
- **Separation of duties for banks (optional).** Lifting by a different principal than placed it: the lift record carries a signed authorization (the same `bide.audit.redaction-authorization.v1` shape as an erasure) and the verifier checks the lifter's key differs from the placer's. A deployment sets "lift needs a second person"; off by default.
- **Plan and refusal.** `PlanRedaction` gains `Holds` (id and matter of each hold covering the run) and a `Refusal` naming them; `bide journal redact` prints the hold and exits 1 with `held`. A dry run names the hold. The refusal is `ErrRedactHeld`, wrapping ErrConfig, written by the store, so it holds for raw callers too; on Postgres the role that erases has no privilege on `bide_holds` either, and a separate role places holds.
- **Verification.** `bide audit redactions` lists active holds, hold history, and a redaction that crossed a hold (the trail's redaction record falls within a placement and its lift) as a finding, which also finds a redaction made by hand with SQL during a hold when it shows as an unaccounted tombstone.
- **The anchor's own legal hold.** An S3 Object Lock legal hold on the anchor's objects (the signed heads) is a separate control: it stops the heads from being deleted or overwritten and says nothing about the journal rows. It does not replace this hold, and this hold does not protect the anchor. A deployment under a matter should have both, and the docs say so: bide's hold stops its own redaction path; it cannot stop a database administrator, a backup restore or a manual `DELETE`.

## Questions for approval

1. Command home: `bide journal redact` (write, full build) plus `bide audit redactions` (read-only verifier). Recommend yes.
2. Store capability named `Redactor` with compare-and-swap `Redact(ctx, runID, name, current, tombstone)`, not the `Redact(runID, name, tombstone)` of api-v1, so the store can re-verify the hash. Recommend yes, and update api-v1.
3. Protected list as in section 2; `approval:*` records redactable only with `-include-approvals`. Recommend yes.
4. `run:complete` holds the final result and is protected in v1. Recommend a follow-up that makes `firstEnd`/`endedErr` treat a tombstone named like an end marker as an end (protocol-marked, model check needed), after which end markers become redactable. Decide now or defer; deferring leaves final results unerasable in v1 and the docs must say so.
5. Single deployment trail `redact:@log`, begin/end records, written before the effect. Recommend yes.
6. Separation of duties: DB roles and plan-digest handoff in PR 1 and 2, signed authorization as PR 3, optional by flag, with "required" the bank's setting. Recommend this order.
7. Signer for the CLI: a key source flag shared with `approvecmd` (file or env seed, no argv secret), HSM and KMS through the library and `bide-hsm`; head output to a file, anchor by the operator. Needs the identity agents' key-source code to be reused, not edited. Recommend yes, and I will coordinate before touching it.
8. Event store copies: v1 refuses a run with event leaves unless `-keep-event-copies`. Recommend yes; event-leaf tombstones are a separate design.
9. Sessions and sub-runs: sessions refused in v1; `-subruns` opt-in. Recommend yes.
10. Add the `Redact` action and invariant to the lifecycle TLA+ model. Recommend yes (rigor first, cheap).
11. Govern logs: out of scope, documented. Recommend yes.
12. Backups and WAL: the tool checkpoints SQLite WAL after redaction and reports what it did; vacuum stays the operator's. Recommend yes.
