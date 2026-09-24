# Positioning (internal source of truth)

This is the messaging spine. README, landing pages, decks, and talks derive from it; do not
invent claims elsewhere that are not backed here. The governing rule: **every headline claim
has a click-through proof, or it does not go in the headline.** Accuracy is the product's moat
with the buyer we want (regulated/fintech), so overclaiming is not a growth hack here, it is a
credibility leak.

Status of the artifact: living doc. When a claim's backing changes (a proof compiles, a
benchmark shifts, a feature ships), update it here first, then propagate.

---

## 1. The one claim

> **One durable journal. Four guarantees no other agent library pairs: at-most-once side
> effects, a tamper-evident audit trail, provably convergent shared state, and all of it as a
> plain-Go library with no cluster.**

The synthesis that makes this a *position* and not a feature list: the four properties are not
four features bolted together. They are four things that fall out of the same append-only
journal done correctly. That is the moat. A competitor can copy any one pillar; the defensible
thing is that they share one substrate, so a buyer gets them from one mechanism instead of
integrating four systems (a durable-execution engine + an audit log + a CRDT/consensus layer +
a framework).

If a reader remembers one thing: **it is the durable agent runtime for work that must not
happen twice.**

---

## 2. The one-liner ladder

Pick altitude by context; keep the spine identical.

- **Tagline (visceral, 6 words):** *Durable agents that can't double-charge.*
  ("Double-charge" is concrete and felt; "at-most-once" is jargon. Use the plain word first,
  the term second.)

- **One sentence:** *A plain-Go agent library where side effects fire at most once, every step
  is cryptographically auditable, and shared state provably converges, all from one durable
  journal, no cluster.*

- **Paragraph (the README top-fold):** the four pillars, sequenced, benchmark table under
  pillar 1. See `README.md`.

- **Elevator (spoken, ~20s):** *Every durable-execution system for agents (Temporal, DBOS) and
  every agent framework (ADK, eino, trpc) resumes a crashed run by re-running the step. If that
  step charged a card, it charges twice. We built the crash benchmark that measures it: they
  double-fire 4 to 64 times, we hold at one. And because the journal that guarantees that is
  also append-only and hash-committed, it doubles as a tamper-evident audit trail. All as a Go
  import, against a Postgres you already run.*

---

## 3. Audiences and the wedge for each

**Primary: regulated / fintech engineering.** Highest willingness to pay, and the two pillars
that are hardest to fake (at-most-once + audit) are exactly what they buy. Lead with the
double-charge story, close with the audit spine.

| Audience | Open with | Close with | Why this order |
|---|---|---|---|
| **Regulated / fintech (primary)** | At-most-once (no double-charge), measured | The RFC 6962 audit spine | Their two non-negotiables; both provable today |
| Go platform engineers | "Temporal guarantees as a library, no cluster" | Multi-node failover, stdlib core | Infra-fatigue is the wedge |
| AI agent builders | "Agents that survive crashes and don't repeat side effects" | Plain-Go authoring, any model | Broadest but noisiest category; lead with reliability, not features |

For any audience, the sequencing principle is the same: **open with the claim that is most
visceral (the double-charge), and hold the deepest/most-technical claim (convergence) as depth
for a reader who is already leaning in.** All four pillars are now proven and click-through
backed; sequencing is about attention and impact, not about hiding a weak claim.

---

## 4. The four pillars

Each pillar has: the claim, the proof (what a skeptic can click), the scope (the line we
do not cross), and the phrasing to use / avoid.

### Pillar 1: At most once, not at least once

- **Claim:** a non-idempotent side effect fires at most once across any crash schedule.
- **Proof:** the `chaos/` crash-injection benchmark and cross-SDK results in `benchmarks/`.
  Measured: go-agents `maxFired=1`; trpc `5`; adk `4`; langchaingo `64`; eino `64`. The
  competitor adapters each carry a *fairness test* proving their resume genuinely works, so the
  double-fires are real, not a rigged setup.
- **Mechanism (the part nobody else has):** a durable **attempt marker** written before a
  non-idempotent write, plus **halt-on-unknown-outcome** on resume. If a write's result was
  never journaled, the run stops for a human decision instead of re-running.
- **Scope:** the guarantee is for side effects declared non-idempotent through the
  `Safety` type; read-only and idempotent tools re-run freely by design. We say "at most once
  for declared side effects," not "exactly once for everything."
