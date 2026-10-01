------------------------------ MODULE ToolCall ------------------------------
(***************************************************************************)
(* Model 9: the tool-call state machine of redesign P12 (#117). One turn's *)
(* tool calls run concurrently (an errgroup). Each call is claimed (a side *)
(* effect: an attempt marker), then its tool-middleware chain runs, and    *)
(* the chain's base handler is entered zero or more times: by the          *)
(* middleware calling next, retrying it, or leaving it running after the   *)
(* chain returned. Each chain has a call state, changed only by CAS        *)
(* (open, reached, refused, closed, refusedClosed), and a began word       *)
(* (none, yes, sealed). When the chain returns, the loop closes the call,  *)
(* seals the began word if nothing began, decides what the journal gets    *)
(* (a result, a known failure, a saga failure, nothing, a not-started      *)
(* record), and the errgroup holds or propagates the goroutine's error.    *)
(* Store writes can error and still commit (A3); a process can crash; the  *)
(* run is driven again until it completes or halts.                        *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS
  NCalls,        \* tool calls of the turn (1 or 2), run concurrently
  Kinds,         \* [c |-> "side" | "idem" | "deleg"]: a side effect (claimed, not retry-safe);
                 \* a retry-safe tool that changes state (Idempotent, not ReadOnly, a
                 \* Compensator), unclaimed; or a delegation (retry-safe, no effect of its own)
                 \* that refuses Unrecorded under the wrong authority
  MW,            \* [c |-> what c's tool middleware may do], see LMw
  Saga,          \* the run is a saga (RunSaga)
  Rewrite,       \* in a saga, call 1 is a Compensator whose middleware rewrites its arguments,
                 \* so the base handler journals them (journalAcceptedArgs)
  Timeout,       \* the tools have a ToolSpec.Timeout: callTool's context, cancelled on return
  MaxExtra,      \* invocations of next beyond each chain's first (retries, leaks, renamed calls,
                 \* direct calls), over the whole behaviour
  MaxErr,        \* store writes that return an error (committed or not)
  MaxCrash,      \* process crashes
  MaxCancel,     \* run cancellations
  MaxDeadline,   \* tool deadlines that expire
  MaxGuard,      \* CallGuard refusals (a delegated grant expired: a recorded refusal)
  MaxUnk,        \* tool calls that return ErrToolOutcomeUnknown
  MaxToolErr,    \* tool calls that fail with an error of their own, having done nothing
  MaxWrongAuth,  \* times the delegation's authority becomes wrong (an Unrecorded refusal)
  MaxAtt,        \* attempt bound per call
  Bugs           \* reverted rules, see regress/

Calls == 1..NCalls
Slots == 1..2
Invs == {10 * c + k : c \in Calls, k \in Slots}
CallOf(i) == i \div 10
Atts == 0..MaxAtt
NoRes == [k |-> "none", nc |-> FALSE, ran |-> FALSE, a |-> 0]
\* The goroutine errors the errgroup holds until every sibling is done (a pause or halt, an
\* Unrecorded refusal); every other error cancels the group's context.
HeldKinds(bugs) == IF "UnrecCancels" \in bugs THEN {"unk"} ELSE {"unrec", "unk"}

ASSUME Bugs \subseteq {"FlagRule", "RefusedNotTerminal", "RanEarly", "NoBeganWord", "UnrecCancels",
                       "NoToolOutcome", "RanBeforeArgs", "IdemSagaSkip",
                       "NoIdemBegan", "NoRunningCheck",
                       "NoRbUnknown", "RunningReachedOnly",
                       "NoClosedBegin"}
ASSUME NCalls \in 1..2 /\ MaxAtt >= 1

(* --algorithm toolcall
variables
  \* The journal: attempt markers, the result record, the saga's accepted arguments, run:complete.
  marker   = [c \in Calls |-> [a \in Atts |-> "none"]],
  att      = [c \in Calls |-> 0],
  res      = [c \in Calls |-> NoRes],
  argsRec  = [c \in Calls |-> FALSE],
  \* (T3's fix) in a saga, a retry-safe call that changes state journals that it began, before
  \* its tool's Call: the journal then knows an attempt may have taken effect
  ibeg     = [c \in Calls |-> FALSE],
  ibs      = [c \in Calls |-> FALSE],
  \* (T3) the chain's earlier flag: an invocation began after an earlier one that did not fail
  iear     = [c \in Calls |-> FALSE],
  complete = FALSE,
  \* The process: pendingClaims, an attempt whose not-started write failed (lost in a crash).
  pend     = [c \in Calls |-> 0],
  \* The drive: the run's context, the errgroup's, the goroutines, the group's errors.
  cancelled = FALSE,
  gcancel  = FALSE,
  auth     = "right",
  go       = [c \in Calls |-> FALSE],
  werr     = "none",
  held     = {},
  runRet   = "none",
  \* The current chain of each call: its state, began word, ran mark, and the tool's own outcome.
  st       = [c \in Calls |-> "open"],
  bg       = [c \in Calls |-> "none"],
  ranc     = [c \in Calls |-> FALSE],
  tout     = [c \in Calls |-> "none"],
  closedc  = [c \in Calls |-> FALSE],
  dl       = [c \in Calls |-> FALSE],
  \* Invocations of the base handler, two slots per call. A sync one is awaited by the
  \* middleware; an async one is left running. One that outlives its drive keeps a frozen view of
  \* its (terminal) chain.
  ion      = [i \in Invs |-> FALSE],
  iarg     = [i \in Invs |-> "ok"],
  iret     = [i \in Invs |-> ""],
  iatt     = [i \in Invs |-> 0],
  iold     = [i \in Invs |-> FALSE],
  ost      = [i \in Invs |-> "open"],
  obg      = [i \in Invs |-> "none"],
  oran     = [i \in Invs |-> FALSE],
  \* Ghosts: effects fired, per call and per attempt; attempts whose tool began; budgets.
  fired    = [c \in Calls |-> 0],
  firedAt  = [c \in Calls |-> [a \in Atts |-> FALSE]],
  began    = [c \in Calls |-> [a \in Atts |-> FALSE]],
  everReached = [c \in Calls |-> FALSE],
  lostAtt  = [c \in Calls |-> {}],
  rolled   = FALSE,
  \* The rollback: a call re-run through its chain by rollbackRun, and what that re-run returned;
  \* compensations recorded (memoized steps); a rollback that stopped at a halt.
  rbm      = [c \in Calls |-> FALSE],
  rbOut    = [c \in Calls |-> "err"],
  compd    = [c \in Calls |-> FALSE],
  rbHalt   = FALSE,
  \* the calls the rollback lists as unknown outcomes after re-running them (SagaAborted)
  rbUnk    = {},
  \* Ghost: the call's effect is in place (fired, and not compensated since).
  fx       = [c \in Calls |-> FALSE],
  boundHit = FALSE,
  extras = 0, errs = 0, crashes = 0, cancels = 0, deadlines = 0, guards = 0, unks = 0,
  toolErrs = 0, wrongs = 0;

define
  Side(c) == Kinds[c] = "side"
  Idem(c) == Kinds[c] = "idem"
  Bug(b) == b \in Bugs
  \* unprovenFailure: a chain error for a tool that began and did not itself fail leaves the
  \* outcome unknown for a side effect, and in a saga for a retry-safe tool that changes state
  \* (before #117's fix of it, IdemSagaSkip: for a side effect only, so the rollback skipped the
  \* step as one that made no change).
  Unproven(c) == Side(c) \/ (Idem(c) /\ Saga /\ ~Bug("IdemSagaSkip"))
  \* (T3) a known failure of a retry-safe saga step that changes state is a known failure only if
  \* no attempt of it began: an earlier attempt, cut off with nothing recorded (a retry-safe call
  \* has no attempt marker), may have taken effect. Before the fix (NoIdemBegan) it was skipped.
  \* The loop reads the journal its drive loaded (sagaStepMayHaveBegun over Agent.run's values), so
  \* the record counts when an earlier drive wrote it; the rollback reads it whenever written.
  IdemBegan(c) == Idem(c) /\ ibs[c] /\ ~Bug("NoIdemBegan")
  IdemMark(c) == Idem(c) /\ Saga /\ ~Bug("NoIdemBegan")
  \* The loop's contexts: sctx (the run's, the errgroup's) and tctx (the tool's timeout too).
  SDone == cancelled \/ gcancel
  TDone(c) == SDone \/ (Timeout /\ dl[c])
  \* An invocation's context: its chain's tctx; callTool cancels it on return (with a timeout), and
  \* the errgroup when the group ends.
  IDone(i) == iold[i] \/ SDone \/ (Timeout /\ (dl[CallOf(i)] \/ closedc[CallOf(i)]))
  ISt(i) == IF iold[i] THEN ost[i] ELSE st[CallOf(i)]
  IBg(i) == IF iold[i] THEN obg[i] ELSE bg[CallOf(i)]
  IRan(i) == IF iold[i] THEN oran[i] ELSE ranc[CallOf(i)]
  \* refuse: CAS open -> refused on the invocation's own chain.
  RefuseSt(i) == IF ~iold[i] /\ st[CallOf(i)] = "open"
                 THEN [st EXCEPT ![CallOf(i)] = "refused"] ELSE st
  \* enterTool: from open or refused, or a call already reached; never from a closed state.
  EnterOK(i) == IF Bug("FlagRule") THEN TRUE
                ELSE IF Bug("RefusedNotTerminal") THEN ISt(i) # "closed"
                ELSE ISt(i) \in {"open", "refused", "reached"}
  EnterSt(i) == IF iold[i] \/ Bug("FlagRule") THEN st ELSE [st EXCEPT ![CallOf(i)] = "reached"]
  Live(c) == att[c] > 0 /\ marker[c][att[c]] = "live"
  FreeSlots(c) == {i \in Invs : CallOf(i) = c /\ ~ion[i]}
  FreeSlot(c) == CHOOSE i \in FreeSlots(c) : \A j \in FreeSlots(c) : i <= j
  \* closeCall and the seal, from the state before the chain returned.
  CloseSt(c) == CASE st[c] = "open" -> "closed"
                  [] st[c] = "refused" /\ ~Bug("RefusedNotTerminal") -> "refusedClosed"
                  [] OTHER -> st[c]
  Seal(c) == ~Bug("NoBeganWord") /\ ~Bug("FlagRule") /\ CloseSt(c) = "reached" /\ bg[c] = "none"
  FinalSt(c) == IF Seal(c) THEN "refusedClosed" ELSE CloseSt(c)
  \* "Not called" needs positive proof: refusedClosed, or closed with ErrToolNotCalled. Round 1
  \* (FlagRule): the flag the base handler sets immediately before t.Call was not set.
  NotCalled(c, e) == IF Bug("FlagRule") THEN bg[c] # "yes"
                     ELSE \/ FinalSt(c) = "refusedClosed"
                          \/ FinalSt(c) = "refused"
                          \/ FinalSt(c) = "closed" /\ e = "nc"
  ReachedAt(c) == IF Bug("FlagRule") THEN bg[c] = "yes" ELSE FinalSt(c) = "reached"
  \* The tool is running: the process's in-flight count of the call (inflightAdd) is above zero.
  \* An invocation of the call, of this chain or of an earlier one in this process (a next left
  \* running past its drive), counts itself in at IBegin, in one step with the closed check (the
  \* chain reads the count after closeCall, so either it sees the count or the invocation sees
  \* the call closed), and out when t.Call returns. One word per chain would be overwritten by a
  \* sibling invocation's outcome, and would not see an earlier chain's.
  ToolRunning(c) == \E i \in Invs : CallOf(i) = c /\ ion[i] /\ pc[i] = "ICall"
  \* toolHandler's running: reached, and the count above zero; it makes a result unknown where
  \* unprovenFailure holds (a retry-safe tool outside a saga, or a ReadOnly one, may run again).
  \* (T6) whatever the chain's state: a cache answer of a later drive while an earlier drive's
  \* invocation is in the tool is no result either. Before the fix (RunningReachedOnly) the count
  \* was read only for a chain that reached the tool.
  RunOk(c) == ~Bug("NoRunningCheck") /\ Unproven(c) /\ ToolRunning(c)
              /\ (ReachedAt(c) \/ ~Bug("RunningReachedOnly"))
  Running(c) == ~Bug("NoRunningCheck") /\ ReachedAt(c) /\ ToolRunning(c)
  \* The chain's error class after the loop's rules: a closed call's error without the sentinel
  \* is an unknown outcome for a side effect, and so is any error, while the tool's context is not
  \* done, for a call whose tool began and did not itself fail: it is still running, it succeeded,
  \* its outcome is unknown, or it was cut off (model 9's T1; before it, NoToolOutcome). With the
  \* context done the rules below already record nothing.
  Cls(c, e) ==
    IF e = "ok" /\ RunOk(c) THEN "unk"
    ELSE IF e # "ok" /\ ~Bug("FlagRule") /\ FinalSt(c) = "closed" /\ ~NotCalled(c, e) /\ Side(c)
      THEN "unk"
    ELSE IF ~Bug("NoToolOutcome") /\ e # "ok" /\ ReachedAt(c) /\ bg[c] = "yes" /\ ~TDone(c)
            /\ \/ (tout[c] # "err" \/ Running(c)) /\ Unproven(c)
               \* (T3) a later invocation failed after an earlier one of the chain did not (earlier)
               \/ iear[c] /\ Idem(c) /\ Saga /\ ~Bug("NoIdemBegan")
      THEN "unk"
    ELSE e
  Late(c, e) == e # "ok" /\ ReachedAt(c) /\ TDone(c)
  \* A success the chain returned while its tool still runs (next left running, the call answered
  \* from a cache) is not the tool's: an unknown outcome (T4). Before the fix (NoRunningCheck) it
  \* was recorded, and in a saga compensated before the tool's effect landed.
  Ok(c, e) == e = "ok" /\ ~RunOk(c)
  \* A result of the rollback's re-run: the tool was reached (a cache answer says nothing of it).
  RbOk(c, e) == Ok(c, e) /\ ReachedAt(c)
  \* What recordFresh's function returns for the journal.
  RecOf(c, e) ==
    IF Ok(c, e) THEN "ok"
    ELSE IF SDone \/ Cls(c, e) = "unrec" THEN "none"
    \* (T3) a failed accepted-arguments write of a call never begun records nothing (argsJournalError)
    ELSE IF e = "argserr" /\ NotCalled(c, e) /\ ~Bug("NoIdemBegan") THEN "none"
    ELSE IF (Late(c, e) \/ Cls(c, e) = "unk") /\ Side(c) THEN "none"
    ELSE IF Saga THEN (IF Late(c, e) \/ Cls(c, e) = "unk" \/ IdemBegan(c) THEN "sagaUnk" ELSE "sagaFail")
    ELSE "fail"
  \* What the goroutine returns when nothing is recorded.
  GRetOf(c, e) ==
    IF SDone THEN (IF Cls(c, e) \in {"unrec", "unk"} THEN Cls(c, e) ELSE "cancel")
    ELSE IF Cls(c, e) = "unrec" THEN "unrec"
    ELSE IF e = "argserr" THEN "storage"
    ELSE "unk"
  AllRecorded == \A c \in Calls : res[c].k # "none"
  SagaFailed == Saga /\ \E c \in Calls : res[c].k \in {"sagaFail", "sagaUnk"}
  GateCalls == {c \in Calls : Side(c) /\ res[c].k = "none" /\ Live(c)}
end define;

macro Reply(r) begin
  either r := "ok";
  or await errs < MaxErr; errs := errs + 1; r := "err_nc";
  or await errs < MaxErr; errs := errs + 1; r := "err_c";
  end either;
end macro;

macro Spawn(k, a) begin
  ion[k] := TRUE; iarg[k] := a; iret[k] := ""; iatt[k] := att[self]; iold[k] := FALSE;
end macro;

\* The base handler (toolHandler's h), one invocation of it.
fair process inv \in Invs
variables r = "";
begin
IWait:
  await ion[self];
  r := "";
IEnter:
  \* The checks before the tool and enterTool, one step: a closed state is terminal, so a
  \* check that passes before the chain closes and an enterTool after it refuse alike.
  if iarg[self] = "renamed" then
    \* a call renamed or re-identified: refused, ErrConfig and ErrToolNotCalled
    st := RefuseSt(self); iret[self] := "nc"; ion[self] := FALSE;
    goto IWait;
  elsif Bug("RanEarly") then
    \* before round 4: the ran mark came first, then the arguments, then the checks
    if Side(CallOf(self)) /\ IRan(self) then
      iret[self] := "reinv"; ion[self] := FALSE;
      goto IWait;
    else
      ranc := IF iold[self] THEN ranc ELSE [ranc EXCEPT ![CallOf(self)] = TRUE];
      goto IArgs;
    end if;
  elsif IDone(self) then
    \* ctxDone: the deadline passed in the middleware, or the run or group was cancelled
    st := RefuseSt(self); iret[self] := "nc"; ion[self] := FALSE;
    goto IWait;
  else
    either
      \* toolhook.CallGuard: the delegated grant expired; a recorded refusal
      await guards < MaxGuard;
      guards := guards + 1;
      st := RefuseSt(self); iret[self] := "nc"; ion[self] := FALSE;
      goto IWait;
    or
      if ~EnterOK(self) then
        \* the chain already returned: never reaches the tool
        iret[self] := "nc"; ion[self] := FALSE;
        goto IWait;
      else
        st := EnterSt(self);
        everReached[CallOf(self)] := TRUE;
        if Side(CallOf(self)) /\ IRan(self) /\ Bug("RanBeforeArgs") then
          \* (T2) ran.LoadOrStore after enterTool, before the arguments: "already ran"
          iret[self] := "reinv"; ion[self] := FALSE;
          goto IWait;
        else
          ranc := IF iold[self] \/ ~Side(CallOf(self)) \/ ~Bug("RanBeforeArgs") THEN ranc
                  ELSE [ranc EXCEPT ![CallOf(self)] = TRUE];
        end if;
      end if;
    end either;
  end if;
IArgs:
  \* journalAcceptedArgs: store.Do of the arguments the call accepted (a saga Compensator whose
  \* middleware rewrote them). An errored write may have committed (A3); a write whose context
  \* is done fails, committed or not. A failure returns before the tool's Call began.
  if Saga /\ Rewrite /\ CallOf(self) = 1 then
    either
      await ~IDone(self);
      argsRec[1] := TRUE; ibeg[1] := IdemMark(1); r := "ok";
    or
      await errs < MaxErr \/ IDone(self);
      errs := IF IDone(self) THEN errs ELSE errs + 1;
      either argsRec[1] := TRUE; ibeg[1] := IdemMark(1); or skip; end either;
      r := "err";
    end either;
    if r = "err" then
      if Bug("RanEarly") then
        \* before round 4 a failed write refused the call
        st := RefuseSt(self); iret[self] := "nc";
      else
        iret[self] := IF Bug("NoIdemBegan") THEN "err" ELSE "argserr";
      end if;
      ion[self] := FALSE;
      goto IWait;
    end if;
  end if;
IEnter2:
  \* RanEarly only: the checks and enterTool came after the ran mark and the arguments
  if Bug("RanEarly") then
    if IDone(self) then
      st := RefuseSt(self); iret[self] := "nc"; ion[self] := FALSE;
      goto IWait;
    else
      either
        await guards < MaxGuard;
        guards := guards + 1;
        st := RefuseSt(self); iret[self] := "nc"; ion[self] := FALSE;
        goto IWait;
      or
        if ~EnterOK(self) then
          iret[self] := "nc"; ion[self] := FALSE;
          goto IWait;
        else
          st := EnterSt(self);
          everReached[CallOf(self)] := TRUE;
        end if;
      end either;
    end if;
  end if;
IIdem:
  \* (T3's fix) the saga-args record, written before the call, is also the "may have begun" mark
  \* of a retry-safe saga call that changes state, whether or not its arguments changed (IArgs
  \* wrote it for a rewritten call); a failed write (which may have committed, A3) returns
  \* before the tool's Call
  if IdemMark(CallOf(self)) /\ ~(Rewrite /\ CallOf(self) = 1) then
    either
      await ~IDone(self);
      ibeg[CallOf(self)] := TRUE; r := "ok";
    or
      await errs < MaxErr \/ IDone(self);
      errs := IF IDone(self) THEN errs ELSE errs + 1;
      either ibeg[CallOf(self)] := TRUE; or skip; end either;
      r := "err";
    end either;
    if r = "err" then
      iret[self] := "argserr"; ion[self] := FALSE;
      goto IWait;
    end if;
  end if;
IBegin:
  \* beginCall: CAS none -> yes immediately before t.Call; a sealed word refuses
  if ~(Bug("FlagRule") \/ Bug("NoBeganWord")) /\ IBg(self) = "sealed" then
    iret[self] := "nc"; ion[self] := FALSE;
    goto IWait;
  elsif ~Bug("FlagRule") /\ ~Bug("NoClosedBegin") /\ (iold[self] \/ closedc[CallOf(self)]) then
    \* (T5's fix) no invocation begins the tool once its chain has returned: a next left running
    \* past the return could otherwise fire a retry-safe tool after its result, or its
    \* compensation, was recorded
    iret[self] := "nc"; ion[self] := FALSE;
    goto IWait;
  elsif ~Bug("RanBeforeArgs") /\ ~Bug("RanEarly") /\ Side(CallOf(self)) /\ IBg(self) = "yes" then
    \* "already ran" (ErrToolReinvoked) only when an earlier invocation began the tool
    iret[self] := "reinv"; ion[self] := FALSE;
    goto IWait;
  else
    \* (FlagRule, NoBeganWord: no began word to check; the flag is set and the tool called)
    bg := IF iold[self] THEN bg ELSE [bg EXCEPT ![CallOf(self)] = "yes"];
    iear := IF iold[self] \/ tout[CallOf(self)] \in {"none", "err"} THEN iear
            ELSE [iear EXCEPT ![CallOf(self)] = TRUE];
    began[CallOf(self)][iatt[self]] := TRUE;
    tout := IF iold[self] THEN tout ELSE [tout EXCEPT ![CallOf(self)] = "running"];
  end if;
ICall:
  \* t.Call: the tool fires its effect or not, and returns
  if Kinds[CallOf(self)] = "deleg" then
    if auth = "wrong" then
      r := "unrec";
    elsif IDone(self) then
      either r := "ok"; or r := "ctxerr"; end either;
    else
      r := "ok";
    end if;
  else
    either
      \* (capped at 2: a retry-safe tool may fire on every re-run)
      fired[CallOf(self)] := IF fired[CallOf(self)] >= 2 THEN 2 ELSE fired[CallOf(self)] + 1;
      firedAt[CallOf(self)][iatt[self]] := TRUE;
      fx[CallOf(self)] := TRUE;
      either r := "ok";
      or await unks < MaxUnk; unks := unks + 1; r := "unk";
      or await IDone(self); r := "ctxerr";
      end either;
    or
      either await toolErrs < MaxToolErr; toolErrs := toolErrs + 1; r := "err";
      or await unks < MaxUnk; unks := unks + 1; r := "unk";
      or await IDone(self); r := "ctxerr";
      end either;
    end either;
  end if;
  \* the tool's own outcome: failed only for its own error before its context was done
  tout := IF iold[self] THEN tout
          ELSE [tout EXCEPT ![CallOf(self)] = IF r = "err" /\ IDone(self) THEN "unk" ELSE r];
  iret[self] := r; ion[self] := FALSE;
  goto IWait;
end process;

\* One call's goroutine in the errgroup, with its middleware chain.
fair process loop \in Calls
variables slot = 0, last = "", cerr = "", rec = "none", called = TRUE, made = FALSE,
          gret = "nil", r2 = "";
begin
LIdle:
  await go[self];
  last := ""; cerr := ""; rec := "none"; called := TRUE; made := FALSE; gret := "nil";
LStart:
  \* gctx.Err(): a call whose turn was cut short never starts
  if SDone then
    gret := "cancel";
    goto LRet;
  elsif Side(self) then
    \* claimNextAttempt (its faults are model 1's: here it succeeds, or the process crashes)
    if att[self] >= MaxAtt then
      boundHit := TRUE; gret := "cancel";
      goto LRet;
    else
      marker[self] := [marker[self] EXCEPT ![att[self] + 1] = "live"];
      att[self] := att[self] + 1;
    end if;
  end if;
LPre:
  if Side(self) /\ SDone then
    \* cancelled after the claim and before the call: not called, recorded below
    called := FALSE; gret := "cancel";
    goto LNS;
  else
    \* the chain starts: a fresh call state, began word and ran map
    st[self] := "open"; bg[self] := "none"; ranc[self] := FALSE; tout[self] := "none";
    iear[self] := FALSE;
    closedc[self] := FALSE; dl[self] := FALSE;
  end if;
LMw:
  either
    \* next, synchronously
    await "next" \in MW[self] /\ ~made /\ FreeSlots(self) # {};
    slot := FreeSlot(self);
    Spawn(FreeSlot(self), "ok");
    made := TRUE;
    goto LWait;
  or
    \* next again (a retry)
    await "retry" \in MW[self] /\ made /\ extras < MaxExtra /\ FreeSlots(self) # {};
    slot := FreeSlot(self);
    Spawn(FreeSlot(self), "ok");
    extras := extras + 1;
    goto LWait;
  or
    \* next with a renamed or re-identified call: refused (a second call to next is a retry)
    await "renamed" \in MW[self] /\ FreeSlots(self) # {}
          /\ (~made \/ ("retry" \in MW[self] /\ extras < MaxExtra));
    slot := FreeSlot(self);
    Spawn(FreeSlot(self), "renamed");
    extras := IF made THEN extras + 1 ELSE extras;
    made := TRUE;
    goto LWait;
  or
    \* next in a goroutine the middleware does not wait for
    await "async" \in MW[self] /\ extras < MaxExtra /\ FreeSlots(self) # {};
    Spawn(FreeSlot(self), "ok");
    extras := extras + 1;
    made := TRUE;
  or
    \* return what next returned
    await "ret" \in MW[self] /\ last # "";
    cerr := last;
    goto LClose;
  or
    \* end the call without next: ErrToolNotCalled (ToolRateLimit giving up, a denial)
    await "deny" \in MW[self] /\ ~made;
    cerr := "nc";
    goto LClose;
  or
    \* an error of its own, without the sentinel
    await "bare" \in MW[self];
    cerr := "err";
    goto LClose;
  or
    \* turn next's success into an error (a result check)
    await "maperr" \in MW[self] /\ last = "ok";
    cerr := "err";
    goto LClose;
  or
    \* give up when the context is done (an abandon-on-cancel or hedging wrapper)
    await "ctxerr" \in MW[self] /\ TDone(self);
    cerr := "ctxerr";
    goto LClose;
  or
    \* a result of its own (a cache hit)
    await "cache" \in MW[self];
    cerr := "ok";
    goto LClose;
  or
    \* the middleware calls the tool itself, not through next
    await "direct" \in MW[self] /\ Side(self) /\ extras < MaxExtra;
    extras := extras + 1;
    began[self][att[self]] := TRUE;
    either
      fired[self] := fired[self] + 1;
      firedAt[self][att[self]] := TRUE;
      fx[self] := TRUE;
      either cerr := "ok"; or cerr := "err"; end either;
    or
      cerr := "err";
    end either;
    goto LClose;
  end either;
LMwAgain:
  goto LMw;
LWait:
  await ~ion[slot];
  last := iret[slot];
  goto LMw;
LClose:
  \* callTool returns: closeCall, the seal, and the loop's decision of what to record
  st[self] := FinalSt(self);
  bg[self] := IF Seal(self) THEN "sealed" ELSE bg[self];
  closedc[self] := TRUE;
  called := ~NotCalled(self, cerr);
  if rbm[self] then
    \* rollbackRun's re-run, through the same base handler (so T4's rule holds). Its result is one
    \* only if the re-run reached the tool (a cache answer is an unknown outcome). An unknown
    \* outcome is listed and the walk goes on; any other error stops the rollback, and a later
    \* RunSaga resumes it. Before the fix (NoRbUnknown) every error stopped it, so a result check
    \* that always rejects the re-run's success left the rollback with no end.
    rec := IF RbOk(self, cerr) THEN "ok" ELSE "none";
    rbOut[self] := IF RbOk(self, cerr) \/ SDone \/ Bug("NoRbUnknown") THEN "err"
                   ELSE IF Cls(self, cerr) = "unk" \/ cerr = "ok" THEN "unk"
                   ELSE "err";
  else
    rec := RecOf(self, cerr);
    gret := IF RecOf(self, cerr) = "none" THEN GRetOf(self, cerr) ELSE "nil";
  end if;
LRec:
  if rec # "none" then
    Reply(r2);
    if r2 # "err_nc" /\ res[self].k = "none" then
      res[self] := [k |-> rec,
                    nc |-> rec # "ok" /\ ~called,
                    ran |-> rec # "ok" /\ cerr = "reinv",
                    a |-> att[self]];
    end if;
    if rbm[self] then
      rbOut[self] := IF r2 = "ok" THEN "ok" ELSE "err";
      goto LRet;
    elsif r2 # "ok" then
      gret := "storage";
    elsif rec \in {"sagaFail", "sagaUnk"} then
      gret := "trip";
      goto LRet;
    else
      goto LRet;
    end if;
  elsif rbm[self] then
    goto LRet;
  end if;
LNS:
  \* recordNotStarted, for a claimed call that was not called; a failed write is remembered
  if Side(self) /\ ~called then
    Reply(r2);
    if r2 # "err_nc" /\ marker[self][att[self]] = "live" then
      marker[self] := [marker[self] EXCEPT ![att[self]] = "void"];
    end if;
    if r2 # "ok" then
      pend[self] := att[self];
    end if;
  end if;
LRet:
  \* the errgroup: held errors wait for the siblings; any other cancels the group's context (a
  \* rollback re-run is not in a group)
  if rbm[self] then
    skip;
  elsif gret \in HeldKinds(Bugs) then
    held := held \cup {gret};
  elsif gret # "nil" then
    werr := IF werr = "none" THEN gret ELSE werr;
    gcancel := TRUE;
  end if;
  go[self] := FALSE;
  goto LIdle;
end process;

\* The run's driver: a drive, then a re-drive (Recover, a resume) until it completes.
fair process driver = 0
variables gc = 0, r3 = "", rc = 0;
begin
DOpen:
  if complete \/ rolled then
    goto Done;
  elsif SagaFailed then
    goto DRollback;
  elsif AllRecorded then
    complete := TRUE;
    goto Done;
  elsif GateCalls # {} then
    gc := CHOOSE c \in GateCalls : TRUE;
    goto DGate;
  else
    cancelled := FALSE; gcancel := FALSE; werr := "none"; held := {}; runRet := "none";
    ibs := ibeg;
    go := [c \in Calls |-> res[c].k = "none"];
  end if;
DWait:
  await \A c \in Calls : ~go[c];
  runRet := CASE werr # "none" -> IF werr = "unrec" THEN "unrec" ELSE "fail"
              [] "unrec" \in held -> IF "unk" \in held THEN "unrecHalt" ELSE "unrec"
              [] "unk" \in held -> "halt"
              [] OTHER -> "turn";
  \* the group ends: its context is cancelled, and every invocation still running keeps a
  \* frozen view of its (closed) chain
  ost := [i \in Invs |-> IF ion[i] /\ ~iold[i] THEN st[CallOf(i)] ELSE ost[i]];
  obg := [i \in Invs |-> IF ion[i] /\ ~iold[i] THEN bg[CallOf(i)] ELSE obg[i]];
  oran := [i \in Invs |-> IF ion[i] /\ ~iold[i] THEN ranc[CallOf(i)] ELSE oran[i]];
  iold := [i \in Invs |-> iold[i] \/ ion[i]];
  goto DOpen;
DGate:
  \* the resume gate: a live marker and no result halts, unless the process remembers the
  \* claim, whose not-started record it writes again (model 1's rule 4)
  if pend[gc] = att[gc] then
    Reply(r3);
    if r3 # "err_nc" then
      marker[gc] := [marker[gc] EXCEPT ![att[gc]] = "void"];
    end if;
    if r3 = "ok" then
      pend[gc] := 0;
    end if;
  end if;
  goto DOpen;
DRollback:
  \* rollbackRun, in this RunSaga or a later one (a fresh context): the calls in reverse order
  cancelled := FALSE; gcancel := FALSE; rc := NCalls; rbUnk := {};
DRbStep:
  if rc = 0 then
    rolled := TRUE;
    goto Done;
  elsif Kinds[rc] = "deleg" \/ res[rc].k \in {"sagaFail", "sagaUnk", "fail"} then
    \* the failed step (an unknown outcome is listed, see SagaAccounted; so is (T3) a failed
    \* retry-safe step that changes state whose "may have begun" record is in the journal), or no
    \* effect of its own
    if Idem(rc) /\ res[rc].k = "sagaFail" /\ ibeg[rc] /\ ~Bug("NoIdemBegan") then
      rbUnk := rbUnk \cup {rc};
    end if;
    rc := rc - 1;
    goto DRbStep;
  elsif res[rc].k = "ok" then
    goto DRbComp;
  elsif Side(rc) then
    if Live(rc) then
      \* started, no recorded outcome: the rollback stops for a human
      rbHalt := TRUE; rolled := TRUE;
      goto Done;
    else
      rc := rc - 1;
      goto DRbStep;
    end if;
  else
    \* retry-safe with a compensator and no result: run it again, through its middleware chain
    rbm[rc] := TRUE; rbOut[rc] := "err"; go[rc] := TRUE;
  end if;
DRbWait:
  await ~go[rc];
  rbm[rc] := FALSE;
  if rbOut[rc] = "unk" then
    rbUnk := rbUnk \cup {rc};
    goto DRbNext;
  elsif rbOut[rc] # "ok" then
    \* the rollback stops with an error (the call listed as uncompensated); a later RunSaga resumes
    goto DOpen;
  end if;
DRbComp:
  \* the compensation, a memoized step: Compensate undoes the effect, then its record is written
  if ~compd[rc] then
    fx[rc] := FALSE;
    Reply(r3);
    if r3 # "err_nc" then
      compd[rc] := TRUE;
    end if;
    if r3 # "ok" then
      goto DOpen;
    end if;
  end if;
DRbNext:
  rc := rc - 1;
  goto DRbStep;
end process;
end algorithm; *)
\* BEGIN TRANSLATION
VARIABLES marker, att, res, argsRec, ibeg, ibs, iear, complete, pend, 
          cancelled, gcancel, auth, go, werr, held, runRet, st, bg, ranc, 
          tout, closedc, dl, ion, iarg, iret, iatt, iold, ost, obg, oran, 
          fired, firedAt, began, everReached, lostAtt, rolled, rbm, rbOut, 
          compd, rbHalt, rbUnk, fx, boundHit, extras, errs, crashes, cancels, 
          deadlines, guards, unks, toolErrs, wrongs, pc

(* define statement *)
Side(c) == Kinds[c] = "side"
Idem(c) == Kinds[c] = "idem"
Bug(b) == b \in Bugs




Unproven(c) == Side(c) \/ (Idem(c) /\ Saga /\ ~Bug("IdemSagaSkip"))





IdemBegan(c) == Idem(c) /\ ibs[c] /\ ~Bug("NoIdemBegan")
IdemMark(c) == Idem(c) /\ Saga /\ ~Bug("NoIdemBegan")

SDone == cancelled \/ gcancel
TDone(c) == SDone \/ (Timeout /\ dl[c])


IDone(i) == iold[i] \/ SDone \/ (Timeout /\ (dl[CallOf(i)] \/ closedc[CallOf(i)]))
ISt(i) == IF iold[i] THEN ost[i] ELSE st[CallOf(i)]
IBg(i) == IF iold[i] THEN obg[i] ELSE bg[CallOf(i)]
IRan(i) == IF iold[i] THEN oran[i] ELSE ranc[CallOf(i)]

RefuseSt(i) == IF ~iold[i] /\ st[CallOf(i)] = "open"
               THEN [st EXCEPT ![CallOf(i)] = "refused"] ELSE st

EnterOK(i) == IF Bug("FlagRule") THEN TRUE
              ELSE IF Bug("RefusedNotTerminal") THEN ISt(i) # "closed"
              ELSE ISt(i) \in {"open", "refused", "reached"}
EnterSt(i) == IF iold[i] \/ Bug("FlagRule") THEN st ELSE [st EXCEPT ![CallOf(i)] = "reached"]
Live(c) == att[c] > 0 /\ marker[c][att[c]] = "live"
FreeSlots(c) == {i \in Invs : CallOf(i) = c /\ ~ion[i]}
FreeSlot(c) == CHOOSE i \in FreeSlots(c) : \A j \in FreeSlots(c) : i <= j

CloseSt(c) == CASE st[c] = "open" -> "closed"
                [] st[c] = "refused" /\ ~Bug("RefusedNotTerminal") -> "refusedClosed"
                [] OTHER -> st[c]
Seal(c) == ~Bug("NoBeganWord") /\ ~Bug("FlagRule") /\ CloseSt(c) = "reached" /\ bg[c] = "none"
FinalSt(c) == IF Seal(c) THEN "refusedClosed" ELSE CloseSt(c)


NotCalled(c, e) == IF Bug("FlagRule") THEN bg[c] # "yes"
                   ELSE \/ FinalSt(c) = "refusedClosed"
                        \/ FinalSt(c) = "refused"
                        \/ FinalSt(c) = "closed" /\ e = "nc"
ReachedAt(c) == IF Bug("FlagRule") THEN bg[c] = "yes" ELSE FinalSt(c) = "reached"






ToolRunning(c) == \E i \in Invs : CallOf(i) = c /\ ion[i] /\ pc[i] = "ICall"





RunOk(c) == ~Bug("NoRunningCheck") /\ Unproven(c) /\ ToolRunning(c)
            /\ (ReachedAt(c) \/ ~Bug("RunningReachedOnly"))
Running(c) == ~Bug("NoRunningCheck") /\ ReachedAt(c) /\ ToolRunning(c)





Cls(c, e) ==
  IF e = "ok" /\ RunOk(c) THEN "unk"
  ELSE IF e # "ok" /\ ~Bug("FlagRule") /\ FinalSt(c) = "closed" /\ ~NotCalled(c, e) /\ Side(c)
    THEN "unk"
  ELSE IF ~Bug("NoToolOutcome") /\ e # "ok" /\ ReachedAt(c) /\ bg[c] = "yes" /\ ~TDone(c)
          /\ \/ (tout[c] # "err" \/ Running(c)) /\ Unproven(c)

             \/ iear[c] /\ Idem(c) /\ Saga /\ ~Bug("NoIdemBegan")
    THEN "unk"
  ELSE e
Late(c, e) == e # "ok" /\ ReachedAt(c) /\ TDone(c)



Ok(c, e) == e = "ok" /\ ~RunOk(c)

RbOk(c, e) == Ok(c, e) /\ ReachedAt(c)

RecOf(c, e) ==
  IF Ok(c, e) THEN "ok"
  ELSE IF SDone \/ Cls(c, e) = "unrec" THEN "none"

  ELSE IF e = "argserr" /\ NotCalled(c, e) /\ ~Bug("NoIdemBegan") THEN "none"
  ELSE IF (Late(c, e) \/ Cls(c, e) = "unk") /\ Side(c) THEN "none"
  ELSE IF Saga THEN (IF Late(c, e) \/ Cls(c, e) = "unk" \/ IdemBegan(c) THEN "sagaUnk" ELSE "sagaFail")
  ELSE "fail"

GRetOf(c, e) ==
  IF SDone THEN (IF Cls(c, e) \in {"unrec", "unk"} THEN Cls(c, e) ELSE "cancel")
  ELSE IF Cls(c, e) = "unrec" THEN "unrec"
  ELSE IF e = "argserr" THEN "storage"
  ELSE "unk"
AllRecorded == \A c \in Calls : res[c].k # "none"
SagaFailed == Saga /\ \E c \in Calls : res[c].k \in {"sagaFail", "sagaUnk"}
GateCalls == {c \in Calls : Side(c) /\ res[c].k = "none" /\ Live(c)}

VARIABLES r, slot, last, cerr, rec, called, made, gret, r2, gc, r3, rc

vars == << marker, att, res, argsRec, ibeg, ibs, iear, complete, pend, 
           cancelled, gcancel, auth, go, werr, held, runRet, st, bg, ranc, 
           tout, closedc, dl, ion, iarg, iret, iatt, iold, ost, obg, oran, 
           fired, firedAt, began, everReached, lostAtt, rolled, rbm, rbOut, 
           compd, rbHalt, rbUnk, fx, boundHit, extras, errs, crashes, cancels, 
           deadlines, guards, unks, toolErrs, wrongs, pc, r, slot, last, cerr, 
           rec, called, made, gret, r2, gc, r3, rc >>

ProcSet == (Invs) \cup (Calls) \cup {0}

Init == (* Global variables *)
        /\ marker = [c \in Calls |-> [a \in Atts |-> "none"]]
        /\ att = [c \in Calls |-> 0]
        /\ res = [c \in Calls |-> NoRes]
        /\ argsRec = [c \in Calls |-> FALSE]
        /\ ibeg = [c \in Calls |-> FALSE]
        /\ ibs = [c \in Calls |-> FALSE]
        /\ iear = [c \in Calls |-> FALSE]
        /\ complete = FALSE
        /\ pend = [c \in Calls |-> 0]
        /\ cancelled = FALSE
        /\ gcancel = FALSE
        /\ auth = "right"
        /\ go = [c \in Calls |-> FALSE]
        /\ werr = "none"
        /\ held = {}
        /\ runRet = "none"
        /\ st = [c \in Calls |-> "open"]
        /\ bg = [c \in Calls |-> "none"]
        /\ ranc = [c \in Calls |-> FALSE]
        /\ tout = [c \in Calls |-> "none"]
        /\ closedc = [c \in Calls |-> FALSE]
        /\ dl = [c \in Calls |-> FALSE]
        /\ ion = [i \in Invs |-> FALSE]
        /\ iarg = [i \in Invs |-> "ok"]
        /\ iret = [i \in Invs |-> ""]
        /\ iatt = [i \in Invs |-> 0]
        /\ iold = [i \in Invs |-> FALSE]
        /\ ost = [i \in Invs |-> "open"]
        /\ obg = [i \in Invs |-> "none"]
        /\ oran = [i \in Invs |-> FALSE]
        /\ fired = [c \in Calls |-> 0]
        /\ firedAt = [c \in Calls |-> [a \in Atts |-> FALSE]]
        /\ began = [c \in Calls |-> [a \in Atts |-> FALSE]]
        /\ everReached = [c \in Calls |-> FALSE]
        /\ lostAtt = [c \in Calls |-> {}]
        /\ rolled = FALSE
        /\ rbm = [c \in Calls |-> FALSE]
        /\ rbOut = [c \in Calls |-> "err"]
        /\ compd = [c \in Calls |-> FALSE]
        /\ rbHalt = FALSE
        /\ rbUnk = {}
        /\ fx = [c \in Calls |-> FALSE]
        /\ boundHit = FALSE
        /\ extras = 0
        /\ errs = 0
        /\ crashes = 0
        /\ cancels = 0
        /\ deadlines = 0
        /\ guards = 0
        /\ unks = 0
        /\ toolErrs = 0
        /\ wrongs = 0
        (* Process inv *)
        /\ r = [self \in Invs |-> ""]
        (* Process loop *)
        /\ slot = [self \in Calls |-> 0]
        /\ last = [self \in Calls |-> ""]
        /\ cerr = [self \in Calls |-> ""]
        /\ rec = [self \in Calls |-> "none"]
        /\ called = [self \in Calls |-> TRUE]
        /\ made = [self \in Calls |-> FALSE]
        /\ gret = [self \in Calls |-> "nil"]
        /\ r2 = [self \in Calls |-> ""]
        (* Process driver *)
        /\ gc = 0
        /\ r3 = ""
        /\ rc = 0
        /\ pc = [self \in ProcSet |-> CASE self \in Invs -> "IWait"
                                        [] self \in Calls -> "LIdle"
                                        [] self = 0 -> "DOpen"]

IWait(self) == /\ pc[self] = "IWait"
               /\ ion[self]
               /\ r' = [r EXCEPT ![self] = ""]
               /\ pc' = [pc EXCEPT ![self] = "IEnter"]
               /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                               complete, pend, cancelled, gcancel, auth, go, 
                               werr, held, runRet, st, bg, ranc, tout, closedc, 
                               dl, ion, iarg, iret, iatt, iold, ost, obg, oran, 
                               fired, firedAt, began, everReached, lostAtt, 
                               rolled, rbm, rbOut, compd, rbHalt, rbUnk, fx, 
                               boundHit, extras, errs, crashes, cancels, 
                               deadlines, guards, unks, toolErrs, wrongs, slot, 
                               last, cerr, rec, called, made, gret, r2, gc, r3, 
                               rc >>

IEnter(self) == /\ pc[self] = "IEnter"
                /\ IF iarg[self] = "renamed"
                      THEN /\ st' = RefuseSt(self)
                           /\ iret' = [iret EXCEPT ![self] = "nc"]
                           /\ ion' = [ion EXCEPT ![self] = FALSE]
                           /\ pc' = [pc EXCEPT ![self] = "IWait"]
                           /\ UNCHANGED << ranc, everReached, guards >>
                      ELSE /\ IF Bug("RanEarly")
                                 THEN /\ IF Side(CallOf(self)) /\ IRan(self)
                                            THEN /\ iret' = [iret EXCEPT ![self] = "reinv"]
                                                 /\ ion' = [ion EXCEPT ![self] = FALSE]
                                                 /\ pc' = [pc EXCEPT ![self] = "IWait"]
                                                 /\ ranc' = ranc
                                            ELSE /\ ranc' = IF iold[self] THEN ranc ELSE [ranc EXCEPT ![CallOf(self)] = TRUE]
                                                 /\ pc' = [pc EXCEPT ![self] = "IArgs"]
                                                 /\ UNCHANGED << ion, iret >>
                                      /\ UNCHANGED << st, everReached, guards >>
                                 ELSE /\ IF IDone(self)
                                            THEN /\ st' = RefuseSt(self)
                                                 /\ iret' = [iret EXCEPT ![self] = "nc"]
                                                 /\ ion' = [ion EXCEPT ![self] = FALSE]
                                                 /\ pc' = [pc EXCEPT ![self] = "IWait"]
                                                 /\ UNCHANGED << ranc, 
                                                                 everReached, 
                                                                 guards >>
                                            ELSE /\ \/ /\ guards < MaxGuard
                                                       /\ guards' = guards + 1
                                                       /\ st' = RefuseSt(self)
                                                       /\ iret' = [iret EXCEPT ![self] = "nc"]
                                                       /\ ion' = [ion EXCEPT ![self] = FALSE]
                                                       /\ pc' = [pc EXCEPT ![self] = "IWait"]
                                                       /\ UNCHANGED <<ranc, everReached>>
                                                    \/ /\ IF ~EnterOK(self)
                                                             THEN /\ iret' = [iret EXCEPT ![self] = "nc"]
                                                                  /\ ion' = [ion EXCEPT ![self] = FALSE]
                                                                  /\ pc' = [pc EXCEPT ![self] = "IWait"]
                                                                  /\ UNCHANGED << st, 
                                                                                  ranc, 
                                                                                  everReached >>
                                                             ELSE /\ st' = EnterSt(self)
                                                                  /\ everReached' = [everReached EXCEPT ![CallOf(self)] = TRUE]
                                                                  /\ IF Side(CallOf(self)) /\ IRan(self) /\ Bug("RanBeforeArgs")
                                                                        THEN /\ iret' = [iret EXCEPT ![self] = "reinv"]
                                                                             /\ ion' = [ion EXCEPT ![self] = FALSE]
                                                                             /\ pc' = [pc EXCEPT ![self] = "IWait"]
                                                                             /\ ranc' = ranc
                                                                        ELSE /\ ranc' = (IF iold[self] \/ ~Side(CallOf(self)) \/ ~Bug("RanBeforeArgs") THEN ranc
                                                                                         ELSE [ranc EXCEPT ![CallOf(self)] = TRUE])
                                                                             /\ pc' = [pc EXCEPT ![self] = "IArgs"]
                                                                             /\ UNCHANGED << ion, 
                                                                                             iret >>
                                                       /\ UNCHANGED guards
                /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                                complete, pend, cancelled, gcancel, auth, go, 
                                werr, held, runRet, bg, tout, closedc, dl, 
                                iarg, iatt, iold, ost, obg, oran, fired, 
                                firedAt, began, lostAtt, rolled, rbm, rbOut, 
                                compd, rbHalt, rbUnk, fx, boundHit, extras, 
                                errs, crashes, cancels, deadlines, unks, 
                                toolErrs, wrongs, r, slot, last, cerr, rec, 
                                called, made, gret, r2, gc, r3, rc >>

IArgs(self) == /\ pc[self] = "IArgs"
               /\ IF Saga /\ Rewrite /\ CallOf(self) = 1
                     THEN /\ \/ /\ ~IDone(self)
                                /\ argsRec' = [argsRec EXCEPT ![1] = TRUE]
                                /\ ibeg' = [ibeg EXCEPT ![1] = IdemMark(1)]
                                /\ r' = [r EXCEPT ![self] = "ok"]
                                /\ errs' = errs
                             \/ /\ errs < MaxErr \/ IDone(self)
                                /\ errs' = IF IDone(self) THEN errs ELSE errs + 1
                                /\ \/ /\ argsRec' = [argsRec EXCEPT ![1] = TRUE]
                                      /\ ibeg' = [ibeg EXCEPT ![1] = IdemMark(1)]
                                   \/ /\ TRUE
                                      /\ UNCHANGED <<argsRec, ibeg>>
                                /\ r' = [r EXCEPT ![self] = "err"]
                          /\ IF r'[self] = "err"
                                THEN /\ IF Bug("RanEarly")
                                           THEN /\ st' = RefuseSt(self)
                                                /\ iret' = [iret EXCEPT ![self] = "nc"]
                                           ELSE /\ iret' = [iret EXCEPT ![self] = IF Bug("NoIdemBegan") THEN "err" ELSE "argserr"]
                                                /\ st' = st
                                     /\ ion' = [ion EXCEPT ![self] = FALSE]
                                     /\ pc' = [pc EXCEPT ![self] = "IWait"]
                                ELSE /\ pc' = [pc EXCEPT ![self] = "IEnter2"]
                                     /\ UNCHANGED << st, ion, iret >>
                     ELSE /\ pc' = [pc EXCEPT ![self] = "IEnter2"]
                          /\ UNCHANGED << argsRec, ibeg, st, ion, iret, errs, 
                                          r >>
               /\ UNCHANGED << marker, att, res, ibs, iear, complete, pend, 
                               cancelled, gcancel, auth, go, werr, held, 
                               runRet, bg, ranc, tout, closedc, dl, iarg, iatt, 
                               iold, ost, obg, oran, fired, firedAt, began, 
                               everReached, lostAtt, rolled, rbm, rbOut, compd, 
                               rbHalt, rbUnk, fx, boundHit, extras, crashes, 
                               cancels, deadlines, guards, unks, toolErrs, 
                               wrongs, slot, last, cerr, rec, called, made, 
                               gret, r2, gc, r3, rc >>

IEnter2(self) == /\ pc[self] = "IEnter2"
                 /\ IF Bug("RanEarly")
                       THEN /\ IF IDone(self)
                                  THEN /\ st' = RefuseSt(self)
                                       /\ iret' = [iret EXCEPT ![self] = "nc"]
                                       /\ ion' = [ion EXCEPT ![self] = FALSE]
                                       /\ pc' = [pc EXCEPT ![self] = "IWait"]
                                       /\ UNCHANGED << everReached, guards >>
                                  ELSE /\ \/ /\ guards < MaxGuard
                                             /\ guards' = guards + 1
                                             /\ st' = RefuseSt(self)
                                             /\ iret' = [iret EXCEPT ![self] = "nc"]
                                             /\ ion' = [ion EXCEPT ![self] = FALSE]
                                             /\ pc' = [pc EXCEPT ![self] = "IWait"]
                                             /\ UNCHANGED everReached
                                          \/ /\ IF ~EnterOK(self)
                                                   THEN /\ iret' = [iret EXCEPT ![self] = "nc"]
                                                        /\ ion' = [ion EXCEPT ![self] = FALSE]
                                                        /\ pc' = [pc EXCEPT ![self] = "IWait"]
                                                        /\ UNCHANGED << st, 
                                                                        everReached >>
                                                   ELSE /\ st' = EnterSt(self)
                                                        /\ everReached' = [everReached EXCEPT ![CallOf(self)] = TRUE]
                                                        /\ pc' = [pc EXCEPT ![self] = "IIdem"]
                                                        /\ UNCHANGED << ion, 
                                                                        iret >>
                                             /\ UNCHANGED guards
                       ELSE /\ pc' = [pc EXCEPT ![self] = "IIdem"]
                            /\ UNCHANGED << st, ion, iret, everReached, guards >>
                 /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                                 complete, pend, cancelled, gcancel, auth, go, 
                                 werr, held, runRet, bg, ranc, tout, closedc, 
                                 dl, iarg, iatt, iold, ost, obg, oran, fired, 
                                 firedAt, began, lostAtt, rolled, rbm, rbOut, 
                                 compd, rbHalt, rbUnk, fx, boundHit, extras, 
                                 errs, crashes, cancels, deadlines, unks, 
                                 toolErrs, wrongs, r, slot, last, cerr, rec, 
                                 called, made, gret, r2, gc, r3, rc >>

IIdem(self) == /\ pc[self] = "IIdem"
               /\ IF IdemMark(CallOf(self)) /\ ~(Rewrite /\ CallOf(self) = 1)
                     THEN /\ \/ /\ ~IDone(self)
                                /\ ibeg' = [ibeg EXCEPT ![CallOf(self)] = TRUE]
                                /\ r' = [r EXCEPT ![self] = "ok"]
                                /\ errs' = errs
                             \/ /\ errs < MaxErr \/ IDone(self)
                                /\ errs' = IF IDone(self) THEN errs ELSE errs + 1
                                /\ \/ /\ ibeg' = [ibeg EXCEPT ![CallOf(self)] = TRUE]
                                   \/ /\ TRUE
                                      /\ ibeg' = ibeg
                                /\ r' = [r EXCEPT ![self] = "err"]
                          /\ IF r'[self] = "err"
                                THEN /\ iret' = [iret EXCEPT ![self] = "argserr"]
                                     /\ ion' = [ion EXCEPT ![self] = FALSE]
                                     /\ pc' = [pc EXCEPT ![self] = "IWait"]
                                ELSE /\ pc' = [pc EXCEPT ![self] = "IBegin"]
                                     /\ UNCHANGED << ion, iret >>
                     ELSE /\ pc' = [pc EXCEPT ![self] = "IBegin"]
                          /\ UNCHANGED << ibeg, ion, iret, errs, r >>
               /\ UNCHANGED << marker, att, res, argsRec, ibs, iear, complete, 
                               pend, cancelled, gcancel, auth, go, werr, held, 
                               runRet, st, bg, ranc, tout, closedc, dl, iarg, 
                               iatt, iold, ost, obg, oran, fired, firedAt, 
                               began, everReached, lostAtt, rolled, rbm, rbOut, 
                               compd, rbHalt, rbUnk, fx, boundHit, extras, 
                               crashes, cancels, deadlines, guards, unks, 
                               toolErrs, wrongs, slot, last, cerr, rec, called, 
                               made, gret, r2, gc, r3, rc >>

IBegin(self) == /\ pc[self] = "IBegin"
                /\ IF ~(Bug("FlagRule") \/ Bug("NoBeganWord")) /\ IBg(self) = "sealed"
                      THEN /\ iret' = [iret EXCEPT ![self] = "nc"]
                           /\ ion' = [ion EXCEPT ![self] = FALSE]
                           /\ pc' = [pc EXCEPT ![self] = "IWait"]
                           /\ UNCHANGED << iear, bg, tout, began >>
                      ELSE /\ IF ~Bug("FlagRule") /\ ~Bug("NoClosedBegin") /\ (iold[self] \/ closedc[CallOf(self)])
                                 THEN /\ iret' = [iret EXCEPT ![self] = "nc"]
                                      /\ ion' = [ion EXCEPT ![self] = FALSE]
                                      /\ pc' = [pc EXCEPT ![self] = "IWait"]
                                      /\ UNCHANGED << iear, bg, tout, began >>
                                 ELSE /\ IF ~Bug("RanBeforeArgs") /\ ~Bug("RanEarly") /\ Side(CallOf(self)) /\ IBg(self) = "yes"
                                            THEN /\ iret' = [iret EXCEPT ![self] = "reinv"]
                                                 /\ ion' = [ion EXCEPT ![self] = FALSE]
                                                 /\ pc' = [pc EXCEPT ![self] = "IWait"]
                                                 /\ UNCHANGED << iear, bg, 
                                                                 tout, began >>
                                            ELSE /\ bg' = IF iold[self] THEN bg ELSE [bg EXCEPT ![CallOf(self)] = "yes"]
                                                 /\ iear' = (IF iold[self] \/ tout[CallOf(self)] \in {"none", "err"} THEN iear
                                                             ELSE [iear EXCEPT ![CallOf(self)] = TRUE])
                                                 /\ began' = [began EXCEPT ![CallOf(self)][iatt[self]] = TRUE]
                                                 /\ tout' = IF iold[self] THEN tout ELSE [tout EXCEPT ![CallOf(self)] = "running"]
                                                 /\ pc' = [pc EXCEPT ![self] = "ICall"]
                                                 /\ UNCHANGED << ion, iret >>
                /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, complete, 
                                pend, cancelled, gcancel, auth, go, werr, held, 
                                runRet, st, ranc, closedc, dl, iarg, iatt, 
                                iold, ost, obg, oran, fired, firedAt, 
                                everReached, lostAtt, rolled, rbm, rbOut, 
                                compd, rbHalt, rbUnk, fx, boundHit, extras, 
                                errs, crashes, cancels, deadlines, guards, 
                                unks, toolErrs, wrongs, r, slot, last, cerr, 
                                rec, called, made, gret, r2, gc, r3, rc >>

ICall(self) == /\ pc[self] = "ICall"
               /\ IF Kinds[CallOf(self)] = "deleg"
                     THEN /\ IF auth = "wrong"
                                THEN /\ r' = [r EXCEPT ![self] = "unrec"]
                                ELSE /\ IF IDone(self)
                                           THEN /\ \/ /\ r' = [r EXCEPT ![self] = "ok"]
                                                   \/ /\ r' = [r EXCEPT ![self] = "ctxerr"]
                                           ELSE /\ r' = [r EXCEPT ![self] = "ok"]
                          /\ UNCHANGED << fired, firedAt, fx, unks, toolErrs >>
                     ELSE /\ \/ /\ fired' = [fired EXCEPT ![CallOf(self)] = IF fired[CallOf(self)] >= 2 THEN 2 ELSE fired[CallOf(self)] + 1]
                                /\ firedAt' = [firedAt EXCEPT ![CallOf(self)][iatt[self]] = TRUE]
                                /\ fx' = [fx EXCEPT ![CallOf(self)] = TRUE]
                                /\ \/ /\ r' = [r EXCEPT ![self] = "ok"]
                                      /\ unks' = unks
                                   \/ /\ unks < MaxUnk
                                      /\ unks' = unks + 1
                                      /\ r' = [r EXCEPT ![self] = "unk"]
                                   \/ /\ IDone(self)
                                      /\ r' = [r EXCEPT ![self] = "ctxerr"]
                                      /\ unks' = unks
                                /\ UNCHANGED toolErrs
                             \/ /\ \/ /\ toolErrs < MaxToolErr
                                      /\ toolErrs' = toolErrs + 1
                                      /\ r' = [r EXCEPT ![self] = "err"]
                                      /\ unks' = unks
                                   \/ /\ unks < MaxUnk
                                      /\ unks' = unks + 1
                                      /\ r' = [r EXCEPT ![self] = "unk"]
                                      /\ UNCHANGED toolErrs
                                   \/ /\ IDone(self)
                                      /\ r' = [r EXCEPT ![self] = "ctxerr"]
                                      /\ UNCHANGED <<unks, toolErrs>>
                                /\ UNCHANGED <<fired, firedAt, fx>>
               /\ tout' = IF iold[self] THEN tout
                          ELSE [tout EXCEPT ![CallOf(self)] = IF r'[self] = "err" /\ IDone(self) THEN "unk" ELSE r'[self]]
               /\ iret' = [iret EXCEPT ![self] = r'[self]]
               /\ ion' = [ion EXCEPT ![self] = FALSE]
               /\ pc' = [pc EXCEPT ![self] = "IWait"]
               /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                               complete, pend, cancelled, gcancel, auth, go, 
                               werr, held, runRet, st, bg, ranc, closedc, dl, 
                               iarg, iatt, iold, ost, obg, oran, began, 
                               everReached, lostAtt, rolled, rbm, rbOut, compd, 
                               rbHalt, rbUnk, boundHit, extras, errs, crashes, 
                               cancels, deadlines, guards, wrongs, slot, last, 
                               cerr, rec, called, made, gret, r2, gc, r3, rc >>

inv(self) == IWait(self) \/ IEnter(self) \/ IArgs(self) \/ IEnter2(self)
                \/ IIdem(self) \/ IBegin(self) \/ ICall(self)

LIdle(self) == /\ pc[self] = "LIdle"
               /\ go[self]
               /\ last' = [last EXCEPT ![self] = ""]
               /\ cerr' = [cerr EXCEPT ![self] = ""]
               /\ rec' = [rec EXCEPT ![self] = "none"]
               /\ called' = [called EXCEPT ![self] = TRUE]
               /\ made' = [made EXCEPT ![self] = FALSE]
               /\ gret' = [gret EXCEPT ![self] = "nil"]
               /\ pc' = [pc EXCEPT ![self] = "LStart"]
               /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                               complete, pend, cancelled, gcancel, auth, go, 
                               werr, held, runRet, st, bg, ranc, tout, closedc, 
                               dl, ion, iarg, iret, iatt, iold, ost, obg, oran, 
                               fired, firedAt, began, everReached, lostAtt, 
                               rolled, rbm, rbOut, compd, rbHalt, rbUnk, fx, 
                               boundHit, extras, errs, crashes, cancels, 
                               deadlines, guards, unks, toolErrs, wrongs, r, 
                               slot, r2, gc, r3, rc >>

LStart(self) == /\ pc[self] = "LStart"
                /\ IF SDone
                      THEN /\ gret' = [gret EXCEPT ![self] = "cancel"]
                           /\ pc' = [pc EXCEPT ![self] = "LRet"]
                           /\ UNCHANGED << marker, att, boundHit >>
                      ELSE /\ IF Side(self)
                                 THEN /\ IF att[self] >= MaxAtt
                                            THEN /\ boundHit' = TRUE
                                                 /\ gret' = [gret EXCEPT ![self] = "cancel"]
                                                 /\ pc' = [pc EXCEPT ![self] = "LRet"]
                                                 /\ UNCHANGED << marker, att >>
                                            ELSE /\ marker' = [marker EXCEPT ![self] = [marker[self] EXCEPT ![att[self] + 1] = "live"]]
                                                 /\ att' = [att EXCEPT ![self] = att[self] + 1]
                                                 /\ pc' = [pc EXCEPT ![self] = "LPre"]
                                                 /\ UNCHANGED << boundHit, 
                                                                 gret >>
                                 ELSE /\ pc' = [pc EXCEPT ![self] = "LPre"]
                                      /\ UNCHANGED << marker, att, boundHit, 
                                                      gret >>
                /\ UNCHANGED << res, argsRec, ibeg, ibs, iear, complete, pend, 
                                cancelled, gcancel, auth, go, werr, held, 
                                runRet, st, bg, ranc, tout, closedc, dl, ion, 
                                iarg, iret, iatt, iold, ost, obg, oran, fired, 
                                firedAt, began, everReached, lostAtt, rolled, 
                                rbm, rbOut, compd, rbHalt, rbUnk, fx, extras, 
                                errs, crashes, cancels, deadlines, guards, 
                                unks, toolErrs, wrongs, r, slot, last, cerr, 
                                rec, called, made, r2, gc, r3, rc >>

LPre(self) == /\ pc[self] = "LPre"
              /\ IF Side(self) /\ SDone
                    THEN /\ called' = [called EXCEPT ![self] = FALSE]
                         /\ gret' = [gret EXCEPT ![self] = "cancel"]
                         /\ pc' = [pc EXCEPT ![self] = "LNS"]
                         /\ UNCHANGED << iear, st, bg, ranc, tout, closedc, dl >>
                    ELSE /\ st' = [st EXCEPT ![self] = "open"]
                         /\ bg' = [bg EXCEPT ![self] = "none"]
                         /\ ranc' = [ranc EXCEPT ![self] = FALSE]
                         /\ tout' = [tout EXCEPT ![self] = "none"]
                         /\ iear' = [iear EXCEPT ![self] = FALSE]
                         /\ closedc' = [closedc EXCEPT ![self] = FALSE]
                         /\ dl' = [dl EXCEPT ![self] = FALSE]
                         /\ pc' = [pc EXCEPT ![self] = "LMw"]
                         /\ UNCHANGED << called, gret >>
              /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, complete, 
                              pend, cancelled, gcancel, auth, go, werr, held, 
                              runRet, ion, iarg, iret, iatt, iold, ost, obg, 
                              oran, fired, firedAt, began, everReached, 
                              lostAtt, rolled, rbm, rbOut, compd, rbHalt, 
                              rbUnk, fx, boundHit, extras, errs, crashes, 
                              cancels, deadlines, guards, unks, toolErrs, 
                              wrongs, r, slot, last, cerr, rec, made, r2, gc, 
                              r3, rc >>

LMw(self) == /\ pc[self] = "LMw"
             /\ \/ /\ "next" \in MW[self] /\ ~made[self] /\ FreeSlots(self) # {}
                   /\ slot' = [slot EXCEPT ![self] = FreeSlot(self)]
                   /\ ion' = [ion EXCEPT ![(FreeSlot(self))] = TRUE]
                   /\ iarg' = [iarg EXCEPT ![(FreeSlot(self))] = "ok"]
                   /\ iret' = [iret EXCEPT ![(FreeSlot(self))] = ""]
                   /\ iatt' = [iatt EXCEPT ![(FreeSlot(self))] = att[self]]
                   /\ iold' = [iold EXCEPT ![(FreeSlot(self))] = FALSE]
                   /\ made' = [made EXCEPT ![self] = TRUE]
                   /\ pc' = [pc EXCEPT ![self] = "LWait"]
                   /\ UNCHANGED <<fired, firedAt, began, fx, extras, cerr>>
                \/ /\ "retry" \in MW[self] /\ made[self] /\ extras < MaxExtra /\ FreeSlots(self) # {}
                   /\ slot' = [slot EXCEPT ![self] = FreeSlot(self)]
                   /\ ion' = [ion EXCEPT ![(FreeSlot(self))] = TRUE]
                   /\ iarg' = [iarg EXCEPT ![(FreeSlot(self))] = "ok"]
                   /\ iret' = [iret EXCEPT ![(FreeSlot(self))] = ""]
                   /\ iatt' = [iatt EXCEPT ![(FreeSlot(self))] = att[self]]
                   /\ iold' = [iold EXCEPT ![(FreeSlot(self))] = FALSE]
                   /\ extras' = extras + 1
                   /\ pc' = [pc EXCEPT ![self] = "LWait"]
                   /\ UNCHANGED <<fired, firedAt, began, fx, cerr, made>>
                \/ /\ "renamed" \in MW[self] /\ FreeSlots(self) # {}
                      /\ (~made[self] \/ ("retry" \in MW[self] /\ extras < MaxExtra))
                   /\ slot' = [slot EXCEPT ![self] = FreeSlot(self)]
                   /\ ion' = [ion EXCEPT ![(FreeSlot(self))] = TRUE]
                   /\ iarg' = [iarg EXCEPT ![(FreeSlot(self))] = "renamed"]
                   /\ iret' = [iret EXCEPT ![(FreeSlot(self))] = ""]
                   /\ iatt' = [iatt EXCEPT ![(FreeSlot(self))] = att[self]]
                   /\ iold' = [iold EXCEPT ![(FreeSlot(self))] = FALSE]
                   /\ extras' = IF made[self] THEN extras + 1 ELSE extras
                   /\ made' = [made EXCEPT ![self] = TRUE]
                   /\ pc' = [pc EXCEPT ![self] = "LWait"]
                   /\ UNCHANGED <<fired, firedAt, began, fx, cerr>>
                \/ /\ "async" \in MW[self] /\ extras < MaxExtra /\ FreeSlots(self) # {}
                   /\ ion' = [ion EXCEPT ![(FreeSlot(self))] = TRUE]
                   /\ iarg' = [iarg EXCEPT ![(FreeSlot(self))] = "ok"]
                   /\ iret' = [iret EXCEPT ![(FreeSlot(self))] = ""]
                   /\ iatt' = [iatt EXCEPT ![(FreeSlot(self))] = att[self]]
                   /\ iold' = [iold EXCEPT ![(FreeSlot(self))] = FALSE]
                   /\ extras' = extras + 1
                   /\ made' = [made EXCEPT ![self] = TRUE]
                   /\ pc' = [pc EXCEPT ![self] = "LMwAgain"]
                   /\ UNCHANGED <<fired, firedAt, began, fx, slot, cerr>>
                \/ /\ "ret" \in MW[self] /\ last[self] # ""
                   /\ cerr' = [cerr EXCEPT ![self] = last[self]]
                   /\ pc' = [pc EXCEPT ![self] = "LClose"]
                   /\ UNCHANGED <<ion, iarg, iret, iatt, iold, fired, firedAt, began, fx, extras, slot, made>>
                \/ /\ "deny" \in MW[self] /\ ~made[self]
                   /\ cerr' = [cerr EXCEPT ![self] = "nc"]
                   /\ pc' = [pc EXCEPT ![self] = "LClose"]
                   /\ UNCHANGED <<ion, iarg, iret, iatt, iold, fired, firedAt, began, fx, extras, slot, made>>
                \/ /\ "bare" \in MW[self]
                   /\ cerr' = [cerr EXCEPT ![self] = "err"]
                   /\ pc' = [pc EXCEPT ![self] = "LClose"]
                   /\ UNCHANGED <<ion, iarg, iret, iatt, iold, fired, firedAt, began, fx, extras, slot, made>>
                \/ /\ "maperr" \in MW[self] /\ last[self] = "ok"
                   /\ cerr' = [cerr EXCEPT ![self] = "err"]
                   /\ pc' = [pc EXCEPT ![self] = "LClose"]
                   /\ UNCHANGED <<ion, iarg, iret, iatt, iold, fired, firedAt, began, fx, extras, slot, made>>
                \/ /\ "ctxerr" \in MW[self] /\ TDone(self)
                   /\ cerr' = [cerr EXCEPT ![self] = "ctxerr"]
                   /\ pc' = [pc EXCEPT ![self] = "LClose"]
                   /\ UNCHANGED <<ion, iarg, iret, iatt, iold, fired, firedAt, began, fx, extras, slot, made>>
                \/ /\ "cache" \in MW[self]
                   /\ cerr' = [cerr EXCEPT ![self] = "ok"]
                   /\ pc' = [pc EXCEPT ![self] = "LClose"]
                   /\ UNCHANGED <<ion, iarg, iret, iatt, iold, fired, firedAt, began, fx, extras, slot, made>>
                \/ /\ "direct" \in MW[self] /\ Side(self) /\ extras < MaxExtra
                   /\ extras' = extras + 1
                   /\ began' = [began EXCEPT ![self][att[self]] = TRUE]
                   /\ \/ /\ fired' = [fired EXCEPT ![self] = fired[self] + 1]
                         /\ firedAt' = [firedAt EXCEPT ![self][att[self]] = TRUE]
                         /\ fx' = [fx EXCEPT ![self] = TRUE]
                         /\ \/ /\ cerr' = [cerr EXCEPT ![self] = "ok"]
                            \/ /\ cerr' = [cerr EXCEPT ![self] = "err"]
                      \/ /\ cerr' = [cerr EXCEPT ![self] = "err"]
                         /\ UNCHANGED <<fired, firedAt, fx>>
                   /\ pc' = [pc EXCEPT ![self] = "LClose"]
                   /\ UNCHANGED <<ion, iarg, iret, iatt, iold, slot, made>>
             /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                             complete, pend, cancelled, gcancel, auth, go, 
                             werr, held, runRet, st, bg, ranc, tout, closedc, 
                             dl, ost, obg, oran, everReached, lostAtt, rolled, 
                             rbm, rbOut, compd, rbHalt, rbUnk, boundHit, errs, 
                             crashes, cancels, deadlines, guards, unks, 
                             toolErrs, wrongs, r, last, rec, called, gret, r2, 
                             gc, r3, rc >>

LMwAgain(self) == /\ pc[self] = "LMwAgain"
                  /\ pc' = [pc EXCEPT ![self] = "LMw"]
                  /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                                  complete, pend, cancelled, gcancel, auth, go, 
                                  werr, held, runRet, st, bg, ranc, tout, 
                                  closedc, dl, ion, iarg, iret, iatt, iold, 
                                  ost, obg, oran, fired, firedAt, began, 
                                  everReached, lostAtt, rolled, rbm, rbOut, 
                                  compd, rbHalt, rbUnk, fx, boundHit, extras, 
                                  errs, crashes, cancels, deadlines, guards, 
                                  unks, toolErrs, wrongs, r, slot, last, cerr, 
                                  rec, called, made, gret, r2, gc, r3, rc >>

LWait(self) == /\ pc[self] = "LWait"
               /\ ~ion[slot[self]]
               /\ last' = [last EXCEPT ![self] = iret[slot[self]]]
               /\ pc' = [pc EXCEPT ![self] = "LMw"]
               /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                               complete, pend, cancelled, gcancel, auth, go, 
                               werr, held, runRet, st, bg, ranc, tout, closedc, 
                               dl, ion, iarg, iret, iatt, iold, ost, obg, oran, 
                               fired, firedAt, began, everReached, lostAtt, 
                               rolled, rbm, rbOut, compd, rbHalt, rbUnk, fx, 
                               boundHit, extras, errs, crashes, cancels, 
                               deadlines, guards, unks, toolErrs, wrongs, r, 
                               slot, cerr, rec, called, made, gret, r2, gc, r3, 
                               rc >>

LClose(self) == /\ pc[self] = "LClose"
                /\ st' = [st EXCEPT ![self] = FinalSt(self)]
                /\ bg' = [bg EXCEPT ![self] = IF Seal(self) THEN "sealed" ELSE bg[self]]
                /\ closedc' = [closedc EXCEPT ![self] = TRUE]
                /\ called' = [called EXCEPT ![self] = ~NotCalled(self, cerr[self])]
                /\ IF rbm[self]
                      THEN /\ rec' = [rec EXCEPT ![self] = IF RbOk(self, cerr[self]) THEN "ok" ELSE "none"]
                           /\ rbOut' = [rbOut EXCEPT ![self] = IF RbOk(self, cerr[self]) \/ SDone \/ Bug("NoRbUnknown") THEN "err"
                                                               ELSE IF Cls(self, cerr[self]) = "unk" \/ cerr[self] = "ok" THEN "unk"
                                                               ELSE "err"]
                           /\ gret' = gret
                      ELSE /\ rec' = [rec EXCEPT ![self] = RecOf(self, cerr[self])]
                           /\ gret' = [gret EXCEPT ![self] = IF RecOf(self, cerr[self]) = "none" THEN GRetOf(self, cerr[self]) ELSE "nil"]
                           /\ rbOut' = rbOut
                /\ pc' = [pc EXCEPT ![self] = "LRec"]
                /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                                complete, pend, cancelled, gcancel, auth, go, 
                                werr, held, runRet, ranc, tout, dl, ion, iarg, 
                                iret, iatt, iold, ost, obg, oran, fired, 
                                firedAt, began, everReached, lostAtt, rolled, 
                                rbm, compd, rbHalt, rbUnk, fx, boundHit, 
                                extras, errs, crashes, cancels, deadlines, 
                                guards, unks, toolErrs, wrongs, r, slot, last, 
                                cerr, made, r2, gc, r3, rc >>

LRec(self) == /\ pc[self] = "LRec"
              /\ IF rec[self] # "none"
                    THEN /\ \/ /\ r2' = [r2 EXCEPT ![self] = "ok"]
                               /\ errs' = errs
                            \/ /\ errs < MaxErr
                               /\ errs' = errs + 1
                               /\ r2' = [r2 EXCEPT ![self] = "err_nc"]
                            \/ /\ errs < MaxErr
                               /\ errs' = errs + 1
                               /\ r2' = [r2 EXCEPT ![self] = "err_c"]
                         /\ IF r2'[self] # "err_nc" /\ res[self].k = "none"
                               THEN /\ res' = [res EXCEPT ![self] = [k |-> rec[self],
                                                                     nc |-> rec[self] # "ok" /\ ~called[self],
                                                                     ran |-> rec[self] # "ok" /\ cerr[self] = "reinv",
                                                                     a |-> att[self]]]
                               ELSE /\ TRUE
                                    /\ res' = res
                         /\ IF rbm[self]
                               THEN /\ rbOut' = [rbOut EXCEPT ![self] = IF r2'[self] = "ok" THEN "ok" ELSE "err"]
                                    /\ pc' = [pc EXCEPT ![self] = "LRet"]
                                    /\ gret' = gret
                               ELSE /\ IF r2'[self] # "ok"
                                          THEN /\ gret' = [gret EXCEPT ![self] = "storage"]
                                               /\ pc' = [pc EXCEPT ![self] = "LNS"]
                                          ELSE /\ IF rec[self] \in {"sagaFail", "sagaUnk"}
                                                     THEN /\ gret' = [gret EXCEPT ![self] = "trip"]
                                                          /\ pc' = [pc EXCEPT ![self] = "LRet"]
                                                     ELSE /\ pc' = [pc EXCEPT ![self] = "LRet"]
                                                          /\ gret' = gret
                                    /\ rbOut' = rbOut
                    ELSE /\ IF rbm[self]
                               THEN /\ pc' = [pc EXCEPT ![self] = "LRet"]
                               ELSE /\ pc' = [pc EXCEPT ![self] = "LNS"]
                         /\ UNCHANGED << res, rbOut, errs, gret, r2 >>
              /\ UNCHANGED << marker, att, argsRec, ibeg, ibs, iear, complete, 
                              pend, cancelled, gcancel, auth, go, werr, held, 
                              runRet, st, bg, ranc, tout, closedc, dl, ion, 
                              iarg, iret, iatt, iold, ost, obg, oran, fired, 
                              firedAt, began, everReached, lostAtt, rolled, 
                              rbm, compd, rbHalt, rbUnk, fx, boundHit, extras, 
                              crashes, cancels, deadlines, guards, unks, 
                              toolErrs, wrongs, r, slot, last, cerr, rec, 
                              called, made, gc, r3, rc >>

LNS(self) == /\ pc[self] = "LNS"
             /\ IF Side(self) /\ ~called[self]
                   THEN /\ \/ /\ r2' = [r2 EXCEPT ![self] = "ok"]
                              /\ errs' = errs
                           \/ /\ errs < MaxErr
                              /\ errs' = errs + 1
                              /\ r2' = [r2 EXCEPT ![self] = "err_nc"]
                           \/ /\ errs < MaxErr
                              /\ errs' = errs + 1
                              /\ r2' = [r2 EXCEPT ![self] = "err_c"]
                        /\ IF r2'[self] # "err_nc" /\ marker[self][att[self]] = "live"
                              THEN /\ marker' = [marker EXCEPT ![self] = [marker[self] EXCEPT ![att[self]] = "void"]]
                              ELSE /\ TRUE
                                   /\ UNCHANGED marker
                        /\ IF r2'[self] # "ok"
                              THEN /\ pend' = [pend EXCEPT ![self] = att[self]]
                              ELSE /\ TRUE
                                   /\ pend' = pend
                   ELSE /\ TRUE
                        /\ UNCHANGED << marker, pend, errs, r2 >>
             /\ pc' = [pc EXCEPT ![self] = "LRet"]
             /\ UNCHANGED << att, res, argsRec, ibeg, ibs, iear, complete, 
                             cancelled, gcancel, auth, go, werr, held, runRet, 
                             st, bg, ranc, tout, closedc, dl, ion, iarg, iret, 
                             iatt, iold, ost, obg, oran, fired, firedAt, began, 
                             everReached, lostAtt, rolled, rbm, rbOut, compd, 
                             rbHalt, rbUnk, fx, boundHit, extras, crashes, 
                             cancels, deadlines, guards, unks, toolErrs, 
                             wrongs, r, slot, last, cerr, rec, called, made, 
                             gret, gc, r3, rc >>

LRet(self) == /\ pc[self] = "LRet"
              /\ IF rbm[self]
                    THEN /\ TRUE
                         /\ UNCHANGED << gcancel, werr, held >>
                    ELSE /\ IF gret[self] \in HeldKinds(Bugs)
                               THEN /\ held' = (held \cup {gret[self]})
                                    /\ UNCHANGED << gcancel, werr >>
                               ELSE /\ IF gret[self] # "nil"
                                          THEN /\ werr' = (IF werr = "none" THEN gret[self] ELSE werr)
                                               /\ gcancel' = TRUE
                                          ELSE /\ TRUE
                                               /\ UNCHANGED << gcancel, werr >>
                                    /\ held' = held
              /\ go' = [go EXCEPT ![self] = FALSE]
              /\ pc' = [pc EXCEPT ![self] = "LIdle"]
              /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                              complete, pend, cancelled, auth, runRet, st, bg, 
                              ranc, tout, closedc, dl, ion, iarg, iret, iatt, 
                              iold, ost, obg, oran, fired, firedAt, began, 
                              everReached, lostAtt, rolled, rbm, rbOut, compd, 
                              rbHalt, rbUnk, fx, boundHit, extras, errs, 
                              crashes, cancels, deadlines, guards, unks, 
                              toolErrs, wrongs, r, slot, last, cerr, rec, 
                              called, made, gret, r2, gc, r3, rc >>

loop(self) == LIdle(self) \/ LStart(self) \/ LPre(self) \/ LMw(self)
                 \/ LMwAgain(self) \/ LWait(self) \/ LClose(self)
                 \/ LRec(self) \/ LNS(self) \/ LRet(self)

DOpen == /\ pc[0] = "DOpen"
         /\ IF complete \/ rolled
               THEN /\ pc' = [pc EXCEPT ![0] = "Done"]
                    /\ UNCHANGED << ibs, complete, cancelled, gcancel, go, 
                                    werr, held, runRet, gc >>
               ELSE /\ IF SagaFailed
                          THEN /\ pc' = [pc EXCEPT ![0] = "DRollback"]
                               /\ UNCHANGED << ibs, complete, cancelled, 
                                               gcancel, go, werr, held, runRet, 
                                               gc >>
                          ELSE /\ IF AllRecorded
                                     THEN /\ complete' = TRUE
                                          /\ pc' = [pc EXCEPT ![0] = "Done"]
                                          /\ UNCHANGED << ibs, cancelled, 
                                                          gcancel, go, werr, 
                                                          held, runRet, gc >>
                                     ELSE /\ IF GateCalls # {}
                                                THEN /\ gc' = (CHOOSE c \in GateCalls : TRUE)
                                                     /\ pc' = [pc EXCEPT ![0] = "DGate"]
                                                     /\ UNCHANGED << ibs, 
                                                                     cancelled, 
                                                                     gcancel, 
                                                                     go, werr, 
                                                                     held, 
                                                                     runRet >>
                                                ELSE /\ cancelled' = FALSE
                                                     /\ gcancel' = FALSE
                                                     /\ werr' = "none"
                                                     /\ held' = {}
                                                     /\ runRet' = "none"
                                                     /\ ibs' = ibeg
                                                     /\ go' = [c \in Calls |-> res[c].k = "none"]
                                                     /\ pc' = [pc EXCEPT ![0] = "DWait"]
                                                     /\ gc' = gc
                                          /\ UNCHANGED complete
         /\ UNCHANGED << marker, att, res, argsRec, ibeg, iear, pend, auth, st, 
                         bg, ranc, tout, closedc, dl, ion, iarg, iret, iatt, 
                         iold, ost, obg, oran, fired, firedAt, began, 
                         everReached, lostAtt, rolled, rbm, rbOut, compd, 
                         rbHalt, rbUnk, fx, boundHit, extras, errs, crashes, 
                         cancels, deadlines, guards, unks, toolErrs, wrongs, r, 
                         slot, last, cerr, rec, called, made, gret, r2, r3, rc >>

DWait == /\ pc[0] = "DWait"
         /\ \A c \in Calls : ~go[c]
         /\ runRet' = (CASE werr # "none" -> IF werr = "unrec" THEN "unrec" ELSE "fail"
                         [] "unrec" \in held -> IF "unk" \in held THEN "unrecHalt" ELSE "unrec"
                         [] "unk" \in held -> "halt"
                         [] OTHER -> "turn")
         /\ ost' = [i \in Invs |-> IF ion[i] /\ ~iold[i] THEN st[CallOf(i)] ELSE ost[i]]
         /\ obg' = [i \in Invs |-> IF ion[i] /\ ~iold[i] THEN bg[CallOf(i)] ELSE obg[i]]
         /\ oran' = [i \in Invs |-> IF ion[i] /\ ~iold[i] THEN ranc[CallOf(i)] ELSE oran[i]]
         /\ iold' = [i \in Invs |-> iold[i] \/ ion[i]]
         /\ pc' = [pc EXCEPT ![0] = "DOpen"]
         /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, complete, 
                         pend, cancelled, gcancel, auth, go, werr, held, st, 
                         bg, ranc, tout, closedc, dl, ion, iarg, iret, iatt, 
                         fired, firedAt, began, everReached, lostAtt, rolled, 
                         rbm, rbOut, compd, rbHalt, rbUnk, fx, boundHit, 
                         extras, errs, crashes, cancels, deadlines, guards, 
                         unks, toolErrs, wrongs, r, slot, last, cerr, rec, 
                         called, made, gret, r2, gc, r3, rc >>

DGate == /\ pc[0] = "DGate"
         /\ IF pend[gc] = att[gc]
               THEN /\ \/ /\ r3' = "ok"
                          /\ errs' = errs
                       \/ /\ errs < MaxErr
                          /\ errs' = errs + 1
                          /\ r3' = "err_nc"
                       \/ /\ errs < MaxErr
                          /\ errs' = errs + 1
                          /\ r3' = "err_c"
                    /\ IF r3' # "err_nc"
                          THEN /\ marker' = [marker EXCEPT ![gc] = [marker[gc] EXCEPT ![att[gc]] = "void"]]
                          ELSE /\ TRUE
                               /\ UNCHANGED marker
                    /\ IF r3' = "ok"
                          THEN /\ pend' = [pend EXCEPT ![gc] = 0]
                          ELSE /\ TRUE
                               /\ pend' = pend
               ELSE /\ TRUE
                    /\ UNCHANGED << marker, pend, errs, r3 >>
         /\ pc' = [pc EXCEPT ![0] = "DOpen"]
         /\ UNCHANGED << att, res, argsRec, ibeg, ibs, iear, complete, 
                         cancelled, gcancel, auth, go, werr, held, runRet, st, 
                         bg, ranc, tout, closedc, dl, ion, iarg, iret, iatt, 
                         iold, ost, obg, oran, fired, firedAt, began, 
                         everReached, lostAtt, rolled, rbm, rbOut, compd, 
                         rbHalt, rbUnk, fx, boundHit, extras, crashes, cancels, 
                         deadlines, guards, unks, toolErrs, wrongs, r, slot, 
                         last, cerr, rec, called, made, gret, r2, gc, rc >>

DRollback == /\ pc[0] = "DRollback"
             /\ cancelled' = FALSE
             /\ gcancel' = FALSE
             /\ rc' = NCalls
             /\ rbUnk' = {}
             /\ pc' = [pc EXCEPT ![0] = "DRbStep"]
             /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                             complete, pend, auth, go, werr, held, runRet, st, 
                             bg, ranc, tout, closedc, dl, ion, iarg, iret, 
                             iatt, iold, ost, obg, oran, fired, firedAt, began, 
                             everReached, lostAtt, rolled, rbm, rbOut, compd, 
                             rbHalt, fx, boundHit, extras, errs, crashes, 
                             cancels, deadlines, guards, unks, toolErrs, 
                             wrongs, r, slot, last, cerr, rec, called, made, 
                             gret, r2, gc, r3 >>

DRbStep == /\ pc[0] = "DRbStep"
           /\ IF rc = 0
                 THEN /\ rolled' = TRUE
                      /\ pc' = [pc EXCEPT ![0] = "Done"]
                      /\ UNCHANGED << go, rbm, rbOut, rbHalt, rbUnk, rc >>
                 ELSE /\ IF Kinds[rc] = "deleg" \/ res[rc].k \in {"sagaFail", "sagaUnk", "fail"}
                            THEN /\ IF Idem(rc) /\ res[rc].k = "sagaFail" /\ ibeg[rc] /\ ~Bug("NoIdemBegan")
                                       THEN /\ rbUnk' = (rbUnk \cup {rc})
                                       ELSE /\ TRUE
                                            /\ rbUnk' = rbUnk
                                 /\ rc' = rc - 1
                                 /\ pc' = [pc EXCEPT ![0] = "DRbStep"]
                                 /\ UNCHANGED << go, rolled, rbm, rbOut, 
                                                 rbHalt >>
                            ELSE /\ IF res[rc].k = "ok"
                                       THEN /\ pc' = [pc EXCEPT ![0] = "DRbComp"]
                                            /\ UNCHANGED << go, rolled, rbm, 
                                                            rbOut, rbHalt, rc >>
                                       ELSE /\ IF Side(rc)
                                                  THEN /\ IF Live(rc)
                                                             THEN /\ rbHalt' = TRUE
                                                                  /\ rolled' = TRUE
                                                                  /\ pc' = [pc EXCEPT ![0] = "Done"]
                                                                  /\ rc' = rc
                                                             ELSE /\ rc' = rc - 1
                                                                  /\ pc' = [pc EXCEPT ![0] = "DRbStep"]
                                                                  /\ UNCHANGED << rolled, 
                                                                                  rbHalt >>
                                                       /\ UNCHANGED << go, rbm, 
                                                                       rbOut >>
                                                  ELSE /\ rbm' = [rbm EXCEPT ![rc] = TRUE]
                                                       /\ rbOut' = [rbOut EXCEPT ![rc] = "err"]
                                                       /\ go' = [go EXCEPT ![rc] = TRUE]
                                                       /\ pc' = [pc EXCEPT ![0] = "DRbWait"]
                                                       /\ UNCHANGED << rolled, 
                                                                       rbHalt, 
                                                                       rc >>
                                 /\ rbUnk' = rbUnk
           /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                           complete, pend, cancelled, gcancel, auth, werr, 
                           held, runRet, st, bg, ranc, tout, closedc, dl, ion, 
                           iarg, iret, iatt, iold, ost, obg, oran, fired, 
                           firedAt, began, everReached, lostAtt, compd, fx, 
                           boundHit, extras, errs, crashes, cancels, deadlines, 
                           guards, unks, toolErrs, wrongs, r, slot, last, cerr, 
                           rec, called, made, gret, r2, gc, r3 >>

DRbWait == /\ pc[0] = "DRbWait"
           /\ ~go[rc]
           /\ rbm' = [rbm EXCEPT ![rc] = FALSE]
           /\ IF rbOut[rc] = "unk"
                 THEN /\ rbUnk' = (rbUnk \cup {rc})
                      /\ pc' = [pc EXCEPT ![0] = "DRbNext"]
                 ELSE /\ IF rbOut[rc] # "ok"
                            THEN /\ pc' = [pc EXCEPT ![0] = "DOpen"]
                            ELSE /\ pc' = [pc EXCEPT ![0] = "DRbComp"]
                      /\ rbUnk' = rbUnk
           /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                           complete, pend, cancelled, gcancel, auth, go, werr, 
                           held, runRet, st, bg, ranc, tout, closedc, dl, ion, 
                           iarg, iret, iatt, iold, ost, obg, oran, fired, 
                           firedAt, began, everReached, lostAtt, rolled, rbOut, 
                           compd, rbHalt, fx, boundHit, extras, errs, crashes, 
                           cancels, deadlines, guards, unks, toolErrs, wrongs, 
                           r, slot, last, cerr, rec, called, made, gret, r2, 
                           gc, r3, rc >>

DRbComp == /\ pc[0] = "DRbComp"
           /\ IF ~compd[rc]
                 THEN /\ fx' = [fx EXCEPT ![rc] = FALSE]
                      /\ \/ /\ r3' = "ok"
                            /\ errs' = errs
                         \/ /\ errs < MaxErr
                            /\ errs' = errs + 1
                            /\ r3' = "err_nc"
                         \/ /\ errs < MaxErr
                            /\ errs' = errs + 1
                            /\ r3' = "err_c"
                      /\ IF r3' # "err_nc"
                            THEN /\ compd' = [compd EXCEPT ![rc] = TRUE]
                            ELSE /\ TRUE
                                 /\ compd' = compd
                      /\ IF r3' # "ok"
                            THEN /\ pc' = [pc EXCEPT ![0] = "DOpen"]
                            ELSE /\ pc' = [pc EXCEPT ![0] = "DRbNext"]
                 ELSE /\ pc' = [pc EXCEPT ![0] = "DRbNext"]
                      /\ UNCHANGED << compd, fx, errs, r3 >>
           /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                           complete, pend, cancelled, gcancel, auth, go, werr, 
                           held, runRet, st, bg, ranc, tout, closedc, dl, ion, 
                           iarg, iret, iatt, iold, ost, obg, oran, fired, 
                           firedAt, began, everReached, lostAtt, rolled, rbm, 
                           rbOut, rbHalt, rbUnk, boundHit, extras, crashes, 
                           cancels, deadlines, guards, unks, toolErrs, wrongs, 
                           r, slot, last, cerr, rec, called, made, gret, r2, 
                           gc, rc >>

DRbNext == /\ pc[0] = "DRbNext"
           /\ rc' = rc - 1
           /\ pc' = [pc EXCEPT ![0] = "DRbStep"]
           /\ UNCHANGED << marker, att, res, argsRec, ibeg, ibs, iear, 
                           complete, pend, cancelled, gcancel, auth, go, werr, 
                           held, runRet, st, bg, ranc, tout, closedc, dl, ion, 
                           iarg, iret, iatt, iold, ost, obg, oran, fired, 
                           firedAt, began, everReached, lostAtt, rolled, rbm, 
                           rbOut, compd, rbHalt, rbUnk, fx, boundHit, extras, 
                           errs, crashes, cancels, deadlines, guards, unks, 
                           toolErrs, wrongs, r, slot, last, cerr, rec, called, 
                           made, gret, r2, gc, r3 >>

driver == DOpen \/ DWait \/ DGate \/ DRollback \/ DRbStep \/ DRbWait
             \/ DRbComp \/ DRbNext

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == driver
           \/ (\E self \in Invs: inv(self))
           \/ (\E self \in Calls: loop(self))
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ \A self \in Invs : WF_vars(inv(self))
        /\ \A self \in Calls : WF_vars(loop(self))
        /\ WF_vars(driver)

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION
-----------------------------------------------------------------------------

\* A process crash: every goroutine dies (the drive, the calls, leaked invocations), and so does
\* pendingClaims. Any attempt live now loses the only record that it may not have started.
Crash ==
  /\ crashes < MaxCrash
  /\ pc[0] # "Done"
  /\ crashes' = crashes + 1
  /\ pc' = [p \in ProcSet |-> IF p = 0 THEN "DOpen" ELSE IF p \in Calls THEN "LIdle" ELSE "IWait"]
  /\ go' = [c \in Calls |-> FALSE]
  /\ ion' = [i \in Invs |-> FALSE]
  /\ iold' = [i \in Invs |-> FALSE]
  /\ pend' = [c \in Calls |-> 0]
  /\ rbm' = [c \in Calls |-> FALSE]
  /\ lostAtt' = [c \in Calls |-> lostAtt[c] \cup {a \in Atts : marker[c][a] = "live"}]
  /\ UNCHANGED <<marker, att, res, argsRec, complete, cancelled, gcancel, auth, werr, held,
                 runRet, st, bg, ranc, tout, closedc, dl, iarg, iret, iatt, ost, obg, oran,
                 fired, firedAt, began, everReached, rolled, boundHit, extras, errs, cancels,
                 deadlines, guards, unks, toolErrs, wrongs, r, slot, last, cerr, rec, called,
                 made, gret, r2, gc, r3, rbOut, compd, rbHalt, fx, rc, ibeg, rbUnk, ibs, iear>>

\* The run is cancelled while its calls run.
Cancel ==
  /\ cancels < MaxCancel /\ pc[0] \in {"DWait", "DRbWait"} /\ ~cancelled
  /\ cancels' = cancels + 1 /\ cancelled' = TRUE
  /\ UNCHANGED <<marker, att, res, argsRec, complete, pend, gcancel, auth, go, werr, held, runRet,
                 st, bg, ranc, tout, closedc, dl, ion, iarg, iret, iatt, iold, ost, obg, oran,
                 fired, firedAt, began, everReached, lostAtt, rolled, boundHit, extras, errs,
                 crashes, deadlines, guards, unks, toolErrs, wrongs, pc, r, slot, last, cerr,
                 rec, called, made, gret, r2, gc, r3, rbm, rbOut, compd, rbHalt, fx, rc, ibeg, rbUnk, ibs, iear>>

\* A call's ToolSpec.Timeout expires while its chain runs.
Deadline(c) ==
  /\ Timeout /\ deadlines < MaxDeadline /\ go[c] /\ pc[c] \in {"LMw", "LWait"} /\ ~dl[c]
  /\ deadlines' = deadlines + 1 /\ dl' = [dl EXCEPT ![c] = TRUE]
  /\ UNCHANGED <<marker, att, res, argsRec, complete, pend, cancelled, gcancel, auth, go, werr,
                 held, runRet, st, bg, ranc, tout, closedc, ion, iarg, iret, iatt, iold, ost,
                 obg, oran, fired, firedAt, began, everReached, lostAtt, rolled, boundHit, extras,
                 errs, crashes, cancels, guards, unks, toolErrs, wrongs, pc, r, slot, last, cerr,
                 rec, called, made, gret, r2, gc, r3, rbm, rbOut, compd, rbHalt, fx, rc, ibeg, rbUnk, ibs, iear>>

\* The delegation is resumed under other authority (an Unrecorded refusal), and an operator binds
\* the right one again (fair: a re-drive with the correct authority comes).
WrongAuth ==
  /\ wrongs < MaxWrongAuth /\ auth = "right"
  /\ wrongs' = wrongs + 1 /\ auth' = "wrong"
  /\ UNCHANGED <<marker, att, res, argsRec, complete, pend, cancelled, gcancel, go, werr, held,
                 runRet, st, bg, ranc, tout, closedc, dl, ion, iarg, iret, iatt, iold, ost, obg,
                 oran, fired, firedAt, began, everReached, lostAtt, rolled, boundHit, extras, errs,
                 crashes, cancels, deadlines, guards, unks, toolErrs, pc, r, slot, last, cerr, rec,
                 called, made, gret, r2, gc, r3, rbm, rbOut, compd, rbHalt, fx, rc, ibeg, rbUnk, ibs, iear>>
FixAuth ==
  /\ auth = "wrong" /\ auth' = "right"
  /\ UNCHANGED <<marker, att, res, argsRec, complete, pend, cancelled, gcancel, go, werr, held,
                 runRet, st, bg, ranc, tout, closedc, dl, ion, iarg, iret, iatt, iold, ost, obg,
                 oran, fired, firedAt, began, everReached, lostAtt, rolled, boundHit, extras, errs,
                 crashes, cancels, deadlines, guards, unks, toolErrs, wrongs, pc, r, slot, last,
                 cerr, rec, called, made, gret, r2, gc, r3, rbm, rbOut, compd, rbHalt, fx, rc, ibeg, rbUnk, ibs, iear>>

FullNext == Next \/ Crash \/ Cancel \/ (\E c \in Calls : Deadline(c)) \/ WrongAuth \/ FixAuth
\* Every goroutine's step is weakly fair (a drive that can re-drive does); faults have budgets and
\* no fairness, so every order in which they happen, then stop, is checked.
Fairness == /\ \A self \in Invs : WF_vars(inv(self))
            /\ \A self \in Calls : WF_vars(loop(self))
            /\ WF_vars(driver)
FullSpec == Init /\ [][FullNext]_vars /\ Fairness /\ WF_vars(FixAuth)

(***************************************************************************)
(* Properties                                                               *)
(***************************************************************************)

\* The effect fires at most once per claim across drives, and a call recorded as not started
\* (a voided attempt) or as a known failure never had its effect fire. For any call with an
\* effect (a retry-safe one may fire again by its declaration), no effect fires after the
\* rollback recorded its compensation.
NoDoubleFire ==
  /\ \A c \in Calls : Side(c) =>
       /\ fired[c] <= 1
       /\ res[c].k \in {"fail", "sagaFail"} => fired[c] = 0
       /\ \A a \in Atts : marker[c][a] = "void" => ~firedAt[c][a]
  /\ \A c \in Calls : compd[c] => ~fx[c]

\* A not-started record, or a failure recorded because the call was not called, means the tool
\* never began; a failure whose text says the tool already ran means it began.
TruthfulRecord ==
  \A c \in Calls : Side(c) =>
    /\ \A a \in Atts : marker[c][a] = "void" => ~began[c][a]
    /\ res[c].nc => ~began[c][res[c].a]
    /\ res[c].ran => began[c][res[c].a]

\* A drive that returns only an Unrecorded refusal has recorded every call whose tool began: no
\* sibling was cut off with an unknown outcome the error does not report.
NoLostSibling ==
  runRet = "unrec" =>
    \A c \in Calls : Side(c) /\ att[c] > 0 /\ began[c][att[c]] => res[c].k # "none"

\* After a saga's rollback, every effect still in place (a side effect, or a retry-safe call that
\* changes state) is listed as an unknown outcome, or the rollback halted at it or before it was
\* walked (a live marker): none is skipped as failed or unstarted, and the compensated are undone.
SagaAccounted ==
  rolled =>
    \A c \in Calls : fx[c] => res[c].k = "sagaUnk" \/ c \in rbUnk \/ (rbHalt /\ c <= rc)

\* Liveness: a failed saga's rollback reaches its end (SagaAborted, or a halt for a human).
RollbackEnds == [](SagaFailed => <>rolled)

\* The saga's accepted arguments are journaled only for a call that reached the base handler.
ArgsAfterReach == \A c \in Calls : argsRec[c] => everReached[c]

BoundNotHit == ~boundHit

\* Liveness: a side effect whose live attempt never began does not halt for ever, unless a crash
\* erased the process's record of that attempt.
StuckUnbegun(c) == Live(c) /\ ~began[c][att[c]] /\ att[c] \notin lostAtt[c]
NeverBegunProgress == \A c \in Calls : Side(c) => <>[](res[c].k # "none" \/ ~StuckUnbegun(c))

\* Liveness: a run whose only issue is an Unrecorded refusal completes once it is driven again
\* under the right authority.
UnrecordedContinues == <>complete

\* Vacuity, expected violated in every passing configuration.
EffectNotReachable == \A c \in Calls : fired[c] = 0
=============================================================================
