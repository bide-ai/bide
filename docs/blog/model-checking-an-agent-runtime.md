---
title: "Model-checking an agent runtime with TLA+"
description: "How bide uses TLA+ and TLC to check that a side effect fires at most once, what the models caught, and what they cannot see."
date: 2026-10-13
author: Dayna Blackwell
draft: true
---

<!--
DRAFT. Not linked from the blog index (the index entry is commented out there) and not published.
Every number below has its source in a comment next to it. Counts marked "re-run" were
reproduced on 2026-10-13 with spec/tla/check.sh (TLC 1.7.4, OpenJDK 21, Apple Silicon).
-->

# Model-checking an agent runtime with TLA+

*Dayna Blackwell, 2026-10-13 (draft)*

A payment agent calls `charge_card`. The request leaves the process. The process dies before the
response comes back. On restart, did the card get charged?

Nobody knows. The provider may have charged it. It may have dropped the request. The journal
holds a marker that says "attempt started" and no result. Retry and you may charge twice. Skip and
you may never charge. [bide](https://github.com/bide-ai/bide) halts the run with `OutcomeUnknown`
and waits for a person or a reconciler to say what happened.

That rule is simple. Making it hold is not. This post is about how we check it with TLA+, what
that found, and where the check stops.

## Why a test is not enough

The rule sits on a handful of small protocols: an exclusive claim on each attempt, a record that an
attempt never started, numbered re-attempts, an in-process memory of claims whose write was
ambiguous, and a gate that decides whether a resumed run may proceed. Each is easy to state.
Together they interleave with crashes, cancellations, two processes driving one run, and a store
that can return an error for a write it committed.

That last fault is the nasty one. In Go, `err != nil` from a database write does not tell you the
row is absent.

```text
driver                       store
  | insert marker (id 1)       |
  |--------------------------->| committed
  |<------ error (lost) -------|
  | insert not-started(1)      |
  |--------------------------->|
  |<------ error again --------|
  | marker is live, the effect never ran, nobody knows it
```

A test injects the faults someone thought of. The bugs that matter need a particular order of three
or four of them. bide has crash sweeps, a randomized reference model and a multi-process Postgres
harness. It still wanted something that tries every order.

## What TLC does

[TLA+](https://lamport.azurewebsites.net/tla/tla.html) describes a system as states and the steps
that change them. bide writes its models in PlusCal, which reads like an algorithm, and the tools
translate it to TLA+. TLC, the model checker, starts from the initial state and visits every
state reachable within the bounds a configuration sets: how many drivers, how many crashes, how
many ambiguous replies. In each state it checks the properties.

Every store write in the models has three outcomes, by construction: it succeeded, it failed and
did not commit, or it failed and did commit. The Go code cannot branch on which one happened. The
model branches on all of them at every write.

## Model 1: the claim protocol

The claims model (`spec/tla/claims/Claims.tla`) covers one run, one or two calls, and drivers in
one process or in several. Its core property is one line:

```tla
AtMostOnce == \A c \in Calls : fired[c] <= 1
```

`fired[c]` counts how many times the effect of call `c` ran. In no reachable state may it exceed one.

The step that makes this interesting is a claim whose marker write returned an error. The marker
may have committed, so the claim must record that its effect never ran, and if that write fails
too, remember the claim id in process memory so a later claim can retry:

```tla
ClaimNS:
  \* The marker may have committed: record that this claim never called the effect.
  Reply(reply);
  WriteNS(CallOf[self], g, cid[self], reply);
  if reply # "ok" then
    Remember(g, cid[self]);
  end if;
  outcome := "error"; goto Finish;
```

`Reply(reply)` lets TLC pick any of the three outcomes. A second property says a call that
provably never started does not halt forever:

```tla
Excused(c) == fired[c] > 0 \/ \E x \in Gens : Live(c, x) /\ marker[c][x] \in lost
Progress   == \A c \in Calls : <>[](~Issued(c) \/ Recorded(c) \/ Excused(c))
```

Safety alone is cheap: a runtime that halts on everything never fires twice. `Progress` rules that
out. Every passing configuration also runs a second time with the invariant "the effect never
fires", which TLC must report violated. If it is not, the model passes because nothing happens in it.

## Model 1b: the approval gate

Some tools wait for k of n people to approve. The approvals model, an extension of model 1,
checks that an effect under a gate fires only with enough valid approvals, signed over this exact
call, by distinct people:

```tla
TallySound ==
  \A c \in Calls : (tally[c].rec /\ tally[c].passed)
                     => Cardinality(tally[c].signers) >= Policies[tally[c].pol].need
```

`signers` is a ghost variable: the set of people holding the keys that signed the seated approvals.
The property counts people, not seats. That is where the model found F5: two approvers whose
verifiers resolved to one key were two seats for one person, so one key holder could meet an
m-of-n quorum alone. The model fails `TallySound` in 9 states on
the old rule (`regress/ap-shared-key`). The fix made the gate refuse a policy whose approvers share
a key.

## Model 10: the run lifecycle

The lifecycle model covers end markers, leases and the recovery loop that finds runs whose holder
died. It has a timed property:

```tla
BoundedPickup == \A r \in Runs : age[r] <= Bound
```

A run whose lease holder died must be taken over within `Bound` ticks of the lease lapsing. In
v0.9.0, `RecoverLoop` visited every unfinished run in id order, halted runs included. The model
showed the cost: with zero, one, two and three halted runs listed before it, a dead holder's run
waited 4, 5, 10 and 15 ticks. A second loop that drives only lapsed leases brought that to 3, 3, 4
and 4. This one shipped in a release and was fixed in v0.10.0.

## A bug the models caught

Here is the one we like best, because the code passed review and tests when the model found it.

Operators can resolve a halted call. When no lease says whether a driver is alive, the operator
passes `WithMinHaltAge`: a halt older than that age is assumed to have no live driver. The model
states that as an assumption, in `RCheck`: a drive holding the live claim is over by then. A
comment beside it says what the assumption leaves out: "A remembered claim (pendingClaims) is not
a drive: its process may retry its not-started record at any later time."

That comment is the bug. Consider this schedule, found by TLC in the configuration
`regress/minage-revoid`:

1. Driver d1 claims attempt 0. The marker write errors but committed.
2. d1's not-started write errors too. Its process remembers the claim id. The marker is live and
   the effect never ran.
3. The operator sees an old halt and resolves it: the verdict is "not charged".
4. d1's process, on its next drive, retries the remembered record. That voids attempt 0. It claims
   attempt 1, wins, and calls the effect, under a verdict that says it was not charged.

If the caller then reads "not charged" and asks again, the effect fires twice. Two error replies. No
crash. The counterexample is 22 states and TLC finds it after exploring 9,299 distinct states
(re-run, under a second).

```text
\* Finding F2 (fixed in #92): WithMinHaltAge without the resolver's claim of the next attempt
\* GROUP: regress
\* EXPECT: invariant NoLiveOverride
```

The fix the model checked: before it writes, the resolver claims the attempt after the live one.
A driver that later voids the live attempt then loses that claim and reads the verdict. If a driver
already holds it, the resolution refuses. `resolve-minage-claim` passes with 531,951 distinct states
(re-run). The old rule stays in the repo as a configuration (`ResolveClaim = FALSE`), and CI requires
that it keeps failing with `NoLiveOverride`.

F2 was in the halt resolution of #90 on main, found by the claims model in #100, fixed in the
design of #92 before it merged, and caught before any release.

The same model found three more in the claim protocol (F1, F3, F4), all in a pull request that had
been through three adversarial review rounds and an exhaustive fault-schedule harness. Three of the
four were double fires. In all, the models have found 28 bugs in bide's own design or code.

## How the models stay tied to the code

A model checks a design. The Go code can drift from it. Three mechanisms slow that down.

**Region markers.** Each Go region that implements a model step is wrapped in comments:

<!-- docsnip: skip excerpt: the signature is abbreviated -->
```go
// protocol:claims begin Claim ClaimRetry ClaimInsert ClaimNS RClaim RRetry RInsert RClaimNS
func (j *Journal) claim(...)
// protocol:claims end
```

The names after `begin` are PlusCal labels. A tool, `modelsync`, runs in the required Lint job. It
fails if a marker names a label the model does not define, or if the model's code map and the spec
disagree, so a rename on either side breaks the build.

**The path rule.** `modelsync` also diffs the pull request against its base. If the diff touches a
marked region and changes nothing under `spec/tla/claims/`, it fails, with a message that tells you
to update the model. For a change that leaves the modelled behavior alone, a rename or an error
message, the pull request says so, with a reason:

```text
Protocol-Impact: none (error message wording only)
```

Every override shows as a warning annotation on the pull request, so a reviewer sees it.

**Regressions.** Every historical bug a model can state is kept as a configuration, usually behind
a `Bug` constant, that must keep failing with its named property. There are 116 of them. One that starts passing means the model
lost the behavior it was written to check.

One more check runs on the vocabulary. `TestProtocolVocabulary` compares the record kinds in
`Claims.tla` with the key constructors in the Go claim code, in both directions.

None of this checks that a Go function does what its model step says. Review does that.

## What it costs

Counts, from the repo as of this draft:

- 10 models. 232 configurations run in the merge queue and on every push to main (96 `ci`, 116
  regression, 1 finding, 19 limit). 93 must pass, each also run for vacuity; 139 must fail with
  their named property. 81 larger configurations run nightly.
- A small safety configuration, `step-same-a1` (two drivers in one process, one error reply, one
  crash, one cancellation), visits 146,864 distinct states in 3 seconds (re-run). Its nightly
  counterpart with two error replies visits 2,282,585 in about 12 seconds. The largest, `deep-faults`,
  visits 31,402,708 in about 2 minutes.
- The longest of six parallel CI shards took 4 minutes 10 seconds on a recent main run; the whole
  workflow took 5 minutes 18 seconds. The scheduled nightly workflow, which includes Apalache,
  took between 2 hours 20 minutes and 3 hours 43 minutes over the last six runs.

TLC is exhaustive within bounds. Apalache is a second checker that sends the model to an SMT
solver. On model 1 it proves an inductive invariant: it holds initially, every step preserves it,
and it implies `AtMostOnce` and `NotStartedExclusive`. That covers any number and mix of faults, at
any depth, for two drivers over two processes. It does not cover more drivers, more attempts than
0..3, or more claim ids than the pool of 8 (6 with halt resolution). Its bounded symbolic checks
reach far less than TLC: three drivers got 4 steps in about 25 minutes, and an effect needs at
least 7 steps to fire. So the nightly job runs no bounded property check with Apalache. It runs one
regression, which must find F2.

## What it does not give you

- **A proof about the code.** The models describe the design. The map from model steps to Go
  functions is checked by names and by review. Trace validation, where Go test runs emit events and
  TLC checks that each trace is a behavior of the model, is planned and not built.
- **Anything beyond the bounds.** Typical bounds are two drivers, one crash, one cancellation and up
  to two error replies on a pull request. A bug that needs more is outside the check. The
  small-scope hypothesis says most bugs show up small. Every bug in this post did. It remains a
  hypothesis.
- **The stores.** The models assume the store contract: first writer wins per key, reads see a
  prefix, writes can return ambiguous errors. Whether SQLite and Postgres meet it is tested by a
  conformance suite and a multi-process harness, not modelled.
- **Wall-clock time**, outside the lifecycle model. `WithMinHaltAge` is encoded as the assumption
  it rests on.
- **Everything the models leave out**: pauses inside a tool, joins in flows, the token budget's
  whole-tree bound, recovery concurrency above one. Each model's README section lists its gaps.
- **Two of the ten models describe designs, not code.** The bide protocol and probes for unknown
  outcomes are not implemented.

## Try it

Java 11 or later and a clone of the repo:

```sh
spec/tla/check.sh run spec/tla/claims/regress/minage-revoid.cfg
```

It downloads the pinned TLC jar, checks its SHA-256, and reports that `NoLiveOverride` is violated.
Add `TLC_KEEP_OUTPUT=/tmp/tlc` to keep the 22-state trace. The proofs page, [Formal verification](../formal-verification.md),
has the bug table and the per-model properties. [The guarantee](../GUARANTEE.md) states what the
runtime promises and under which conditions.

<!-- SOURCES (hidden; keep in sync when the numbers change)
- Three resume cases, OutcomeUnknown: docs/blog/introducing-bide.md "What bide is".
- Rule list (claims, not-started record, numbered re-attempts, pendingClaims, resume gate): docs/formal-verification.md "Why bide model-checks".
- Three store reply outcomes (ok, err_nc, err_c): spec/tla/README.md "Model 1, What is modelled".
- Vacuity run: spec/tla/README.md "Running it"; docs/formal-verification.md "What TLA+ and model checking are".
- AtMostOnce, ClaimNS, Excused, Progress, TallySound, BoundedPickup snippets: spec/tla/claims/Claims.tla lines 443-450, 2028, 2069, 2111, 2086-2088; spec/tla/lifecycle/Lifecycle.tla line 1781.
- F5: spec/tla/README.md "Model 1b, Findings"; regress/ap-shared-key 9 states ("Regressions" paragraph in Model 1b); fixed in #109.
- L1 table 4/5/10/15 vs 3/3/4/4: spec/tla/README.md "Recovery cost: the pickup bound". Shipped in v0.9.0, fixed in v0.10.0 (#126): docs/formal-verification.md "Bugs the models caught". Confirmed PR #126 title "RecoverLoop takes over lapsed leases in a loop of its own (model 10 L1)".
- F2 story and trace: spec/tla/README.md "Model 1, Findings" F2; PR #100 body "Findings"; re-run of claims/regress/minage-revoid.cfg on 2026-10-13: "NoLiveOverride violated, 9299 states, depth 23" and a trace of 22 states (states 1 to 22 in the TLC output), config text from spec/tla/claims/regress/minage-revoid.cfg. The README says "20 to 26" states for F2 across configurations; the #100 body said 20.
- Fixed-rule config: claims/resolve-minage-claim.cfg re-run 2026-10-13: 531,951 distinct states, depth 75 (the README table lists 320,662; the README figure is stale or measured differently).
- "Caught before any release": docs/formal-verification.md says only L1, S1, S2 and S4 shipped.
- "Three of the four were double fires", "three review rounds", "28 bugs": docs/formal-verification.md ("Why bide model-checks", "At a glance").
- modelsync, markers, path rule, Protocol-Impact, TestProtocolVocabulary: docs/formal-verification.md "How the models and the code stay in step"; spec/tla/README.md "Keeping the code and the models in step"; marker text from agent/journal.go line 675; `go run ./internal/tools/modelsync` prints "modelsync: ok" on origin/main 21c5f604.
- 116 regressions: `spec/tla/check.sh list regress | wc -l` = 116.
- 232 = 96 + 116 + 1 + 19 (`check.sh list ci|regress|finding|limit`); 81 nightly (`check.sh list nightly`). "10 models": docs/formal-verification.md "At a glance".
- step-same-a1 146,864 states, 3 s: re-run 2026-10-13 and spec/tla/README.md table. step-same 2,282,585 / 12 s, deep-faults 31,402,708 / 2 min: spec/tla/README.md nightly table (development machine, Apple M1 Pro).
- CI shard times: GitHub Actions run 38443313721 (push to main, 2026-10-13): Models (toolcall-flows) 18:01:53 to 18:06:03 = 4 min 10 s; whole workflow 18:00:53 to 18:06:11 = 5 min 18 s.
- Nightly durations: scheduled Models runs 37765386131, 37919276985, 38043275489, 38130867661, 38240171387, 38385624156 (2026-10-08 to 2026-10-13): 3 h 40, 2 h 54, 2 h 20, 3 h 43, 2 h 49, 2 h 58. The workflow includes the Apalache jobs.
- Apalache scope, bounds (8 and 6 ids, attempts 0..3), 4 steps in 25 minutes, 7 steps to fire, regression finds F2: spec/tla/README.md "Apalache results".
- Limits: docs/formal-verification.md "What the models do not cover"; models 2 and 13 not implemented: the same page, model table.
- The "A remembered claim is not a drive" comment: spec/tla/claims/Claims.tla, RCheck.
-->