- **Say:** "won't double-charge," "measured, not claimed," "the benchmark is the product."
  **Avoid:** "exactly-once" as an unqualified absolute (it invites the distributed-systems
  pedant; "at most once, and it halts rather than guess" is the precise, defensible frame).

### Pillar 2: Durable execution as a library, not a cluster

- **Claim:** Temporal-class durability without operating a server or worker fleet.
- **Proof:** a hello-world imports the standard library only, enforced by
  `architecture_test.go`. Durability comes from a store adapter (SQLite, Postgres).
- **Scope:** we are not claiming Temporal's full feature set (schedules, signals at
  their scale, visibility tooling). The claim is specifically the *durable-resume guarantee* as
  a library. Say "the guarantees you wanted Temporal for, as an import," not "replaces
  Temporal."
- **Say:** "library, not infrastructure," "import it, don't operate it."

### Pillar 3: A tamper-evident audit spine from the same journal

- **Claim:** the durable journal is also a verifiable, selectively-disclosable, tamper-evident
  record, with continuous out-of-band anchoring.
- **Proof:** the `audit/` package. RFC 6962 (Certificate Transparency) inclusion + consistency
  proofs + signed tree heads, checked against the published RFC reference vectors.
  `AuditedStore` auto-signs and publishes an STH per step to an external transparency log.
  Verification is one flow across the journal, the live event stream, and separate-retention
  storage. → `docs/AUDIT.md`.
- **Scope (load-bearing):** **integrity is unconditional; tamper-evidence requires
  anchoring the commitment out-of-band.** A hash tree in a DB the attacker controls can be
  rewritten and rehashed. We ship the anchoring machinery; we never say "tamper-proof" or
  "immutable." This is the sell to a security buyer who has been lied to by everyone
  else's "immutable audit log."
- **Say:** "tamper-evident when anchored," "prove one action without revealing the rest," "no
  other agent framework has this at all" (true).
  **Avoid:** "tamper-proof," "immutable," "blockchain."

### Pillar 4: Provably convergent shared state (gsm)

- **Claim:** multiple processes replaying the same durable log converge on identical state.
- **Proof:** the gsm engine's normalization-confluence result, written up and published in the
  `normalization-confluence` papers (`normalization_confluence_2026` and the federated-registry
  version). This pillar now has the same click-through backing as 1 to 3; it is fully
  co-headlined, not hedged.
- **Scope (precision, not a hedge):** the claim is **confluence of the normalization rewrite
  system** (replay order cannot change the result), NOT "agents always agree on a correct
  answer" and NOT "automatic consensus." Keep this scope in the sentence every time. This is
  not tentativeness: it is the exact wording that lets us assert the claim *aggressively*
  without an evaluator catching an overreach. Confident about what is proven; precise about
  what that is.
- **Say:** "provably convergent shared state," with the confluence scope. Lead with it as a
  co-equal pillar now that the proof is published.
  **Avoid:** "consensus," "always correct," "CRDT" (unless precise), any *unscoped* "provable"
  (the scope is what makes it defensible, so it stays even when we assert confidently).

---

## 5. Competitor map

Measured numbers only where we measured; everything else qualitative.

| | go-agents | Temporal / DBOS | ADK · eino · trpc · langchaingo | Sema4.ai |
|---|---|---|---|---|
| Non-idempotent side effect on crash | **At most once (halts on unknown)** | At least once; steps must be idempotent | At least once; re-runs (**measured 4–64×**) | Not claimed (their "deterministic" = reproducible output) |
| Deployment | **Library + a DB you run** | Server + worker fleet | Library | Cloud / VPC-native platform you operate |
| Tamper-evident audit | **RFC 6962 spine (same journal)** | Not built in | None | Observability / trace logs (trust-based, not cryptographic proofs) |
| Convergent shared state | **Provable (gsm)** | N/A | None | N/A |
| Ecosystem | Go | Go / multi | Go | Python |

