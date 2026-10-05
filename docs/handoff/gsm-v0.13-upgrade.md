# Handoff: upgrade gsm v0.12.0 to v0.13.0

Status: ready. gsm v0.13.0 is released
([release notes](https://github.com/blackwell-systems/gsm/releases/tag/v0.13.0),
[CHANGELOG](https://github.com/blackwell-systems/gsm/blob/main/CHANGELOG.md)). A trial upgrade of
this repository at `eb0aa3a` passed everything listed below, so the bump itself is mechanical. The
work in this handoff is the code and wording that v0.13.0 makes stale.

## Why upgrade

v0.13.0 checks the conditions Bide's federated guarantee depends on. Before it, two were assumed:

- **Event order across registries (C1, C2).** `Federation.Build` now rejects a federation where a
  target event races a source change (`CrossOrderError`), or where two target events commute on
  their own registry but not across the morphism repair between them (`SameTargetOrderError`).
  On v0.12.0 such a federation built, and its outcome could depend on which agent appended first.
  Replicas still agreed (they replay one shared `EventLog`), but the agreed result was
  timing-dependent, which is exactly what a governed federation is meant to rule out.
- **Event order on monotone cycles.** The same C1 and C2 checks now certify cycles built with
  `AllowMonotoneCycles` (mechanized as `cyc_check_gc_lfp`). This covers `examples/govern/mesh`.

The theory behind this changed: the published claim that cross-registry conflicts resolve without
coordination by the authority argument alone is false. Repair composes freely (the federated normal
form is unique), but event order needs C1 and C2. The corrected statements and counterexamples are
in the version 2 papers and in
[normalization-confluence](https://github.com/blackwell-systems/normalization-confluence)
(`FederationGRS.v`: `fed_thm_fed_convergence_refuted`, `fed_thm_fed_convergence_guarded`).
`FedMachine.Apply` is the guarded system the corrected theorem is proved for, so Bide's federated
governor is covered once C1 and C2 pass.

## Trial upgrade results (at `eb0aa3a`)

| Check | Result |
|---|---|
| `go get github.com/blackwell-systems/gsm@v0.13.0` in `govern`, `govern/redislog`, `govern/postgreslog` (indirect), `govern/sqlitelog`, `integration`, `examples/govern`; `go work sync` | clean |
| `go build ./...`, `go vet ./...` in each module | clean |
| `go test ./...` in each module | all pass |
| `go run` every program in `examples/govern` (authority, compliance, compose, coordination, delegation, earned-authority, mesh, proof-carrying-run, quorum) | all exit 0; `coordination` still shows plain `Build` rejecting its cycle, as intended |

No federation in this repository fails C1 or C2. If a new one does after the upgrade, treat the
rejection as a real finding: the error names both events, the target and a witness state where
the two orders diverge.

## Tasks

1. **Bump gsm to v0.13.0** in the six `go.mod` files above and run `go work sync`. Re-run the
   checks in the table, including every example program (a C1 or C2 failure only shows when
   `Build` runs).
2. **`govern/confluence.go`: the certificate.**
   - `PairsDisjoint` has been always 0 for `Build` since v0.12.0 (the comment already says so).
     Keep it for format compatibility or drop it, and mirror the decision in `cmd/bide-audit`
     (`main.go`, the `pairs_disjoint` field near line 1352).
   - `Converges` is `WFC && CC` and ignores the obligations v0.13.0 reports. Carry them in the
     certificate: `Report.CausalOrderRequired` (undeclared pairs that must be delivered in causal
     order), `Report.NotIdempotent` (events that need deduplication under at-least-once delivery),
     and `Report.Saturations`.
   - State what Bide already guarantees for two of those, so the certificate does not read as an
     open obligation: the shared `EventLog` with fold-before-append is a causally consistent total
     order, and `ApplyOnce` deduplicates by id.
3. **`govern/federated.go`: refuted wording.** The `FederatedGovernor` doc comment (line 49,
   "Cross-registry conflicts resolve WITHOUT coordination via the authority argument (paper §8.3)")
   and the `FederatedEventTool` comment (line 233, "conflicts resolved by the authority argument")
   repeat the refuted claim. Replace with: the authority argument makes the federated repair
   terminate in a unique valid normal form, and event order across registries is safe because
   `Federation.Build` checks C1 and C2 (it rejects a federation that fails them).
4. **Docs with the same claim or now-stale wording.**
   - `docs/guides/governance.md`: the federation section and the coordination paragraph near line
     300. The coordination plan is "a correct coordination of size at most the number of
     independent cycles", not minimal, and each `CoordinationPoint` now names its `Authority` (the
     root the cycle is driven from; the normal form depends on which edge is cut).
   - `README.md` section 4, `docs/GUARANTEE.md`, `docs/KNOWN-LIMITATIONS.md`,
     `docs/formal-verification.md`, `docs/CONCEPTS.md`: anything stating that federated
     convergence follows from M1 or the authority argument alone, and any theorem count (the
     development is now 729 axiom-free theorems; prefer linking to `coq/verify.sh` over a number).
   - `docs/ROADMAP.md`: the federation-oracle item is unchanged (still open in gsm); add that C1/C2
     and cycle event order are checked as of gsm v0.13.0.
   - Open PR #170 (causal and oracle-scope docs) touches the same pages; rebase it after this, or
     fold its changes in.
   - Regenerate `docs/i18n` with the repository's usual process after the English text settles.
5. **Framing.** Where Bide's README and guides describe the guarantee, use the canonical line for
   the theory: "an exact regime map of governed concurrent state: in every regime, a
   machine-checked exact condition, a hardness result showing no efficient one exists, or a gap
   stated in the open, with a checker for the practical ones", and the concept name
   **convergence by compensation**. Do not use "complete" or "exact in every regime": the regime
   audit (`REGIME-AUDIT.md` in normalization-confluence) found those overclaim. For federations,
   state the split the corrected theory makes: repair composes freely (unique federated normal
   form), and event order across registries costs two local checks per edge (C1, C2) that gsm runs
   at build time. Scope is discrete, deterministic state; the exact conditions quantify over
   reachable states; gsm checks the cheap sufficient ones.
6. **Optional: surface the new federation report.** `FedReport.Checks`, `FedReport.Assurance` and
   `FedReport.Runtime` state which federation-level checks ran and whether certified subs execute
   verified tables. `bide-audit` could print them next to the registry certificate.

## API changes in v0.13.0 that touch Bide

Bide uses `NewRegistry`, the rule combinators (`Set`, `Do`, `SetTo`, `Lit`, `Is`, `Le`, `Or`,
`Add`, `Inc`), `Machine`, `Report`, `NewFederation`, `FedMachine`, `FedState`,
`AllowMonotoneCycles`, `CoordinationPlan` and `BuildCoordinated`. None of these changed signature.
Behavior changes that could matter:

- `Machine.Apply` normalizes an invalid input before applying the event. Replays from a valid
  initial state are unaffected.
- `Int(min, max)` panics at declaration if the range overflows an int.
- A second `Resolve` for the same target panics.
- `Embed`/`EmbedCertified` no longer copy `AllowMonotoneCycles`; a parent with a cycle must opt in
  itself.
- `BuildCoordinated` rejects a `CoordinationPoint.Authority` other than `Dst` (empty is accepted;
  `CoordinationPlan` fills it).
- `DiagnoseCycle`: `Converges == false` no longer means "no consistent state exists"; use
  `Obstructed()`. Bide does not call it today.
- Report text changed; parse fields, not text.

The full list with migration notes is in the release notes' "Breaking / behavior changes" table.
