# Formal verification

bide checks the designs of its coordination protocols with TLA+ models, written in PlusCal and
explored exhaustively, within stated bounds, by the TLC model checker in CI. This page is the overview: why bide does it,
what each model guarantees and which code it covers, every bug the models caught before release,
what runs on a pull request and what runs nightly, how the models and the Go code stay in step,
and what the models do not cover.

The detailed reference for every model (its rules, model-to-code map, properties, configurations
and bounds) is [spec/tla/README.md](../spec/tla/README.md). The plan behind the work, with its
milestones, is the design note [formal models of the coordination protocols](design/formal-models.md).

At a glance:

- **9 models** (1, 1b, 2, 7, 8, 9, 10, 11 and 12), each checked on every pull request that changes it,
  and all of them in the merge queue and on main. **Models** is a required check.
- **207 configurations** in the merge queue (82 that must pass, each also run for vacuity, and 125
  regression, finding and limit configurations that must fail with their named property), and
  **75 larger ones nightly**.
- **29 bugs caught before release** in bide's own design or code (F1 to F5, P1, P2, T1 to T6, a
  rollback that never ended, L1 to L7, a spend-accounting bug model 8 confirmed, D1 to D3, and
  S1 to S4). Each fixed one is kept as a regression configuration (S1, S2 and S4 since #137);
  L2 to L7 stay open until P14 implements their fix, D1 to D3 until their fixes land in the code,
  and S3 until P14 implements its adopted rule.

## What TLA+ and model checking are

**TLA+** is a language for describing a system as its states and the steps that change them.
**PlusCal** is a code-like form of TLA+ that reads like an algorithm, with labels marking the steps;
bide writes its models in PlusCal, and the tools translate them to TLA+. **TLC** is the model
checker: it starts from the initial states and explores every state reachable within the bounds a
configuration sets (how many drivers, crashes, ambiguous replies, attempts).

A test runs the schedules someone wrote or a randomizer happened to reach. TLC tries every
interleaving of the modelled steps and every placement of every fault, up to the bound, and checks
the stated properties in each state it reaches.

The properties come in two kinds. An **invariant** must hold in every reachable state: "a side
effect never fires twice". A **liveness** property says something good eventually happens: "a dead
holder's run is eventually picked up". The bounds are small, so a result says nothing beyond them.
Small bounds still find real bugs because most protocol bugs show up in small cases: two drivers,
one crash, two or three ambiguous replies (the small-scope hypothesis). Every bug on this page was
found at such bounds. A **vacuity check** guards against a model that passes because nothing
happens in it: each passing configuration is run again with the invariant "the effect never
fires", which TLC must report violated.

When a property fails, TLC prints a **counterexample**: the step-by-step trace of states that
leads to the violation, each state one atomic step such as a store write, a crash or a
cancellation. In bide's models these are typically 10 to 30 states long.

A small excerpt from the claims model (`spec/tla/claims/Claims.tla`): the step a claim takes when
its marker write returned an error, and the at-most-once invariant.

```tla
ClaimNS:
  \* The marker may have committed: record that this claim never called the effect.
  Reply(reply);
  WriteNS(CallOf[self], g, cid[self], reply);
  if reply # "ok" then
    Remember(g, cid[self]);
  end if;
  outcome := "error"; goto Finish;

AtMostOnce == \A c \in Calls : fired[c] <= 1
```

`Reply(reply)` lets TLC pick any store reply (success, an error that did not commit, an error that
did, while the configuration's error budget lasts), the claim writes its not-started record and remembers its claim id if that write fails too,
and `AtMostOnce` says that no call's effect has fired more than once, in any state TLC reaches.

To learn more, see Leslie Lamport's [TLA+ home page](https://lamport.azurewebsites.net/tla/tla.html).

## Why bide model-checks

bide's core promise is that a side effect fires at most once, and that what the journal says about
it is true, across process crashes, overlapping drivers, cancellations and a store that can report
an error for a write it actually committed (an ambiguous write). That promise rests on small
protocols: an exclusive attempt claim, a not-started record bound to the claim, numbered
re-attempts, an in-process memory of claims whose write was ambiguous, a resume gate, halt
resolution, leases and recovery.

bide already tests this layer hard: deterministic crash sweeps, a reference model with randomized
schedules, exhaustive fault-schedule explorations of the claim protocol, a multi-process harness
against real Postgres, and mutation checks (see [How bide is verified](testing/verification.md)).
Those tests check the implementation on the schedules they reach. The bugs that matter here need
a specific combination of faults and interleavings, which is where tests run out
([plan, section 2](design/formal-models.md#2-why-models-given-the-existing-tests)):

- **Fault placement is combinatorial.** F1 below needs three ambiguous store replies and a
  cancellation, in a specific order, in one process. F3 needs three error replies and no crash.
- **Interleaving is combinatorial.** T6 needs a drive cancelled while a leaked call is still inside
  a tool, a re-drive that answers from a cache, and a later saga step that fails.
- **Reply ambiguity is invisible to the code.** A write that errors may have committed. The code
  cannot branch on which happened, so a test must inject both outcomes at every write. A model
  gives every write both outcomes by construction.
- **Some bugs need no fault at all.** L2 below is a plain race between `Cancel` and a drive.

TLC explores every interleaving of a model's rules and every placement of its faults within the
configuration's bounds, and checks each property in every reachable state. Two results show what
that adds:

- **The claims model** (model 1) found F1 to F4 in the claim protocol of P6a (#92) after that pull
  request had been through three adversarial review rounds and its exhaustive fault-schedule
  harness (`agent/claim_explore_test.go`, `agent/claim_explore_conc_test.go`) was passing. Three of
  the four were double fires. All four were fixed in #92 before it merged.
- **The tool-call model** (model 9) found T1 to T6 in redesign P12 (#117) after five review
  rounds. Each got a failing Go test, and all were fixed in #117 before it merged.

## The models

PR and nightly counts are configurations. On a pull request, each model runs its `ci` configurations
(each also run a second time for vacuity: it must show the effect can fire at all) and its
regression, finding and limit configurations, each of which must fail with exactly its named
property. Nightly runs larger bounds.

| Model | What it guarantees | Key properties | Code it covers | PR | Nightly | Status |
|---|---|---|---|---|---|---|
| [1: claims and attempts](../spec/tla/README.md#model-1-claims-and-attempts) | A side effect or a `Step` fires at most once across crashes, ambiguous writes, cancellations and drivers in one or several processes; a won claim never halts; halt resolution never overrides a live driver; a call that provably never started does not halt for ever. | `AtMostOnce`, `AtMostOncePerIntent`, `NotStartedExclusive`, `NoLiveOverride`, `WinnerNeverHalts`, `ResultStable`, `Progress` | `agent`: `journal.go`, `attempt.go`, `step.go`, `loop.go` (resume gate, tool claim), `halt.go`, `keys.go`, `saga.go` | 36 | 23 | Checked; F1 to F4 fixed in #92 |
| [1b: the approval gate](../spec/tla/README.md#model-1b-the-approval-gate) | An effect under an approval gate fires only with a recorded sufficient approval; a recorded denial is final; a passing tally rests on enough valid approvals by distinct people, signed over this exact call; the gate never waits for approvals already in. | `NoUnapprovedFire`, `DenialFinal`, `TallySound`, `DenialSound`, `NoStuckPause` | `agent`: `approval.go`, `toolexec.go` (`quorumTally`), the pre-pass in `loop.go` | 17 | 1 | Checked; F5 fixed in #109 |
| [2: the bide protocol's claim rules](../spec/tla/README.md#model-2-the-bide-protocols-claim-rules) | A remote tool call fires at most once under lost, late and duplicated deliveries, engine and worker crashes; at most one worker runs under a marker; `DELIVERY_EXHAUSTED` is recorded only when no effect ran. | `AtMostOnce`, `BeginExclusive`, `BeginIdempotent`, `NoRunAfterAbandon`, `ExhaustedTruthful`, `DownstreamOnce` | None yet: a design model of [the bide protocol](design/protocol.md), which is not implemented | 16 | 8 | Checked; P1 and P2 fixed in the design (#95) |
| [7: flow semantics](../spec/tla/README.md#model-7-flow-semantics) | A lowered plan flow runs each node that is not retry-safe at most once per loop iteration, and the journal always holds a path the flow declares, resolutions included. | `AtMostOncePerIteration`, `NestedOncePerIteration`, `Conform`, `ResultsTyped`, `Completes` | `plan`: `flow.go`, `resolve.go`; `agent/journalhook.go`, `agent/keys.go` | 8 | 5 | Checked; no new finding |
| [8: spend accounting](../spec/tla/README.md#model-8-spend-accounting) | The journal holds every billed model request exactly once, across hedged and retried requests, failed calls, ambiguous writes, crashes and two drivers; `Result.Spend` never exceeds it. | `NoDoubleCount`, `SpendExact`, `ResultSpend` | `agent`: `modelcall.go`, `generate.go`, `loop.go` (Load, `settle`) | 6 | 2 | Checked; confirmed the #104 shared-key bug on the old rule |
| [9: the tool-call state machine](../spec/tla/README.md#model-9-the-tool-call-state-machine) | Under any tool middleware, retries, leaked `next` calls and sibling calls, a side effect fires at most once, the journal's record of a call is true, a saga's rollback accounts for every effect left in place, and the rollback ends. | `NoDoubleFire`, `TruthfulRecord`, `NoLostSibling`, `SagaAccounted`, `NeverBegunProgress`, `RollbackEnds` | `agent`: `toolexec.go`, `loop.go`, `saga.go`, `tool_middleware.go`; `internal/toolhook` | 26 | 13 | Checked; T1 to T6 fixed in #117 |
| [10: the run lifecycle and recovery](../spec/tla/README.md#model-10-the-run-lifecycle-and-recovery) | Recovery never resumes a finished run; one live lease holder per epoch; each end marker is written once; nothing fires after a run completed or aborted, or under a claim won after it was cancelled; `Status` and every writer report the first end marker; per-run options and the tool filter survive recovery; a cancelled saga is rolled back; a dead holder's run is taken over within a bounded time of its lease lapsing. | `NoResumeOfFinished`, `OneDriverPerEpoch`, `NoDoubleCompletion`, `FinishedFinal`, `CancelFinal`, `VerdictAgreement`, `StatusTruthful`, `FilterHonoured`, `RunOptionsDurable`, `CancelRollsBack`, `BoundedPickup`, `PickedUp` | `agent`: `recovery.go`, `lease.go`, `loop.go`, `saga.go`, `halt.go`, `journal.go`; the stores' `Leaser` and `Lister` | 41 | 9 | Checked, and gates P14 (#129); L1 fixed in #126; L2 to L7 adopted, open until P14 |
| [11: delegation, sub-run authority and saga trees](../spec/tla/README.md#model-11-delegation-sub-run-authority-and-saga-trees) | A child never acts with authority its delegation did not grant, nor reaches a tool after its grant expired; a saga's rollback compensates or lists every write anywhere in the tree, under the authority each sub-run journaled; a halt or lost outcome anywhere in the tree stops later saga steps; a sub-run runs only under its own call's ID while the call is open; a storage error or a wrong-authority resume is never recorded as a delegation failure, and a run it stopped continues once the right grant is bound. | `AuthorityNarrows`, `RollbackSound`, `RollbackUnderGrant`, `HaltPropagates`, `NoForgedSubRun`, `NoFalseFailure`, `UnrecordedContinues`, `RollbackEnds` | `audit`: `delegate.go`; `agent`: `subagent.go`, `saga.go`, `runctx.go`, `loop.go`; `internal/toolhook` | 33 | 8 | Checked; D1 to D3 open until their fixes land |
| [12: sessions](../spec/tla/README.md#model-12-sessions) | Each turn of a session is recorded once and answers its own message; a `SendOnce` key is one turn, whose work runs in one run; the history is append-only and every reader sees one order; a resumed turn sees the transcript it started from; no run is shared between sessions or with a root run; a session whose driver died resumes when its message is sent again. | `TurnOnce`, `KeyOnce`, `NoCrossTalk`, `NoLostTurn`, `SeedFaithful`, `AppendOnly`, `BudgetHeld`, `NoFalseRefusal`, `Answered`, `TurnsSettle` | `agent`: `session.go`, `keys.go` (session run IDs) | 21 | 5 | Checked; S1, S2 and S4 fixed ([#137](https://github.com/bide-ai/bide/pull/137)); S3 open until P14 |

Model numbers are those of `spec/tla/README.md`. The plan's deferred models 3 (leases) and 5 (saga
rollback) are partly covered by models 10, 9 and 11; its model 4 (the store contract) is not built.

## Bugs the models caught

Every row was caught before it reached a release. Each fixed one is now a regression
configuration: the old rule, restored behind a `Bug` flag, that must keep failing with its
property. L2 and L3 are regressions of the rules D1 replaced, and L4 to L7 finding configurations; all six fail until P14 implements the adopted rules. Trace lengths and
details are in the linked README sections.

| ID | Model | What the model found | Severity | Fixed in |
|---|---|---|---|---|
| F1 | 1 | `pendingClaims` held one claim id per marker key, so a second failed claim of the key replaced the first, and a live marker whose effect never ran was never voided. Needs three error replies and a cancellation. | Stuck run: a never-started effect halts for ever, with no crash | [#92](https://github.com/bide-ai/bide/pull/92) (found in [#100](https://github.com/bide-ai/bide/pull/100)) |
| F2 | 1 | A `WithMinHaltAge` resolution did not cover a remembered claim: the claim's process could void the live attempt, claim the next one and call the effect while the resolution recorded "not charged". | Double fire | [#92](https://github.com/bide-ai/bide/pull/92): the resolver claims the next attempt first (found in [#100](https://github.com/bide-ai/bide/pull/100); the rule was on main from #90) |
| F3 | 1 | F2's first fix recorded the resolution's own attempt as not started after a verdict write that errored but may have committed. | Double fire | [#92](https://github.com/bide-ai/bide/pull/92) (found in [#106](https://github.com/bide-ai/bide/pull/106)) |
| F4 | 1 | On the lease path, a plain `Run` revived a leased driver's remembered claim and fired while the resolution recorded its verdict. | Double fire | [#92](https://github.com/bide-ai/bide/pull/92): claim the next attempt on the lease path too (found in [#106](https://github.com/bide-ai/bide/pull/106)) |
| F5 | 1b | Two approvers whose verifiers resolve to the same key were two seats for one person, so one key holder could meet an m-of-n quorum alone. | Approval bypass | [#109](https://github.com/bide-ai/bide/pull/109) (found in [#108](https://github.com/bide-ai/bide/pull/108)) |
| P1 | 2 | `BeginTask` checked the dispatch table before reading the stored begin record, so a worker whose begin landed and whose answer was lost was refused on retry and never ran. | Stuck call: halted `worker_lost` for an effect that never started | [#95](https://github.com/bide-ai/bide/pull/95) design text, 10.4 step 0 (found in [#112](https://github.com/bide-ai/bide/pull/112)) |
| P2 | 2 | `DELIVERY_EXHAUSTED` was recorded for a retry-safe call whose deliveries were only lost, though a lost delivery may have run its handler; the next call ran under a new once-key scope. | Double effect downstream | [#95](https://github.com/bide-ai/bide/pull/95) design text, 10.9 and 6.3: only `read_only` tools record it, an idempotent call halts (found in [#112](https://github.com/bide-ai/bide/pull/112)) |
| T1 | 9 | A known failure was recorded for a side effect whose tool began and did not itself fail (a middleware turned success into an error, a later invocation's refusal, or `next` still running). | Double fire; in a saga, an effect left in place and listed nowhere | [#117](https://github.com/bide-ai/bide/pull/117) (found by model 9, [#123](https://github.com/bide-ai/bide/pull/123)) |
| T2 | 9 | "Already ran" was recorded for a tool that never began, when the saga's accepted-arguments write failed. | False record | [#117](https://github.com/bide-ai/bide/pull/117) |
| T3 | 9 | An earlier attempt of a retry-safe saga step that changes state fired, was cut off with nothing recorded, and its re-run failed: the rollback skipped it. | Effect left in place after rollback, listed nowhere | [#117](https://github.com/bide-ai/bide/pull/117) |
| T4 | 9 | A success was recorded (and, in a saga, compensated) while a `next` left running was still in the tool. | Effect lands after its compensation | [#117](https://github.com/bide-ai/bide/pull/117) |
| T5 | 9 | A retry-safe tool began after its chain returned and its compensation was recorded. | Effect lands after its compensation | [#117](https://github.com/bide-ai/bide/pull/117) |
| T6 | 9 | A re-drive's cache answer was recorded while an earlier drive's invocation was still in the tool. | Effect lands after its compensation | [#117](https://github.com/bide-ai/bide/pull/117) |
| (unnamed) | 9 | The rollback's re-run stopped on any error, so a result check that rejects every success left the rollback with no end. | Stuck rollback | [#117](https://github.com/bide-ai/bide/pull/117) |
| L1 | 10 | `RecoverLoop` visited every unfinished run in id order, halted ones included, so a dead holder's run waited behind the halted runs listed before it (10 ticks with two halted runs and 15 with three, against 3 or 4 after the fix). | Slow recovery | [#126](https://github.com/bide-ai/bide/pull/126): a second loop drives only runs whose lease lapsed (found in [#124](https://github.com/bide-ai/bide/pull/124)) |
| L2 | 10 | `Cancel`'s turn-boundary check races the claim: a drive past its check claims and fires after `run:cancelled` landed, with no fault. | Fire after cancel | Rule adopted into P14's design (D1 in [api-v1](design/api-v1.md)) by [#124](https://github.com/bide-ai/bide/pull/124); open until P14 implements it |
| L3 | 10 | `Cancel` and completion race on two keys: `Cancel` can report a run cancelled that completed, or the run report an answer after `Cancel` landed first. | Wrong verdict reported | As L2: the first end marker in journal order is the verdict; open until P14 |
| L4 | 10 | `Cancel` on a saga wrote `run:cancelled`, an end marker that recovery excludes, so a saga cancelled when no drive would reach a check (or completed after the marker) was never rolled back. | Effects never compensated | Rule adopted into P14's design by [#129](https://github.com/bide-ai/bide/pull/129): a rollback request `run:cancel-requested`, then the rollback and `run:cancelled`; open until P14 |
| L5 | 10 | The per-run tool filter was journaled but not said to apply at dispatch; a turn naming a filtered-out tool (unoffered, or replayed) would fire, with no fault. | Filtered tool fires | Rule adopted by [#129](https://github.com/bide-ai/bide/pull/129): the filter is enforced at dispatch; open until P14 |
| L6 | 10 | `Status` by one `Get` per end marker can read `run:complete` before the run completes and `run:cancelled` after `Cancel` lands, and report a completed run cancelled. | Wrong status reported | Rule adopted by [#129](https://github.com/bide-ai/bide/pull/129): one `Load`, or the `Get`s again once a marker is found; open until P14 |
| L7 | 10 | Recovery that remembers a not-started run's skip, not its report, never recovers the run once it starts and its holder dies. | Run never recovered | Rule adopted by [#129](https://github.com/bide-ai/bide/pull/129): `run:start` read again every pass; open until P14 |
| D1 | 11 | `BindRollback` checked each journaled grant against the one grant bound now, so a saga that delegated under two bound grants (the refusal for an expired grant asks for a live one) could not verify both, and its rollback never ended. | Stuck rollback | Open: the caller binds every grant the saga delegated under, and each child grant is checked against its own parent (found in [#130](https://github.com/bide-ai/bide/pull/130)) |
| D2 | 11 | `BindRollback` dropped the delegated mark, so the rollback's re-run of a retry-safe write in a delegated sub-run passed `CallGuard` and called the tool after the grant expired. | Act past grant expiry | Open: keep the mark, and list a refused re-run as an unknown outcome (found in [#130](https://github.com/bide-ai/bide/pull/130)) |
| D3 | 11 | In a plain sub-run of a saga, a failed sub-agent call is an error result, and the rollback skipped error results before recursing, so the sub-agent's writes were neither compensated nor listed. | Write left in place, listed nowhere | Open: recurse into a sub-agent call whatever its result (found in [#130](https://github.com/bide-ai/bide/pull/130)) |
| S1 | 12 | A handle whose `Send` started a turn and then failed or paused keeps it open in its own view; once another worker finished that turn, the handle refused every other message with `ErrConfig` ("a turn for x is still open") and never read the journal again. | Session refuses every new message on that handle | [#137](https://github.com/bide-ai/bide/pull/137): reload before refusing |
| S2 | 12 | Two callers on one handle sending one message both drive its run; the second's append started past the slot the first had recorded and its reload had loaded, so the run's turn was recorded twice (and a `SendOnce` key had two records). | Duplicate turn in the history | [#137](https://github.com/bide-ai/bide/pull/137): `appendTurn` returns at once for a run among the loaded turns |
| S3 | 12 | Against P14's design: `Cancel` of an open Send turn's run leaves the turn open for ever, so every other message on the session is refused. | Session blocked for ever | Rule adopted into P14's contract (rule 16 in [api-v1](design/api-v1.md)); open until P14 implements it |
| S4 | 12 | A turn's run is driven with no lease, so two workers given one message both drive it, each counting only the spend it has seen, and the turn spends up to its budget once per worker. | Budget overspent | [#137](https://github.com/bide-ai/bide/pull/137): each turn's run is driven under its lease; a worker without it gets `ErrTurnContended` |
| Shared late key | 8 | Late spend was keyed by a sequence number each driver counted itself, so two drivers wrote one `@spend-late` key and the second's spend was lost. Found as a suspicion in the #104 re-review; model 8 confirmed it on the old rule. | Lost spend | [#104](https://github.com/bide-ai/bide/pull/104): a fresh id per spend record (confirmed in [#111](https://github.com/bide-ai/bide/pull/111)) |

Models 7 and 8 found no new bug in the rules they check. Their regressions encode bugs earlier
reviews found (#103's F3, F4 and F7 for flows; #104's F3, F4 and the lost-turn case for spend), so
those rules cannot come back. The other regression configurations of models 1, 1b, 9 and 10 do the
same for bugs found by review and testing before the models existed, back to #31 and #58.

## What runs where

| Where | What | Time |
|---|---|---|
| Every pull request, the merge queue and main (**Models**, required) | The checker self-test, the PlusCal translation check, and every `ci`, `regress`, `finding` and `limit` configuration (207), each passing one also run for vacuity, four at a time; on a pull request, of the models it changes | About 6 minutes for every model on the CI runner (job timeout 30 minutes) |
| Nightly and on demand (**Models (nightly)**) | The 75 `nightly` configurations: more faults, more drivers, liveness at two error replies, weak A3 (late commits) | About 1 hour 50 minutes on the CI runner (1 hour 40 minutes measured before this split, plus about 7 minutes moved from pull requests, and model 10's four P14 configurations, about 6 minutes on the development machine; job timeout 4 hours) |
| Nightly and on demand (**Explore (full bound)**) | The Go fault-schedule explorations of the claim protocol and of flow lowering at their full bound (`BIDE_EXPLORE=1`); every pull request runs them at a smaller bound under `-race` in the Test job | About 15 to 22 minutes |
| Every pull request (**Lint**, required) | `modelsync` and `TestProtocolVocabulary` (next section) | Part of Lint |

On a pull request, the Models steps check only the models whose directory under `spec/tla/` it
changes (every model when it changes `check.sh`, `tools.lock` or the workflow); otherwise the job
reports success without re-checking. TLC reads nothing outside the model's directory, and a change
to the Go code a model describes must change the model or carry a `Protocol-Impact` override (the
Lint job), so this skips no check whose result could differ. The merge queue and pushes to main
always run every model. Workflows: `.github/workflows/models.yml` and
`.github/workflows/explore.yml`.

**What a counterexample looks like.** When a property fails, TLC prints the violated invariant (or
temporal property) and the shortest sequence of states that reaches it: each state is one atomic
step, such as one store round trip, a crash or a cancellation, with every variable's value.
`spec/tla/check.sh` compares the result with the configuration's declared expectation and prints a
summary line per configuration; `TLC_KEEP_OUTPUT=<dir>` keeps the full trace. Traces are short:
F1's is 37 states, F2's 20 to 26, T6's 55, L2's 10. A counterexample in the current rules is a
bug. It gets a deterministic Go test that fails before the fix, then the fix, then a regression
configuration that keeps the old rule failing.

## How the models and the code stay in step

A model checks the design, not the code. These mechanisms keep the two from drifting apart today
(milestone M4, [#125](https://github.com/bide-ai/bide/pull/125)):

- **Region markers.** Each Go region a model's map names is wrapped in comments that name the
  model and the model actions it implements, for example
  `// protocol:claims begin Claim ClaimRetry ClaimInsert ClaimNS` ... `// protocol:claims end`.
  Models 1, 1b, 7, 8, 9, 10, 11 and 12 are marked; model 2 has no Go code yet.
- **The path rule.** `go run ./internal/tools/modelsync`, in the required Lint job, fails a pull
  request that touches a marked region of a model without changing anything under
  `spec/tla/<model>/`.
- **The Protocol-Impact override.** A change that leaves the modelled behavior as it is (a rename,
  a comment, an error message) says so in the description or a commit message:
  `Protocol-Impact: none (<reason>)`, or `Protocol-Impact: <model>[,<model>] none (<reason>)`. The
  reason is required, and every override used is shown to the reviewer as a warning annotation.
- **Rename checks.** modelsync also fails when a marker, the model-to-code map in
  `spec/tla/README.md` and the spec disagree on an action's name, so a rename on any side fails.
- **`TestProtocolVocabulary`** (package `agent`, run in Lint) checks that the claim code's key
  constructors and record kinds match the record kinds `Claims.tla` declares, in both directions,
  and that each constructor's keys parse back to their kind.
- **Bug-flag regressions.** Every historical bug a model can state is a `Bug` value and a
  configuration in `regress/` that must keep failing with its property (68 today). One that starts
  passing means the model lost the behavior or a property weakened.
- **The CONTRIBUTING rule.** A change to a modelled rule changes the model in the same pull
  request, and a counterexample in the current rules is reproduced as a deterministic Go test
  before it is fixed ([CONTRIBUTING, Formal models](../CONTRIBUTING.md#formal-models)). The pull
  request template asks the reviewer to confirm the model-to-code map still holds.

What the markers and the map do not yet check is meaning: that each Go function does what its model
step says is checked by review. The planned work closes that gap.

### Planned

The next models, in order (also on the [roadmap](ROADMAP.md#formal-models-of-the-coordination-protocols)):

1. **Delegation and sub-run authority, including saga trees** (next up): grants, recorded
   authority, rollback binding, halts propagating from sub-runs, and links to programmatic
   sub-runs. Most of P12's and P13's late bugs were in this area, and model 9 treats a delegation
   as a black box.
2. **Sessions:** multiple turns, resumes and shared history, built when P14 or later work touches
   sessions.
3. **M3 trace validation:** the Go test suites emit protocol events under a build tag, and TLC
   checks that every emitted trace is a behavior of the model, so the models cannot drift from real
   Go runs ([plan, section 6.1](design/formal-models.md#61-trace-validation)).

Also planned in the [milestones](design/formal-models.md#8-milestones): the rest of M2, a helper
(`agent/internal/interleave`) that turns a TLC counterexample into a deterministic Go test
skeleton; and M5, merging and validating the traces of the multi-process HA harness nightly.

## What the models do not cover

- **Only the bounds checked.** TLC checks every behavior within each configuration's bounds and
  nothing beyond them. Typical bounds: two drivers (three in one nightly configuration), one crash,
  one cancellation, up to two error replies on pull requests and four nightly; model 9, one turn
  of one or two calls with at most two invocations of a call at once; model 10, one or two
  recovery workers, up to three runs; model 1b, three approvers and up to three decisions. A bug
  that needs more is outside the check.
- **The design, not the code**, until trace validation lands. The map from model steps to Go
  functions is reviewed by hand; modelsync checks only names and that marked code changes with
  its model.
- **Postgres and SQLite store internals.** The models assume the store contract (first writer
  wins per key, prefix-closed reads, ambiguous errors). Whether a store meets it is checked by the
  `storetest` conformance suite and the multi-process harness against real Postgres
  (`store/postgres`, the required Integration check), not by a model. The store contract and
  journal header model (plan model 4) is not built.
- **Sessions** (concurrent sends, turn ordering, resumes): planned.
- **Delegation and sub-run authority**: sub-agent rollback recursion, grants and halts from
  sub-runs. Model 9 treats a delegation as a black box. This is the next model.
- **Multi-process traces** (M5) and trace validation of any kind (M3).
- **Wall-clock time** outside model 10: `WithMinHaltAge` is encoded as the assumption it rests on.
- **Per-model gaps** listed in the README: the bide protocol's call deadline as a clock, push
  delivery and approvals over the protocol (model 2); a flow input's canonical comparison, the run
  ID check, and joins (model 7); `OnAnswer`, `Cost` and the token budget's stop (model 8); pauses
  inside a tool and an unregistered tool in a rollback (model 9); a recovery pass's concurrency
  above 1, sub-runs and sessions (model 10); and the whole-tree token budget bound.

For the full list per model, see
[What the bounds do not cover](../spec/tla/README.md#what-the-bounds-do-not-cover) and each
model's section.

## Further reading

- [spec/tla/README.md](../spec/tla/README.md): running the models
  ([Running it](../spec/tla/README.md#running-it)), CI ([In CI](../spec/tla/README.md#in-ci)),
  [keeping the code and the models in step](../spec/tla/README.md#keeping-the-code-and-the-models-in-step),
  and one section per model.
- [Formal models of the coordination protocols](design/formal-models.md): the plan, the reasoning
  behind each model, and the milestones M0 to M5.
- [How bide is verified](testing/verification.md): the tests, sweeps and harnesses the models sit
  beside.
