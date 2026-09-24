# Tier 2: Distributed (gsm-backed) Compensating Agents — research seed

The moonshot: **provably order-independent convergence of compensation across ≥2
concurrent agents mutating shared state.** Tier 1 (sequential/hierarchical saga, built)
reverses one causal order. Tier 2 has NO single order — correctness must be a build-time
algebraic property. `gsm` (normalization confluence) is the engine.

## Verdict

Real and defensible; **nobody offers this for AI agents.** But it's a *research project
with a fast narrow PoC*, not a quick product. PoC 4–6 wks; usable internal ~3 mo; production
w/ federation 9–12 mo. The hard part is NOT runtime (gsm gives O(1) convergent apply) — it's
the **modeling discipline**: forcing fuzzy LLM actions through a finite, verified event
alphabet, and drawing the "agent decides / machine governs" boundary.

## Positioning (the empty lane)

Axis = how rollback treats ordering: **(a) reverse-the-log along one causal order** (Saga,
Temporal, Restate, TCC, process managers, our Tier 1) vs **(b) converge-to-normal-form
regardless of interleaving** (gsm). Temporal/Restate give crash-durable *single-workflow*
sagas and explicitly punt concurrent-writer anomalies to "semantic locks / commutative
design." CRDTs need raw commutativity (can't express "never ship unpaid"). I-confluence
(Bailis, VLDB'14) requires ops *preserve* invariants; CALM requires monotonicity. gsm is the
third option: *allow violation, then compensate to a unique normal form* — commutativity
modulo normalization (`NF(e1;e2)=NF(e2;e1)`), proven at build time (WFC + CC, Newman's lemma).
LangGraph/AutoGen/CrewAI have compensation-ish primitives but no convergence guarantee
(2026 surveys confirm "no atomicity / no systematic compensation").

## gsm → multi-agent mapping (verified against source)

| Multi-agent | gsm | anchor |
|---|---|---|
| agent tool call mutating shared state | Event (Writes/Guard/Apply) | registry.go:190 |
| shared domain (status, inventory, balances) | finite-domain State vars | registry.go:85 |
| business rules | Invariants (Watches/Holds) | README example |
| compensations | Repair (footprint-bounded) | verify.go:277 |
| "these actions may arrive either order" | Independent(e1,e2) | registry.go:57 |
| convergent runtime | machine.Apply = step[event][state], O(1) | machine.go:32 |

Constraints: finite governed state (≤~1M states / ≤20 bits) — governs the *shared
invariant-bearing state only*, not agent reasoning; events are deterministic effects with
declared write-sets; repairs identity-on-valid + terminate (WFC); declared-independent pairs
commute after normalization (CC). Over-declaring Independent = safe; under-declaring = unsafe.

## Substrate options

- **A — single shared governed-state row in Postgres, one registry.** Governor applies
  `machine.Apply` + CAS. Simplest; best PoC; partition by entity for throughput; ≤1M states.
- **B — event-sourced unordered log per registry.** Any reader folds the event *set* → same
  normal form (paper Thm 5.1). Multi-reader/replica convergence + audit trail; needs
  idempotent event identity + compaction. `Export()` lets non-Go replicas converge identically.
- **C — federated registries** (paper §7). The scaling answer past 1M states; **unbuilt**
  everywhere; the real research frontier. Phase 3+.

## Architecture (governor sits BESIDE the durable journal, orthogonal — per DESIGN.md)

Agents (go-agents LLM loops) → **Governor middleware** (`func(Handler) Handler` at the tool
boundary: classify tool→event, Guard, reject if ungoverned) → (a) durable event append
(idempotent) into the existing journal, (b) advance shared governed state via a built
`*gsm.Machine` (`Apply` = pure O(1) lookup, naturally idempotent, so it slots *below* the
side-effect-safety layer without disturbing it). Ungoverned tools pass through untouched.

Key iface: `Governor.Apply(ctx, entityKey, event, args) (State, error)` — load governed state
for entityKey, apply event (table lookup), persist normal form (optimistic CAS).

## Phased plan

- **Phase 0 — prereqs: ✅ DONE.** Single-flight `Durable.Do` across all stores (concurrent
  same-key = fn once), `-race`-proven. (Stable execution-index deferred as benign — see
  KNOWN-LIMITATIONS.) In-memory `Governor` state suffices for the PoC; a persistent
  `governed_state` table is a later hardening step.
- **Phase 1 — PoC / headline demo: ✅ DONE (`govern/` package).** `Governor` wraps a verified
  `gsm.Machine`; `EventTool` is the tool-boundary bridge (agent tool call → gsm event on
  shared state). Tests (all `-race`-clean): `BuildProvesConvergence` (build = the guarantee);
  `AllInterleavingsConverge` (any order of the proven-independent pair → same state);
  `ConcurrentAgentsConverge` (300 trials, real goroutines); **`TwoRealAgentsConverge`** (two
  actual `agent.Agent` loops sharing one Governor via governed tools, 50 concurrent trials,
  always converge); `CompensatesInvalidState` (invalid state auto-repaired).
  **Key validation:** gsm's build-time CC check *rejected* an incorrect independence claim
  (`ship_item ⊥ restock`) — proving the "modeling discipline is the hard part" thesis and
  that gsm won't let a divergent model ship. Core stays gsm-free (arch guard enforces it).
- **Phase 2 — event-sourced persistence: ✅ DONE (in-memory log).** `PersistentGovernor`
  durably appends every event to an `EventLog` and reconstructs state by REPLAYING the log
  on restart. Tested: `ReconstructsAfterRestart` (fresh governor over the same log rebuilds
  the exact state) and `ReplayOrderIndependent` (replaying the same independent event set in
  either order → same state — the Option-B convergence property). `MemEventLog` AND an
  on-disk SQLite `EventLog` (`govern/sqlitelog`) both ship —
  `TestSQLiteLog_DurableReconstructAcrossReopen` proves REAL crash recovery: apply events,
  close the DB file (process exit), reopen, reconstruct the exact state by replay. A
  Postgres `EventLog` (same 2-method interface, for multi-process HA) is a thin drop-in;
  cross-language replica via `Export()` remains a later item.
- **Phase 3 — modeling ergonomics:** DSL/adapter to author the event alphabet from tool
  schemas; quantization helpers (unbounded→bounded); surface gsm's CC counterexamples.
- **Phase 4 — federation (research, months):** paper §7; break the 1M ceiling.

## Grounded vs speculative

Grounded: the mapping, WFC/CC applicability, O(1) convergent runtime, governor-beside-journal
composition, the PoC, the "nobody offers this for agents" positioning. Speculative: federation
at scale (unbuilt), quantizing messy real domains into ≤1M states, and how gracefully the
agent-decides/machine-governs boundary holds for real workflows — the research risks.

Sources (verified): Garcia-Molina & Salem "Sagas" (SIGMOD'87); Helland "Life Beyond
Distributed Transactions" (CIDR'07); Shapiro et al. CRDTs (SSS'11); Bailis et al.
I-confluence (VLDB'14); Temporal/Restate saga docs; SagaLLM (arXiv:2503.11951 — saga-style,
NOT a convergence proof); gsm THEORY.md.

Citation caveat: an earlier draft cited arXiv:2606.17182 / 2607.00269 / 2605.03409 as
agent-compensation prior art. These have FUTURE-DATED arXiv IDs and are UNVERIFIED (likely
hallucinated) — treat as leads only, not established work. The core positioning does not
depend on them; it rests on the verified classical sources above + direct product-doc checks.
