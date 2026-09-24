# Tier-2 Federation: implementation plan

Turning the paper's Section 8 (Federated Convergence) into working code, split across gsm
(the convergence engine) and go-agents (the durable governor tier).

> Source: Blackwell, D. (2026), *Normalization Confluence in Federated Registry Networks*,
> Zenodo [DOI 10.5281/zenodo.18677400]. Federation is **§8** in the published PDF (§7 is
> Complexity Analysis). The gsm docs cite it as "Section 7" — stale; fix in a housekeeping pass.

## 1. What the paper gives us (and why it's tractable)

The result is stronger and *simpler to implement* than "distributed frontier" implied:

- **A federation is a registry** (§8 intro). The federated state space is the product of
  component spaces; morphism-consistency conditions are just extra invariants; morphism repair
  is just extra compensation. So federated convergence is a *corollary* of the single-registry
  theorem (Thm 5.4) once WFC+CC are shown to hold — and the paper shows both are **derived from
  the network's structure, not assumed** (Thm 8.8, Thm 8.9).

- **The normal form is constructive** (Corollary 8.10):
  1. Compute each **source** registry's single-registry normal form independently.
  2. Propagate shared components through morphisms in **topological order**.
  3. Let each **non-source** converge its local component via its own (component) CC.

  This is the whole algorithm. Critically: **we never build the product machine.** We keep the
  component `gsm.Machine`s separate and only compute per-component normal forms plus propagate a
  small shared-component message along each edge. → federation *sidesteps* gsm's >1M-state build
  ceiling entirely.

- **Two structural conditions, both decidable at build time:**
  - *Acyclicity / tree shape* (Def 8.3): the morphism network is a directed forest — each
    component tree is rooted at a source (no incoming morphism), every non-source has **exactly
    one** incoming edge. Necessity proven: a cyclic network with all conditions met still cycles
    (Prop 8.13).
  - *M1 — validity preservation under overwrite* (Def 8.1): overwriting a valid target's shared
    component with the morphism image of a valid source keeps the target valid. Necessity proven:
    violating M1 makes compensation oscillate forever (Prop 8.14).

- **The authority argument** (§8.3): in a directed morphism ϕ: A→B, **A is authoritative over
  B's shared component** — the source's unique normal form deterministically fixes the target's
  shared state (Lemma 8.6). This is our **coordination-free conflict resolution**: when a source
  event and a target event race, the source wins, deterministically, with no locking. The
  manufacturer–supplier trace (§8.5) shows both event orderings converging to `(active, listed)`
  because "the supplier event's effect is overwritten by morphism repair: the manufacturer is
  authoritative."

- **The operator is one-shot** (Lemma 8.7): a single application of ρ_Fed produces a federally
  valid state. Phase 1 normalizes every component locally; Phase 2 walks edges in topo order and
  overwrites each target's shared component exactly once. M1 guarantees Phase 2 can't reintroduce
  local invalidity, so no iteration is needed. Federated events target one registry and modify
  only that component; cross-registry effects arise solely through morphism violation + repair.

## 2. Scope decision for v1

**In scope:** tree-shaped (forest) morphism networks — the full guarantee of Thm 8.9.

**Explicitly out (and detected + rejected at build time):** the **multi-source** case
(Remark 8.15) — a registry with two incoming morphisms from independent sources. There the
authority argument breaks (no single source determines the target) and it needs additional
machinery (composition-consistency along common ancestors, target-agreement predicates, or
conflict-resolution operators). The paper names this "the natural next problem." We mirror gsm's
existing philosophy: **refuse the model at `Build()` with a precise error** rather than silently
converge-or-not. This keeps the "gsm rejects divergent models" story intact at the federated tier.

## 3. gsm layer — new API (grounded in the existing builders)

gsm owns the theory (needs `Machine` internals: normal forms, state packing, validity). New file
`federation.go` (+ `morphism.go`). Mirrors the existing fluent `Registry`/`Machine` style.

