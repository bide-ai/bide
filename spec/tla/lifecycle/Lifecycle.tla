------------------------------ MODULE Lifecycle ------------------------------
(***************************************************************************)
(* Model 10: the run lifecycle and its recovery.                           *)
(*                                                                          *)
(* A run's end markers (run:complete, run:aborted, and run:cancelled, which *)
(* P14's Cancel will write), the drives that reach them (a leased Run, a    *)
(* plain Run, the resume a recovery pass calls), the recovery passes        *)
(* (Recover, RecoverLoop: list, lease, re-check the end markers under the   *)
(* lease, resume, release), halts, pauses, ResolveHalt and Approve, under   *)
(* lease expiry, a holder that stalls past its TTL, ambiguous store writes  *)
(* (A3) and crashes. A discrete clock (Tick) runs the leases and the        *)
(* recovery interval; under Timed, every process takes at most one store    *)
(* round trip per tick and no process lets a tick pass while it can step,   *)
(* so the time a recovery pass takes is visible.                            *)
(*                                                                          *)
(* The claim protocol under the calls is model 1's: here a claim is one     *)
(* marker per call (first writer wins), a lost or errored claim leaves the  *)
(* run halted, and a not-started record frees the call.                     *)
(***************************************************************************)
EXTENDS Integers, Sequences, FiniteSets, TLC

CONSTANTS
  NRuns,        \* runs 1..NRuns, listed in this order (the SQL stores list by id)
  Halted0,      \* runs that start halted: call 1's attempt is live with no result
  Paused0,      \* runs whose first call waits for an approval
  SagaRuns,     \* runs driven by RunSaga: a failing call rolls the run back
  NCalls,       \* side-effect calls per run before run:complete
  Workers,      \* recovery workers (strings), each one process
  Name,         \* Name[w]: worker w's WithLeaseHolder name
  Primary,      \* "none", "leased" (Lease around Run) or "plain" (Run): run NRuns's first drive
  PrimName,     \* the primary's WithLeaseHolder name
  Loop,         \* TRUE: RecoverLoop; FALSE: one Recover pass per worker
  PassRule,     \* "all" (v0.9.0), "lapsedFirst" or "split" (RecoverLoop since #126), see the README
  Resolver,     \* an operator resolving halts (ResolveHalt) and approving (Approve)
  Canceller,    \* P14's Cancel of run NRuns, once
  CancelRule,   \* "none" (no Cancel), "turn" (D1: at drive start and turn boundaries), "claim"
  VerdictRule,  \* "none" (each reader its own marker) or "first" (the first end marker in journal order)
  Crashable,    \* subset of {"prim", "workers"}: which processes may crash
  Timed,        \* each process takes at most one step per tick, and none is slower
  TTL,          \* lease TTL, in ticks
  Interval,     \* RecoverLoop's pass interval, in ticks
  Bound,        \* BoundedPickup: ticks from a dead holder's lease lapsing to its takeover
  MaxAmbig,     \* budget of error replies on writes (A3: committed or not)
  MaxCrash,     \* budget of process crashes
  MaxStall,     \* budget of stalls of a lease holder
  Bug,          \* "none" or a reverted rule, see regress/
  Api           \* P14's Run API rules and per-run options (a record, see ApiFields below)

Runs == 1..NRuns
PrimRun == NRuns
LastCall == IF Bug = "ReplayFinished" THEN NCalls + 1 ELSE NCalls
Calls == 1..LastCall

\* Process identities: pairs of strings, so every identity compares with every other.
Prim == <<"prim", "-">>
Res == <<"res", "-">>       \* a later caller's Resume of the primary's run, with options of its own
Op == <<"op", "-">>
Canc == <<"canc", "-">>
St == <<"stat", "-">>
Slot(w) == <<"slot", w>>
TkSlot(w) == <<"tko", w>>
SweepP(w) == <<"pass", w>>
TakeP(w) == <<"tkp", w>>
Drivers == {Prim} \cup {Slot(w) : w \in Workers}
           \cup (IF PassRule = "split" THEN {TkSlot(w) : w \in Workers} ELSE {})
           \cup (IF Api.res # {} THEN {Res} ELSE {})
PassProcs == {SweepP(w) : w \in Workers}
             \cup (IF PassRule = "split" THEN {TakeP(w) : w \in Workers} ELSE {})
SlotOf(q) == IF q[1] = "pass" THEN Slot(q[2]) ELSE TkSlot(q[2])
WorkerProcs(w) == {SweepP(w), Slot(w), TakeP(w), TkSlot(w)} \cap (Drivers \cup PassProcs)

NoOwner == <<"none", "-">>
Dead == <<"dead", "-">>
NoLease == [o |-> NoOwner, left |-> 0]
NoClaim == <<"none", "-">>
Unknown == <<"x", "-">>     \* a claim whose drive is gone (the halted runs of the initial state)

\* The lease owner a driver claims under: its own token per drive (Lease's <holder>#<token>), or,
\* historically (#58 finding 4), the bare holder name, which a second driver of that name renews.
HolderOf(p) == IF p \in {Prim, Res} THEN PrimName ELSE Name[p[2]]
Tok(p) == IF Bug = "SharedHolder" /\ p \in Drivers THEN <<"name", HolderOf(p)>> ELSE p

Range(s) == {s[i] : i \in DOMAIN s}
Min(S) == CHOOSE x \in S : \A y \in S : x <= y

ASSUME Bug \in {"none", "NoRecheck", "SharedHolder", "NoAbortMarker", "RecordUnderCtx",
                "SkipWhenBusy", "ReplayFinished"}
ASSUME Primary \in {"none", "leased", "plain"} /\ PassRule \in {"all", "lapsedFirst", "split"}
ASSUME CancelRule \in {"none", "turn", "claim"} /\ VerdictRule \in {"none", "first"}
ASSUME (CancelRule = "none") <=> ~Canceller

(***************************************************************************)
(* P14's Run API (docs/design/api-v1.md, item 1). Per-run options are a    *)
(* record: f, the tool filter (D3: the calls whose tools the run may use;  *)
(* call c uses tool c); l, the turn limit (WithMaxTurns or WithTokenBudget: *)
(* the calls the whole run may make); c, every other journaled setting      *)
(* (system prompt, sampling, tool choice, output mode, typed schema,        *)
(* principal), which must match. NoOpt: the caller passes none.             *)
(*                                                                          *)
(* Api's fields:                                                            *)
(*   opt     "journal": the journaling rule (B1): run:start holds the first *)
(*           drive's options, every later drive uses them, a different      *)
(*           limit is an amendment run:limits:<n>, any other difference is  *)
(*           ErrConfig; "caller": each drive uses its own caller's options  *)
(*           (the code before P14).                                         *)
(*   filter  "dispatch": a call outside the journaled filter is refused at  *)
(*           dispatch; "request": the filter only narrows Request.Tools.    *)
(*   prim    the primary's options (Run); res: the options a later Resume   *)
(*           may pass ({}: no later caller); dflt: the agent's defaults,    *)
(*           which a drive that passes nothing uses under "caller"; init:   *)
(*           run:start of the runs started before the model begins.         *)
(*   sagaCancel  "marker": D1 as written, Cancel writes run:cancelled on a  *)
(*           saga too and a drive that sees it rolls back; "request": the   *)
(*           proposed rule, Cancel on a saga writes a rollback request (not *)
(*           an end marker) and the drive that rolls back writes            *)
(*           run:cancelled; Cancel of a run with no run:start is            *)
(*           ErrNotStarted.                                                 *)
(*   status  "off", "load" (Status reads one Load, a prefix), "gets" (one   *)
(*           Get per end marker), "regets" (the Gets, then, if any marker   *)
(*           was found, the Gets again).                                    *)
(*   notStarted  recovery of a run with no run:start: "report" (skipped,    *)
(*           reported once per process, run:start read again every pass),   *)
(*           "every" (reported every pass), "remember" (skipped for good    *)
(*           once reported).                                                *)
(***************************************************************************)
NoOpt == [f |-> {}, l |-> -1, c |-> "none"]
NoStart == [f |-> {}, l |-> -1, c |-> "nostart"]
ASSUME Api.opt \in {"journal", "caller"} /\ Api.filter \in {"dispatch", "request"}
ASSUME Api.sagaCancel \in {"marker", "request"} /\ Api.status \in {"off", "load", "gets", "regets"}
ASSUME Api.notStarted \in {"report", "every", "remember"}
EndKinds == <<"complete", "aborted", "cancelled">>   \* Status's Gets, in this order

(* --algorithm lifecycle
variables
  \* The journal of each run: one attempt marker per call (the claimant, or NoClaim), each
  \* call's result, the end markers in journal order, and the approvals.
  marker   = [r \in Runs |-> [c \in Calls |-> IF r \in Halted0 /\ c = 1 THEN Unknown ELSE NoClaim]],
  result   = [r \in Runs |-> [c \in Calls |-> "none"]],
  ends     = [r \in Runs |-> <<>>],
  approved = [r \in Runs |-> FALSE],
  \* P14: run:start's per-run options (written by the first drive, the primary's), the limit
  \* amendments run:limits:<n> in order, and a saga's rollback request (sagaCancel "request").
  start    = [r \in Runs |-> IF r = PrimRun /\ Primary # "none" THEN NoStart ELSE Api.init],
  lims     = [r \in Runs |-> <<>>],
  creq     = [r \in Runs |-> FALSE],
  \* The leases table: the owner and the ticks left before the lease lapses.
  lease    = [r \in Runs |-> NoLease],
  \* Per driver: the run it is asked to drive (0: idle), the run it drives, the lease it took and
  \* has not released (0: none), its call, the tool's outcome, its return, its last reply, and
  \* whether its context is done because the lease was lost.
  job      = [p \in Drivers |-> IF (p = Prim /\ Primary # "none") \/ p = Res THEN PrimRun ELSE 0],
  jr       = [p \in Drivers |-> 0],
  holds    = [p \in Drivers \cup {Op} |-> 0],
  cc       = [p \in Drivers |-> 1],
  outc     = [p \in Drivers |-> "none"],
  ret      = [p \in Drivers |-> "none"],
  reply    = [p \in Drivers \cup {Op, Canc} |-> ""],
  ctxDead  = [p \in Drivers |-> FALSE],
  stalled  = [p \in Drivers |-> FALSE],
  \* Per driver (P14): the options its caller passed (a recovery drive passes none), the
  \* options it drives under, and why it rolls back ("fail" or "cancel").
  copt     = [p \in Drivers |-> IF p = Prim THEN Api.prim ELSE NoOpt],
  eo       = [p \in Drivers |-> NoOpt],
  why      = [p \in Drivers |-> "fail"],
  \* Status (D8): the run it reads, the Get it is at, the markers found with their journal
  \* positions, the round of Gets, and what it returns.
  srun = 0, sk = 1, sseen = {}, sround = 1, sret = "none",
  \* Per worker process: the runs with no run:start it has reported (WithRecoverErrors).
  nsRep    = [w \in Workers |-> {}],
  \* Per recovery pass: the runs listed and not yet handed out, the lapsed ones it takes first
  \* (PassRule "lapsedFirst"), the run it is handing out, and its ticker.
  listed   = [q \in PassProcs |-> {}],
  pri      = [q \in PassProcs |-> {}],
  cand     = [q \in PassProcs |-> 0],
  tk       = [q \in PassProcs |-> Interval],
  fire     = [q \in PassProcs |-> TRUE],
  \* The operator and Cancel.
  orun = 0, ocall = 0, cret = "none",
  \* Budgets.
  ambig = 0, crashes = 0, stalls = 0,
  \* Ghosts: effect calls, the end markers in place when each call's claim was won, an effect
  \* called with run:cancelled in place, a resume of a run holding an end marker, a record that
  \* errored, an effect whose driver died before recording it, and the pickup clock.
  fired     = [r \in Runs |-> [c \in Calls |-> 0]],
  seenEnds  = [r \in Runs |-> [c \in Calls |-> {}]],
  fireAfterCancel = FALSE,
  resumedEnded = FALSE,
  recErr    = [r \in Runs |-> [c \in Calls |-> FALSE]],
  crashLost = [r \in Runs |-> [c \in Calls |-> FALSE]],
  orphan    = [r \in Runs |-> FALSE],
  age       = [r \in Runs |-> 0],
  \* P14 ghosts: a rollback ran; an effect fired outside the journaled filter; a drive ran under
  \* options other than the journaled ones when it loaded the run, or fired past its limit; an
  \* effect fired past the limit journaled at that moment; Status's read had an instant with no
  \* end marker; and the not-started reports per worker process.
  rolled    = [r \in Runs |-> FALSE],
  filterBroken = FALSE,
  optBroken = FALSE,
  overLimit = FALSE,
  sOpen     = FALSE,
  nsCnt     = [w \in Workers |-> [r \in Runs |-> 0]];

define
  Ended(r) == ends[r] # <<>>
  Live(r, c) == marker[r][c] # NoClaim /\ result[r][c] = "none"
  Lapsed(r) == lease[r].o # NoOwner /\ lease[r].left = 0
  \* AcquireLease: granted if the run is unleased, its lease lapsed, or the owner is the caller.
  LeaseFree(r, p) == lease[r].o = NoOwner \/ lease[r].left = 0 \/ lease[r].o = Tok(p)
  LiveFor(p) == holds[p] # 0 /\ lease[holds[p]].o = Tok(p) /\ lease[holds[p]].left > 0
  Leased(p) == p \notin {Prim, Res} \/ Primary = "leased"
  NextCall(r) == IF \E c \in Calls : result[r][c] = "none"
                 THEN Min({c \in Calls : result[r][c] = "none"}) ELSE LastCall + 1
  \* What a drive that read run:cancelled reports: under VerdictRule "first", the first end marker.
  CancelledVerdict(r) == IF VerdictRule = "first" THEN Head(ends[r]) ELSE "cancelled"
  InFlight(q, r) == \E d \in Drivers : d[2] = q[2] /\ job[d] = r
  \* P14. The journaled limit: run:start's, or the last amendment's.
  JLimit(r) == IF lims[r] = <<>> THEN start[r].l ELSE lims[r][Len(lims[r])]
  \* Rule 3: a later caller's options differ from run:start in a setting other than a limit.
  Mismatch(p, r) == Api.opt = "journal" /\ p = Res /\ copt[p] # NoOpt
                    /\ (copt[p].f # start[r].f \/ copt[p].c # start[r].c)
  \* Rule 2: a later caller's limit differs from the journaled one, an amendment.
  Amends(p, r) == Api.opt = "journal" /\ p = Res /\ copt[p] # NoOpt /\ copt[p].l # JLimit(r)
  \* The options a drive runs under: the journaled ones (rule 1), or its caller's (or the agent's
  \* defaults when it passes none) under the rule before P14.
  Journaled(r) == [f |-> start[r].f, l |-> JLimit(r), c |-> start[r].c]
  Eff(p, r) == IF Api.opt = "caller"
               THEN (IF copt[p] = NoOpt THEN Api.dflt ELSE copt[p])
               ELSE Journaled(r)
  \* A cancellation the drive's checks see: run:cancelled, or a saga's rollback request.
  CancelSeen(r) == "cancelled" \in Range(ends[r]) \/ creq[r]
  \* sagaCancel "marker": a saga whose first end marker is run:cancelled and whose rollback has
  \* not finished (no run:aborted) is rolled back by the drive that opens it.
  SagaCancelPending(r) == Api.sagaCancel = "marker" /\ r \in SagaRuns /\ Ended(r)
                          /\ Head(ends[r]) = "cancelled" /\ "aborted" \notin Range(ends[r])
  \* The end marker a finished rollback writes.
  AbortKind(p) == IF why[p] = "cancel" /\ Api.sagaCancel = "request" THEN "cancelled" ELSE "aborted"
  \* Status's Gets: the markers found so far, with this Get's result added.
  Pos(r, k) == CHOOSE i \in DOMAIN ends[r] : ends[r][i] = k
  Seen1 == IF EndKinds[sk] \in Range(ends[srun])
           THEN sseen \cup {<<EndKinds[sk], Pos(srun, EndKinds[sk])>>} ELSE sseen
  StatusOf(s) == IF s = {} THEN "started" ELSE (CHOOSE x \in s : \A y \in s : x[2] <= y[2])[1]
end define;

\* A store write: ok, error and not committed, or error and committed (A3).
macro Reply(r) begin
  either r := "ok";
  or await ambig < MaxAmbig; ambig := ambig + 1; r := "err_nc";
  or await ambig < MaxAmbig; ambig := ambig + 1; r := "err_c";
  end either;
end macro;

\* A drive of one run: the primary's (Lease around Run, or a plain Run), or a recovery pass's
\* (recoverRun: Lease, runEnded, resume). One label is one store round trip or local decision.
fair process drv \in Drivers
begin
DIdle:
  await job[self] # 0;
  jr[self] := job[self]; ret[self] := "none"; ctxDead[self] := FALSE; outc[self] := "none";
  if self = Res then
    with o \in Api.res do copt[self] := o; end with;
  end if;
  if Leased(self) then
    if LeaseFree(job[self], self) then
      lease[job[self]] := [o |-> Tok(self), left |-> TTL];
      holds[self] := job[self];
      orphan[job[self]] := FALSE;
      if self \in {Prim, Res} then goto DOpen; else goto DCheck; end if;
    else
      goto DRel;                    \* another holder is driving it
    end if;
  else
    goto DOpen;
  end if;
DCheck:
  \* runEnded under the lease (#114): a run that ended since the listing is not resumed.
  if Bug # "NoRecheck" /\ Ended(jr[self]) then
    goto DRel;
  elsif start[jr[self]] = NoStart \/ (Api.notStarted = "remember" /\ jr[self] \in nsRep[self[2]]) then
    \* P14's dispatch reads run:start: a run with none is skipped and reported (ErrNotStarted)
    \* once per process.
    if jr[self] \notin nsRep[self[2]] \/ Api.notStarted = "every" then
      nsCnt[self[2]][jr[self]] := nsCnt[self[2]][jr[self]] + 1;
    end if;
    nsRep[self[2]] := nsRep[self[2]] \cup {jr[self]};
    goto DRel;
  end if;
DResume:
  \* resume(ctx, runID) is called.
  if Ended(jr[self]) then resumedEnded := TRUE; end if;
DOpen:
  \* Agent.run's Load (openRun), the end of a finished run, the resume gate, the approval pause.
  \* P14: the Load also holds run:start (the journaled options) and the limit amendments.
  if ctxDead[self] then
    ret[self] := "lost"; goto DRel;
  elsif start[jr[self]] = NoStart then
    \* The first drive journals its caller's options; a Resume of a run never started is
    \* ErrNotStarted.
    if self = Prim then goto DStart; else ret[self] := "notstarted"; goto DRel; end if;
  elsif SagaCancelPending(jr[self]) then
    why[self] := "cancel"; goto DRollback;
  elsif VerdictRule = "first" /\ Ended(jr[self]) then
    ret[self] := Head(ends[jr[self]]); goto DRel;
  elsif CancelRule # "none" /\ "cancelled" \in Range(ends[jr[self]]) then
    ret[self] := "cancelled"; goto DRel;
  elsif "complete" \in Range(ends[jr[self]]) /\ Bug # "ReplayFinished" then
    ret[self] := "complete"; goto DRel;
  elsif "aborted" \in Range(ends[jr[self]]) then
    ret[self] := "aborted"; goto DRel;
  elsif Mismatch(self, jr[self]) then
    ret[self] := "config"; goto DRel;             \* rule 3: ErrConfig
  elsif Amends(self, jr[self]) then
    goto DAmend;                                  \* rule 2: run:limits:<n>
  elsif \E c \in Calls : result[jr[self]][c] = "fail" then
    why[self] := "fail"; goto DRollback;
  elsif NextCall(jr[self]) > NCalls /\ "complete" \notin Range(ends[jr[self]]) then
    goto DComplete;
  elsif Live(jr[self], NextCall(jr[self])) then
    ret[self] := "halt"; goto DRel;
  elsif creq[jr[self]] then
    why[self] := "cancel"; goto DRollback;        \* a saga's rollback request
  elsif jr[self] \in Paused0 /\ ~approved[jr[self]] then
    ret[self] := "pause"; goto DRel;
  elsif NextCall(jr[self]) > Eff(self, jr[self]).l then
    ret[self] := "limit"; goto DRel;              \* the run's turn limit is spent
  else
    eo[self] := Eff(self, jr[self]);
    optBroken := optBroken \/ Eff(self, jr[self]) # Journaled(jr[self]);
    cc[self] := NextCall(jr[self]); goto DClaim;
  end if;
DStart:
  \* P14: run:start with the caller's options, first writer wins (#70's header).
  Reply(reply[self]);
  if reply[self] # "err_nc" /\ start[jr[self]] = NoStart then
    start[jr[self]] := copt[self];
  end if;
  if reply[self] # "ok" then ret[self] := "error"; goto DRel; else goto DOpen; end if;
DAmend:
  \* P14, rule 2: a later drive's different limit is journaled as run:limits:<n>.
  Reply(reply[self]);
  if reply[self] # "err_nc" then
    lims[jr[self]] := Append(lims[jr[self]], copt[self].l);
  end if;
  if reply[self] # "ok" then ret[self] := "error"; goto DRel; else goto DOpen; end if;
DTurn:
  \* A turn boundary: D1's run:cancelled check (one Get) before the next model turn, and the
  \* turn limit the drive runs under.
  if CancelRule # "none" /\ CancelSeen(jr[self]) then
    if jr[self] \in SagaRuns then
      why[self] := "cancel"; goto DRollback;
    else
      ret[self] := CancelledVerdict(jr[self]); goto DRel;
    end if;
  elsif cc[self] > eo[self].l then
    ret[self] := "limit"; goto DRel;
  end if;
DClaim:
  \* The attempt claim (model 1), an Insert under the drive's context. A call outside the run's
  \* tool filter is refused at dispatch (filter "dispatch"): its error result is recorded and
  \* the model's next turn sees it.
  if ctxDead[self] then
    ret[self] := "lost"; goto DRel;
  elsif Api.filter = "dispatch" /\ cc[self] \notin eo[self].f then
    Reply(reply[self]);
    if reply[self] # "err_nc" /\ result[jr[self]][cc[self]] = "none" then
      result[jr[self]][cc[self]] := "refused";
    end if;
    if reply[self] # "ok" then ret[self] := "error"; goto DRel;
    elsif cc[self] < NCalls then cc[self] := cc[self] + 1; goto DTurn;
    else goto DComplete;
    end if;
  else
    Reply(reply[self]);
    if reply[self] # "err_nc" /\ marker[jr[self]][cc[self]] = NoClaim then
      marker[jr[self]][cc[self]] := self;
      seenEnds[jr[self]][cc[self]] := Range(ends[jr[self]])
                                      \cup (IF creq[jr[self]] THEN {"cancelled"} ELSE {});
    end if;
    if reply[self] # "ok" then ret[self] := "error"; goto DRel;
    elsif marker[jr[self]][cc[self]] # self then ret[self] := "halt"; goto DRel;
    elsif CancelRule = "claim" then goto DPost;
    else goto DCall;
    end if;
  end if;
DPost:
  \* The proposed P14 rule: run:cancelled is read again once the claim is won, before the call;
  \* a cancelled run records the attempt as not started.
  if CancelSeen(jr[self]) then
    marker[jr[self]][cc[self]] := NoClaim;
    if jr[self] \in SagaRuns then
      why[self] := "cancel"; goto DRollback;
    else
      ret[self] := CancelledVerdict(jr[self]); goto DRel;
    end if;
  end if;
DCall:
  \* recordFresh: the sctx.Err() check, then the effect.
  if ctxDead[self] then
    marker[jr[self]][cc[self]] := NoClaim;
    ret[self] := "lost"; goto DRel;
  else
    fired[jr[self]][cc[self]] := fired[jr[self]][cc[self]] + 1;
    if CancelSeen(jr[self]) then fireAfterCancel := TRUE; end if;
    filterBroken := filterBroken \/ cc[self] \notin start[jr[self]].f;
    optBroken := optBroken \/ cc[self] > eo[self].l;
    overLimit := overLimit \/ cc[self] > JLimit(jr[self]);
    either outc[self] := "ok";
    or await jr[self] \in SagaRuns; outc[self] := "fail";
    end either;
  end if;
DRecord:
  \* The result, recorded under context.WithoutCancel (#58 finding 1).
  if Bug = "RecordUnderCtx" /\ ctxDead[self] then
    ret[self] := "lost"; goto DRel;
  else
    Reply(reply[self]);
    if reply[self] # "err_nc" /\ result[jr[self]][cc[self]] = "none" then
      result[jr[self]][cc[self]] := outc[self];
    end if;
    if reply[self] # "ok" then
      recErr[jr[self]][cc[self]] := TRUE; ret[self] := "error"; goto DRel;
    elsif result[jr[self]][cc[self]] = "fail" then
      goto DRollback;
    elsif cc[self] < NCalls then
      cc[self] := cc[self] + 1; goto DTurn;
    else
      goto DComplete;
    end if;
  end if;
DRollback:
  \* rollbackRun: the compensations, memoized steps (model 9 and model 5).
  if ctxDead[self] then ret[self] := "lost"; goto DRel; else rolled[jr[self]] := TRUE; end if;
DAbort:
  \* The finished rollback's run:aborted (#31); under sagaCancel "request", a rollback a
  \* cancellation asked for writes run:cancelled instead.
  if Bug = "NoAbortMarker" then
    ret[self] := "aborted"; goto DRel;
  elsif ctxDead[self] then
    ret[self] := "lost"; goto DRel;
  else
    Reply(reply[self]);
    if reply[self] # "err_nc" /\ AbortKind(self) \notin Range(ends[jr[self]]) then
      ends[jr[self]] := Append(ends[jr[self]], AbortKind(self));
    end if;
    if reply[self] # "ok" then ret[self] := "error"; goto DRel;
    elsif VerdictRule = "first" then goto DVerdict;
    else ret[self] := AbortKind(self); goto DRel;
    end if;
  end if;
DComplete:
  \* run:complete, first writer wins (putRecord).
  if ctxDead[self] then
    ret[self] := "lost"; goto DRel;
  else
    Reply(reply[self]);
    if reply[self] # "err_nc" /\ "complete" \notin Range(ends[jr[self]]) then
      ends[jr[self]] := Append(ends[jr[self]], "complete");
    end if;
    if reply[self] # "ok" then ret[self] := "error"; goto DRel;
    elsif VerdictRule = "first" then goto DVerdict;
    else ret[self] := "complete"; goto DRel;
    end if;
  end if;
DVerdict:
  \* VerdictRule "first": the end markers are read again; the first in journal order is the run's.
  ret[self] := Head(ends[jr[self]]);
DRel:
  \* Lease's deferred ReleaseLease (a no-op unless the caller is the owner), and the slot is free.
  if holds[self] # 0 /\ lease[holds[self]].o = Tok(self) then
    lease[holds[self]] := NoLease;
  end if;
  holds[self] := 0;
  job[self] := 0;
  if self \in {Prim, Res} then goto Done; else goto DIdle; end if;
end process;

\* A recovery pass: RecoverLoop's pass (or Recover's one pass) over the runs the Lister returns,
\* handing each to the worker's slot (concurrency 1). Under PassRule "split", a second loop per
\* worker lists only the runs whose lease lapsed, with its own slot.
fair process pass \in PassProcs
begin
PList:
  if self[1] = "tkp" then
    listed[self] := {r \in Runs : Lapsed(r) /\ ~Ended(r)};
  else
    listed[self] := {r \in Runs : ~Ended(r)};
  end if;
  pri[self] := IF PassRule = "lapsedFirst" THEN {r \in Runs : Lapsed(r) /\ ~Ended(r)} ELSE {};
PNext:
  if listed[self] = {} then
    goto PWait;
  else
    cand[self] := IF pri[self] \cap listed[self] # {} THEN Min(pri[self] \cap listed[self])
                  ELSE Min(listed[self]);
    listed[self] := listed[self] \ {cand[self]};
    if InFlight(self, cand[self]) then goto PNext; else goto PSlot; end if;
  end if;
PSlot:
  if Bug = "SkipWhenBusy" /\ job[SlotOf(self)] # 0 then
    goto PWait;                     \* the first RecoverLoop (#58): a busy slot ended the pass
  else
    await job[SlotOf(self)] = 0;
    job[SlotOf(self)] := cand[self];
    goto PNext;
  end if;
PWait:
  if ~Loop then
    goto Done;
  else
    await fire[self];
    fire[self] := FALSE;
    goto PList;
  end if;
end process;

\* An operator: ResolveHalt (checkNoLiveDriver takes the run's lease) and Approve. Never assumed
\* to act.
process resolveOp = Op
begin
OPick:
  await Resolver;
  either
    with x \in {<<r, c>> \in Runs \X Calls : Live(r, c)} do
      orun := x[1]; ocall := x[2];
    end with;
  or
    with r \in {r \in Paused0 : ~approved[r]} do approved[r] := TRUE; end with;
    goto OPick;
  end either;
OLease:
  if LeaseFree(orun, Op) then
    lease[orun] := [o |-> Op, left |-> TTL]; holds[Op] := orun;
  else
    goto OPick;                     \* HaltInFlight: a driver holds the run's lease
  end if;
OWrite:
  Reply(reply[Op]);
  if reply[Op] # "err_nc" /\ result[orun][ocall] = "none" then
    result[orun][ocall] := "resolved";
  end if;
ORel:
  if lease[orun].o = Op then lease[orun] := NoLease; end if;
  holds[Op] := 0;
  goto OPick;
end process;

\* P14's Cancel of the primary's run: refused for a run that is over, else run:cancelled.
process cancelOp = Canc
begin
CGet:
  \* Under sagaCancel "request", Cancel also reads run:start (the saga flag).
  await Canceller;
  if Ended(PrimRun) then
    cret := "over"; goto Done;
  elsif Api.sagaCancel = "request" /\ start[PrimRun] = NoStart then
    cret := "notstarted"; goto Done;              \* ErrNotStarted: nothing is written
  elsif Api.sagaCancel = "request" /\ PrimRun \in SagaRuns then
    goto CReq;
  end if;
CIns:
  Reply(reply[Canc]);
  if reply[Canc] # "err_nc" /\ "cancelled" \notin Range(ends[PrimRun]) then
    ends[PrimRun] := Append(ends[PrimRun], "cancelled");
  end if;
  if reply[Canc] # "ok" then cret := "error"; goto Done;
  elsif VerdictRule = "first" then goto CRead;
  else cret := "cancelled"; goto Done;
  end if;
CRead:
  cret := Head(ends[PrimRun]); goto Done;
CReq:
  \* The proposed rule for a saga: a rollback request, not an end marker, so recovery still
  \* lists the run; the drive that rolls it back writes run:cancelled.
  Reply(reply[Canc]);
  if reply[Canc] # "err_nc" then creq[PrimRun] := TRUE; end if;
  if reply[Canc] = "ok" then cret := "requested"; else cret := "error"; end if;
end process;

\* P14's Status (D8) of one run, called once at any time: run:start, then the end markers, by one
\* Load (a prefix of the journal, A2) or by one Get each.
process statOp = St
begin
SPick:
  await Api.status # "off";
  with r \in Runs do srun := r; end with;
SStart:
  if start[srun] = NoStart then
    sret := "notstarted"; goto Done;
  elsif Api.status = "load" then
    sOpen := ~Ended(srun);
    if Ended(srun) then sret := Head(ends[srun]); else sret := "started"; end if;
    goto Done;
  end if;
SGet:
  \* One Get; the first Get's instant is when an empty answer ("started") holds.
  if sround = 1 /\ sk = 1 then sOpen := ~Ended(srun); end if;
  if sk < 3 then
    sseen := Seen1; sk := sk + 1; goto SGet;
  elsif sround = 1 /\ Seen1 # {} /\ Api.status = "regets" then
    sround := 2; sk := 1; sseen := {}; goto SGet;
  else
    sret := StatusOf(Seen1);
  end if;
end process;
end algorithm; *)
\* BEGIN TRANSLATION
VARIABLES marker, result, ends, approved, start, lims, creq, lease, job, jr, 
          holds, cc, outc, ret, reply, ctxDead, stalled, copt, eo, why, srun, 
          sk, sseen, sround, sret, nsRep, listed, pri, cand, tk, fire, orun, 
          ocall, cret, ambig, crashes, stalls, fired, seenEnds, 
          fireAfterCancel, resumedEnded, recErr, crashLost, orphan, age, 
          rolled, filterBroken, optBroken, overLimit, sOpen, nsCnt, pc

(* define statement *)
Ended(r) == ends[r] # <<>>
Live(r, c) == marker[r][c] # NoClaim /\ result[r][c] = "none"
Lapsed(r) == lease[r].o # NoOwner /\ lease[r].left = 0

LeaseFree(r, p) == lease[r].o = NoOwner \/ lease[r].left = 0 \/ lease[r].o = Tok(p)
LiveFor(p) == holds[p] # 0 /\ lease[holds[p]].o = Tok(p) /\ lease[holds[p]].left > 0
Leased(p) == p \notin {Prim, Res} \/ Primary = "leased"
NextCall(r) == IF \E c \in Calls : result[r][c] = "none"
               THEN Min({c \in Calls : result[r][c] = "none"}) ELSE LastCall + 1

CancelledVerdict(r) == IF VerdictRule = "first" THEN Head(ends[r]) ELSE "cancelled"
InFlight(q, r) == \E d \in Drivers : d[2] = q[2] /\ job[d] = r

JLimit(r) == IF lims[r] = <<>> THEN start[r].l ELSE lims[r][Len(lims[r])]

Mismatch(p, r) == Api.opt = "journal" /\ p = Res /\ copt[p] # NoOpt
                  /\ (copt[p].f # start[r].f \/ copt[p].c # start[r].c)

Amends(p, r) == Api.opt = "journal" /\ p = Res /\ copt[p] # NoOpt /\ copt[p].l # JLimit(r)


Journaled(r) == [f |-> start[r].f, l |-> JLimit(r), c |-> start[r].c]
Eff(p, r) == IF Api.opt = "caller"
             THEN (IF copt[p] = NoOpt THEN Api.dflt ELSE copt[p])
             ELSE Journaled(r)

CancelSeen(r) == "cancelled" \in Range(ends[r]) \/ creq[r]


SagaCancelPending(r) == Api.sagaCancel = "marker" /\ r \in SagaRuns /\ Ended(r)
                        /\ Head(ends[r]) = "cancelled" /\ "aborted" \notin Range(ends[r])

AbortKind(p) == IF why[p] = "cancel" /\ Api.sagaCancel = "request" THEN "cancelled" ELSE "aborted"

Pos(r, k) == CHOOSE i \in DOMAIN ends[r] : ends[r][i] = k
Seen1 == IF EndKinds[sk] \in Range(ends[srun])
         THEN sseen \cup {<<EndKinds[sk], Pos(srun, EndKinds[sk])>>} ELSE sseen
StatusOf(s) == IF s = {} THEN "started" ELSE (CHOOSE x \in s : \A y \in s : x[2] <= y[2])[1]


vars == << marker, result, ends, approved, start, lims, creq, lease, job, jr, 
           holds, cc, outc, ret, reply, ctxDead, stalled, copt, eo, why, srun, 
           sk, sseen, sround, sret, nsRep, listed, pri, cand, tk, fire, orun, 
           ocall, cret, ambig, crashes, stalls, fired, seenEnds, 
           fireAfterCancel, resumedEnded, recErr, crashLost, orphan, age, 
           rolled, filterBroken, optBroken, overLimit, sOpen, nsCnt, pc >>

ProcSet == (Drivers) \cup (PassProcs) \cup {Op} \cup {Canc} \cup {St}

Init == (* Global variables *)
        /\ marker = [r \in Runs |-> [c \in Calls |-> IF r \in Halted0 /\ c = 1 THEN Unknown ELSE NoClaim]]
        /\ result = [r \in Runs |-> [c \in Calls |-> "none"]]
        /\ ends = [r \in Runs |-> <<>>]
        /\ approved = [r \in Runs |-> FALSE]
        /\ start = [r \in Runs |-> IF r = PrimRun /\ Primary # "none" THEN NoStart ELSE Api.init]
        /\ lims = [r \in Runs |-> <<>>]
        /\ creq = [r \in Runs |-> FALSE]
        /\ lease = [r \in Runs |-> NoLease]
        /\ job = [p \in Drivers |-> IF (p = Prim /\ Primary # "none") \/ p = Res THEN PrimRun ELSE 0]
        /\ jr = [p \in Drivers |-> 0]
        /\ holds = [p \in Drivers \cup {Op} |-> 0]
        /\ cc = [p \in Drivers |-> 1]
        /\ outc = [p \in Drivers |-> "none"]
        /\ ret = [p \in Drivers |-> "none"]
        /\ reply = [p \in Drivers \cup {Op, Canc} |-> ""]
        /\ ctxDead = [p \in Drivers |-> FALSE]
        /\ stalled = [p \in Drivers |-> FALSE]
        /\ copt = [p \in Drivers |-> IF p = Prim THEN Api.prim ELSE NoOpt]
        /\ eo = [p \in Drivers |-> NoOpt]
        /\ why = [p \in Drivers |-> "fail"]
        /\ srun = 0
        /\ sk = 1
        /\ sseen = {}
        /\ sround = 1
        /\ sret = "none"
        /\ nsRep = [w \in Workers |-> {}]
        /\ listed = [q \in PassProcs |-> {}]
        /\ pri = [q \in PassProcs |-> {}]
        /\ cand = [q \in PassProcs |-> 0]
        /\ tk = [q \in PassProcs |-> Interval]
        /\ fire = [q \in PassProcs |-> TRUE]
        /\ orun = 0
        /\ ocall = 0
        /\ cret = "none"
        /\ ambig = 0
        /\ crashes = 0
        /\ stalls = 0
        /\ fired = [r \in Runs |-> [c \in Calls |-> 0]]
        /\ seenEnds = [r \in Runs |-> [c \in Calls |-> {}]]
        /\ fireAfterCancel = FALSE
        /\ resumedEnded = FALSE
        /\ recErr = [r \in Runs |-> [c \in Calls |-> FALSE]]
        /\ crashLost = [r \in Runs |-> [c \in Calls |-> FALSE]]
        /\ orphan = [r \in Runs |-> FALSE]
        /\ age = [r \in Runs |-> 0]
        /\ rolled = [r \in Runs |-> FALSE]
        /\ filterBroken = FALSE
        /\ optBroken = FALSE
        /\ overLimit = FALSE
        /\ sOpen = FALSE
        /\ nsCnt = [w \in Workers |-> [r \in Runs |-> 0]]
        /\ pc = [self \in ProcSet |-> CASE self \in Drivers -> "DIdle"
                                        [] self \in PassProcs -> "PList"
                                        [] self = Op -> "OPick"
                                        [] self = Canc -> "CGet"
                                        [] self = St -> "SPick"]

DIdle(self) == /\ pc[self] = "DIdle"
               /\ job[self] # 0
               /\ jr' = [jr EXCEPT ![self] = job[self]]
               /\ ret' = [ret EXCEPT ![self] = "none"]
               /\ ctxDead' = [ctxDead EXCEPT ![self] = FALSE]
               /\ outc' = [outc EXCEPT ![self] = "none"]
               /\ IF self = Res
                     THEN /\ \E o \in Api.res:
                               copt' = [copt EXCEPT ![self] = o]
                     ELSE /\ TRUE
                          /\ copt' = copt
               /\ IF Leased(self)
                     THEN /\ IF LeaseFree(job[self], self)
                                THEN /\ lease' = [lease EXCEPT ![job[self]] = [o |-> Tok(self), left |-> TTL]]
                                     /\ holds' = [holds EXCEPT ![self] = job[self]]
                                     /\ orphan' = [orphan EXCEPT ![job[self]] = FALSE]
                                     /\ IF self \in {Prim, Res}
                                           THEN /\ pc' = [pc EXCEPT ![self] = "DOpen"]
                                           ELSE /\ pc' = [pc EXCEPT ![self] = "DCheck"]
                                ELSE /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                     /\ UNCHANGED << lease, holds, orphan >>
                     ELSE /\ pc' = [pc EXCEPT ![self] = "DOpen"]
                          /\ UNCHANGED << lease, holds, orphan >>
               /\ UNCHANGED << marker, result, ends, approved, start, lims, 
                               creq, job, cc, reply, stalled, eo, why, srun, 
                               sk, sseen, sround, sret, nsRep, listed, pri, 
                               cand, tk, fire, orun, ocall, cret, ambig, 
                               crashes, stalls, fired, seenEnds, 
                               fireAfterCancel, resumedEnded, recErr, 
                               crashLost, age, rolled, filterBroken, optBroken, 
                               overLimit, sOpen, nsCnt >>

DCheck(self) == /\ pc[self] = "DCheck"
                /\ IF Bug # "NoRecheck" /\ Ended(jr[self])
                      THEN /\ pc' = [pc EXCEPT ![self] = "DRel"]
                           /\ UNCHANGED << nsRep, nsCnt >>
                      ELSE /\ IF start[jr[self]] = NoStart \/ (Api.notStarted = "remember" /\ jr[self] \in nsRep[self[2]])
                                 THEN /\ IF jr[self] \notin nsRep[self[2]] \/ Api.notStarted = "every"
                                            THEN /\ nsCnt' = [nsCnt EXCEPT ![self[2]][jr[self]] = nsCnt[self[2]][jr[self]] + 1]
                                            ELSE /\ TRUE
                                                 /\ nsCnt' = nsCnt
                                      /\ nsRep' = [nsRep EXCEPT ![self[2]] = nsRep[self[2]] \cup {jr[self]}]
                                      /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                 ELSE /\ pc' = [pc EXCEPT ![self] = "DResume"]
                                      /\ UNCHANGED << nsRep, nsCnt >>
                /\ UNCHANGED << marker, result, ends, approved, start, lims, 
                                creq, lease, job, jr, holds, cc, outc, ret, 
                                reply, ctxDead, stalled, copt, eo, why, srun, 
                                sk, sseen, sround, sret, listed, pri, cand, tk, 
                                fire, orun, ocall, cret, ambig, crashes, 
                                stalls, fired, seenEnds, fireAfterCancel, 
                                resumedEnded, recErr, crashLost, orphan, age, 
                                rolled, filterBroken, optBroken, overLimit, 
                                sOpen >>

DResume(self) == /\ pc[self] = "DResume"
                 /\ IF Ended(jr[self])
                       THEN /\ resumedEnded' = TRUE
                       ELSE /\ TRUE
                            /\ UNCHANGED resumedEnded
                 /\ pc' = [pc EXCEPT ![self] = "DOpen"]
                 /\ UNCHANGED << marker, result, ends, approved, start, lims, 
                                 creq, lease, job, jr, holds, cc, outc, ret, 
                                 reply, ctxDead, stalled, copt, eo, why, srun, 
                                 sk, sseen, sround, sret, nsRep, listed, pri, 
                                 cand, tk, fire, orun, ocall, cret, ambig, 
                                 crashes, stalls, fired, seenEnds, 
                                 fireAfterCancel, recErr, crashLost, orphan, 
                                 age, rolled, filterBroken, optBroken, 
                                 overLimit, sOpen, nsCnt >>

DOpen(self) == /\ pc[self] = "DOpen"
               /\ IF ctxDead[self]
                     THEN /\ ret' = [ret EXCEPT ![self] = "lost"]
                          /\ pc' = [pc EXCEPT ![self] = "DRel"]
                          /\ UNCHANGED << cc, eo, why, optBroken >>
                     ELSE /\ IF start[jr[self]] = NoStart
                                THEN /\ IF self = Prim
                                           THEN /\ pc' = [pc EXCEPT ![self] = "DStart"]
                                                /\ ret' = ret
                                           ELSE /\ ret' = [ret EXCEPT ![self] = "notstarted"]
                                                /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                     /\ UNCHANGED << cc, eo, why, optBroken >>
                                ELSE /\ IF SagaCancelPending(jr[self])
                                           THEN /\ why' = [why EXCEPT ![self] = "cancel"]
                                                /\ pc' = [pc EXCEPT ![self] = "DRollback"]
                                                /\ UNCHANGED << cc, ret, eo, 
                                                                optBroken >>
                                           ELSE /\ IF VerdictRule = "first" /\ Ended(jr[self])
                                                      THEN /\ ret' = [ret EXCEPT ![self] = Head(ends[jr[self]])]
                                                           /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                                           /\ UNCHANGED << cc, 
                                                                           eo, 
                                                                           why, 
                                                                           optBroken >>
                                                      ELSE /\ IF CancelRule # "none" /\ "cancelled" \in Range(ends[jr[self]])
                                                                 THEN /\ ret' = [ret EXCEPT ![self] = "cancelled"]
                                                                      /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                                                      /\ UNCHANGED << cc, 
                                                                                      eo, 
                                                                                      why, 
                                                                                      optBroken >>
                                                                 ELSE /\ IF "complete" \in Range(ends[jr[self]]) /\ Bug # "ReplayFinished"
                                                                            THEN /\ ret' = [ret EXCEPT ![self] = "complete"]
                                                                                 /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                                                                 /\ UNCHANGED << cc, 
                                                                                                 eo, 
                                                                                                 why, 
                                                                                                 optBroken >>
                                                                            ELSE /\ IF "aborted" \in Range(ends[jr[self]])
                                                                                       THEN /\ ret' = [ret EXCEPT ![self] = "aborted"]
                                                                                            /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                                                                            /\ UNCHANGED << cc, 
                                                                                                            eo, 
                                                                                                            why, 
                                                                                                            optBroken >>
                                                                                       ELSE /\ IF Mismatch(self, jr[self])
                                                                                                  THEN /\ ret' = [ret EXCEPT ![self] = "config"]
                                                                                                       /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                                                                                       /\ UNCHANGED << cc, 
                                                                                                                       eo, 
                                                                                                                       why, 
                                                                                                                       optBroken >>
                                                                                                  ELSE /\ IF Amends(self, jr[self])
                                                                                                             THEN /\ pc' = [pc EXCEPT ![self] = "DAmend"]
                                                                                                                  /\ UNCHANGED << cc, 
                                                                                                                                  ret, 
                                                                                                                                  eo, 
                                                                                                                                  why, 
                                                                                                                                  optBroken >>
                                                                                                             ELSE /\ IF \E c \in Calls : result[jr[self]][c] = "fail"
                                                                                                                        THEN /\ why' = [why EXCEPT ![self] = "fail"]
                                                                                                                             /\ pc' = [pc EXCEPT ![self] = "DRollback"]
                                                                                                                             /\ UNCHANGED << cc, 
                                                                                                                                             ret, 
                                                                                                                                             eo, 
                                                                                                                                             optBroken >>
                                                                                                                        ELSE /\ IF NextCall(jr[self]) > NCalls /\ "complete" \notin Range(ends[jr[self]])
                                                                                                                                   THEN /\ pc' = [pc EXCEPT ![self] = "DComplete"]
                                                                                                                                        /\ UNCHANGED << cc, 
                                                                                                                                                        ret, 
                                                                                                                                                        eo, 
                                                                                                                                                        why, 
                                                                                                                                                        optBroken >>
                                                                                                                                   ELSE /\ IF Live(jr[self], NextCall(jr[self]))
                                                                                                                                              THEN /\ ret' = [ret EXCEPT ![self] = "halt"]
                                                                                                                                                   /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                                                                                                                                   /\ UNCHANGED << cc, 
                                                                                                                                                                   eo, 
                                                                                                                                                                   why, 
                                                                                                                                                                   optBroken >>
                                                                                                                                              ELSE /\ IF creq[jr[self]]
                                                                                                                                                         THEN /\ why' = [why EXCEPT ![self] = "cancel"]
                                                                                                                                                              /\ pc' = [pc EXCEPT ![self] = "DRollback"]
                                                                                                                                                              /\ UNCHANGED << cc, 
                                                                                                                                                                              ret, 
                                                                                                                                                                              eo, 
                                                                                                                                                                              optBroken >>
                                                                                                                                                         ELSE /\ IF jr[self] \in Paused0 /\ ~approved[jr[self]]
                                                                                                                                                                    THEN /\ ret' = [ret EXCEPT ![self] = "pause"]
                                                                                                                                                                         /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                                                                                                                                                         /\ UNCHANGED << cc, 
                                                                                                                                                                                         eo, 
                                                                                                                                                                                         optBroken >>
                                                                                                                                                                    ELSE /\ IF NextCall(jr[self]) > Eff(self, jr[self]).l
                                                                                                                                                                               THEN /\ ret' = [ret EXCEPT ![self] = "limit"]
                                                                                                                                                                                    /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                                                                                                                                                                    /\ UNCHANGED << cc, 
                                                                                                                                                                                                    eo, 
                                                                                                                                                                                                    optBroken >>
                                                                                                                                                                               ELSE /\ eo' = [eo EXCEPT ![self] = Eff(self, jr[self])]
                                                                                                                                                                                    /\ optBroken' = (optBroken \/ Eff(self, jr[self]) # Journaled(jr[self]))
                                                                                                                                                                                    /\ cc' = [cc EXCEPT ![self] = NextCall(jr[self])]
                                                                                                                                                                                    /\ pc' = [pc EXCEPT ![self] = "DClaim"]
                                                                                                                                                                                    /\ ret' = ret
                                                                                                                                                              /\ why' = why
               /\ UNCHANGED << marker, result, ends, approved, start, lims, 
                               creq, lease, job, jr, holds, outc, reply, 
                               ctxDead, stalled, copt, srun, sk, sseen, sround, 
                               sret, nsRep, listed, pri, cand, tk, fire, orun, 
                               ocall, cret, ambig, crashes, stalls, fired, 
                               seenEnds, fireAfterCancel, resumedEnded, recErr, 
                               crashLost, orphan, age, rolled, filterBroken, 
                               overLimit, sOpen, nsCnt >>

DStart(self) == /\ pc[self] = "DStart"
                /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                      /\ ambig' = ambig
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ reply' = [reply EXCEPT ![self] = "err_c"]
                /\ IF reply'[self] # "err_nc" /\ start[jr[self]] = NoStart
                      THEN /\ start' = [start EXCEPT ![jr[self]] = copt[self]]
                      ELSE /\ TRUE
                           /\ start' = start
                /\ IF reply'[self] # "ok"
                      THEN /\ ret' = [ret EXCEPT ![self] = "error"]
                           /\ pc' = [pc EXCEPT ![self] = "DRel"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "DOpen"]
                           /\ ret' = ret
                /\ UNCHANGED << marker, result, ends, approved, lims, creq, 
                                lease, job, jr, holds, cc, outc, ctxDead, 
                                stalled, copt, eo, why, srun, sk, sseen, 
                                sround, sret, nsRep, listed, pri, cand, tk, 
                                fire, orun, ocall, cret, crashes, stalls, 
                                fired, seenEnds, fireAfterCancel, resumedEnded, 
                                recErr, crashLost, orphan, age, rolled, 
                                filterBroken, optBroken, overLimit, sOpen, 
                                nsCnt >>

DAmend(self) == /\ pc[self] = "DAmend"
                /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                      /\ ambig' = ambig
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ reply' = [reply EXCEPT ![self] = "err_c"]
                /\ IF reply'[self] # "err_nc"
                      THEN /\ lims' = [lims EXCEPT ![jr[self]] = Append(lims[jr[self]], copt[self].l)]
                      ELSE /\ TRUE
                           /\ lims' = lims
                /\ IF reply'[self] # "ok"
                      THEN /\ ret' = [ret EXCEPT ![self] = "error"]
                           /\ pc' = [pc EXCEPT ![self] = "DRel"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "DOpen"]
                           /\ ret' = ret
                /\ UNCHANGED << marker, result, ends, approved, start, creq, 
                                lease, job, jr, holds, cc, outc, ctxDead, 
                                stalled, copt, eo, why, srun, sk, sseen, 
                                sround, sret, nsRep, listed, pri, cand, tk, 
                                fire, orun, ocall, cret, crashes, stalls, 
                                fired, seenEnds, fireAfterCancel, resumedEnded, 
                                recErr, crashLost, orphan, age, rolled, 
                                filterBroken, optBroken, overLimit, sOpen, 
                                nsCnt >>

DTurn(self) == /\ pc[self] = "DTurn"
               /\ IF CancelRule # "none" /\ CancelSeen(jr[self])
                     THEN /\ IF jr[self] \in SagaRuns
                                THEN /\ why' = [why EXCEPT ![self] = "cancel"]
                                     /\ pc' = [pc EXCEPT ![self] = "DRollback"]
                                     /\ ret' = ret
                                ELSE /\ ret' = [ret EXCEPT ![self] = CancelledVerdict(jr[self])]
                                     /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                     /\ why' = why
                     ELSE /\ IF cc[self] > eo[self].l
                                THEN /\ ret' = [ret EXCEPT ![self] = "limit"]
                                     /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                ELSE /\ pc' = [pc EXCEPT ![self] = "DClaim"]
                                     /\ ret' = ret
                          /\ why' = why
               /\ UNCHANGED << marker, result, ends, approved, start, lims, 
                               creq, lease, job, jr, holds, cc, outc, reply, 
                               ctxDead, stalled, copt, eo, srun, sk, sseen, 
                               sround, sret, nsRep, listed, pri, cand, tk, 
                               fire, orun, ocall, cret, ambig, crashes, stalls, 
                               fired, seenEnds, fireAfterCancel, resumedEnded, 
                               recErr, crashLost, orphan, age, rolled, 
                               filterBroken, optBroken, overLimit, sOpen, 
                               nsCnt >>

DClaim(self) == /\ pc[self] = "DClaim"
                /\ IF ctxDead[self]
                      THEN /\ ret' = [ret EXCEPT ![self] = "lost"]
                           /\ pc' = [pc EXCEPT ![self] = "DRel"]
                           /\ UNCHANGED << marker, result, cc, reply, ambig, 
                                           seenEnds >>
                      ELSE /\ IF Api.filter = "dispatch" /\ cc[self] \notin eo[self].f
                                 THEN /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                            /\ ambig' = ambig
                                         \/ /\ ambig < MaxAmbig
                                            /\ ambig' = ambig + 1
                                            /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                         \/ /\ ambig < MaxAmbig
                                            /\ ambig' = ambig + 1
                                            /\ reply' = [reply EXCEPT ![self] = "err_c"]
                                      /\ IF reply'[self] # "err_nc" /\ result[jr[self]][cc[self]] = "none"
                                            THEN /\ result' = [result EXCEPT ![jr[self]][cc[self]] = "refused"]
                                            ELSE /\ TRUE
                                                 /\ UNCHANGED result
                                      /\ IF reply'[self] # "ok"
                                            THEN /\ ret' = [ret EXCEPT ![self] = "error"]
                                                 /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                                 /\ cc' = cc
                                            ELSE /\ IF cc[self] < NCalls
                                                       THEN /\ cc' = [cc EXCEPT ![self] = cc[self] + 1]
                                                            /\ pc' = [pc EXCEPT ![self] = "DTurn"]
                                                       ELSE /\ pc' = [pc EXCEPT ![self] = "DComplete"]
                                                            /\ cc' = cc
                                                 /\ ret' = ret
                                      /\ UNCHANGED << marker, seenEnds >>
                                 ELSE /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                            /\ ambig' = ambig
                                         \/ /\ ambig < MaxAmbig
                                            /\ ambig' = ambig + 1
                                            /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                         \/ /\ ambig < MaxAmbig
                                            /\ ambig' = ambig + 1
                                            /\ reply' = [reply EXCEPT ![self] = "err_c"]
                                      /\ IF reply'[self] # "err_nc" /\ marker[jr[self]][cc[self]] = NoClaim
                                            THEN /\ marker' = [marker EXCEPT ![jr[self]][cc[self]] = self]
                                                 /\ seenEnds' = [seenEnds EXCEPT ![jr[self]][cc[self]] = Range(ends[jr[self]])
                                                                                                         \cup (IF creq[jr[self]] THEN {"cancelled"} ELSE {})]
                                            ELSE /\ TRUE
                                                 /\ UNCHANGED << marker, 
                                                                 seenEnds >>
                                      /\ IF reply'[self] # "ok"
                                            THEN /\ ret' = [ret EXCEPT ![self] = "error"]
                                                 /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                            ELSE /\ IF marker'[jr[self]][cc[self]] # self
                                                       THEN /\ ret' = [ret EXCEPT ![self] = "halt"]
                                                            /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                                       ELSE /\ IF CancelRule = "claim"
                                                                  THEN /\ pc' = [pc EXCEPT ![self] = "DPost"]
                                                                  ELSE /\ pc' = [pc EXCEPT ![self] = "DCall"]
                                                            /\ ret' = ret
                                      /\ UNCHANGED << result, cc >>
                /\ UNCHANGED << ends, approved, start, lims, creq, lease, job, 
                                jr, holds, outc, ctxDead, stalled, copt, eo, 
                                why, srun, sk, sseen, sround, sret, nsRep, 
                                listed, pri, cand, tk, fire, orun, ocall, cret, 
                                crashes, stalls, fired, fireAfterCancel, 
                                resumedEnded, recErr, crashLost, orphan, age, 
                                rolled, filterBroken, optBroken, overLimit, 
                                sOpen, nsCnt >>

DPost(self) == /\ pc[self] = "DPost"
               /\ IF CancelSeen(jr[self])
                     THEN /\ marker' = [marker EXCEPT ![jr[self]][cc[self]] = NoClaim]
                          /\ IF jr[self] \in SagaRuns
                                THEN /\ why' = [why EXCEPT ![self] = "cancel"]
                                     /\ pc' = [pc EXCEPT ![self] = "DRollback"]
                                     /\ ret' = ret
                                ELSE /\ ret' = [ret EXCEPT ![self] = CancelledVerdict(jr[self])]
                                     /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                     /\ why' = why
                     ELSE /\ pc' = [pc EXCEPT ![self] = "DCall"]
                          /\ UNCHANGED << marker, ret, why >>
               /\ UNCHANGED << result, ends, approved, start, lims, creq, 
                               lease, job, jr, holds, cc, outc, reply, ctxDead, 
                               stalled, copt, eo, srun, sk, sseen, sround, 
                               sret, nsRep, listed, pri, cand, tk, fire, orun, 
                               ocall, cret, ambig, crashes, stalls, fired, 
                               seenEnds, fireAfterCancel, resumedEnded, recErr, 
                               crashLost, orphan, age, rolled, filterBroken, 
                               optBroken, overLimit, sOpen, nsCnt >>

DCall(self) == /\ pc[self] = "DCall"
               /\ IF ctxDead[self]
                     THEN /\ marker' = [marker EXCEPT ![jr[self]][cc[self]] = NoClaim]
                          /\ ret' = [ret EXCEPT ![self] = "lost"]
                          /\ pc' = [pc EXCEPT ![self] = "DRel"]
                          /\ UNCHANGED << outc, fired, fireAfterCancel, 
                                          filterBroken, optBroken, overLimit >>
                     ELSE /\ fired' = [fired EXCEPT ![jr[self]][cc[self]] = fired[jr[self]][cc[self]] + 1]
                          /\ IF CancelSeen(jr[self])
                                THEN /\ fireAfterCancel' = TRUE
                                ELSE /\ TRUE
                                     /\ UNCHANGED fireAfterCancel
                          /\ filterBroken' = (filterBroken \/ cc[self] \notin start[jr[self]].f)
                          /\ optBroken' = (optBroken \/ cc[self] > eo[self].l)
                          /\ overLimit' = (overLimit \/ cc[self] > JLimit(jr[self]))
                          /\ \/ /\ outc' = [outc EXCEPT ![self] = "ok"]
                             \/ /\ jr[self] \in SagaRuns
                                /\ outc' = [outc EXCEPT ![self] = "fail"]
                          /\ pc' = [pc EXCEPT ![self] = "DRecord"]
                          /\ UNCHANGED << marker, ret >>
               /\ UNCHANGED << result, ends, approved, start, lims, creq, 
                               lease, job, jr, holds, cc, reply, ctxDead, 
                               stalled, copt, eo, why, srun, sk, sseen, sround, 
                               sret, nsRep, listed, pri, cand, tk, fire, orun, 
                               ocall, cret, ambig, crashes, stalls, seenEnds, 
                               resumedEnded, recErr, crashLost, orphan, age, 
                               rolled, sOpen, nsCnt >>

DRecord(self) == /\ pc[self] = "DRecord"
                 /\ IF Bug = "RecordUnderCtx" /\ ctxDead[self]
                       THEN /\ ret' = [ret EXCEPT ![self] = "lost"]
                            /\ pc' = [pc EXCEPT ![self] = "DRel"]
                            /\ UNCHANGED << result, cc, reply, ambig, recErr >>
                       ELSE /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                  /\ ambig' = ambig
                               \/ /\ ambig < MaxAmbig
                                  /\ ambig' = ambig + 1
                                  /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                               \/ /\ ambig < MaxAmbig
                                  /\ ambig' = ambig + 1
                                  /\ reply' = [reply EXCEPT ![self] = "err_c"]
                            /\ IF reply'[self] # "err_nc" /\ result[jr[self]][cc[self]] = "none"
                                  THEN /\ result' = [result EXCEPT ![jr[self]][cc[self]] = outc[self]]
                                  ELSE /\ TRUE
                                       /\ UNCHANGED result
                            /\ IF reply'[self] # "ok"
                                  THEN /\ recErr' = [recErr EXCEPT ![jr[self]][cc[self]] = TRUE]
                                       /\ ret' = [ret EXCEPT ![self] = "error"]
                                       /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                       /\ cc' = cc
                                  ELSE /\ IF result'[jr[self]][cc[self]] = "fail"
                                             THEN /\ pc' = [pc EXCEPT ![self] = "DRollback"]
                                                  /\ cc' = cc
                                             ELSE /\ IF cc[self] < NCalls
                                                        THEN /\ cc' = [cc EXCEPT ![self] = cc[self] + 1]
                                                             /\ pc' = [pc EXCEPT ![self] = "DTurn"]
                                                        ELSE /\ pc' = [pc EXCEPT ![self] = "DComplete"]
                                                             /\ cc' = cc
                                       /\ UNCHANGED << ret, recErr >>
                 /\ UNCHANGED << marker, ends, approved, start, lims, creq, 
                                 lease, job, jr, holds, outc, ctxDead, stalled, 
                                 copt, eo, why, srun, sk, sseen, sround, sret, 
                                 nsRep, listed, pri, cand, tk, fire, orun, 
                                 ocall, cret, crashes, stalls, fired, seenEnds, 
                                 fireAfterCancel, resumedEnded, crashLost, 
                                 orphan, age, rolled, filterBroken, optBroken, 
                                 overLimit, sOpen, nsCnt >>

DRollback(self) == /\ pc[self] = "DRollback"
                   /\ IF ctxDead[self]
                         THEN /\ ret' = [ret EXCEPT ![self] = "lost"]
                              /\ pc' = [pc EXCEPT ![self] = "DRel"]
                              /\ UNCHANGED rolled
                         ELSE /\ rolled' = [rolled EXCEPT ![jr[self]] = TRUE]
                              /\ pc' = [pc EXCEPT ![self] = "DAbort"]
                              /\ ret' = ret
                   /\ UNCHANGED << marker, result, ends, approved, start, lims, 
                                   creq, lease, job, jr, holds, cc, outc, 
                                   reply, ctxDead, stalled, copt, eo, why, 
                                   srun, sk, sseen, sround, sret, nsRep, 
                                   listed, pri, cand, tk, fire, orun, ocall, 
                                   cret, ambig, crashes, stalls, fired, 
                                   seenEnds, fireAfterCancel, resumedEnded, 
                                   recErr, crashLost, orphan, age, 
                                   filterBroken, optBroken, overLimit, sOpen, 
                                   nsCnt >>

DAbort(self) == /\ pc[self] = "DAbort"
                /\ IF Bug = "NoAbortMarker"
                      THEN /\ ret' = [ret EXCEPT ![self] = "aborted"]
                           /\ pc' = [pc EXCEPT ![self] = "DRel"]
                           /\ UNCHANGED << ends, reply, ambig >>
                      ELSE /\ IF ctxDead[self]
                                 THEN /\ ret' = [ret EXCEPT ![self] = "lost"]
                                      /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                      /\ UNCHANGED << ends, reply, ambig >>
                                 ELSE /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                            /\ ambig' = ambig
                                         \/ /\ ambig < MaxAmbig
                                            /\ ambig' = ambig + 1
                                            /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                         \/ /\ ambig < MaxAmbig
                                            /\ ambig' = ambig + 1
                                            /\ reply' = [reply EXCEPT ![self] = "err_c"]
                                      /\ IF reply'[self] # "err_nc" /\ AbortKind(self) \notin Range(ends[jr[self]])
                                            THEN /\ ends' = [ends EXCEPT ![jr[self]] = Append(ends[jr[self]], AbortKind(self))]
                                            ELSE /\ TRUE
                                                 /\ ends' = ends
                                      /\ IF reply'[self] # "ok"
                                            THEN /\ ret' = [ret EXCEPT ![self] = "error"]
                                                 /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                            ELSE /\ IF VerdictRule = "first"
                                                       THEN /\ pc' = [pc EXCEPT ![self] = "DVerdict"]
                                                            /\ ret' = ret
                                                       ELSE /\ ret' = [ret EXCEPT ![self] = AbortKind(self)]
                                                            /\ pc' = [pc EXCEPT ![self] = "DRel"]
                /\ UNCHANGED << marker, result, approved, start, lims, creq, 
                                lease, job, jr, holds, cc, outc, ctxDead, 
                                stalled, copt, eo, why, srun, sk, sseen, 
                                sround, sret, nsRep, listed, pri, cand, tk, 
                                fire, orun, ocall, cret, crashes, stalls, 
                                fired, seenEnds, fireAfterCancel, resumedEnded, 
                                recErr, crashLost, orphan, age, rolled, 
                                filterBroken, optBroken, overLimit, sOpen, 
                                nsCnt >>

DComplete(self) == /\ pc[self] = "DComplete"
                   /\ IF ctxDead[self]
                         THEN /\ ret' = [ret EXCEPT ![self] = "lost"]
                              /\ pc' = [pc EXCEPT ![self] = "DRel"]
                              /\ UNCHANGED << ends, reply, ambig >>
                         ELSE /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                    /\ ambig' = ambig
                                 \/ /\ ambig < MaxAmbig
                                    /\ ambig' = ambig + 1
                                    /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                 \/ /\ ambig < MaxAmbig
                                    /\ ambig' = ambig + 1
                                    /\ reply' = [reply EXCEPT ![self] = "err_c"]
                              /\ IF reply'[self] # "err_nc" /\ "complete" \notin Range(ends[jr[self]])
                                    THEN /\ ends' = [ends EXCEPT ![jr[self]] = Append(ends[jr[self]], "complete")]
                                    ELSE /\ TRUE
                                         /\ ends' = ends
                              /\ IF reply'[self] # "ok"
                                    THEN /\ ret' = [ret EXCEPT ![self] = "error"]
                                         /\ pc' = [pc EXCEPT ![self] = "DRel"]
                                    ELSE /\ IF VerdictRule = "first"
                                               THEN /\ pc' = [pc EXCEPT ![self] = "DVerdict"]
                                                    /\ ret' = ret
                                               ELSE /\ ret' = [ret EXCEPT ![self] = "complete"]
                                                    /\ pc' = [pc EXCEPT ![self] = "DRel"]
                   /\ UNCHANGED << marker, result, approved, start, lims, creq, 
                                   lease, job, jr, holds, cc, outc, ctxDead, 
                                   stalled, copt, eo, why, srun, sk, sseen, 
                                   sround, sret, nsRep, listed, pri, cand, tk, 
                                   fire, orun, ocall, cret, crashes, stalls, 
                                   fired, seenEnds, fireAfterCancel, 
                                   resumedEnded, recErr, crashLost, orphan, 
                                   age, rolled, filterBroken, optBroken, 
                                   overLimit, sOpen, nsCnt >>

DVerdict(self) == /\ pc[self] = "DVerdict"
                  /\ ret' = [ret EXCEPT ![self] = Head(ends[jr[self]])]
                  /\ pc' = [pc EXCEPT ![self] = "DRel"]
                  /\ UNCHANGED << marker, result, ends, approved, start, lims, 
                                  creq, lease, job, jr, holds, cc, outc, reply, 
                                  ctxDead, stalled, copt, eo, why, srun, sk, 
                                  sseen, sround, sret, nsRep, listed, pri, 
                                  cand, tk, fire, orun, ocall, cret, ambig, 
                                  crashes, stalls, fired, seenEnds, 
                                  fireAfterCancel, resumedEnded, recErr, 
                                  crashLost, orphan, age, rolled, filterBroken, 
                                  optBroken, overLimit, sOpen, nsCnt >>

DRel(self) == /\ pc[self] = "DRel"
              /\ IF holds[self] # 0 /\ lease[holds[self]].o = Tok(self)
                    THEN /\ lease' = [lease EXCEPT ![holds[self]] = NoLease]
                    ELSE /\ TRUE
                         /\ lease' = lease
              /\ holds' = [holds EXCEPT ![self] = 0]
              /\ job' = [job EXCEPT ![self] = 0]
              /\ IF self \in {Prim, Res}
                    THEN /\ pc' = [pc EXCEPT ![self] = "Done"]
                    ELSE /\ pc' = [pc EXCEPT ![self] = "DIdle"]
              /\ UNCHANGED << marker, result, ends, approved, start, lims, 
                              creq, jr, cc, outc, ret, reply, ctxDead, stalled, 
                              copt, eo, why, srun, sk, sseen, sround, sret, 
                              nsRep, listed, pri, cand, tk, fire, orun, ocall, 
                              cret, ambig, crashes, stalls, fired, seenEnds, 
                              fireAfterCancel, resumedEnded, recErr, crashLost, 
                              orphan, age, rolled, filterBroken, optBroken, 
                              overLimit, sOpen, nsCnt >>

drv(self) == DIdle(self) \/ DCheck(self) \/ DResume(self) \/ DOpen(self)
                \/ DStart(self) \/ DAmend(self) \/ DTurn(self)
                \/ DClaim(self) \/ DPost(self) \/ DCall(self)
                \/ DRecord(self) \/ DRollback(self) \/ DAbort(self)
                \/ DComplete(self) \/ DVerdict(self) \/ DRel(self)

PList(self) == /\ pc[self] = "PList"
               /\ IF self[1] = "tkp"
                     THEN /\ listed' = [listed EXCEPT ![self] = {r \in Runs : Lapsed(r) /\ ~Ended(r)}]
                     ELSE /\ listed' = [listed EXCEPT ![self] = {r \in Runs : ~Ended(r)}]
               /\ pri' = [pri EXCEPT ![self] = IF PassRule = "lapsedFirst" THEN {r \in Runs : Lapsed(r) /\ ~Ended(r)} ELSE {}]
               /\ pc' = [pc EXCEPT ![self] = "PNext"]
               /\ UNCHANGED << marker, result, ends, approved, start, lims, 
                               creq, lease, job, jr, holds, cc, outc, ret, 
                               reply, ctxDead, stalled, copt, eo, why, srun, 
                               sk, sseen, sround, sret, nsRep, cand, tk, fire, 
                               orun, ocall, cret, ambig, crashes, stalls, 
                               fired, seenEnds, fireAfterCancel, resumedEnded, 
                               recErr, crashLost, orphan, age, rolled, 
                               filterBroken, optBroken, overLimit, sOpen, 
                               nsCnt >>

PNext(self) == /\ pc[self] = "PNext"
               /\ IF listed[self] = {}
                     THEN /\ pc' = [pc EXCEPT ![self] = "PWait"]
                          /\ UNCHANGED << listed, cand >>
                     ELSE /\ cand' = [cand EXCEPT ![self] = IF pri[self] \cap listed[self] # {} THEN Min(pri[self] \cap listed[self])
                                                            ELSE Min(listed[self])]
                          /\ listed' = [listed EXCEPT ![self] = listed[self] \ {cand'[self]}]
                          /\ IF InFlight(self, cand'[self])
                                THEN /\ pc' = [pc EXCEPT ![self] = "PNext"]
                                ELSE /\ pc' = [pc EXCEPT ![self] = "PSlot"]
               /\ UNCHANGED << marker, result, ends, approved, start, lims, 
                               creq, lease, job, jr, holds, cc, outc, ret, 
                               reply, ctxDead, stalled, copt, eo, why, srun, 
                               sk, sseen, sround, sret, nsRep, pri, tk, fire, 
                               orun, ocall, cret, ambig, crashes, stalls, 
                               fired, seenEnds, fireAfterCancel, resumedEnded, 
                               recErr, crashLost, orphan, age, rolled, 
                               filterBroken, optBroken, overLimit, sOpen, 
                               nsCnt >>

PSlot(self) == /\ pc[self] = "PSlot"
               /\ IF Bug = "SkipWhenBusy" /\ job[SlotOf(self)] # 0
                     THEN /\ pc' = [pc EXCEPT ![self] = "PWait"]
                          /\ job' = job
                     ELSE /\ job[SlotOf(self)] = 0
                          /\ job' = [job EXCEPT ![SlotOf(self)] = cand[self]]
                          /\ pc' = [pc EXCEPT ![self] = "PNext"]
               /\ UNCHANGED << marker, result, ends, approved, start, lims, 
                               creq, lease, jr, holds, cc, outc, ret, reply, 
                               ctxDead, stalled, copt, eo, why, srun, sk, 
                               sseen, sround, sret, nsRep, listed, pri, cand, 
                               tk, fire, orun, ocall, cret, ambig, crashes, 
                               stalls, fired, seenEnds, fireAfterCancel, 
                               resumedEnded, recErr, crashLost, orphan, age, 
                               rolled, filterBroken, optBroken, overLimit, 
                               sOpen, nsCnt >>

PWait(self) == /\ pc[self] = "PWait"
               /\ IF ~Loop
                     THEN /\ pc' = [pc EXCEPT ![self] = "Done"]
                          /\ fire' = fire
                     ELSE /\ fire[self]
                          /\ fire' = [fire EXCEPT ![self] = FALSE]
                          /\ pc' = [pc EXCEPT ![self] = "PList"]
               /\ UNCHANGED << marker, result, ends, approved, start, lims, 
                               creq, lease, job, jr, holds, cc, outc, ret, 
                               reply, ctxDead, stalled, copt, eo, why, srun, 
                               sk, sseen, sround, sret, nsRep, listed, pri, 
                               cand, tk, orun, ocall, cret, ambig, crashes, 
                               stalls, fired, seenEnds, fireAfterCancel, 
                               resumedEnded, recErr, crashLost, orphan, age, 
                               rolled, filterBroken, optBroken, overLimit, 
                               sOpen, nsCnt >>

pass(self) == PList(self) \/ PNext(self) \/ PSlot(self) \/ PWait(self)

OPick == /\ pc[Op] = "OPick"
         /\ Resolver
         /\ \/ /\ \E x \in {<<r, c>> \in Runs \X Calls : Live(r, c)}:
                    /\ orun' = x[1]
                    /\ ocall' = x[2]
               /\ pc' = [pc EXCEPT ![Op] = "OLease"]
               /\ UNCHANGED approved
            \/ /\ \E r \in {r \in Paused0 : ~approved[r]}:
                    approved' = [approved EXCEPT ![r] = TRUE]
               /\ pc' = [pc EXCEPT ![Op] = "OPick"]
               /\ UNCHANGED <<orun, ocall>>
         /\ UNCHANGED << marker, result, ends, start, lims, creq, lease, job, 
                         jr, holds, cc, outc, ret, reply, ctxDead, stalled, 
                         copt, eo, why, srun, sk, sseen, sround, sret, nsRep, 
                         listed, pri, cand, tk, fire, cret, ambig, crashes, 
                         stalls, fired, seenEnds, fireAfterCancel, 
                         resumedEnded, recErr, crashLost, orphan, age, rolled, 
                         filterBroken, optBroken, overLimit, sOpen, nsCnt >>

OLease == /\ pc[Op] = "OLease"
          /\ IF LeaseFree(orun, Op)
                THEN /\ lease' = [lease EXCEPT ![orun] = [o |-> Op, left |-> TTL]]
                     /\ holds' = [holds EXCEPT ![Op] = orun]
                     /\ pc' = [pc EXCEPT ![Op] = "OWrite"]
                ELSE /\ pc' = [pc EXCEPT ![Op] = "OPick"]
                     /\ UNCHANGED << lease, holds >>
          /\ UNCHANGED << marker, result, ends, approved, start, lims, creq, 
                          job, jr, cc, outc, ret, reply, ctxDead, stalled, 
                          copt, eo, why, srun, sk, sseen, sround, sret, nsRep, 
                          listed, pri, cand, tk, fire, orun, ocall, cret, 
                          ambig, crashes, stalls, fired, seenEnds, 
                          fireAfterCancel, resumedEnded, recErr, crashLost, 
                          orphan, age, rolled, filterBroken, optBroken, 
                          overLimit, sOpen, nsCnt >>

OWrite == /\ pc[Op] = "OWrite"
          /\ \/ /\ reply' = [reply EXCEPT ![Op] = "ok"]
                /\ ambig' = ambig
             \/ /\ ambig < MaxAmbig
                /\ ambig' = ambig + 1
                /\ reply' = [reply EXCEPT ![Op] = "err_nc"]
             \/ /\ ambig < MaxAmbig
                /\ ambig' = ambig + 1
                /\ reply' = [reply EXCEPT ![Op] = "err_c"]
          /\ IF reply'[Op] # "err_nc" /\ result[orun][ocall] = "none"
                THEN /\ result' = [result EXCEPT ![orun][ocall] = "resolved"]
                ELSE /\ TRUE
                     /\ UNCHANGED result
          /\ pc' = [pc EXCEPT ![Op] = "ORel"]
          /\ UNCHANGED << marker, ends, approved, start, lims, creq, lease, 
                          job, jr, holds, cc, outc, ret, ctxDead, stalled, 
                          copt, eo, why, srun, sk, sseen, sround, sret, nsRep, 
                          listed, pri, cand, tk, fire, orun, ocall, cret, 
                          crashes, stalls, fired, seenEnds, fireAfterCancel, 
                          resumedEnded, recErr, crashLost, orphan, age, rolled, 
                          filterBroken, optBroken, overLimit, sOpen, nsCnt >>

ORel == /\ pc[Op] = "ORel"
        /\ IF lease[orun].o = Op
              THEN /\ lease' = [lease EXCEPT ![orun] = NoLease]
              ELSE /\ TRUE
                   /\ lease' = lease
        /\ holds' = [holds EXCEPT ![Op] = 0]
        /\ pc' = [pc EXCEPT ![Op] = "OPick"]
        /\ UNCHANGED << marker, result, ends, approved, start, lims, creq, job, 
                        jr, cc, outc, ret, reply, ctxDead, stalled, copt, eo, 
                        why, srun, sk, sseen, sround, sret, nsRep, listed, pri, 
                        cand, tk, fire, orun, ocall, cret, ambig, crashes, 
                        stalls, fired, seenEnds, fireAfterCancel, resumedEnded, 
                        recErr, crashLost, orphan, age, rolled, filterBroken, 
                        optBroken, overLimit, sOpen, nsCnt >>

resolveOp == OPick \/ OLease \/ OWrite \/ ORel

CGet == /\ pc[Canc] = "CGet"
        /\ Canceller
        /\ IF Ended(PrimRun)
              THEN /\ cret' = "over"
                   /\ pc' = [pc EXCEPT ![Canc] = "Done"]
              ELSE /\ IF Api.sagaCancel = "request" /\ start[PrimRun] = NoStart
                         THEN /\ cret' = "notstarted"
                              /\ pc' = [pc EXCEPT ![Canc] = "Done"]
                         ELSE /\ IF Api.sagaCancel = "request" /\ PrimRun \in SagaRuns
                                    THEN /\ pc' = [pc EXCEPT ![Canc] = "CReq"]
                                    ELSE /\ pc' = [pc EXCEPT ![Canc] = "CIns"]
                              /\ cret' = cret
        /\ UNCHANGED << marker, result, ends, approved, start, lims, creq, 
                        lease, job, jr, holds, cc, outc, ret, reply, ctxDead, 
                        stalled, copt, eo, why, srun, sk, sseen, sround, sret, 
                        nsRep, listed, pri, cand, tk, fire, orun, ocall, ambig, 
                        crashes, stalls, fired, seenEnds, fireAfterCancel, 
                        resumedEnded, recErr, crashLost, orphan, age, rolled, 
                        filterBroken, optBroken, overLimit, sOpen, nsCnt >>

CIns == /\ pc[Canc] = "CIns"
        /\ \/ /\ reply' = [reply EXCEPT ![Canc] = "ok"]
              /\ ambig' = ambig
           \/ /\ ambig < MaxAmbig
              /\ ambig' = ambig + 1
              /\ reply' = [reply EXCEPT ![Canc] = "err_nc"]
           \/ /\ ambig < MaxAmbig
              /\ ambig' = ambig + 1
              /\ reply' = [reply EXCEPT ![Canc] = "err_c"]
        /\ IF reply'[Canc] # "err_nc" /\ "cancelled" \notin Range(ends[PrimRun])
              THEN /\ ends' = [ends EXCEPT ![PrimRun] = Append(ends[PrimRun], "cancelled")]
              ELSE /\ TRUE
                   /\ ends' = ends
        /\ IF reply'[Canc] # "ok"
              THEN /\ cret' = "error"
                   /\ pc' = [pc EXCEPT ![Canc] = "Done"]
              ELSE /\ IF VerdictRule = "first"
                         THEN /\ pc' = [pc EXCEPT ![Canc] = "CRead"]
                              /\ cret' = cret
                         ELSE /\ cret' = "cancelled"
                              /\ pc' = [pc EXCEPT ![Canc] = "Done"]
        /\ UNCHANGED << marker, result, approved, start, lims, creq, lease, 
                        job, jr, holds, cc, outc, ret, ctxDead, stalled, copt, 
                        eo, why, srun, sk, sseen, sround, sret, nsRep, listed, 
                        pri, cand, tk, fire, orun, ocall, crashes, stalls, 
                        fired, seenEnds, fireAfterCancel, resumedEnded, recErr, 
                        crashLost, orphan, age, rolled, filterBroken, 
                        optBroken, overLimit, sOpen, nsCnt >>

CRead == /\ pc[Canc] = "CRead"
         /\ cret' = Head(ends[PrimRun])
         /\ pc' = [pc EXCEPT ![Canc] = "Done"]
         /\ UNCHANGED << marker, result, ends, approved, start, lims, creq, 
                         lease, job, jr, holds, cc, outc, ret, reply, ctxDead, 
                         stalled, copt, eo, why, srun, sk, sseen, sround, sret, 
                         nsRep, listed, pri, cand, tk, fire, orun, ocall, 
                         ambig, crashes, stalls, fired, seenEnds, 
                         fireAfterCancel, resumedEnded, recErr, crashLost, 
                         orphan, age, rolled, filterBroken, optBroken, 
                         overLimit, sOpen, nsCnt >>

CReq == /\ pc[Canc] = "CReq"
        /\ \/ /\ reply' = [reply EXCEPT ![Canc] = "ok"]
              /\ ambig' = ambig
           \/ /\ ambig < MaxAmbig
              /\ ambig' = ambig + 1
              /\ reply' = [reply EXCEPT ![Canc] = "err_nc"]
           \/ /\ ambig < MaxAmbig
              /\ ambig' = ambig + 1
              /\ reply' = [reply EXCEPT ![Canc] = "err_c"]
        /\ IF reply'[Canc] # "err_nc"
              THEN /\ creq' = [creq EXCEPT ![PrimRun] = TRUE]
              ELSE /\ TRUE
                   /\ creq' = creq
        /\ IF reply'[Canc] = "ok"
              THEN /\ cret' = "requested"
              ELSE /\ cret' = "error"
        /\ pc' = [pc EXCEPT ![Canc] = "Done"]
        /\ UNCHANGED << marker, result, ends, approved, start, lims, lease, 
                        job, jr, holds, cc, outc, ret, ctxDead, stalled, copt, 
                        eo, why, srun, sk, sseen, sround, sret, nsRep, listed, 
                        pri, cand, tk, fire, orun, ocall, crashes, stalls, 
                        fired, seenEnds, fireAfterCancel, resumedEnded, recErr, 
                        crashLost, orphan, age, rolled, filterBroken, 
                        optBroken, overLimit, sOpen, nsCnt >>

cancelOp == CGet \/ CIns \/ CRead \/ CReq

SPick == /\ pc[St] = "SPick"
         /\ Api.status # "off"
         /\ \E r \in Runs:
              srun' = r
         /\ pc' = [pc EXCEPT ![St] = "SStart"]
         /\ UNCHANGED << marker, result, ends, approved, start, lims, creq, 
                         lease, job, jr, holds, cc, outc, ret, reply, ctxDead, 
                         stalled, copt, eo, why, sk, sseen, sround, sret, 
                         nsRep, listed, pri, cand, tk, fire, orun, ocall, cret, 
                         ambig, crashes, stalls, fired, seenEnds, 
                         fireAfterCancel, resumedEnded, recErr, crashLost, 
                         orphan, age, rolled, filterBroken, optBroken, 
                         overLimit, sOpen, nsCnt >>

SStart == /\ pc[St] = "SStart"
          /\ IF start[srun] = NoStart
                THEN /\ sret' = "notstarted"
                     /\ pc' = [pc EXCEPT ![St] = "Done"]
                     /\ sOpen' = sOpen
                ELSE /\ IF Api.status = "load"
                           THEN /\ sOpen' = ~Ended(srun)
                                /\ IF Ended(srun)
                                      THEN /\ sret' = Head(ends[srun])
                                      ELSE /\ sret' = "started"
                                /\ pc' = [pc EXCEPT ![St] = "Done"]
                           ELSE /\ pc' = [pc EXCEPT ![St] = "SGet"]
                                /\ UNCHANGED << sret, sOpen >>
          /\ UNCHANGED << marker, result, ends, approved, start, lims, creq, 
                          lease, job, jr, holds, cc, outc, ret, reply, ctxDead, 
                          stalled, copt, eo, why, srun, sk, sseen, sround, 
                          nsRep, listed, pri, cand, tk, fire, orun, ocall, 
                          cret, ambig, crashes, stalls, fired, seenEnds, 
                          fireAfterCancel, resumedEnded, recErr, crashLost, 
                          orphan, age, rolled, filterBroken, optBroken, 
                          overLimit, nsCnt >>

SGet == /\ pc[St] = "SGet"
        /\ IF sround = 1 /\ sk = 1
              THEN /\ sOpen' = ~Ended(srun)
              ELSE /\ TRUE
                   /\ sOpen' = sOpen
        /\ IF sk < 3
              THEN /\ sseen' = Seen1
                   /\ sk' = sk + 1
                   /\ pc' = [pc EXCEPT ![St] = "SGet"]
                   /\ UNCHANGED << sround, sret >>
              ELSE /\ IF sround = 1 /\ Seen1 # {} /\ Api.status = "regets"
                         THEN /\ sround' = 2
                              /\ sk' = 1
                              /\ sseen' = {}
                              /\ pc' = [pc EXCEPT ![St] = "SGet"]
                              /\ sret' = sret
                         ELSE /\ sret' = StatusOf(Seen1)
                              /\ pc' = [pc EXCEPT ![St] = "Done"]
                              /\ UNCHANGED << sk, sseen, sround >>
        /\ UNCHANGED << marker, result, ends, approved, start, lims, creq, 
                        lease, job, jr, holds, cc, outc, ret, reply, ctxDead, 
                        stalled, copt, eo, why, srun, nsRep, listed, pri, cand, 
                        tk, fire, orun, ocall, cret, ambig, crashes, stalls, 
                        fired, seenEnds, fireAfterCancel, resumedEnded, recErr, 
                        crashLost, orphan, age, rolled, filterBroken, 
                        optBroken, overLimit, nsCnt >>

statOp == SPick \/ SStart \/ SGet

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == resolveOp \/ cancelOp \/ statOp
           \/ (\E self \in Drivers: drv(self))
           \/ (\E self \in PassProcs: pass(self))
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ \A self \in Drivers : WF_vars(drv(self))
        /\ \A self \in PassProcs : WF_vars(pass(self))

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION
-----------------------------------------------------------------------------

VARIABLE spent   \* Timed: the processes that took their step in this tick

\* The variables P14's extension added; no fault or tick changes them but a crash's.
p14Vars == <<start, lims, creq, copt, eo, why, srun, sk, sseen, sround, sret, rolled,
             filterBroken, optBroken, overLimit, sOpen>>

allVars == <<vars, spent>>
TimedProcs == Drivers \cup PassProcs

Step(p) == IF p \in Drivers THEN drv(p) ELSE pass(p)

\* A process may not let a tick pass while it can step (Timed): every process that can step has
\* stepped in this tick.
Settled == \A p \in TimedProcs : spent[p] \/ (p \in Drivers /\ stalled[p]) \/ ~ENABLED Step(p)

\* A lease is renewed while its holder runs (driveWithRenew); a stalled or dead holder's lapses.
Renewed(r) == \E p \in Drivers \cup {Op} :
                /\ holds[p] = r /\ lease[r].o = Tok(p) /\ lease[r].left > 0
                /\ (p \in Drivers => ~stalled[p] /\ ~ctxDead[p])

\* A run whose lease holder died, no one holds its lease, and it is not over.
Orphaned(r) == orphan[r] /\ ~Ended(r) /\ (lease[r].o = NoOwner \/ lease[r].left = 0)

Tick ==
  /\ Timed => Settled
  /\ lease' = [r \in Runs |->
                 IF lease[r].o = NoOwner THEN lease[r]
                 ELSE IF Renewed(r) THEN [lease[r] EXCEPT !.left = TTL]
                 ELSE [lease[r] EXCEPT !.left = IF @ > 0 THEN @ - 1 ELSE 0]]
  /\ tk' = [q \in PassProcs |-> IF tk[q] <= 1 THEN Interval ELSE tk[q] - 1]
  /\ fire' = [q \in PassProcs |-> fire[q] \/ tk[q] <= 1]
  /\ age' = [r \in Runs |-> IF Orphaned(r) /\ age[r] <= Bound THEN age[r] + 1 ELSE age[r]]
  /\ spent' = [p \in TimedProcs |-> FALSE]
  /\ UNCHANGED <<marker, result, ends, approved, job, jr, holds, cc, outc, ret, reply, ctxDead,
                 stalled, listed, pri, cand, orun, ocall, cret, ambig, crashes, stalls, fired,
                 seenEnds, fireAfterCancel, resumedEnded, recErr, crashLost, orphan, pc,
                 p14Vars, nsRep, nsCnt>>

\* A process crash. A worker restarts at once (a new process: new lease tokens, an empty
\* in-flight set); the primary does not. Leases its drives held are not released: they lapse.
CrashOf(P, restart) ==
  LET D == P \cap Drivers IN
  /\ crashes < MaxCrash
  /\ \E p \in P : pc[p] # "Done"
  /\ crashes' = crashes + 1
  /\ pc' = [p \in DOMAIN pc |-> IF p \notin P THEN pc[p]
                                ELSE IF ~restart THEN "Done"
                                ELSE IF p \in Drivers THEN "DIdle" ELSE "PList"]
  /\ lease' = [r \in Runs |-> IF Bug # "SharedHolder" /\ \E d \in D : holds[d] = r /\ lease[r].o = Tok(d)
                              THEN [lease[r] EXCEPT !.o = Dead] ELSE lease[r]]
  /\ orphan' = [r \in Runs |-> orphan[r] \/ \E d \in D : holds[d] = r /\ LiveFor(d) /\ ~Ended(r)]
  /\ crashLost' = [r \in Runs |-> [c \in Calls |-> crashLost[r][c] \/
                     \E d \in D : jr[d] = r /\ cc[d] = c /\ pc[d] = "DRecord"]]
  /\ job' = [p \in Drivers |-> IF p \in D THEN 0 ELSE job[p]]
  /\ holds' = [p \in DOMAIN holds |-> IF p \in D THEN 0 ELSE holds[p]]
  /\ ctxDead' = [p \in Drivers |-> IF p \in D THEN FALSE ELSE ctxDead[p]]
  /\ stalled' = [p \in Drivers |-> IF p \in D THEN FALSE ELSE stalled[p]]
  /\ listed' = [q \in PassProcs |-> IF q \in P THEN {} ELSE listed[q]]
  /\ fire' = [q \in PassProcs |-> IF q \in P THEN TRUE ELSE fire[q]]
  /\ tk' = [q \in PassProcs |-> IF q \in P THEN Interval ELSE tk[q]]
  /\ UNCHANGED <<marker, result, ends, approved, jr, cc, outc, ret, reply, pri, cand, orun, ocall,
                 cret, ambig, stalls, fired, seenEnds, fireAfterCancel, resumedEnded, recErr, age,
                 spent, p14Vars>>
  \* A restarted worker is a new process: it has reported no run yet.
  /\ nsRep' = [w \in Workers |-> IF SweepP(w) \in P THEN {} ELSE nsRep[w]]
  /\ nsCnt' = [w \in Workers |-> IF SweepP(w) \in P THEN [r \in Runs |-> 0] ELSE nsCnt[w]]

Crash ==
  \/ "prim" \in Crashable /\ Primary # "none" /\ CrashOf({Prim}, FALSE)
  \/ "workers" \in Crashable /\ \E w \in Workers : CrashOf(WorkerProcs(w), TRUE)

\* A lease holder stalls (a GC pause, a suspended VM, a partition): it takes no step and renews
\* nothing until it wakes.
Stall(p) ==
  /\ stalls < MaxStall /\ holds[p] # 0 /\ ~stalled[p] /\ pc[p] \notin {"Done", "DIdle"}
  /\ stalls' = stalls + 1
  /\ stalled' = [stalled EXCEPT ![p] = TRUE]
  /\ UNCHANGED <<marker, result, ends, approved, lease, job, jr, holds, cc, outc, ret, reply,
                 ctxDead, listed, pri, cand, tk, fire, orun, ocall, cret, ambig, crashes, fired,
                 seenEnds, fireAfterCancel, resumedEnded, recErr, crashLost, orphan, age, pc,
                 spent, p14Vars, nsRep, nsCnt>>
Wake(p) ==
  /\ stalled[p]
  /\ stalled' = [stalled EXCEPT ![p] = FALSE]
  /\ UNCHANGED <<marker, result, ends, approved, lease, job, jr, holds, cc, outc, ret, reply,
                 ctxDead, listed, pri, cand, tk, fire, orun, ocall, cret, ambig, crashes, stalls,
                 fired, seenEnds, fireAfterCancel, resumedEnded, recErr, crashLost, orphan, age,
                 pc, spent, p14Vars, nsRep, nsCnt>>
\* The renewer finds the lease gone (or not renewed by the cutoff) and cancels the drive with
\* ErrLeaseLost. A holder that woke from a stall can take steps before this happens.
LeaseNotice(p) ==
  /\ holds[p] # 0 /\ ~stalled[p] /\ ~ctxDead[p] /\ ~LiveFor(p)
  /\ ctxDead' = [ctxDead EXCEPT ![p] = TRUE]
  /\ UNCHANGED <<marker, result, ends, approved, lease, job, jr, holds, cc, outc, ret, reply,
                 stalled, listed, pri, cand, tk, fire, orun, ocall, cret, ambig, crashes, stalls,
                 fired, seenEnds, fireAfterCancel, resumedEnded, recErr, crashLost, orphan, age,
                 pc, spent, p14Vars, nsRep, nsCnt>>

ProcStep(p) ==
  /\ p \in Drivers => ~stalled[p]
  /\ Timed => ~spent[p]
  /\ Step(p)
  /\ spent' = IF Timed THEN [spent EXCEPT ![p] = TRUE] ELSE spent

FullInit == Init /\ spent = [p \in TimedProcs |-> FALSE]
FullNext ==
  \/ \E p \in TimedProcs : ProcStep(p)
  \/ (resolveOp \/ cancelOp \/ statOp) /\ UNCHANGED spent
  \/ Tick
  \/ Crash
  \/ \E p \in Drivers : Stall(p) \/ Wake(p) \/ LeaseNotice(p)
FullSpec == FullInit /\ [][FullNext]_allVars
\* Liveness: every process step, the renewer and time are weakly fair; crashes, stalls, error
\* replies, the operator and Cancel are not.
LiveSpec == FullSpec /\ WF_allVars(Tick)
            /\ (\A p \in TimedProcs : WF_allVars(ProcStep(p)))
            /\ (\A p \in Drivers : WF_allVars(LeaseNotice(p)) /\ WF_allVars(Wake(p)))

(***************************************************************************)
(* Properties                                                               *)
(***************************************************************************)

\* #114: a recovery pass never calls resume for a run that holds an end marker.
NoResumeOfFinished == ~resumedEnded

\* At most one driver per lease epoch: no two drives hold a live lease on one run at once (each
\* acquisition of a free or lapsed lease is an epoch, owned by one drive's token).
OneDriverPerEpoch ==
  \A r \in Runs : Cardinality({p \in Drivers \cup {Op} : holds[p] = r /\ LiveFor(p)}) <= 1

\* At most one live driver: no two drives of one run are running with their contexts live.
\* A holder that stalled past its TTL breaks it (limits/stall-two-drivers): leases are not fenced.
Driving(p) == p \in Drivers /\ holds[p] # 0 /\ ~ctxDead[p] /\ ~stalled[p] /\ pc[p] \notin {"DIdle", "DRel", "Done"}
OneLiveDriver == \A r \in Runs : Cardinality({p \in Drivers : Driving(p) /\ holds[p] = r}) <= 1

\* No double completion: a run is never both completed and aborted, and each end marker is
\* written once (its key's first writer).
NoDoubleCompletion == \A r \in Runs : ~({"complete", "aborted"} \subseteq Range(ends[r]))
OneEnd == \A r \in Runs : Len(ends[r]) <= 1

\* A drive or Cancel that reports how a run ended agrees with the run's first end marker.
VerdictAgreement ==
  /\ \A p \in Drivers : ret[p] \in {"complete", "aborted", "cancelled"} =>
        Ended(jr[p]) /\ Head(ends[jr[p]]) = ret[p]
  /\ cret = "cancelled" => Head(ends[PrimRun]) = "cancelled"

\* A finished run is final: no effect fires under a claim won after run:complete or run:aborted.
FinishedFinal == \A r \in Runs, c \in Calls :
  fired[r][c] > 0 => seenEnds[r][c] \cap {"complete", "aborted"} = {}

\* Cancel is final (the property P14 must satisfy): no effect fires under a claim won after
\* run:cancelled (or a saga's rollback request) is in the journal, and no drive that read it
\* claims anything.
CancelFinal == \A r \in Runs, c \in Calls : fired[r][c] > 0 => "cancelled" \notin seenEnds[r][c]

\* The literal form: no effect is called once run:cancelled is in the journal. Not achievable by
\* any rule that lets a call already past its check finish (limits/cancel-in-flight).
NoFireAfterCancel == ~fireAfterCancel

\* At-most-once per call.
AtMostOnce == \A r \in Runs, c \in Calls : fired[r][c] <= 1

\* #31: a drive that reports the run over leaves its end marker.
EndMarked == \A p \in Drivers : ret[p] \in {"complete", "aborted"} => ret[p] \in Range(ends[jr[p]])

\* #58 finding 1: an effect whose driver returned and whose record write did not fail is recorded.
OutcomeRecorded == \A r \in Runs, c \in Calls :
  (fired[r][c] > 0 /\ result[r][c] = "none") =>
     \/ recErr[r][c] \/ crashLost[r][c]
     \/ \E p \in Drivers : jr[p] = r /\ cc[p] = c /\ pc[p] = "DRecord"

\* Bounded pickup: a run whose lease holder died is taken over within Bound ticks of its lease
\* lapsing (Timed configurations).
BoundedPickup == \A r \in Runs : age[r] <= Bound

\* Liveness: a run whose lease holder died is taken over (or ends).
PickedUp == \A r \in Runs : orphan[r] ~> (~orphan[r] \/ Ended(r))

(***************************************************************************)
(* P14's Run API                                                            *)
(***************************************************************************)

\* D8: Status agrees with the run's first end marker in journal order, and reports a run
\* started (not over) only if, at some instant during the call, it held run:start and no end
\* marker. The first end marker never changes once written (A2, A4), so a terminal answer that
\* matches it now matched it when it was given.
StatusTruthful ==
  /\ sret \in {"complete", "aborted", "cancelled"} => Ended(srun) /\ Head(ends[srun]) = sret
  /\ sret = "started" => sOpen

\* D3: no effect fires for a call whose tool is outside the run's journaled filter, whichever
\* drive runs it (a resume, a recovery drive) and wherever the call came from (a replayed turn).
FilterHonoured == ~filterBroken

\* B1, the journaling rule: every drive, a resume and a recovery drive included, runs under the
\* options journaled when it loaded the run (run:start's filter and settings, and its limit or
\* the last amendment's), and fires no effect past that limit.
RunOptionsDurable == ~optBroken

\* The literal form: no effect fires past the limit journaled at that moment. A drive already
\* running keeps the limit it loaded, so an amendment that lowers it binds the drives that load
\* the run after it (limits/limit-lowered-in-flight).
NoFireOverLimit == ~overLimit

\* Cancel on a saga rolls it back: a saga with a cancellation (run:cancelled or a rollback
\* request), an effect in place and no finished rollback, which did not complete first, is either
\* still listed by recovery (no end marker) or being driven.
NeedsRollback(r) ==
  /\ r \in SagaRuns /\ (creq[r] \/ "cancelled" \in Range(ends[r])) /\ ~rolled[r]
  /\ \E c \in Calls : fired[r][c] > 0 /\ result[r][c] # "fail"
  /\ ~(Ended(r) /\ Head(ends[r]) = "complete")
CancelRollsBack == \A r \in Runs :
  NeedsRollback(r) => ~Ended(r) \/ \E d \in Drivers : jr[d] = r /\ pc[d] \notin {"DIdle", "DRel", "Done"}

\* Recovery reports a run with no run:start at most once per worker process.
NotStartedOnce == \A w \in Workers, r \in Runs : nsCnt[w][r] <= 1

\* Liveness, for configurations with no pauses or limits: a started run ends, or halts for an
\* operator (an attempt whose outcome is unknown, with no drive running). Recovery must not skip
\* a run for good because it once had no run:start.
HaltedRun(r) == \E c \in Calls : Live(r, c) /\ ~\E d \in Drivers : jr[d] = r /\ pc[d] \notin {"DIdle", "Done"}
StartedRunSettles == \A r \in Runs : start[r] # NoStart ~> (Ended(r) \/ HaltedRun(r))

\* Vacuity, expected violated: an effect fires.
EffectNotReachable == \A r \in Runs, c \in Calls : fired[r][c] = 0
\* Vacuity for the pickup configurations: a dead holder's run is taken over.
PickupNotReachable == ~\E r \in Runs : orphan[r] /\ age[r] > 0
=============================================================================
