# Positioning (internal source of truth)

This is the messaging spine. README, landing pages, decks, and talks derive from it; do not
invent claims elsewhere that are not backed here. The governing rule: **every headline claim
has a click-through proof, or it does not go in the headline.** Honesty is the product's moat
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
visceral AND most proven; hold the newest/least-finished claim (gsm) as depth for a reader who
is already leaning in.** Opening with an unbacked claim inverts trust.

---

## 4. The four pillars

Each pillar has: the claim, the proof (what a skeptic can click), the honest scope (the line we
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
- **Scope / honesty:** the guarantee is for side effects declared non-idempotent through the
  `Safety` type; read-only and idempotent tools re-run freely by design. We say "at most once
  for declared side effects," not "exactly once for everything."
- **Say:** "won't double-charge," "measured, not claimed," "the benchmark is the product."
  **Avoid:** "exactly-once" as an unqualified absolute (it invites the distributed-systems
  pedant; "at most once, and it halts rather than guess" is the precise, defensible frame).

### Pillar 2: Durable execution as a library, not a cluster

- **Claim:** Temporal-class durability without operating a server or worker fleet.
- **Proof:** a hello-world imports the standard library only, enforced by
  `architecture_test.go`. Durability comes from a store adapter (SQLite, Postgres).
- **Scope / honesty:** we are not claiming Temporal's full feature set (schedules, signals at
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
- **Scope / honesty (load-bearing):** **integrity is unconditional; tamper-evidence requires
  anchoring the commitment out-of-band.** A hash tree in a DB the attacker controls can be
  rewritten and rehashed. We ship the anchoring machinery; we never say "tamper-proof" or
  "immutable." This honesty is the sell to a security buyer who has been lied to by everyone
  else's "immutable audit log."
- **Say:** "tamper-evident when anchored," "prove one action without revealing the rest," "no
  other agent framework has this at all" (true).
  **Avoid:** "tamper-proof," "immutable," "blockchain."

### Pillar 4: Provably convergent shared state (gsm)

- **Claim:** multiple processes replaying the same durable log converge on identical state.
- **Proof (partial today):** the gsm engine's normalization-confluence result. **The proof
  artifacts are not yet compiled/published**, which is the gap that keeps this pillar from full
  headline parity with 1 to 3.
- **Scope / honesty (critical):** the claim is **confluence of the normalization rewrite
  system** (replay order cannot change the result), NOT "agents always agree on a correct
  answer" and NOT "automatic consensus." State the narrow mathematical claim every time this
  appears. This is the pillar most likely to be attacked first by a technical evaluator, so it
  carries the tightest scoping.
- **Positioning decision (open):** currently **co-headlined** with a scoping parenthetical (per
  owner's call), flagged as the newest tier. The safer alternative is "depth, not headline"
  until the proofs compile. Revisit once pillar 4 has a click-through proof; a co-headline
  claim without linkable evidence is the weakest link in an otherwise fully-backed set.
- **Say:** "provably convergent shared state," always with the confluence scope.
  **Avoid:** "consensus," "always correct," "CRDT" (unless precise), any unscoped "provable."

---

## 5. Competitor map

Honest contrast. Measured numbers only where we measured; everything else qualitative.

| | go-agents | Temporal / DBOS | ADK · eino · trpc · langchaingo |
|---|---|---|---|
| Non-idempotent side effect on crash | **At most once (halts on unknown)** | At least once; steps must be idempotent | At least once; re-runs (**measured 4–64×**) |
| Deployment | **Library + a DB you run** | Server + worker fleet | Library |
| Tamper-evident audit | **RFC 6962 spine (same journal)** | Not built in | None |
| Convergent shared state | **Provable (gsm)** | N/A | None |

Notes for honest use:
- We have **measured** double-fire counts only for the four agent frameworks (fair adapters in
  `benchmarks/`). Temporal and DBOS are described qualitatively ("resume by re-running; steps
  must be idempotent"), which is accurate and documented by them; do not invent a number for
  them.
- DBOS is the closest of the "re-run" camp (it markets once-and-only-once for completed steps);
  the honest differentiator is still the attempt-marker + halt for the execute-to-persist
  window, which it does not have. Be precise, not dismissive.
- The competitor adapters are deliberately fair. That discipline is itself an asset: say we
  represented each SDK at its best and still hold the only `maxFired=1`.

---

## 6. Proof assets (what every claim points to)

- **`chaos/` + `benchmarks/README.md`**: the crash benchmark and cross-SDK table (pillar 1).
- **`architecture_test.go`**: stdlib-only core enforcement (pillar 2).
- **`audit/` + `docs/AUDIT.md`**: RFC 6962 proofs, STH, `AuditedStore`, transparency log,
  checked against published CT reference vectors (pillar 3).
- **gsm confluence proofs**: *to be compiled/published* (pillar 4; the outstanding proof debt).
- **`dst_test.go`, `saga_dst_test.go`**: deterministic simulation tests proving at-most-once
  under a crash-point sweep + randomized schedules (supporting pillar 1).

---

## 7. Messaging guardrails

1. No unqualified "exactly-once," "tamper-proof," "immutable," or unscoped "provable."
2. Every competitor comparison stays fair: measured where measured, qualitative otherwise, and
   never a strawman (we ship the fairness tests that prove it).
3. Pillar 4 always carries its confluence scope in the same sentence.
4. Lead with the plain-language pain (double-charge), then the term (at-most-once).
5. The benchmark number is the marketing. Show it before adjectives.
6. When a claim's proof is not yet clickable (gsm), say so plainly rather than implying it is.

---

## 8. Objections and honest responses

- *"Isn't this just exactly-once, which is theoretically impossible?"* We do not claim
  exactly-once delivery. We claim at-most-once execution of declared side effects, and on an
  unknown outcome we halt for a human instead of guessing. That is a weaker, achievable, and
  more honest guarantee than the impossible one.
- *"Temporal already does durable execution."* It does, and it needs a server + worker fleet,
  and its activities must be idempotent (a non-idempotent one double-fires on worker crash). We
  give the resume guarantee as a library and close the double-fire window they leave to you.
- *"Your audit log lives in my database, so I can rewrite it."* Correct, and we say so.
  Integrity is unconditional; tamper-evidence requires anchoring the signed head out-of-band,
  which `AuditedStore` + the `Anchor` port do. We never claim your DB is immutable.
- *"Provable convergence sounds like hand-waving."* The claim is narrow and mathematical:
  the normalization rewrite system is confluent, so replay order cannot change the result. The
  proof artifacts are being compiled; until they are linkable, treat this pillar as the newest
  and least-finished.

---

## 9. Open positioning debts

Ranked by leverage:

1. **Name the product.** "go-agents (working codename)" in an H1 undercuts a compliance buyer.
   Highest-leverage unfinished item for this positioning.
2. **Compile/publish the gsm confluence proofs.** Gives pillar 4 the click-through evidence the
   other three have; the precondition for co-headlining it without hedging.
3. **A standalone competitor-comparison doc** with methodology, so the benchmark table has a
   rigorous backing page to link.
4. **Tag/publish the core** (retires the `benchmarks/` replace directives; makes "it's a
   library you import" literally true for outsiders).