```go
// A Federation is a network of component registries connected by directed morphisms.
type Federation struct { /* components, edges */ }

func NewFederation(name string) *Federation
func (f *Federation) Add(r *Registry) *Federation            // register a component (pre-Build)
func (f *Federation) Morphism(src, dst *Registry) *MorphismBuilder

// ϕ: src → dst controls a *shared* subset of dst's vars; the rest of dst is local.
type MorphismBuilder struct { /* ... */ }
func (mb *MorphismBuilder) Shared(dstVars ...Var) *MorphismBuilder      // designates dst's shared component
func (mb *MorphismBuilder) Map(fn func(srcNF State, dst State) State) *MorphismBuilder
    // given the source's normal form + current dst, return dst with ONLY shared vars overwritten
    // (Build verifies the fn touches nothing outside Shared()).
func (mb *MorphismBuilder) Add() *Federation

// Build runs all three checks and returns a constructive federated machine.
func (f *Federation) Build() (*FedMachine, *FedReport, error)
```

`Build()` performs, in order (any failure → typed error in `FedReport`, non-nil `error`):
1. **Component convergence:** `r.Build()` each component; propagate WFC/CC failures. (Reuse.)
2. **Forest check:** acyclic + every non-source has exactly one incoming edge. Multi-source or
   cycle → reject, citing the offending target and pointing at Remark 8.15 / Prop 8.13.
3. **M1 per morphism** (finite enumeration — the state spaces are finite, that's gsm's premise):
   - *Fast path* (Remark 8.2): if `dst`'s validity factors as `shared-valid ∧ local-valid`
     independently, M1 reduces to "∀ valid `srcNF`: `ϕ(srcNF)` is shared-valid" — `|valid(src)|`
     checks.
   - *General path:* ∀ valid `σ_src`, ∀ valid `σ_dst`: overwrite is valid — `|valid(src)|·|valid(dst)|`
     **per edge** (pairwise, never the global product).

```go
type FedMachine struct { /* topo order, component machines, morphisms */ }
type FedState  struct { /* per-registry State */ }

func (m *FedMachine) NewState() FedState
func (m *FedMachine) Of(s FedState, r *Registry) State               // read a component
func (m *FedMachine) Apply(s FedState, r *Registry, event string) FedState  // event targets r; then ρ_Fed once
func (m *FedMachine) Normalize(s FedState) FedState                  // the two-phase constructive operator
func (m *FedMachine) IsValid(s FedState) bool                        // all local + all morphism invariants
func (m *FedMachine) SharedProjection(s FedState, edge Edge) []byte  // ϕ_ij(nf_i) — the partial-sync message
```

`Normalize` = Corollary 8.10 verbatim: Phase 1 `Normalize` every component; Phase 2 walk edges in
topo order, `dst = ϕ_ij.Map(nf_i, dst)`. One pass. `Apply` = mutate target component, then one
`Normalize`.

## 4. go-agents layer — the federated governor tier

gsm stays pure. `govern/` consumes `FedMachine`, reusing the existing `EventLog` port
(Mem/SQLite/Redis all work unchanged).

```go
// FederatedGovernor: event-sourced governor over a gsm.FedMachine.
type FederatedGovernor struct { /* *gsm.FedMachine, state FedState, log EventLog, entity string */ }

func NewFederated(ctx, m *gsm.FedMachine, log EventLog, entity string, initial FedState) (*FederatedGovernor, error)
func (g *FederatedGovernor) Apply(ctx, registry, event string) error   // append (registry,event); ρ_Fed
func (g *FederatedGovernor) State() FedState
```

- **Event encoding:** each federated event is `(registry, event)`; append as one log entry
  (e.g. `registry\x1fevent`, or a small JSON `{"r":..,"e":..}`). Reconstruction replays the
  stream — identical to today's `PersistentGovernor`, one extra field.
- **Authority = deterministic conflict resolution.** Two agent subsystems (a manufacturer agent
  and a supplier agent) writing concurrently converge with **no coordination**: source is
  authoritative, morphism repair overwrites (proven, §8.5). This is the real "distributed
  compensating agents across ownership boundaries" story — each component registry is one
  organization / one agent subtree; morphisms are the cross-org constraints.
