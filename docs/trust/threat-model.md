# Threat model

This page is for a security or risk reviewer deciding whether bide fits a regulated deployment. For each asset and actor it states what bide prevents, what it detects and how, and what it does not address. It summarizes and links the authoritative pages; where they disagree with this page, they win: [the guarantee](../GUARANTEE.md), [the security model](../guides/security-model.md), [known limitations](../KNOWN-LIMITATIONS.md) and the [formal verification overview](../formal-verification.md).

bide is a library, not a service. You run it inside your own processes against your own database, so the deployment (network, database, key custody, identity provider) is part of the system under review. This page marks which controls bide provides and which it expects of you. It is not a compliance attestation: no third-party audit is claimed, and nothing here claims conformance to a standard.

## Assets

| Asset | What it is | Where it lives |
|---|---|---|
| Funds and records, reached through side effects | A charge, a refund, an email, a record update that a tool performs outside bide | The downstream system. bide controls whether and how often the call is made, not what the downstream does with it |
| The journal | The append-only record of a run: model turns, tool results, attempt markers, approval decisions | Your store: SQLite file or Postgres tables (`bide_steps`, `bide_leases`, `bide_events`, `bide_schema_version` by default) |
| Signing keys | The tree-head (log) key that signs commitments; the grant issuer key that signs delegations | Supplied by the deployment (`audit.Signer`). Key files, or an Ed25519 or ECDSA P-256 key in a PKCS#11 token, AWS KMS, Google Cloud KMS or (P-256 only) Azure Key Vault or Managed HSM ([key custody](../guides/key-custody.md)); see [operations](operations.md) |
| Approver keys | One key per approver of an m-of-n gate | The approvers. bide holds only public keys (verifiers). A key file, or a key in a PKCS#11 token, AWS KMS, Google Cloud KMS or Azure Key Vault used through `bide-hsm approve` ([key custody](../guides/key-custody.md#deciding-from-the-terminal-bide-hsm)) |
| Webhook secrets | HMAC secrets shared with notification receivers | The sender's configuration and the receiver's |
| Anchored tree heads | Signed heads published to an independent log | The anchor you operate (`audit.Anchor`) |

## Trust boundaries

1. **Model and agent code to tools.** The model proposes tool calls; the runtime decides whether they run (tool filter, approval gate, attempt claim). Model output is untrusted input.
2. **Runtime to store.** The runtime trusts the store to commit atomically and to record a step name at most once across processes (`GUARANTEE.md`, "The boundary conditions"). The audit layer does not trust the store after the fact: it treats the journal as evidence to be verified.
3. **Runtime to the anchor and to the signing key.** Tamper-evidence against an operator depends on the signed tree head leaving the operator's control ([security model, anchoring](../guides/security-model.md#anchoring-the-condition-on-tamper-evidence)).
4. **Runtime to approvers.** Approvers sign a decision over the exact call with their own keys. bide verifies signatures; binding a key to a person is your identity provider's job ([hitl-approval, scope](../guides/hitl-approval.md#scope)).
5. **Runtime to notification receivers.** Outbound signed HTTPS; the receiver verifies ([notifications](../guides/notifications.md)).
6. **Auditor to evidence.** The auditor runs `bide audit` or `audit/verify` offline with a public key obtained out of band, never from the bundle.

## Actors and threats

Each subsection lists what bide prevents (the action does not succeed), what it detects (the action may succeed but leaves checkable evidence), and what it does not address.

### A compromised or misbehaving model or agent

The model may request a destructive call, request the same call twice, craft arguments to exceed its authority, or try to talk a person into approving.

- **Prevents.**
  - A non-idempotent side effect fires at most once per call, including across crashes and a second driver of the same run: an attempt marker is written as an exclusive claim before the effect, and an unknown outcome halts the run (`OutcomeUnknown`) rather than re-running it. Specified in [the guarantee](../GUARANTEE.md); measured in the [crash benchmark](../../benchmarks/README.md); the claim protocol is model-checked ([formal verification](../formal-verification.md)).
  - A call outside the run's journaled tool filter is refused and its tool never runs. A tool with an approval gate does not run until the gate passes; a recorded denial is final even if the gate is later removed.
  - A call to a tool that carries a cap rule (`agent.WithCaps`, per call or cumulative per run, hour or day, per currency) is refused when its amount, in the member the rule names, is over the rule, whatever the model sent (a window can pass up to twice its limit across a boundary, and a `by` key is only as strong as its canonical form), on the arguments the model gave and again on those the tool receives ([amount caps](../guides/amount-caps.md)).
  - With a taint policy (`agent.WithTaintPolicy`), a side-effecting call requested after the run read content the host marked untrusted does not run until the escalation gate passes. This bounds what a steered request can do; it does not detect or stop the steering, and what it misses is listed under "Does not address" below.
  - A sub-agent can only narrow its delegated authority (`CheckAttenuation`, [delegation](../guides/delegation.md)).