Notes for use:
- We have **measured** double-fire counts only for the four agent frameworks (fair adapters in
  `benchmarks/`). Temporal and DBOS are described qualitatively ("resume by re-running; steps
  must be idempotent"), which is accurate and documented by them; do not invent a number for
  them.
- DBOS is the closest of the "re-run" camp (it markets once-and-only-once for completed steps);
  the real differentiator is still the attempt-marker + halt for the execute-to-persist
  window, which it does not have. Be precise, not dismissive.
- The competitor adapters are deliberately fair. That discipline is itself an asset: say we
  represented each SDK at its best and still hold the only `maxFired=1`.

### The Sema4.ai contrast (the vocabulary incumbent)

Sema4.ai (Robocorp + an AI layer, ~$55M raised, ex-Hortonworks/Cloudera founders) is the most
positioning-relevant competitor for our **primary buyer**, more so than langchaingo: it is a
cloud/VPC-native enterprise agent **platform** for regulated back-office finance (invoice
reconciliation, AP, SOX), and its headline words are literally **"deterministic, auditable
outcomes."** It owns our vocabulary, for our buyer, with real funding and SOC2/ISO27001/HIPAA
motion behind it. Treat it as the incumbent to differentiate *against*, not to ignore.

The differentiation is **substance under the same words**:

- **"Deterministic" (theirs is reproducibility, ours is crash-safety).** Sema4's determinism
  means "same query returns the same result" (a semantic data layer + promoting patterns to
  versioned Python modules). That is reproducibility of output. It is **not** side-effect-safe
  resume; their public material makes no at-most-once-across-a-crash claim. Ours is the
  attempt-marker + halt guarantee, measured (`maxFired=1`).
- **"Auditable" (theirs is observability, ours is cryptographic proof).** Sema4 offers
  "three-lens observability" and full run traceability: rich logs an auditor trusts *because
  the vendor is SOC2*. Ours is RFC 6962 inclusion/consistency proofs + STH, **verifiable by a
  third party without trusting us**, anchored out-of-band. "Logs you trust" vs "proofs you can
  verify."
- **Platform vs library.** Sema4 is a hosted runtime you operate inside; go-agents is a Go
  import against a DB you already run. Opposite deployment models. Different ecosystems (Python
  vs Go), so not a drop-in substitution either way.

Guardrail on this contrast: the "they don't do X" claims are inferred from **what they
do not advertise**, not a teardown. Say "they do not claim crash-safe at-most-once / their audit
is observability, not cryptographic proof," never "they can't." Lead with the sharper *thing we
prove*, not a negative about them.

Copy implication: because Sema4 owns the bare words "deterministic" and "auditable" in this
buyer's mind, **do not lead with those words**. Lead with the sharper versions: "won't
double-charge, measured" and "cryptographically verifiable, not just logged." The three-line
wedge: *their observability you must trust vs our proofs you can verify; their reproducible
agents vs our crash-safe (measured) ones; their platform you operate vs our library you import.*

---

## 6. Reframe: turn their strengths into our bar

The highest-leverage move in this whole doc: take the word a well-funded competitor spent
millions teaching our buyer to want, concede it, then raise the bar to the reading only we
meet. We do not create demand; we invert their marketing. Every competitor headline word has a
*weak reading* they quietly rely on and a *strong reading* they cannot deliver. Agree with the
premise (disarming), then show their delivery stops short of it.

The pattern, every time: **"Yes, X matters, which is exactly why we do the version of X that
actually holds."**

| They say | Their (weak) reading | Our (strong) reading = the new bar |
|---|---|---|
| Sema4: **"deterministic"** | Same query → same answer (reproducible output) | Fires **at most once across a crash** (measured). "Deterministic output is table stakes; deterministic execution *under failure* is the hard part." |
| Sema4: **"auditable"** | Rich logs an auditor trusts because the vendor is SOC2 | Cryptographic proofs a third party verifies **without trusting us**. "Auditable should mean *provable*, not *loggable*." |
| Temporal: **"durable execution"** | Guarantees, if you run a server + worker fleet | The same guarantees as a **library**, against a DB you already run. "Durable execution shouldn't require operating a cluster." |
| Temporal / DBOS: **"exactly-once"** | Once, *if your activity/step is idempotent* (your problem) | We close the execute→persist window they hand back to you (attempt-marker + halt). |
| Agent frameworks: **"checkpoint / resume"** | Resume by re-running the step | Resume that **never re-fires a side effect** (they measured 4–64×; we hold 1). |
| "**shared state / multi-agent**" | Best-effort, or a single-writer bottleneck | **Provably convergent** replay of a durable log (confluence, published proof). "Shared state should be *provably* order-independent, not hopefully consistent." |

### The three rules that keep this accurate (not spin)

This move is powerful *because* the proofs exist. It goes hollow the instant we overreach, so:

1. **Only reframe a word we can out-prove with a click.** All four bars now clear this: `audit/`
   + the RFC 6962 vectors back "auditable → verifiable"; the `benchmarks/` numbers back
   "resume → no double-fire"; and the published `normalization-confluence` papers back "shared
   state → provably convergent." Keep the rule as the gate for *future* reframes: no claim ships
   as a bar until its proof is clickable.
2. **Raise the bar; never call theirs a lie.** Say "reproducible output is the floor;
   crash-safety is the bar," not "Sema4 isn't really deterministic." The first is a confident
   category definition; the second is an attack we cannot fully substantiate (we infer from
   their marketing, not a teardown) and it makes us look small. This is guardrail §8.2 applied.
3. **Redefine the category, don't just win a feature.** The strongest form is not "we are more
   auditable than Sema4." It is planting a **definition**: *"Auditable means
   verifiable-without-trust; anything less is just logging."* Now every competitor is measured
   against our bar, and the buyer re-reads their page through our frame. That is positioning,
   not comparison.

Use this as the engine behind every comparison line, headline, and objection response. If a
proposed reframe fails rule 1 (no clickable proof), it does not ship until the proof does.

---

## 7. Proof assets (what every claim points to)

- **`chaos/` + `benchmarks/README.md`**: the crash benchmark and cross-SDK table (pillar 1).
- **`architecture_test.go`**: stdlib-only core enforcement (pillar 2).
- **`audit/` + `docs/AUDIT.md`**: RFC 6962 proofs, STH, `AuditedStore`, transparency log,
  checked against published CT reference vectors (pillar 3).
- **`normalization-confluence` papers** (`normalization_confluence_2026`, federated version):
  the published gsm confluence proof (pillar 4). Compiled and published.
- **`dst_test.go`, `saga_dst_test.go`**: deterministic simulation tests proving at-most-once
  under a crash-point sweep + randomized schedules (supporting pillar 1).

---

## 8. Messaging guardrails

1. No unqualified "exactly-once," "tamper-proof," "immutable," or unscoped "provable."
2. Every competitor comparison stays fair: measured where measured, qualitative otherwise, and
   never a strawman (we ship the fairness tests that prove it).
3. Pillar 4 always carries its confluence scope in the same sentence.
4. Lead with the plain-language pain (double-charge), then the term (at-most-once).
5. The benchmark number is the marketing. Show it before adjectives.
6. When a claim's proof is not yet clickable (gsm), say so plainly rather than implying it is.

---

## 9. Objections and responses

- *"Isn't this just exactly-once, which is theoretically impossible?"* We do not claim
  exactly-once delivery. We claim at-most-once execution of declared side effects, and on an
  unknown outcome we halt for a human instead of guessing. That is a weaker, achievable, and
  more accurate guarantee than the impossible one.
- *"Temporal already does durable execution."* It does, and it needs a server + worker fleet,
  and its activities must be idempotent (a non-idempotent one double-fires on worker crash). We
  give the resume guarantee as a library and close the double-fire window they leave to you.
- *"Your audit log lives in my database, so I can rewrite it."* Correct, and we say so.
  Integrity is unconditional; tamper-evidence requires anchoring the signed head out-of-band,
  which `AuditedStore` + the `Anchor` port do. We never claim your DB is immutable.
- *"Provable convergence sounds like hand-waving."* The claim is narrow and mathematical, and
  it is published: the normalization rewrite system is confluent, so replay order cannot change
  the result. See the `normalization-confluence` papers. It is not "agents always agree"; it is
  order-independent convergence of the replay, which is a proven property, not an aspiration.

---

## 10. Open positioning debts

Ranked by leverage:

1. **Name the product.** "go-agents (working codename)" in an H1 undercuts a compliance buyer.
   Highest-leverage unfinished item for this positioning. Ruled out: **Semel** (Latin "once",
   semantically ideal) for being too close to **Sema4.ai**, a funded incumbent in our exact
   auditable-finance-agents lane; the name would invite conflation with the competitor we
   differentiate against. Also ruled out on collision: Cairn, Keel, Ballast. Surviving
   plain-word candidates: **Docket**, **Holdfast**. Name toward the verifiable-ledger /
   crash-safe center of gravity, not the "deterministic/auditable" words Sema4 owns.
2. ~~Compile/publish the gsm confluence proofs.~~ **Done.** The `normalization-confluence`
   papers are compiled and published (public repo), so pillar 4 is fully co-headlined (see §4).
   A polished public host (arXiv / a docs site) would further strengthen external click-through,
   but the proof is already reachable.
3. **A standalone competitor-comparison doc** with methodology, so the benchmark table has a
   rigorous backing page to link.
4. **Tag/publish the core** (retires the `benchmarks/` replace directives; makes "it's a
   library you import" literally true for outsiders).