- **Partial synchronization** (the paper's named future-work, now concrete): because the normal
  form is constructive, a subtree governor needs only its parent's **shared projection**
  `ϕ_ij(nf_i)` — a small message along each tree edge, not the whole network state. `FedMachine.
  SharedProjection` exposes exactly that. Phase 2 deliverable: geographically/organizationally
  split governors, each owning one registry, exchanging only shared-component deltas on edges.

## 5. Test plan (prove it, don't assert it)

Mirrors how we proved the single-registry + saga tiers.

- **Golden trace:** reproduce §8.5 manufacturer–supplier. Both event orderings
  (`epub` first, `eexp` first) must converge to `(active, listed)`. Direct paper-to-test.
- **Permutation convergence:** reuse the existing 300-trial shuffled-order harness on a
  federated machine — every causal permutation of a federated event multiset lands on one state.
- **Necessity (build-time rejection):**
  - cyclic network (Prop 8.13 flip/id example) → `Build()` returns error.
  - M1-violating morphism (Prop 8.14) → `Build()` returns error.
  - multi-source target (Remark 8.15) → `Build()` returns error naming the target.
- **Event-sourced reconstruction:** apply a federated event sequence, drop the governor,
  reconstruct from the SAME log — state matches. Run across Mem, SQLite, Redis logs (the port
  already abstracts this).
- **Partial-sync equivalence:** a governor fed only its parent's `SharedProjection` reaches the
  same local normal form as one holding the full federated state (validates Corollary 8.10's
  "propagate shared components" as a network protocol).
- **Build-time contract:** `var _ ... = (*FedMachine)(nil)` style assertions; arch guard already
  forbids the core importing govern.

## 6. Sequencing

- **M0 (gsm core) — DONE.** `Federation`/`MorphismBuilder`/`FedMachine`/`FedState`, two-phase
  constructive `Normalize`, `Apply`, `IsValid`. Golden §8.5 trace + authority + cycle-reject
  tests green. (gsm `9cebe09`.)
- **M1 (gsm safety) — DONE.** `Build()` refuses non-convergent networks: multi-source rejection
  (Remark 8.15), M1 validity-preservation by finite enumeration (Prop 8.14), shared-only
  well-formedness, distinct component names. Necessity tests green. *gsm refuses non-convergent
  federations at build time.* (gsm `84494f0`.) Plus `FedMachine.ApplyNamed`/`Registries` for
  name-keyed replay (gsm `601cc1a`).
- **M2 (go-agents) — DONE.** `FederatedGovernor` (+ `FederatedApplier`, `FederatedEventTool`)
  over the existing `EventLog` port; event-sourced (registry,event) log; validate-before-append.
  Reconstruction tests green over Mem + SQLite; order-independent convergence + reject-unknown.
  (go-agents `govern/federated.go`, uncommitted — go-agents has no commits yet.)
- **M3 (partial sync):** `SharedProjection` + distributed-governor protocol + equivalence test.
- **M4 (frontier, not committed):** multi-source resolution (Remark 8.15) — target-agreement
  predicates / conflict operators. Research, deferred by design.

## 7. Risks / notes

- **No product blow-up.** We implement the constructive operator (Cor 8.10), never a product-
  registry `Build()`. Per-component machines stay small; M1 verification is per-edge pairwise.
  This is the load-bearing design choice — a naive "build the product registry" would hit gsm's
  >1M-state wall immediately.
- **M1 general-path cost** is `|valid(src)|·|valid(dst)|` per edge. Fine for typical component
  sizes; the Remark 8.2 fast path covers the common (independent shared/local validity) case
  cheaply. Document the cost; offer both paths.
- **Multi-source is genuinely open** (per the author). v1 rejects it loudly rather than pretending.
- **gsm doc-drift:** update "Section 7" → "Section 8" refs in gsm THEORY/README/etc. (trivial).