- **Detects.** Every model turn records digests of the system prompt and tool set it was given, the model that answered and its finish reason (`Record.PromptDigest`, `ToolsDigest`, `Model`), so an audit can tell which configuration produced each answer. A flow's journal can be checked against its declared topology (`Flow.Conform`). Proof-carrying runs check behavioral properties offline (`RunCertificate`).
- **Does not address.**
  - A tool that is wrongly declared `ReadOnly` or `Idempotent` opts out of the at-most-once protection ([the guarantee](../GUARANTEE.md)). Review tool declarations as code.
  - Whether a permitted call is a good idea. bide enforces what the policy allows; it does not judge the model's reasoning. Quorum lowers the risk of one model's error but models can be wrong together ([known limitations, approval and quorum](../KNOWN-LIMITATIONS.md#approval-and-quorum)).
  - Prompt injection as such. Content the model reads can steer its requests; the controls above bound what a steered request can do, they do not stop it being made. With a taint policy (`agent.WithTaintPolicy`), a run that read content the host marked untrusted needs an escalation gate for each later side effect; that bounds the consequence and does not detect an attack, and it misses content the host did not mark, a human who approves, and `ReadOnly` tools ([guide](../guides/untrusted-content.md#what-this-does-not-do)). `RequireProvenance` makes every tool and run state where its content comes from, so none is unmarked by omission; the statement is the host's word and is not verified.
  - Secrets the model or a tool puts into a result. Results are journaled as written; only URLs in tool errors are redacted by default ([security model](../guides/security-model.md#tool-errors-are-redacted-before-they-are-journaled)).

### An insider with database access

Someone with write access to the journal tables, or to the SQLite file, who edits, deletes or reorders records, or plants a record.

- **Prevents.** Nothing, by bide alone. A database administrator can change any row. Database controls (roles, audit logging, backups; see [operations](operations.md)) are the preventive layer.
- **Detects.** Any modification, insertion, deletion or reorder changes the Merkle root; an inclusion proof, a consistency proof between two signed heads, or an absence proof then fails (`audit.ErrNotVerified`). This holds against an insider **only if** the signed tree head was anchored somewhere the insider cannot rewrite, or signed with a key the insider cannot use. An insider who controls both the database and the signing key can rewrite and re-sign the whole log, and only a copy of an earlier head held elsewhere exposes the divergence ([security model](../guides/security-model.md#anchoring-the-condition-on-tamper-evidence)).
- **Does not address.**
  - Confidentiality: journal leaves are not encrypted; anyone with the database reads every record ([security model](../guides/security-model.md#what-is-not-guaranteed-confidentiality)). Encrypt sensitive payloads before they enter the journal or restrict the database.
  - A delegation's runtime authority is read from the journal, so someone who can write the journal could plant another validly signed child grant. That the journal was not rewritten is what anchoring makes provable afterward ([security model, key custody](../guides/security-model.md#key-custody)).
  - What a cap does not cover: a tool that carries no rule, an amount in a member the rule does not name, a tumbling window (up to twice the limit across a boundary), a `by` key only as strong as its canonical form, a saga's rollback and plan flows ([limits](../KNOWN-LIMITATIONS.md#amount-caps), with the window and key limits in the [guide](../guides/amount-caps.md#cumulative-rules)).
  - A cumulative amount-cap window kept in unsigned journal rows can be undercounted by someone who can write the store. `bide audit caps` with the signed heads finds a slot or release changed or deleted in the part of a ledger a head covers, and fails any entry past the newest head as not attested without saying whether it was tampered with; a record written before a head was signed is attested by it, a plant included. Without heads (`-unanchored`) it finds only what contradicts itself. A writer who can also sign heads writes a consistent history it cannot tell from a real one ([amount caps](../guides/amount-caps.md#auditing-the-ledgers-bide-audit-caps)).
  - Deleting a whole database with no anchored head ever published: nothing remains to compare. Anchor continuously (`AuditedStore`).
  - A storage layer that lies about a commit ([the guarantee](../GUARANTEE.md)).

### A compromised signing key

- **Prevents.** Nothing after the fact. Whoever holds the tree-head key can sign any head, so a forged log can be made self-consistent.
- **Detects.** A head that is inconsistent with a head already anchored elsewhere (a consistency proof between the two fails). The grant issuer key is deliberately distinct from the tree-head key, so a log operator's key cannot mint authority.
- **Does not address.** Key revocation and rotation workflow: the runtime does not manage keys. Keep old public keys so old evidence still verifies, and record when a key was retired. Signature schemes include Ed25519, ECDSA P-256, ML-DSA-65 and a hybrid; ML-DSA is unavailable under Go's FIPS 140-3 module v1.0.0 and available under v1.26.0 and later ([security model](../guides/security-model.md#post-quantum-and-hybrid-signatures)). The PKCS#11, AWS KMS and Google Cloud KMS signers keep an Ed25519 or ECDSA P-256 key out of the application (tested against SoftHSM2, a fake KMS client, and the Cloud KMS emulator with a fake client, never against AWS or Google Cloud); the shipped examples other than `examples/hsm-signing` keep keys in files ([operations](operations.md#key-custody)).

### A compromised approver

An approver whose key is stolen, who is coerced, or who approves carelessly.

- **Prevents.** A single approver cannot satisfy an m-of-n gate alone when m is above one. Two approvers whose verifiers report one key identity are refused with `ErrConfig`. A decision is signed over the exact call (run, call id, tool, canonical arguments), so it cannot be replayed onto a different call, and a decision signed under an earlier encoding does not count.
- **Detects.** `bide audit verify-approvals` checks offline that k named approvers signed this call before it ran, and detects an omitted decision, a recount mismatch, or a different enforced policy. Decision times are flagged when implausible (`audit.CheckDecisionTimes`) but never change a count.
- **Does not address.**
  - One person holding two different keys, or k approvers colluding or all compromised.
  - What an HSM or KMS key does not protect. With the key in a token (`bide-hsm approve`), the key cannot be copied out when the token marks it non-extractable (the token's word, not an attestation), but the token signs the bytes it is handed: on a compromised approver workstation with the token unlocked, malware can have it sign a decision other than the one `bide-hsm` displayed. A token with a trusted display that confirms each signature is not supported. A PIN kept in a file or an environment variable on that workstation is exposed with it.
  - Whether an approver understood what they approved.
  - Trusted time: `decided_at` is the approver's clock, and no RFC 3161 authority stamps decisions yet ([known limitations](../KNOWN-LIMITATIONS.md#approval-and-quorum)).
  - Weighted votes, role rules and approval deadlines are not supported.

### A network attacker

- **Prevents.**
  - Forged or replayed webhook notifications are rejected by `notify.Verify` (HMAC over timestamp and body, constant-time comparison, timestamp tolerance, delivery id bound to the signed body). Outbound notifications require HTTPS (plain `http` only for loopback), TLS 1.2 or newer, never follow redirects, and support mutual TLS, a private CA and an SPKI pin.
  - Model and tool traffic uses whatever transport the adapters and your tools use; bide adds no protection there.
- **Detects.** A stored notification can be re-verified later as evidence (`notify.VerifyBody`). Each accepted delivery records how it was authenticated.
- **Does not address.** Traffic analysis; denial of service; compromise of the database connection (use TLS to Postgres and network controls, outside bide); a receiver that does not verify.

### A malicious or compromised receiver of notifications

- **Prevents.** The payload holds structured fields only (kind, run, step and tool name, approver ids and k of n, principal, times). Error text, tool arguments and approve links are included only if the operator names them. A notification cannot approve anything: approval requires a signed decision.
- **Detects.** An acknowledgement means the endpoint accepted the request, not that a person read it. Undelivered events raise an overdue notice and retry; the delivery trail is Merkle-committed.
- **Does not address.** A receiver that leaks what it was told, or that falsely acknowledges. Delivery is at least once, so receivers must deduplicate on the signed delivery id. If an operator opts in to arguments or error text, those leave the trust boundary.

### A compromised host running the agent

Code execution in the agent's process.

- **Prevents.** Nothing. The process holds whatever keys and credentials it was given, can call any tool, and can write the journal as itself.
- **Detects.** Whatever it journaled stays committed; anchored heads bound what it can later rewrite. Run signing and approver keys outside this process where possible.
- **Does not address.** Isolation, sandboxing, and least-privilege for tool credentials. These are deployment controls.

## What bide does not address, in one list

- Confidentiality of the journal and of disclosed records.
- Authentication of people and services (identity is a recorded claim; your IdP authenticates it).
- Key generation, storage, rotation and revocation.
- Availability: the guarantee is safety (no double fire, no lost completed work), not liveness. A crash can leave a run halted until a person or reconciler resolves it.
- Correctness of tool declarations, of downstream systems, and of the model's decisions.
- Third-party audit or certification. The project documents what is machine-checked ([formal verification](../formal-verification.md)) and tested ([how bide is verified](../testing/verification.md)); that is not an independent assessment.
- Before 1.0, resuming a run across releases and verifying old artifacts under new formats ([stability](stability.md)).

## Related

- [Operations guidance](operations.md): key custody, retention, backups, clocks, database roles.
- [Stability and support](stability.md).
- [Comparison with other approaches](comparison.md).
- [SECURITY.md](../../SECURITY.md): reporting a vulnerability, verifying releases.
