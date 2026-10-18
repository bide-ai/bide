# Retention and archiving

This guide is for the people who decide how long a bank keeps what bide produces, and for the operators who carry it out. It says what exists, what each piece is needed for, how to keep it so it still verifies offline years later, how to rotate and retire keys, and what deletion does to the proofs. It does not say how long to keep anything: that is your retention policy, and it does not claim that any regulation is satisfied. Where a period appears below it is marked as an example.

Everything here was checked against the code, and the deletion behavior in [What deleting does](#what-deleting-does) was also run against a real journal (the commands are listed there). Where the code does not do something, the page says so.

## Questions your retention policy has to answer

bide does not decide any of these. Answer them before you build the archive.

1. **How long** must the evidence of a run stay verifiable, and does the period differ by kind of action (a payment, a refund, a record change)? (As an example only, seven years is a period often used for payment records; yours is set by your policy and counsel.)
2. **What counts as the record**: the journal, the evidence packages made from it, or both? An evidence package proves the records it names and nothing else (see [Evidence packages](#evidence-packages)).
3. **Who holds the copy** that must survive the operator: which account, which team, under what access control and legal hold?
4. **Does any rule require deleting data** (personal data in a prompt or a tool result, for example) before the evidence period ends? bide can replace chosen records with tombstones and keep the rest of the run verifiable, at the cost of those records being unprovable and of the projections listed in [Erasing personal data](#erasing-personal-data); decide before you deploy which evidence you make first and who may erase.
5. **How long must you be able to run the verifier**: which `bide` version will read these artifacts in that year, and where is it kept?
6. **Who owns each key's history**: the log key, the grant issuer keys, each approver's key, and (if adopted) approver certificates?
7. **How is retention proven**: who re-verifies the archive, how often, and where is the result kept?

## What exists and what to keep

| Artifact | What it is | Produced by | Needed to |
|---|---|---|---|
| Journal | The run's append-only records, each with a random salt (store: SQLite or Postgres; or an export, `bide.audit.journal-export.v1`) | the runtime | prove any record, `prove-absent`, `verify-receipts`, `verify-decision-coverage`, the journal cross-check of `notifications` |
| Evidence package | One signed tree head plus an inclusion proof per named action, optionally a run certificate, grants and a consistency proof, sealed (`bide.audit.evidence.v6`; v5 packages still verify) | `audit.Evidence` | prove the named actions with no journal and no database |
| Signed tree heads | The log key's signature over a journal's size and Merkle root (`bide.audit.sth.v5`); one per growth when anchored | `audit.AuditedStore` and your `Anchor`, or `audit.SignTreeHead` | everything that checks a journal against a commitment |
| Anchor entries and receipts | A head as your anchor log recorded it (`bide.audit.anchor-entry.v1`: run, head, position), and whatever receipt or inclusion proof your anchor gives you | your `audit.Anchor` | show the head existed in a trust domain the operator does not control |
| Decision receipts | An approver's signed decision, the call's arguments and the journal position, no key material (`bide.audit.decision-receipt.v1`); `verify-receipts -update` adds the anchoring proof | `bide approve` | show a decision the approver made was not deleted |
| Notification trails | Per run `notify:<run>`; the digest's trail `notify:@digest`; the open trail `notify:@open:<day>` (segments joined by seals and checkpoints) and `notify:@open:index:<year>`; as event leaves in the event store, exportable as `bide.audit.notify-trail-export.v1` | the notifier | `bide audit notifications` |
| Notification-trail heads | A signed head per trail, including each open-trail segment and index | `audit.SignNotifyTrail` or `audit.AuditedEventStore` (the notifier does not sign) | find a deleted record or segment |
| Public keys | The log key; each grant issuer key; each approver's key (`approver-keys.json`) | you | every verb takes `-pubkey` from out of band |
| Policy inputs | The approver sets and k per gate; the approved-policy digests; the notifier's routing (`agent.RoutingOf`) | you | `verify-approvals`, `verify-run`, `notifications -routing` |
| The verifier | The `bide` binary of a known version, with its checksum and signature | the release | read every artifact above |

Not evidence, and not worth archiving for verification: the relay's state file (`bide notify relay -state`) holds delivery IDs and sink names only, no payload and no secret, and exists to avoid a repeated Slack message or PagerDuty trigger after a restart; lease tables (`bide_leases`); webhook secrets (rotate them, do not archive them in clear). What the relay forwarded is proven by the notification trail, not by the relay.

### Journals

The journal is the source every proof is built from, and records are stored unencrypted (see the [security model](security-model.md#what-is-not-guaranteed-confidentiality)); the archive needs the access control of the database it came from. A proof is over the record's stored bytes including its salt, so a transformed copy (re-serialized, re-encoded, or with the salt dropped) does not verify. Keep the stored bytes: a database backup, or an export written with `audit.ExportJournal`, which holds each record's stored bytes verbatim.

The journal format is tagged `bide.journal.v1-dev` before 1.0, and a later pre-release is not promised to read an earlier one's journal for resuming ([stability](../trust/stability.md)). That concerns resuming a run, not verifying exported evidence, but it means an archived database file is best read by the version that wrote it. Prefer exports, which `bide audit` reads by their format tag.

### Evidence packages

A package verifies with the log's public key and nothing else: it needs no journal and no database. `bide audit verify-evidence` on a sample package still printed `PASS` after the run's rows were deleted from the journal database. That makes the package the durable artifact for what it names, and it defines the limit: it proves the tool calls, steps and grants you included when you made it (`WithAllToolCalls`, `WithToolCall`, `WithStep`, `WithGrants`, `WithRunCertificate`) and not the other records of the run (model turns, for example). A claim you may need to prove later that is not in a package is provable only while you still hold the journal export and a head that covers it. Decide what to include before you delete a journal. The package does not check grant issuer signatures (those keys are not in it): keep the issuers' public keys and verify chains with `audit.VerifyDelegationChain`.

Make the package after the run ends, and make it again if you later need to prove more. Keep each one with a head from the anchor, so the package's signed head can be compared with what your anchor recorded.

### Signed heads and anchor entries

A head commits to the first N records of a journal. It is the only thing that lets a verifier notice that a journal it is shown differs from the one that was signed. bide ships the `Anchor` port, `audit.MemAnchorLog` (an in-memory reference, for tests) and `anchor/awss3`, which writes each head to an S3 bucket under Object Lock ([anchoring](anchoring.md)); the retention of the anchored heads is whatever your anchor provides (for S3 Object Lock, the retention date you set), so it is part of your design. Keep the heads at least as long as the journals and evidence they cover. If the anchor service ages out entries earlier than your retention period, copy the heads into your archive: `verify-receipts` and `verify-decision-coverage` take them from a file, as one head or one per line.

### Decision receipts

An approver's receipt is held by the approver (by default `<run>-<call>-<approver>.receipt.json` next to their key, mode 0600) and holds no key material. It is the approver's own evidence that a decision they made was kept. Decide whether the bank collects receipts centrally. Run `bide audit verify-receipts -update` once a head commits to the decision and keep the updated file: only a receipt that carries the anchoring proof can show a decision was deleted after signing; without it, the same verdict is only the approver's claim ([audit guide](audit.md#decision-receipts-noticing-a-decision-deleted-before-it-was-anchored)).

### Notification trails

The trails answer "was every event that needed a person notified, and when was it acknowledged". The open trail is a chain: daily segments (`notify:@open:<date>`) joined by seals, each starting from a checkpoint of the one it seals, plus a yearly index of the segments (`notify:@open:index:<year>`). With signed heads, a missing record, segment or index is a finding, so these cannot be partly kept: archive every segment and every yearly index, and the digest's trail, together with their heads. The notifier does not sign trails; schedule `audit.SignNotifyTrail` for every trail (each run's, the digest's, each open segment, each index), or wrap the event store in `audit.AuditedEventStore`. Without a signed head the report marks the trail `UNANCHORED` and nothing shows a deletion.

### Keys

Keep, with dates, every public key that signed anything you retain: the log key, each grant issuer key, each approver's key. Keep them in the archive next to the evidence, not only in the system that produced it. See [Rotating and retiring keys](#rotating-and-retiring-keys). Private keys are not archived; their custody is in [Key custody](key-custody.md).

## Archiving so it still verifies

`bide audit` reads only the current format of each artifact before 1.0 ([stability](../trust/stability.md)). The verifier version and the artifact formats are part of the archive.

1. **Write a manifest** for each archived run or period: run IDs; the `bide` version that wrote the artifacts and the version that verified them (`bide version` prints the formats that version reads, which belong in the manifest); the signature scheme and key ID of each key (`audit.KeyID`); the SHA-256 of every file; where the anchor holds the heads; and the date.
2. **Archive the verifier.** Keep the release archive for the version in the manifest, its `checksums.txt`, and the cosign bundles ([Verifying releases](../trust/verify-releases.md)). Release binaries are reproducible from the tag, so the source at the tag and the Go toolchain version recorded in the binary (`go version -m bide`) are the fallback if a binary is lost. Do not rely on `latest`.
3. **Pin the format.** If you upgrade `bide`, an artifact in an older format fails with an error that names the format (`audit.ErrFormat`, exit 4). Do not convert old artifacts; re-create them with the producer of the version that reads them while you still hold the journal, or keep the old verifier. `bide audit` still reads evidence v5; other formats have no such grace.
4. **Re-verify on a schedule and after every upgrade.** Run the verifiers on a sample of the archive with the keys from the archive, not the live system, and keep the result. Treat a failure as an incident.
5. **Pin the verification time.** A head is judged against the verifier's clock plus an allowed skew in the future; it is not limited in age. When you check an old archive on a machine whose clock is right, nothing is needed; when your own code checks a grant's expiry, use the time you mean (`audit.WithVerifyTime`; `-as-of` and `-max-clock-skew` on `notifications`).

What each verb needs from the archive:

| Verb | Inputs to archive |
|---|---|
| `verify-evidence`, `verify-approvals` | the package; the log public key; for approvals, the approver keys file and the policy (approvers, k); for a package with a run certificate, the approved-policy digests |
| `verify` / `verify-absent` | the bundle and the log key |
| `prove`, `prove-absent` (making new proofs later) | the journal export and a head that covers the record |
| `verify-receipts` | the receipts; the journal export per run; the heads; the log key; the approver keys |
| `verify-decision-coverage` | the journal export; every head for the run; the log key |
| `notifications` | the trail exports (or the database); the heads of every trail; the journal exports; the log key; the routing file if you check per-receiver completeness |

The CLI has no verb that exports a journal or a trail; use the library. A minimal archive step:

<!-- docsnip: setup ctx context.Context; journal *agent.Journal; evStore audit.EventStore; runID string; returns error -->
```go
jx, err := audit.ExportJournal(ctx, journal, runID) // bide.audit.journal-export.v1
if err != nil {
	return err
}
tx, err := audit.ExportNotifyTrail(ctx, evStore, agent.NotifyTrailName(runID)) // bide.audit.notify-trail-export.v1
if err != nil {
	return err
}
for name, v := range map[string]any{runID + ".journal.json": jx, runID + ".notify.json": tx} {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.WriteFile(name, b, 0o600); err != nil {
		return err
	}
}
```

Then verify the archive before you rely on it, with the files you archived: for example `bide audit notifications -trail run.notify.json -journal run.journal.json -sth run.notify.sth.json -pubkey log.pub`.

## Rotating and retiring keys

A verification takes one public key. A head, a package or a receipt that was signed under another key does not verify under it, and the failure is a finding, not a skip. The rules follow from that.

- **Keep every public key for as long as anything it signed must verify**, and record when each was first and last used. Retiring a key means stopping signing with it. It never means deleting its public half.
- **Verify each key period separately, with its own key.** A package is checked against one key; its `alg` and `public_key` must be the verifier's. A consistency proof inside a package is checked from the earlier head to the package's head under the same key (the report says "not an authentic journal head of this run under this key"), so `WithConsistencyFrom` across a rotation does not verify.
- **Do not rotate the log key in the middle of a run you will need to prove.** Heads of one run signed under two keys are verified by two invocations, and `verify-decision-coverage` and `verify-receipts` ignore heads that do not verify under the key they are given, so decisions covered only by the other key's heads look uncovered. Rotate between runs: let in-flight runs finish or make their evidence first.
- **Rotate on a schedule that matches your retention.** The signature is the part that ages: Ed25519 and ECDSA are not post-quantum. For evidence kept long enough to matter, consider the hybrid or ML-DSA scheme (software keys only, see [known limitations](../KNOWN-LIMITATIONS.md#key-custody)) and re-sign by making new packages while you hold the journals ([security model](security-model.md#post-quantum-and-hybrid-signatures)).
- **Before destroying a key in an HSM or KMS, export and archive its public key**, and confirm it by verifying a head with the exported key. A destroyed or disabled key cannot sign but verification needs only the public half ([Key custody](key-custody.md#verifying-and-rotating)).
- **If a key is compromised**, heads and packages signed before the compromise remain valid only if you can show they existed before it: that is what an anchor outside the operator's control is for. Record the date you believe it was lost, stop signing, re-anchor, and treat evidence signed after that date as suspect. bide has no revocation list for log keys.
- **Approver keys**: keep each approver's public key for every gate that approver decided, after they leave. Keep the keys file as of each period. The approver set per gate is part of the policy and belongs in the manifest.
- **Grant issuer keys**: keep them; packages do not carry them.

### Approver certificates and revocation (design)

Binding an approver's key to a person with a bank-issued certificate, and checking revocation years later, is a design only: see the approver-identity design, [pull request #302](https://github.com/bide-ai/bide/pull/302). Nothing in this guide depends on it, and bide today verifies a decision against a public key, not a certificate. If that design is adopted it will add things to archive (the certificate chains, and a dated series of every revocation list for each issuer, since it judges a decision by the revocation status valid when it was made). Its archiving requirements are not final; do not build an archive for them yet, and check the design when you plan one.

## Erasing personal data

**Make every piece of evidence you will need before you redact.** A journal that holds a tombstone cannot produce a new absence key set (`prove-absent`), used-policy set, run certificate or event stream (`audit.ErrRedacted`), because the content that defines them is gone. Signed heads, evidence packages, receipts, proofs of records that remain, and consistency proofs made earlier keep verifying. So, for a run, before erasing anything: make the evidence packages and run certificate you may need, the absence heads you may want, persist the event trail if you keep one, export the journal if you will keep the other records, and have a signed head that covers the run's current size anchored where you keep heads. A redaction cannot be undone, and none of these can be made afterwards.

What a redaction does, exactly:

- It replaces the stored bytes of the records you select with a tombstone that keeps the record's name (written into the tombstone as well as kept as the row key, so a journal export, which holds bytes only, still says which record it was), its position in the run, and its audit leaf hash. The leaf hash is over bytes that include a random 32-byte salt, and the salt is erased with the bytes, so the retained hash does not let anyone confirm a guess of the content.
- It refuses unless the run is over (it holds `run:complete`, `run:aborted` or `run:cancelled`) and no lease on the run is live. A sub-agent's run (`<parent>><call>`) is refused while any run above it holds records and no end marker, because the parent reads the child's records when it replays the call; redact the parent's tree once the root is over. A session's journal and turn runs, and sub-runs inside them, are always refused: the session reads its turns' runs and never ends, so its transcript cannot be erased this way yet. A record is redacted once: a tombstone is never replaced by another. It is atomic per record. A pass that stops partway leaves a prefix redacted; running the same plan again finishes it.
- It refuses the records a verifier or the engine needs: the journal header, the `run:` records other than `run:start` and the three end markers, attempt and not-started markers, saga failures and the saga compensation records (`@saga/`, which a re-drive of an aborted saga reads), the approval tally, identity, status, exec-hold and deadline records, and the `audit:` and `flow:` records (`agent.RedactionProtected`). Approval decisions are kept unless you ask for them.
- The time a tombstone records (`at_ms`) is the caller's claim, bounded to a valid date, not an attested time; and any role that can call the store's redaction (`EXECUTE` on `bide_redact_v3` on Postgres, write access to the file on SQLite) can redact any finished, unleased record that is not protected. Neither is accountable through the store alone; `audit.RedactJournal` records each erasure in a signed trail (below).
- It does not authenticate the name a tombstone carries. The name is written into the tombstone and kept as the row key, but it is outside the leaf hash: in an export, an evidence package or a `Journal()` read, anyone can rename a tombstone without breaking a root or a signature. Readers that act on it say so (`audit.NoteRedacted` reads "redacted, name not attested"; `agent.RunStatus.StartRedacted`), and `bide audit redactions` checks every tombstone's name against the name the signed redaction trail recorded at that position, so names are attested exactly when the trail verifies.
- A tombstone at a name a redaction never replaces (a saga record, an `audit:` leaf, `run:limits:`) is corruption, not a redaction: reading the journal fails with `agent.ErrStorage` naming it, because the leaf hash keeps signed heads verifying while the record the engine needs is gone. `Status`, `bide view` and the audit readers detect it. `Recover` does not: it lists runs by the end-marker row's name, which a tombstone keeps, so it skips such a run without reading it (nothing re-fires); a live run with such a tombstone over another protected record is refused when it is driven.
- An end marker (`run:complete`, `run:aborted`, `run:cancelled`) can be erased, but only by naming it (`-name run:complete`): `-payloads` leaves it, and its dry run says so (`end marker not selected by the Payloads preset: name run:complete to erase its content`), so the final answer is never left behind silently. Its tombstone keeps its position, and every reader finds the run's end by it, so the run is still over: `agent.Status` reports state `ended` with `agent.RunStatus.EndRedacted` set and no `Terminal` text, `Recover` and `RecoverLoop` skip it, `Cancel` reports `ErrRunEnded`, `IsComplete` is false, a drive of the run is refused (it holds a redacted record), and `bide view` shows `ENDED (ERASED)`. **How the run ended is no longer attested:** a tombstone's name, in the row and inside it, is outside the leaf hash, so renaming it (completed to cancelled) breaks no root or signature, and no reader takes the kind (completed, aborted or cancelled) from it. Only the signed redaction trail records what the marker was: `bide audit redactions` checks the name against it (`NAME_MISMATCH` for a rename) and, for a run whose erased end marker the trail accounts for, prints how the run ended (`run r1: ended: completed (per trail)`; the report's JSON has it as `ended`: the run, the marker and the kind). What is gone with the marker is the final answer (or the cancellation reason and the failure text) and its time (`EndedAt`). It leaves, by design, the record names (step keys, encoded tool-use IDs, the approver and call IDs inside `approval:` keys) and the positions.

The library, for a store that implements `agent.Redactor` (SQLite, Postgres, `MemStore`):

<!-- docsnip: setup ctx context.Context; journal *agent.Journal; runID string; returns error -->
```go
plan, err := agent.PlanRedaction(ctx, journal, runID, agent.RedactionSelector{Payloads: true})
if err != nil {
	return err
}
// plan lists each record by position, name, kind, length and leaf hash, and never its content.
// Review plan.Records, plan.Kept and plan.Digest; refuse a run that is not plan.Terminal.
if _, err := agent.Redact(ctx, journal, plan, time.Now()); err != nil {
	return err // wraps agent.ErrRedactNotTerminal, ErrRedactLeased, ErrRedactChanged, ...
}
```

`Payloads` selects run:start (the input and the principal), model turns, tool results, user steps, signals and approval requests; `Names`, `Prefixes` and `Kinds` select by hand; `IncludeApprovals` also selects approval decisions. `agent.Redact` records nothing about what it did; `audit.RedactJournal` does (below).

### Legal hold

A hold keeps a matter's runs from being erased. `agent.Holder` (implemented by `MemStore`, SQLite and Postgres) places and lifts holds: `PlaceHold(ctx, agent.Hold{ID, Matter, Runs, Tree})` covers each run named, and with `Tree` also the sub-agent runs below it; a matter that covers several runs is one hold with several `Runs`; `LiftHold(ctx, id)` lifts the whole hold; `Holds(ctx)` lists the active ones. The store enforces it where the record is replaced: **in the same statement** (SQLite's `UPDATE`, Postgres's redaction function), so a hold placed while a pass runs either lands first (the erasure of that record is refused with `agent.ErrRedactHeld`) or the record was already erased; and `PlanRedaction` names the holds that cover a run and refuses it. On Postgres the hold table is read by the function as its owner, so the erasing role cannot read or change it, and a separate role holds `SELECT, INSERT, DELETE` on it ([Postgres roles](postgres.md#roles)).

What a hold does not do: it stops bide's own erasure, not a database administrator, a backup restore or a hand-written `DELETE`, and it does not protect the anchor (an S3 Object Lock legal hold on the anchor's objects is a separate control that keeps the signed heads, and neither replaces the other). `bide journal hold` places, lifts and lists holds (on Postgres, with a role that holds `SELECT, INSERT, DELETE` on the holds table, `SELECT, INSERT` on the events table the trail is kept in, and what opening the store needs; an end-to-end test does this):

```text
bide journal hold place -db journal.db -id H-1 -matter "regulator inquiry 12" -run r1 -run r2 -tree \
    -reason "inquiry opened" -ticket LEGAL-5 -deployment bank-a -sign-key log.key -head-out redaction-heads.jsonl
bide journal hold lift  -db journal.db -id H-1 -reason "inquiry closed" -ticket LEGAL-9 -deployment bank-a \
    -sign-key log.key -head-out redaction-heads.jsonl
bide journal hold list  -db journal.db
```

Placing and lifting are **recorded in the signed redaction trail** (who, when, the matter, the runs, the reason, the ticket), with the same rules as an erasure (earlier heads checked, a head signed and appended). The order is chosen so that nothing fails open: a placement reaches the store first (the protection is in force before the record exists; if the record cannot be written the hold stands and the audit reports it; place it again with the same matter and runs and only the record is written), and a lift is recorded first (a hold is never lifted without evidence; if the store then fails to lift it, it stays in force; run the lift again and only the store step is repeated, with no second record). Two operators placing or lifting the same hold at once cannot leave a duplicate record: each appends at the trail position it read, and the one that loses reads the trail again and decides again (it finds the hold recorded and reports it). A hold the trail shows in force and the store lacks (lifted by hand) can still be lifted through the tool, which records the lift; a hold placed outside the tool is first recorded by placing it again, then lifted. `bide journal redact` refuses a held run: the dry run names the hold and the run is `REFUSED`. `bide audit redactions` lists the holds the trail shows in force, compares them with the store's when it reads a database (`HOLD_NOT_IN_STORE`: lifted outside the tool; `HOLD_NOT_IN_TRAIL`: placed outside it; `HOLD_DIFFERS`: other runs, tree setting or matter), finds a lift or a placement the trail does not match (`HOLD_UNMATCHED`), and finds an erasure that began while the trail showed a hold in force over the run (`REDACTED_UNDER_HOLD`, when the pass replaced records or has no end record to say it did not; a pass that began before a hold was placed and ended after it gets a note, as the trail cannot say when each record went). Read from exported files with no store, the report says the holds were not compared. **A lift can need a second person.** `bide journal hold authorize` has a second person read the trail and sign, with a key of their own (a key file, or with `bide-hsm journal` a key in an HSM or KMS), their consent to lift one hold: `bide journal hold authorize -db journal.db -id H-1 -reason ... -ticket ... -deployment prod -trail-pubkey <redaction key> -authorizer-key <their key> -out lift.auth` shows what it will sign (the hold, its matter and runs, the placement, the validity), and `-execute` signs and writes the file (never overwriting one). `bide journal hold lift ... -authorization lift.auth -authorizer-pubkey <their public key> -require-authorization` then checks it against the placement in force before anything is written and embeds it in the lift's signed trail record. The authorization is bound to the hold as it stood (its matter, runs and tree setting), to the placement's trail position and leaf hash (a file cannot be reused against a re-placed hold with the same ID), to the deployment, and to a validity (`-valid-for`, default 24 hours); it can name the trail key that must sign the lift (`-lifter-key-id`), and its key must not be the key that signs the trail or share a secret with it (two keys are one key holder when their `KeyIDs()` sets intersect, the rule approvals and the stop quorum use: a hybrid key and its own Ed25519 or ML-DSA half are one; a pinned authorizer that shares one is refused). The validity is judged against the lift record's own time, which the tool writes from its clock, as every time the tool records: a lifter who controls the clock can use an authorization after it lapsed. Because the record carries it, `bide audit redactions` checks it offline, without the file: a lift with a bad signature, another hold or placement, a lapsed validity at the lift, the lifter's own key, or (with `-authorizer-pubkey`) a key that is not a named authorizer is `LIFT_UNAUTHORIZED` whether or not authorizations are required, and `-require-lift-authorization` (with `-authorizer-pubkey`) makes a lift with none `LIFT_UNAUTHORIZED` too. What it does not do: it does not stop an administrator who lifts a hold in the store by hand (that is `HOLD_NOT_IN_STORE`), and who the authorizer is stays whoever holds the named key; placing a hold needs no second person.

### The redaction trail

`audit.RedactJournal(ctx, journal, trail, plan, opts)` erases accountably. `trail` is an `audit.EventStore` (the `bide_events` table of a database, ideally a separate one the erasing role cannot rewrite) holding the trail `audit.RedactionTrailName` (`redact:@log`); wrap it in `audit.AuditedEventStore`, or call `audit.SignNotifyTrail` and anchor the head, as for a notification trail. It:

1. refuses without a reason, a ticket, an executor, a time, a deployment name and a signer (all but the signer are recorded), and a plan that has a `Refusal`. **Every trail record is signed with the redaction key and bound to its place** (`opts.Signer`; the same log key that signs the trail's heads): someone who can append to the event store but holds no key cannot plant a record, and `bide audit redactions` rejects an unsigned or wrongly signed record under any head (`BAD_SIGNATURE`). The signature also covers the record's position, the hash of the record before it and the required `Deployment` name, so a signed record replayed after a crashed pass (which would hide it), copied, reordered, or moved into another deployment's trail is `REORDERED` (or `BAD_SIGNATURE` for a deployment mismatch);
2. **refuses a trail that is not the one the earlier heads attest**: before it appends or erases anything it checks the trail against `opts.TrailHeads` (the heads so far) and refuses (`ErrRedactTrailUntrusted`) if a head does not verify or does not match, which also catches a typo in the trail's database name starting a new trail. A record planted in the trail (unsigned, signed by another key, for another deployment, at the wrong place, or not a record at all) does **not** stop an erasure, so one person with write access to the event store cannot block every later erasure: it is reported (`BAD_SIGNATURE`, `REORDERED`, `MALFORMED`) and **accounts for nothing**, so an erasure it claims stays `UNACCOUNTED`;
3. **refuses a run no signed journal head covers**: `opts.Heads` must hold one that verifies under `opts.Verifier`, matches the journal and covers all of it. `AllowUncovered` erases anyway and records it (`UNCOVERED`). This is the rule at the top of this section, enforced;
4. appends a `begin` record (the records by position, name, kind, length and leaf hash, never content; the plan digest; reason; ticket; executor; time; the covering head's size), erases, and appends an `end` record. If the begin cannot be written nothing is erased. If the pass stops partway, run the same plan again; if only the end record failed, the erasure stands and the trail shows an unfinished pass until the plan is run again.

Sign the trail's head with `audit.SignRedactionTrail(ctx, trail, signer, verifier, deployment, earlierHeads, now)`, not `SignNotifyTrail`: it signs a head over exactly the leaves it checked, never a fresh load, after verifying the earlier heads against the trail, and refuses (`ErrNotVerified`, naming `TRAIL_ALTERED`) rather than sign over a trail the earlier heads do not cover. A head attests that the leaves exist; it never makes a planted record the tool's, and `bide audit redactions` still reports it. `RedactionOptions.EventCopiesKept` records that the event store still holds events of the run (`EVENT_COPIES_KEPT`).

`bide audit redactions` checks it: the trail is complete under its signed heads (a deleted or changed record is `TRAIL_ALTERED`; records after the newest head are `UNATTESTED_TAIL`; none, `UNANCHORED`); every record carries a signature that verifies under the key and is bound to its position and predecessor (`BAD_SIGNATURE`: a planted or altered record; `REORDERED`: a replayed, copied or reordered one); every plan's last pass has an end without an error (`INCOMPLETE` otherwise; a failed pass that the same plan later finished is a note); every tombstone in the journals is named by the pass that erased it, with the same name, leaf hash and time (`UNACCOUNTED`: an erasure made by hand, which a later plan listing it as "already redacted" does not launder; `NAME_MISMATCH`, `HASH_MISMATCH`, `TIME_MISMATCH`); every record the trail lists is erased (`NOT_ERASED`); every run the trail names has a journal in the input (`UNCHECKED`: a trail checked against no journal checked nothing); no erasure ran uncovered (`UNCOVERED`) or left event-store copies (`EVENT_COPIES_KEPT`) or crossed a legal hold (`REDACTED_UNDER_HOLD`), and the holds in the trail match the store's (`HOLD_NOT_IN_STORE`, `HOLD_NOT_IN_TRAIL`, `HOLD_DIFFERS`, `HOLD_UNMATCHED`). `-pubkey` and `-deployment` are always required (every record is signed for one deployment, so two deployments that share a key cannot copy records between their trails). A deployment name is trimmed of surrounding space and a blank one is refused everywhere, and a check that cannot run is an error, never a pass. `-trail-db` and `-trail-postgres-env` read the trail from the store it is kept in.

**What a planted record means for you.** A signed head over the trail attests that the leaves exist, not that the tool wrote them. A record planted in the trail is reported by every later `bide audit redactions` run, so that deployment's audit does not pass again until the plant is dealt with: a planted record never blocks an erasure and never accounts for one, and it stays in the trail because the trail is append-only. When you see `BAD_SIGNATURE`, `REORDERED` or `MALFORMED`, investigate who could append to the event store (the application's role should not be able to reach the trail's database: keep the trail apart, `-trail-db`), and treat the trail as untrusted from that position. Then either start a new deployment name and trail epoch (a new `Deployment`, a trail store of its own, and your anchor keeping the old trail and its heads), or acknowledge the record: `bide journal acknowledge -db journal.db -position <n> -reason ... -ticket ... -deployment ... -sign-key ... -head-out ...` appends a record signed with the redaction key that names the planted leaf's position and the hash of its bytes (with the reason, the ticket and who) and a new signed head. `bide audit redactions` then reports that leaf as acknowledged (a note that names who acknowledged it, the ticket, the time and the reason, so the history stays visible) and stops failing on it; acknowledging needs the same key as erasing. It acknowledges only that leaf: not a record the tool wrote, not an erasure made by hand (`UNACCOUNTED`), and not other bytes later at that position; it comes after the leaf, and a forged acknowledgement (unsigned, another key) acknowledges nothing. The trail is append-only, so an acknowledgement cannot be taken back. It reads exported files (`-trail`, `-journal`, `-sth`, `-pubkey`) or a database (`-db`, `-postgres-env`). The trail is only as good as its anchoring: anchor its signed heads.

### The command

`bide journal redact` is the way to erase: a dry run, then an erasure that writes to the signed trail before and after it erases.

```text
# 1. List the plan. Nothing changes; no record content is printed; the output ends with a plan digest.
bide journal redact -db journal.db -run r1 -payloads

# 2. Erase: the same selection, the digest you reviewed, why, who authorised, the deployment name,
#    the log signing key, the journal head that covers the run, and the file the signed trail head goes to.
bide journal redact -db journal.db -run r1 -payloads -execute -plan-digest <digest> \
    -reason "customer erasure request" -ticket PRIV-42 -deployment bank-a \
    -sign-key log.key -head-out redaction-heads.jsonl -pubkey log.pub -sth r1.sth

# 3. Verify the trail against the journals, offline or against the database; give every head in the file.
bide audit redactions -db journal.db -deployment bank-a -pubkey log.pub -sth head1.json ...
```

What it does and refuses, in the order it checks:

1. It refuses a run that is not over, whose lease is live, whose parent is not over, or that belongs to a session, and lists protected records as kept. A live lease is checked before anything is written, so a refused run leaves nothing in the trail.
2. **It refuses a run that no signed journal head covers**: `-sth` heads that verify under `-pubkey`, match the journal and cover all of it. `-allow-uncovered` erases anyway; that is recorded in the trail and `bide audit redactions` fails on it (`UNCOVERED`).
3. It refuses a run whose events the event store (`bide_events`) holds, unless `-keep-event-copies`, which is recorded with the number of events left (`EVENT_COPIES_KEPT`).
4. It refuses unless `-plan-digest` is the digest of the plan it computes now, so what was reviewed is what runs.
5. It checks the trail against the signed heads it has had so far (the `-head-out` file) and refuses, before erasing anything, if the trail does not match them (a typo in `-trail-db` starts a new trail). A trail that already holds records and has no earlier head is refused too, unless `-accept-unheaded-trail`: nothing attests those records.
6. It appends a `begin` record, erases each record in its own transaction, appends an `end` record, and signs the trail's head with `-sign-key` (a key file, mode 0600, never an argument) and appends it to `-head-out`, also after a failed pass, so the attempt is attested. **Anchor those heads**: an unanchored trail proves nothing about deletions. If a pass stops partway, run the same command again. A refusal before the first trail record is written leaves no new head (there is nothing to attest), and the lease probe is skipped, not failed, by a role that cannot take a lease (the store's own refusal then applies).
7. It then makes the storage let go of the bytes. On SQLite it ran the erasure with `secure_delete` on, so the replaced bytes were overwritten in the page, and it now checkpoints and truncates the write-ahead log; the report says what the engine reported, and **if a reader holds the database open the checkpoint cannot finish: the command says "STORAGE NOT COMPACTED", exits 1 and tells you to close the readers and run it again** (the old bytes may remain in the log or the file until then). On Postgres the replaced row versions stay as dead tuples until `VACUUM`; that, and replicas, WAL archives and backups, are the operator's, and the report says so.

Keep the trail where the erasing role cannot rewrite it (`-trail-db`, `-trail-postgres-env`, which honours `-schema`: a separate database). On Postgres give the erasing role its own DSN (`-redact-postgres-env`) and read with another (`-postgres-env`): the role that erases holds only `EXECUTE` on the redaction function and can read nothing. `-subruns` plans each sub-agent run of the root as well, each recorded on its own. The report ends with the copies the command cannot reach (see the scope list below). The trail is signed with a key file from the command line; HSM and KMS signers go through the library (`audit.RedactJournal`, `audit.SignRedactionTrail`) until `bide-hsm` gains it.

`bide audit redactions` checks the trail; see [The redaction trail](#the-redaction-trail) for its findings.

Scope: what a redaction does not reach. Say this to the people who ask you to erase:

- Copies outside this database: exported journals (`audit.ExportJournal`), evidence packages, proofs and approvers' receipts hold the bytes they were made from, including the salt; erase them yourself.
- The event store (`bide_events`, written by `audit.PersistJournal`) holds a copy of event content and is append-only, with no tombstone for an event leaf yet; a redaction does not touch it.
- Database internals and other copies: Postgres keeps the replaced row version as a dead tuple until `VACUUM` (and `VACUUM FULL` to return the space), SQLite keeps it in free pages and the write-ahead log until a checkpoint and `VACUUM`, and replicas, WAL archives, base backups and dumps keep their own copies. The operator has to cover each of them.
- Logs, traces (`trace` with content capture), the application's own tables, and the model provider's retention of what you sent it.
- Govern event logs (`govern/sqlitelog`, `postgreslog`, `redislog`): a governed event is an opaque string the application chose, appended once and folded in order, with no tombstone. Do not put personal data in govern events; bide cannot erase it.

Who may erase: the application's database role should not be able to. On Postgres, give the redaction tool its own role with `EXECUTE` on the `bide_redact_v3` function and no privilege on any table ([Postgres roles](postgres.md#roles)): the function repeats the checks server-side, so even raw SQL by that role can erase only a record of a finished run into a tombstone that keeps its leaf hash. Keep the role out of the application's configuration. On SQLite the file's permissions are the only control: whoever can write the file can rewrite it, redaction or not.

## What deleting does

**What bide gives you.** The journal port has no update and no delete (`agent.Store`, rule A6). A store may replace a record's bytes with a reserved tombstone (`{"redacted":{"leaf_hash":"<64 lower-case hex>","at_ms":<ms>,"name":"<name>"}}`, in exactly this byte form: a tombstone with a duplicate or differently cased member, extra space or another member is not read as one) that keeps its position, its name and its leaf hash, so other records' proofs and the root are unchanged; readers (`audit`, `bide view`, `verify-receipts`, which reports `redacted (anchored)`) understand a tombstone. SQLite, Postgres and `MemStore` implement the optional `agent.Redactor` capability that writes one, and `agent.PlanRedaction` and `agent.Redact` use it; see [Erasing personal data](#erasing-personal-data). `bide journal redact` is the command, `audit.RedactJournal` the library behind it, and `bide audit redactions` the check ([the design](../design/journal-redaction.md)). A run that holds a tombstone is over and cannot be driven again. A redacted record can no longer be proven, and a journal holding one refuses to project the absence key sets, a run certificate or the event stream (`audit.ErrRedacted`).

**How tampering is detected.** Detection is by position. A signed head commits to the first N records; a journal that differs from it in that prefix (a record removed, changed or reordered) no longer hashes to the head's root. A record after the newest head is not covered. Deleting a record from a journal you then present with its head is therefore found, and deleting without the head is not.

The following was run on a sample database (`bide sample`), with signed heads for every notification trail and `bide audit notifications -db`:

| Action | Result |
|---|---|
| Delete a run's journal, keep its trail and its head | `FAIL ... the trail has 0 record(s) and the head commits to 3: records were deleted` |
| Delete a run's journal and its notification trail, and stop supplying that run's head | no finding for that run; the other runs' results unchanged |
| Delete the same, but still supply that run's head | the same `records were deleted` FAIL |
| Delete the last record of an open-trail segment, supply its head | `FAIL ... the trail has 5 record(s) and the head commits to 6: records were deleted` |
| Delete a run's journal, then `verify-evidence` on that run's package | `PASS`: the package needs no database |

The same position rule applies to `verify-decision-coverage` (a head that does not match the export is divergent and fails) and `verify-receipts` (a decision missing where the receipt says it was is `receipt not matched`, or `decision deleted after signing` if the receipt carries an anchoring proof).

**What this means for retention.** Plainly: you cannot remove a record from a run that you still want to verify against an anchored head, and you cannot age out part of the open trail, without the verification reporting it. A deletion that is consistent with your policy has to be a deletion of a whole unit, with the verification set changed to match:

1. **Archive and verify first** (see above), so the evidence outlives the deletion.
2. **Delete whole runs, not records.** Remove the run's journal, its `notify:<run>` trail, and its receipts together, and stop supplying that run's journal export, trail and heads to the verifier. The remaining runs verify as before. Do this only for runs that are finished (a run that is waiting for an approval is not).
3. **Keep the heads.** Deleting a run does not require deleting its heads, and keeping them (at your anchor or in the archive) is what lets you show later that the run existed and what it committed to, without its content. Heads hold no record content.
4. **Do not delete open-trail segments or yearly indexes one by one.** Their chain is checked end to end and the audit reads every yearly index from 2026 ([audit guide](audit.md)). They are part of the trail's proof. They grow with the life of the store. To bound them, archive the whole store at the end of a period, verify it, and start the next period on a fresh store when no run is in flight. That cutover was not tested.
5. **Content that must not be kept** (personal data in a prompt, a retrieved document or a tool result) is in the journal in clear text and in each proof that discloses that record. Erasing it from a run that must stay provable is [a redaction](#erasing-personal-data): the record is replaced by a tombstone, the run keeps its place in the signed heads, and that record is unprovable from then on. Or delete the whole run (item 2). Decide before deployment which you will do, and keep such data out of the journal where you can (the tool-error redactor covers error text only: [Tool errors are redacted](security-model.md#tool-errors-are-redacted-before-they-are-journaled)).
6. **The anchor is append-only by design.** Do not ask it to delete heads for runs you deleted unless your policy requires it; a missing head only means the run can no longer be checked against it.
7. **Nothing personal in an anchored run or trail ID.** A write-once anchor cannot be erased before its retention date, and a signed head carries its run ID in clear (the ID is part of the signed bytes), so an anchored ID with a customer's name, email address or account number in it stays for the whole retention, and redaction cannot reach it. Use opaque run IDs and keep the mapping to customers in a store you can erase from ([anchoring](anchoring.md#run-and-trail-ids-must-not-hold-personal-data)). With the S3 anchor, set the retention to what your policy needs and no longer: Object Lock cannot shorten it later.

## Where to store it

Use a store you can lock against change for the retention period, outside the account that runs the application. The property you need, whatever the product, is that no one can overwrite or delete an object until its retention date, including the people who administer the application; most object stores and archives offer a write-once mode for this (for example S3 Object Lock in compliance mode, Azure Blob immutable storage, or a Google Cloud Storage retention policy with a bucket lock; check your provider's current documentation and its limits, and do not treat the examples as a recommendation).

- **Archive store (immutable):** evidence packages, journal and trail exports, signed heads (and anchor entries), receipts, the keys register and public keys, the verifier release files, the manifests.
- **Live database:** the journal and trails as they are written; back up with the database's own consistent mechanism (see [operations](../trust/operations.md#backups)). A database backup is a recovery tool; it is not an immutable archive, and restoring one rolls the journal back ([operations](../trust/operations.md#backups)).
- **Separate trust domain for heads.** The point of anchoring is that the operator cannot rewrite the heads. An archive written by the same operator in the same account is not that; if the anchor is also yours, say so in the manifest. For the S3 Object Lock anchor that means compliance mode in a separate AWS account ([anchoring](anchoring.md#what-it-gives-you-and-what-it-does-not)); export the heads with `bide-anchor export` into the archive before their Object Lock retention ends.
- **Encrypt the archive at rest** and control who can read it: journals and exports are plain text.
- **Replicate** to a second location and **test a restore** of the archive and a verification from the restored copy, on the schedule in your policy.
- **Legal hold** is a property of your store; the proofs neither add it nor conflict with it.

## A short checklist

- [ ] The policy answers the questions at the top, including erasure.
- [ ] Each finished run has an evidence package that names everything you may need to prove, a journal export, and its heads archived.
- [ ] Notification trails (every run trail, every open-trail segment, every yearly index) are exported with signed heads for each.
- [ ] Every public key that signed anything retained is in the archive with dates; none is deleted.
- [ ] The `bide` release, its checksums and signature are archived, with the formats it reads.
- [ ] The manifest lists versions, key IDs, file hashes and where the heads are held.
- [ ] The archive is on immutable storage in another account; a restore and a verification from the restored copy were run and recorded.
- [ ] Deletion is by whole run, after archive and verification, and the verification set is changed to match.
- [ ] Re-verification is scheduled and runs after every `bide` upgrade.

## See also

- [Audit](audit.md): the proofs, the formats and the CLI.
- [Key custody](key-custody.md) and [security model](security-model.md#key-custody).
- [Notifications](notifications.md#auditing-were-the-people-told) and the [trail verifier](audit.md).
- [Security and retention guidance](../trust/operations.md) (backups, clocks, database roles) and [Stability and support](../trust/stability.md).
