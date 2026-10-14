# Security and retention guidance

Operational guidance for running bide where an auditor will later ask you to prove what happened. It covers key custody, journal retention and archiving, backups, clocks, and least-privilege database roles. bide is a library: the runtime does not manage keys, retention or database permissions, so these are your controls. The [threat model](threat-model.md) says what each control defends against.

Statements about bide's behavior cite the code or the guide that specifies it. Recommendations are marked as such and are not requirements of the library.

## Key custody

Four kinds of secret and key exist in a deployment.

| Key | Used for | Held by | Compromise means |
|---|---|---|---|
| Tree-head (log) signing key | Signs tree heads, evidence seals, run certificates | The deployment, through an `audit.Signer` | Whoever holds it can sign a self-consistent forged log; only a head anchored elsewhere exposes it |
| Grant issuer key | Signs delegation grants; deliberately distinct from the tree-head key | The principal that issues authority | Authority can be minted in that principal's name |
| Approver keys | Each approver signs decisions over the exact call | Each approver | That approver's vote can be forged (a quorum needs k keys) |
| Webhook secrets | HMAC of notifications | Sender and receiver configuration | Notifications can be forged to that receiver |

What bide does today:

- Signing goes through the `audit.Signer` interface (`Alg`, `PublicKey`, `Sign`): `Ed25519Signer`, `MLDSASigner` and `HybridSigner` hold a key in process memory, and the shipped demo and `bide init` project write keys as files (the demo writes the log seed with mode `0600`). Three signers keep the key outside the application: `signer/pkcs11` (Ed25519 through `CKM_EDDSA`, or ECDSA P-256 through `CKM_ECDSA`; tested against SoftHSM2 only, no hardware HSM tried) `signer/awskms` (Ed25519 or ECDSA P-256; tested against a fake KMS client only, never against AWS) and `signer/gcpkms` (Ed25519 or ECDSA P-256 through Google Cloud KMS; tested against an emulator and a fake client only, never against Google Cloud). The PKCS#11 signer takes the PIN from an environment variable or a file, the cloud signers authenticate through the cloud SDK's own credentials and never see them, and all three refuse a token or key of the wrong scheme. Under Ed25519 AWS KMS signs at most 4096 bytes, which a tree head with a very long run ID can exceed (evidence seals are 60 bytes and fit); under P-256 there is no limit. The Google Cloud KMS signer requires an HSM-protected key version by default; Google keeps Ed25519 keys in software only, so a bank that needs an HSM-protected Google key uses ECDSA P-256, and Ed25519 needs `gcpkms.Config.AllowSoftware`. There are no ML-DSA or hybrid keys in a token ([key custody guide](../guides/key-custody.md), [known limitations](../KNOWN-LIMITATIONS.md#key-custody)).
- Verification never needs a private key. Auditors need only the public key, which must reach them out of band (`bide audit -pubkey`), never from the bundle being checked.
- The runtime does not rotate, revoke or escrow keys.

Recommendations:

