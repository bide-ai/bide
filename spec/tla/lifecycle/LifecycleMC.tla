----------------------------- MODULE LifecycleMC -----------------------------
(* Named worker sets and holder names, which the configurations select with <-. *)
EXTENDS Lifecycle

W1 == {"w1"}
W2 == {"w1", "w2"}
\* Every worker under a name of its own, and the primary under another.
NameOwn == [w \in {"w1", "w2"} |-> w]
\* #58 finding 4: a worker's primary and its recoverer, or a restarted worker and its stalled
\* predecessor, under one WithLeaseHolder name.
NameShared == [w \in {"w1", "w2"} |-> "w1"]
\* Steering constraints for the regression configurations that reproduce a scenario of the
\* multi-process HA harness: the recovery workers take no step until a lease holder has stalled.
WorkersWaitForStall ==
  stalls = 0 => \A w \in Workers : pc[SweepP(w)] = "PList" /\ pc[Slot(w)] = "DIdle"
\* Only w2 (the restarted worker) waits for the stall.
RestartAfterStall == stalls = 0 => pc[SweepP("w2")] = "PList" /\ pc[Slot("w2")] = "DIdle"
W0 == {}

\* P14's per-run options: f, the calls whose tools the filter lets through; l, the turn limit
\* (calls the whole run may make); c, the other journaled settings (prompt, principal, ...).
AllCalls == {1, 2, 3}
OptAll == [f |-> AllCalls, l |-> 3, c |-> "a"]        \* every tool, a limit no run reaches
OptNo2 == [f |-> {1, 3}, l |-> 3, c |-> "a"]          \* call 2's tool filtered out
OptLim1 == [f |-> AllCalls, l |-> 1, c |-> "a"]       \* one turn
OptLim2 == [f |-> AllCalls, l |-> 2, c |-> "a"]
OptOther == [f |-> AllCalls, l |-> 1, c |-> "b"]      \* another prompt or principal
OptNo2Res == [f |-> {1, 3}, l |-> 2, c |-> "a"]

\* P14's rules as proposed: options journaled (B1), the filter enforced at dispatch (D3), Cancel
\* on a saga a rollback request, Status off, a not-started run reported once per process.
ApiP14 == [opt |-> "journal", filter |-> "dispatch", prim |-> OptAll, res |-> {},
           dflt |-> OptAll, init |-> OptAll, sagaCancel |-> "request", status |-> "off",
           notStarted |-> "report"]
\* Status (D8) by one Load, by Gets with the re-read, and by one round of Gets.
ApiStatusLoad == [ApiP14 EXCEPT !.status = "load"]
ApiStatusRegets == [ApiP14 EXCEPT !.status = "regets"]
ApiStatusGets == [ApiP14 EXCEPT !.status = "gets"]
\* The primary filters call 2's tool out; recovery passes nothing (the agent's defaults allow it).
ApiFilter == [ApiP14 EXCEPT !.prim = OptNo2]
ApiFilterRequest == [ApiFilter EXCEPT !.filter = "request"]
ApiFilterCaller == [ApiFilter EXCEPT !.opt = "caller"]
\* The primary's run has one turn; a later Resume raises the limit to 2 (an amendment), or
\* passes another prompt (ErrConfig), or filters a tool out (ErrConfig).
ApiLimits == [ApiP14 EXCEPT !.prim = OptLim1, !.res = {OptLim2, OptOther, OptNo2Res}]
ApiLimitsCaller == [ApiLimits EXCEPT !.opt = "caller"]
\* A later Resume lowers the limit while the first drive runs.
ApiLower == [ApiP14 EXCEPT !.prim = OptLim2, !.res = {OptLim1}]
\* Cancel on a saga: D1 as written (run:cancelled, then the rollback).
ApiSagaMarker == [ApiP14 EXCEPT !.sagaCancel = "marker"]
\* A run with no run:start: reported every pass, or skipped for good once reported.
ApiNSEvery == [ApiP14 EXCEPT !.notStarted = "every"]
ApiNSRemember == [ApiP14 EXCEPT !.notStarted = "remember"]
=============================================================================