1. **Separate the keys.** The log key, the grant issuer key and each approver key are held by different principals. Do not let the application tier hold a key that lets it re-sign history without a check elsewhere.
2. **Keep the log key out of the application database and image.** Load it from a secret manager or a mounted file with mode `0600` owned by the service account. Never pass keys on the command line or in environment dumps, and keep them out of logs.
3. **Use a signer that does not export the key** (HSM or KMS) when one is available to you: `signer/pkcs11`, `signer/awskms` or `signer/gcpkms`, or your own `audit.Signer` over its signing call. Test the signer against your own token or key before relying on it, and set `RequireNonExtractable` on the PKCS#11 signer so `Open` checks the token marks the key non-extractable.
4. **Anchor.** Publish every signed head to a separate trust domain (`audit.NewAuditedStore` with an `Anchor`): another account's write-once storage, a transparency log, a timestamping service. Without it, tamper-evidence does not hold against whoever controls the database and the key ([security model](../guides/security-model.md#anchoring-the-condition-on-tamper-evidence)).
5. **Rotate on a schedule and on suspicion, and never delete old public keys.** Evidence signed under a retired key still needs that public key to verify. Keep a dated key register: key id (fingerprint), algorithm, first and last use, who held it, why it was retired.
6. **Approver keys: one per person**, issued by your identity provider's process. bide refuses two approvers on one key but cannot see one person holding two ([hitl-approval](../guides/hitl-approval.md#scope)).
7. **Webhook secrets** rotate without downtime with `WithSecrets(current, previous)` ([notifications](../guides/notifications.md#rotating-the-secret)). Prefer mutual TLS and a pin where the receiver supports them.
8. **Post-quantum.** Anchors are long-lived. If your retention is many years, consider the hybrid scheme; ML-DSA cannot be used under the FIPS 140-3 module ([security model](../guides/security-model.md#post-quantum-and-hybrid-signatures)).

## Retention and archiving

To re-verify a run's evidence later, an auditor needs all of the following. Retain them together, for as long as your regulation or contract requires.

1. **The journal records of the run**, as stored (an exported journal, `audit.JournalExport`, or the database rows). Proofs are over the stored bytes, including each record's random salt, so a transformed copy does not verify. Records are not encrypted ([security model](../guides/security-model.md#what-is-not-guaranteed-confidentiality)); apply the same access control to the archive as to the database.
2. **The signed tree heads** you rely on, and their anchor-log entries. A head is the commitment; the journal alone proves nothing until a head covers it.
3. **The evidence packages** you produced for material actions (`audit.Evidence`), which carry the heads and inclusion proofs.
4. **Every public key** needed: the log key, grant issuer keys, and each approver key valid when a gate decided, plus the key register.
5. **The approver policy** in force (the named approvers and k), because a tally records the policy it enforced and the auditor checks it against the allowlist they hold.
6. **The `bide` version that verifies them.** Pre-1.0, a verifier reads only the current format of each artifact, so keep the release binary (or build from its tag) next to the evidence, with its checksums and signature ([Verifying releases](verify-releases.md), [stability](stability.md#what-stays-verifiable-across-versions)). Record the version in the archive's manifest.
7. **Notification trails and delivered payloads**, if notifications are part of your control evidence (`notify.VerifyBody` re-checks a stored delivery).

Practices:

- **Archive on completion.** When a run ends, export its journal and evidence and write them, with the manifest, to write-once (WORM) storage in a different account from the database. Verify the archive by running `bide audit verify-evidence` against it before deleting anything from the live store.
- **Do not delete from the live journal while its heads are your only commitment.** Journal compaction with proof continuity is a [design note, not implemented](../design/compaction.md). Today, removing records from the journal makes inclusion proofs for them impossible to produce. Archive first; delete only runs whose evidence is archived and verified.
- **Absence proofs need the key sets** projected from the journal, so archive those heads too if you may need to show that something never happened.
- **Re-verify periodically.** Run the verifier over a sample of the archive on a schedule, and always after upgrading `bide`; a failure with `audit.ErrFormat` means the verifier no longer reads that format.
- **Retention period** is a legal decision. The library imposes none.

## Backups

- Back up the database that holds the journal (`bide_steps`, `bide_events`, `bide_leases`, `bide_schema_version`, and the `next_seq` function, or the whole schema) with your normal point-in-time recovery. Backups carry the same sensitivity as the journal.
- **A restore can roll the journal back to an earlier state.** The at-most-once guarantee reduces to "the journal did or did not record this step" ([the guarantee](../GUARANTEE.md)). If you restore a database to a point before a step's attempt marker, bide will no longer know the effect fired, and may fire it again. After a restore, reconcile against the downstream systems before resuming runs: treat runs active at the time of the loss as having unknown outcomes. A rollback is also visible to anyone holding a later anchored head, because the restored log is not consistent with it. Do not "fix" that by re-signing.
- Restore into a separate environment first and verify (`bide audit`) before pointing production at it.
- The guarantee needs the store to survive the crash: use Postgres with synchronous commit and replication appropriate to your recovery objectives; SQLite is for one machine on a local disk, not a network file system ([known limitations, stores](../KNOWN-LIMITATIONS.md#stores)).
- Test the restore. An untested backup is not evidence of recoverability.

## Clocks and time

- **Leases use the store's clock on Postgres.** The Postgres store computes lease expiry with the database's `now()`, so nodes compare against one clock rather than their own. SQLite computes expiry from the local system clock; a forward jump expires leases early, which the attempt claims keep safe ([known limitations, stores](../KNOWN-LIMITATIONS.md#stores)). Lease expiry affects availability and duplicate work, not at-most-once.
- **Timers and halts use the process clock** (`WithClock`): `Sleep`, `AwaitFor` deadlines and `WithMinHaltAge`. A reconciler must set `WithMinHaltAge` to cover the tool's real worst-case run time so it does not resolve a halt while the effect may still be landing ([concepts](../CONCEPTS.md)). Keep the nodes of one deployment NTP-synchronized.
- **Tree-head timestamps** are the signing process's clock. Verifiers reject a head more than a skew allowance (default five minutes) ahead of the verifier's clock and a head earlier than the head it extends; they do not bound how old a head may be ([audit guide, timestamps](../guides/audit.md#timestamps)). Run the verifier on a host with a trustworthy clock, and pin the verification time (`WithVerifyTime`) when auditing a past period.
- **Approval decision times are claims.** `decided_at` is signed but is the approver's clock; `recorded_at` is the submitting process's clock, unsigned until a signed head covers it. Neither changes a count. For independent time, anchor heads to a timestamping service ([known limitations](../KNOWN-LIMITATIONS.md#approval-and-quorum)).
- **Notification times** are the notifying process's clock; an acknowledgement is not proof a person read it.
- A system clock that is wrong in the past or future will make evidence look odd without making it invalid. Record clock-sync monitoring in your own controls.

## Least-privilege Postgres roles

The Postgres store creates its own tables when it is opened with the default `Open`: `bide_steps`, `bide_leases`, `bide_schema_version`, `bide_events` (prefix configurable with `WithTablePrefix`) and a `next_seq` function, in a schema you should pin with `WithSchema`. For least privilege, run that migration separately as the owner (`bide migrate`, or `postgres.Migrate`) and open the store with `postgres.WithoutMigration`, which creates and alters nothing and checks that the schema is there at the expected version ([Postgres store guide](../guides/postgres.md)). The journal tables are written by `INSERT` only; the store issues no `UPDATE` or `DELETE` on `bide_steps` or `bide_events`. Leases are inserted, updated and deleted.

The split below is verified by `TestLeastPrivilegeRoles` and `TestWithoutMigration_LeastPrivilege` in `store/postgres` (they run under the Integration job's Postgres): an owner role runs the migration, the service role (with no `CREATE` on the schema) opens the store `WithoutMigration` and drives a journal write, an attempt claim, a lease (acquire, renew, release, list) and the event store, and a read-only role inspects. Opening before the migration fails with `postgres.ErrSchemaMissing`, and a schema at another version with `postgres.ErrSchemaVersion`. Removing any one of the privileges below made the test fail: `INSERT` on the journal tables, each of the lease privileges, `EXECUTE` on `bide_next_seq_v1`, `SELECT` on the version table; the service role is also shown unable to `UPDATE` or `DELETE` the journal, the event trail or the schema version, or to drop a table. Tested on Postgres 16.

| Role | Purpose | Privileges |
|---|---|---|
| `bide_owner` | Creates the schema (or owns it), runs `bide migrate`; owns the tables and `bide_next_seq_v1` | Owner. Runs the migration at deploy time and after upgrading bide. Not used by the running service |
| `bide_app` | The running service, opened with `WithoutMigration` | `USAGE` on the pinned schema; `SELECT`, `INSERT` on `bide_steps` and `bide_events`; `SELECT`, `INSERT`, `UPDATE`, `DELETE` on `bide_leases`; `SELECT` on `bide_schema_version`; `EXECUTE` on `bide_next_seq_v1(text)`. No `CREATE`, no `UPDATE` or `DELETE` on the journal tables, no superuser |
| `bide_reader` | `bide view`, auditors, exports | `USAGE` on the schema and `SELECT` on the four tables, opened with `OpenReadOnly`, which sets `default_transaction_read_only` so the server refuses writes on its connections |

Notes:

- **The service role needs no `CREATE` when it opens the store `WithoutMigration`.** The default `Open` runs `CREATE TABLE IF NOT EXISTS`, for which Postgres checks `CREATE` on the schema even when the tables exist; do not use it under this role split. `WithoutMigration` verifies instead, and the store checks that `bide_next_seq_v1` has the owner of the steps table and the definition it expects, so a function planted by another role is refused. The governed event log (`govern/postgreslog`) has the same option and `postgreslog.Migrate`.
- Because the journal tables take inserts only, withholding `UPDATE` and `DELETE` from the service role means a compromised service cannot edit or remove existing records through SQL it is allowed to run. That is a preventive control against rewriting history by the application; a database administrator, the owner role and anyone with superuser access are not constrained by it, so protect them separately and keep the anchor out of their reach.
- Creating the lease indexes on a database from v0.9.0 requires the table owner, so run `bide migrate` as `bide_owner` after an upgrade; a service on `WithoutMigration` refuses a schema without them (`ErrSchemaMissing`).
- **Pin the schema** (`WithSchema`) and grant `CREATE` on it only to the owner. The store trusts the schema's owner and every role able to create objects in it, and an unpinned store trusts every schema on the search path; the store's source documents these trust assumptions (`store/postgres/postgres.go`). Do not grant `CREATE` on the database to application roles.
- **Use TLS** to Postgres, a dedicated database or schema for bide, network rules that admit only the service and tooling, and credentials from a secret manager.
- Turn on database audit logging for the owner and administrator roles, and for any `UPDATE`/`DELETE` attempt on the journal tables.
- **Run `bide view` with the read-only role**, and over a copy for sensitive journals, since the viewer shows records in the clear.

## A short checklist

- [ ] Log key, grant key and approver keys held by different principals; log key not in the application database.
- [ ] Heads anchored to a separate trust domain; anchor not writable by the operators of the database.
- [ ] Key register with dated rotations; old public keys retained.
- [ ] Completed runs exported to WORM storage with heads, evidence, keys, policy and the verifying `bide` version; archive verified.
- [ ] Restore procedure tested, including reconciliation of in-flight runs against downstream systems.
- [ ] NTP on every node; reconcilers set `WithMinHaltAge`.
- [ ] Owner, service and read-only database roles; schema pinned; TLS on.
- [ ] Release binaries verified before install ([Verifying releases](verify-releases.md)).
