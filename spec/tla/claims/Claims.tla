------------------------------- MODULE Claims -------------------------------
(***************************************************************************)
(* Model 1 of docs/design/formal-models.md: the attempt-claim protocol that *)
(* keeps a side effect (a tool call that is not retry-safe, or a Step that  *)
(* is not) at most once across crashes, overlapping drivers and a store     *)
(* whose writes can fail ambiguously. spec/tla/README.md maps every label   *)
(* to the Go function it abstracts.                                         *)
(*                                                                          *)
(* The rules modelled (P6a, #92, after the third review):                   *)
(*  - every claim inserts its marker under a fresh claim id, so the         *)
(*    not-started key of a won claim is empty;                              *)
(*  - a failed marker Insert is followed by a not-started record under the  *)
(*    claim's own id; any failed not-started write leaves the id remembered *)
(*    in process (pendingClaims, a set of ids per marker key);              *)
(*  - a remembered id is never used to run: the next claim of that marker   *)
(*    key in the process retries its not-started record first, and then     *)
(*    claims with a fresh id (a voided marker moves it to the next numbered *)
(*    attempt);                                                             *)
(*  - the tool resume gate retries a remembered claim's not-started write   *)
(*    before it halts on a live marker;                                     *)
(*  - only a not-started record voids an attempt;                           *)
(*  - a Step loser joins the process's in-flight call or reads the result,  *)
(*    and never leads a flight;                                             *)
(*  - halt resolution refuses while a live driver may be running (#90).     *)
(*                                                                          *)
(* Bug selects one historical rule instead of the current one, for the      *)
(* regression configurations in regress/; "none" is the current protocol.   *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
  Drivers,       \* drives of the run (model values)
  Procs,         \* processes; drivers of one process share pendingClaims and flights
  Calls,         \* logical calls (a tool-use id or a Step name)
  ProcOf,        \* driver -> process
  CallOf,        \* driver -> the call it drives
  Kind,          \* call -> "tool" | "step" | "flow" (one marker key, ClaimAttempt: plan flows)
  PauseCalls,    \* Steps whose body fires and then pauses (stepPauseError)
  NextOf,        \* call -> the call the caller issues when it reads a failure, or "none"
  FirstCalls,    \* calls issued from the start
  MaxGen,        \* highest attempt number (attempt:retry:<n>:...)
  MaxIds,        \* size of the claim-id pool
  MaxAmbig,      \* budget of error replies (either kind)
  MaxCrash,      \* budget of process crashes
  MaxCancel,     \* budget of cancellations between a won claim and the call
  MaxEvict,      \* budget of pendingClaims evictions (maxPendingClaims), one marker key each
  LateCommit,    \* TRUE: an errored write may commit at any later time (weak A3)
  HasResolver,   \* whether an operator resolves halts (ResolveHaltRef)
  LiveCheck,     \* "lease" | "minAge" | "none" (WithoutLiveDriverCheck)
  LeasedDrivers, \* drivers that hold the run's lease while they drive
  ResolveClaim,  \* TRUE: the resolver first claims the attempt after the live one (F2's fix)
  ResolverProc,  \* the process the resolver runs in (sharing its pendingClaims and flights), or "none"
  PlainRunIdleAtCheck, \* TRUE: under the lease check, assume no unleased driver holds the live
                       \* claim when the resolution checks (isolates finding F4 from #90's known limit)
  ResolveVoidOnError, \* TRUE: a resolution whose result write errored records its own attempt as not
                      \* started (#92 at 06408db); FALSE: it leaves that attempt live (finding F3's fix)
  Bug            \* "none" or a historical rule, see the regression configs

None == "none"
Gens == 0..MaxGen
Ids  == 1..MaxIds
ResolverSet == IF HasResolver THEN {"resolver"} ELSE {}
Halts == {"halt_crashed", "halt_contended"}

ASSUME MaxGen \in Nat /\ MaxIds \in Nat /\ MaxAmbig \in Nat /\ MaxCrash \in Nat /\ MaxCancel \in Nat
ASSUME LiveCheck \in {"lease", "minAge", "none"}
ASSUME ResolveClaim \in BOOLEAN /\ LateCommit \in BOOLEAN /\ HasResolver \in BOOLEAN
ASSUME Bug \in {"none", "ReuseNoHold", "HeldPin", "GateNoRetry", "NoMemoAfterCall",
                "MemoOverwrite", "LoserLeads", "PauseAsFailure"}
ASSUME \A c \in Calls : Kind[c] \in {"tool", "step", "flow"}
ASSUME MaxEvict \in Nat /\ ResolverProc \in Procs \cup {"none"} /\ ResolveVoidOnError \in BOOLEAN
ASSUME PlainRunIdleAtCheck \in BOOLEAN
ASSUME PauseCalls \subseteq {c \in Calls : Kind[c] = "step"}

(* --algorithm claims
variables
  \* The journal. marker[c][x]: the claim id stored under call c's attempt x, 0 when absent.
  marker     = [c \in Calls |-> [x \in Gens |-> 0]],
  \* <<c, x, i>> \in nsSet: attempt:not-started:<i>:<marker key of c, x> holds a not-started record.
  nsSet      = {},
  \* <<c, x, i>> \in heldSet: the same key holds a claim-held record (Bug = "HeldPin" only).
  heldSet    = {},
  \* The call's result key: None, "drv" (the driver's record), "res_ok" or "res_err" (a resolution).
  result     = [c \in Calls |-> None],
  \* The parent tool call's recorded failure (Bug = "PauseAsFailure" only).
  toolFail   = [c \in Calls |-> FALSE],
  \* Writes that returned an error and may still commit (LateCommit).
  lateMarker = {}, lateNS = {}, lateResult = {},
  \* Process state. pending[p][c][x]: the claim ids process p remembers for that marker key.
  pending    = [p \in Procs |-> [c \in Calls |-> [x \in Gens |-> {}]]],
  \* fl[p][c]: the driver leading p's in-flight call of c's result key; waiters: who joined it.
  fl         = [p \in Procs |-> [c \in Calls |-> None]],
  waiters    = [p \in Procs |-> [c \in Calls |-> {}]],
  jres       = [d \in Drivers \cup ResolverSet |-> None], \* the outcome a joined flight handed to d
  cid        = [d \in Drivers |-> 0],        \* d's current claim id
  oldId      = [d \in Drivers |-> 0],        \* the remembered id the resume gate took back
  toRetry    = [d \in Drivers \cup ResolverSet |-> {}], \* remembered ids a claim took back (takeAll)
  lease      = None,
  rids       = {},                          \* claim ids the resolver used (ResolveClaim)
  rcid       = 0,                           \* the resolver's current claim id

  \* Fault budgets used.
  ambig = 0, crashes = 0, cancels = 0, evictions = 0,
  \* Ghosts: effect calls per call, the claim each fired under per attempt, and the claim ids
  \* whose holder's knowledge a crash erased.
  fired      = [c \in Calls |-> 0],
  firedAt    = [c \in Calls |-> [x \in Gens |-> 0]],
  lost       = {},
  \* Ghost: the claim ids an eviction from pendingClaims forgot.
  evicted    = {};

define
  Voided(c, x)  == marker[c][x] # 0 /\ <<c, x, marker[c][x]>> \in nsSet
  Live(c, x)    == marker[c][x] # 0 /\ ~Voided(c, x)
  AnyLive(c)    == \E x \in Gens : Live(c, x)
  NSTaken(c, x, i) == <<c, x, i>> \in nsSet \/ <<c, x, i>> \in heldSet
  \* The call's recorded outcome, as the caller and a re-drive read it.
  Recorded(c)   == result[c] # None \/ toolFail[c]
  \* The caller issues a call once the call before it reads as failed.
  Issued(c)     == c \in FirstCalls
                   \/ \E c0 \in Calls : NextOf[c0] = c /\ (result[c0] = "res_err" \/ toolFail[c0])
  \* Canonical fresh names: the smallest claim id nothing in the state refers to.
  Used == {marker[c][x] : c \in Calls, x \in Gens}
          \cup {k[3] : k \in nsSet} \cup {k[3] : k \in heldSet}
          \cup UNION {pending[p][c][x] : p \in Procs, c \in Calls, x \in Gens}
          \cup {cid[d] : d \in Drivers} \cup {oldId[d] : d \in Drivers}
          \cup {k[3] : k \in lateMarker} \cup {k[3] : k \in lateNS} \cup lost \cup rids
          \cup {rcid} \cup UNION {toRetry[d] : d \in DOMAIN toRetry} \cup evicted
  Free    == Ids \ Used
  Min(S)  == CHOOSE i \in S : \A j \in S : i <= j
  MinFree == Min(Free)
  \* The steps of a driver that holds a claim (won, or errored and being recorded as not
  \* started) and has not finished with it: the drive WithMinHaltAge assumes is over.
  Window  == {"ClaimNS", "Hold", "Win", "WinnerWait", "Call", "Record", "NotStarted"}
end define;

\* One Store.Insert reply: ok, error not committed, error committed (A3 at return), or, under
\* LateCommit, error with a commit at any later time or never.
macro Reply(r) begin
  either r := "ok";
  or await ambig < MaxAmbig; ambig := ambig + 1; r := "err_nc";
  or await ambig < MaxAmbig; ambig := ambig + 1; r := "err_c";
  or await LateCommit /\ ambig < MaxAmbig; ambig := ambig + 1; r := "err_late";
  end either;
end macro;

\* pendingClaims.remember: add the id to the ids process p remembers for the marker key
\* (historically, and under Bug = "MemoOverwrite", replace them).
macro RememberIn(p, c, x, i) begin
  pending[p][c][x] := IF Bug = "MemoOverwrite" THEN {i} ELSE pending[p][c][x] \cup {i};
end macro;

macro Remember(x, i) begin
  RememberIn(ProcOf[self], CallOf[self], x, i);
end macro;

\* The first writer of a key wins (A1).
macro WriteMarker(c, x, i, r) begin
  if r \in {"ok", "err_c"} then
    if marker[c][x] = 0 then marker[c][x] := i; end if;
  elsif r = "err_late" then
    lateMarker := lateMarker \cup {<<c, x, i>>};
  end if;
end macro;

macro WriteNS(c, x, i, r) begin
  if r \in {"ok", "err_c"} then
    if ~NSTaken(c, x, i) then nsSet := nsSet \cup {<<c, x, i>>}; end if;
  elsif r = "err_late" then
    lateNS := lateNS \cup {<<c, x, i>>};
  end if;
end macro;

macro WriteResult(c, v, r) begin
  if r \in {"ok", "err_c"} then
    if result[c] = None then result[c] := v; end if;
  elsif r = "err_late" then
    lateResult := lateResult \cup {<<c, v>>};
  end if;
end macro;

\* shareFlight's return: hand the outcome to every caller that joined the flight.
macro EndFlightIn(p, c, v) begin
  fl[p][c] := None;
  jres := [x \in DOMAIN jres |-> IF x \in waiters[p][c] THEN v ELSE jres[x]];
  waiters[p][c] := {};
end macro;

macro EndFlight(v) begin
  EndFlightIn(ProcOf[self], CallOf[self], v);
end macro;

fair process driver \in Drivers
variables g = 0, gg = 0, reply = "", outcome = None, won = FALSE, reused = FALSE;
begin
Start:
  await Issued(CallOf[self]);
Open:
  \* One Load. A leased driver holds the run's lease for the whole drive.
  if self \in LeasedDrivers then
    await lease \in {None, self};
    lease := self;
  end if;
  won := FALSE; reused := FALSE; g := 0; cid[self] := 0;
  if Recorded(CallOf[self]) then
    outcome := "done"; goto Finish;
  elsif Kind[CallOf[self]] = "flow" /\ marker[CallOf[self]][0] # 0 then
    \* A flow node that is not retry-safe halts on any marker of its step (plan's case 2).
    outcome := "halt_crashed"; goto Finish;
  elsif Kind[CallOf[self]] = "tool" /\ AnyLive(CallOf[self]) then
    \* The resume gate: a live marker halts, unless this process remembers its claim.
    with x = CHOOSE x \in Gens : Live(CallOf[self], x) do
      gg := x; oldId[self] := marker[CallOf[self]][x];
    end with;
    outcome := None; goto GateTake;
  else
    outcome := None; goto Claim;
  end if;
GateTake:
  if Bug # "GateNoRetry" /\ oldId[self] \in pending[ProcOf[self]][CallOf[self]][gg] then
    pending[ProcOf[self]][CallOf[self]][gg] := pending[ProcOf[self]][CallOf[self]][gg] \ {oldId[self]};
  else
    oldId[self] := 0; outcome := "halt_crashed"; goto Finish;
  end if;
GateWrite:
  Reply(reply);
  WriteNS(CallOf[self], gg, oldId[self], reply);
  if reply # "ok" then
    Remember(gg, oldId[self]);
    oldId[self] := 0; outcome := "halt_crashed"; goto Finish;
  else
    oldId[self] := 0; goto Claim;
  end if;
Claim:
  \* claimNext at attempt g: take back every id the process remembers for this marker key.
  await g <= MaxGen;
  if pending[ProcOf[self]][CallOf[self]][g] # {} then
    if Bug \in {"ReuseNoHold", "HeldPin"} then
      \* Historical: one remembered id is reused for the claim itself.
      with i = Min(pending[ProcOf[self]][CallOf[self]][g]) do
        pending[ProcOf[self]][CallOf[self]][g] := pending[ProcOf[self]][CallOf[self]][g] \ {i};
        cid[self] := i; reused := TRUE;
      end with;
      goto ClaimInsert;
    else
      toRetry[self] := pending[ProcOf[self]][CallOf[self]][g];
      pending[ProcOf[self]][CallOf[self]][g] := {};
      goto ClaimRetry;
    end if;
  else
    reused := FALSE; goto ClaimInsert;
  end if;
ClaimRetry:
  \* Journal.claim writes each taken id's not-started record again, voiding its marker if that
  \* committed; a failed write remembers the id again (Journal.notStarted), and the claim goes on.
  with i = Min(toRetry[self]) do
    Reply(reply);
    WriteNS(CallOf[self], g, i, reply);
    if reply # "ok" then Remember(g, i); end if;
    toRetry[self] := toRetry[self] \ {i};
    if toRetry[self] \ {i} = {} then goto ClaimInsert; else goto ClaimRetry; end if;
  end with;
ClaimInsert:
  \* Insert the marker under a fresh claim id (or, historically, the reused one).
  await reused \/ Free # {};
  with i = IF reused THEN cid[self] ELSE MinFree do
    cid[self] := i;
    Reply(reply);
    WriteMarker(CallOf[self], g, i, reply);
    if reply # "ok" then
      goto ClaimNS;
    elsif marker[CallOf[self]][g] \in {0, i} then
      if reused /\ Bug = "HeldPin" then
        goto Hold;
      else
        won := TRUE; goto Win;
      end if;
    else
      goto Lost;
    end if;
  end with;
ClaimNS:
  \* The marker may have committed: record that this claim never called the effect.
  Reply(reply);
  WriteNS(CallOf[self], g, cid[self], reply);
  if reply # "ok" then
    Remember(g, cid[self]);
  end if;
  outcome := "error"; goto Finish;
Hold:
  \* Historical (Bug = "HeldPin"): pin the reused id's not-started key with a claim-held record.
  Reply(reply);
  if reply \in {"ok", "err_c"} /\ ~NSTaken(CallOf[self], g, cid[self]) then
    heldSet := heldSet \cup {<<CallOf[self], g, cid[self]>>};
  end if;
  if reply # "ok" then
    Remember(g, cid[self]);
    outcome := "error"; goto Finish;
  elsif <<CallOf[self], g, cid[self]>> \in nsSet then
    goto Lost;
  else
    won := TRUE; goto Win;
  end if;
Lost:
  if Kind[CallOf[self]] = "flow" then
    \* ClaimAttempt claims one key: a lost claim halts, voided or not.
    outcome := "halt_contended"; goto Finish;
  elsif Voided(CallOf[self], g) then
    g := g + 1; goto Claim;
  elsif Kind[CallOf[self]] = "tool" then
    outcome := "halt_contended"; goto Finish;
  end if;
Join:
  \* The Step loser: join the process's in-flight call, else read the result, else halt.
  if fl[ProcOf[self]][CallOf[self]] # None then
    waiters[ProcOf[self]][CallOf[self]] := waiters[ProcOf[self]][CallOf[self]] \cup {self};
    goto LoserWait;
  elsif Bug = "LoserLeads" then
    fl[ProcOf[self]][CallOf[self]] := self;
    goto LoserLead;
  end if;
LoserRead:
  \* No call in flight and no result: the halt names no live claimant (HaltCrashed).
  if result[CallOf[self]] # None then outcome := "done"; else outcome := "halt_crashed"; end if;
  goto Finish;
LoserLead:
  \* Historical (Bug = "LoserLeads"): the loser's read ran as a flight others could join.
  EndFlight(IF result[CallOf[self]] # None THEN "ok" ELSE "halt");
  if result[CallOf[self]] # None then outcome := "done"; else outcome := "halt_contended"; end if;
  goto Finish;
LoserWait:
  \* A joined call that fails is a halt on a live claimant (HaltContended).
  await jres[self] # None;
  if jres[self] = "ok" then outcome := "done"; else outcome := "halt_contended"; end if;
  jres[self] := None;
  goto Finish;
Win:
  \* doFresh: shareFlight on the result key. A flight already in the process is joined.
  if fl[ProcOf[self]][CallOf[self]] # None then
    waiters[ProcOf[self]][CallOf[self]] := waiters[ProcOf[self]][CallOf[self]] \cup {self};
    goto WinnerWait;
  else
    fl[ProcOf[self]][CallOf[self]] := self;
    goto Call;
  end if;
WinnerWait:
  await jres[self] # None;
  if jres[self] = "ok" then
    outcome := "done"; jres[self] := None; goto Finish;
  else
    \* This driver never called the effect: it records that below.
    outcome := IF jres[self] = "halt" THEN "halt_contended" ELSE "error";
    jres[self] := None; goto NotStarted;
  end if;
Call:
  either
    \* Cancelled after the claim and before the call: the effect is not called.
    await cancels < MaxCancel;
    cancels := cancels + 1;
    EndFlight("err");
    outcome := "error";
    \* A flow node's body error records nothing: its marker stays, and the node halts.
    if Kind[CallOf[self]] = "flow" then goto Finish; else goto NotStarted; end if;
  or
    fired[CallOf[self]] := fired[CallOf[self]] + 1;
    firedAt[CallOf[self]][g] := cid[self];
    if CallOf[self] \in PauseCalls then
      \* The body fired and then paused: Step returns stepPauseError and records nothing.
      EndFlight("err");
      outcome := "pause";
      if Bug = "PauseAsFailure" then toolFail[CallOf[self]] := TRUE; end if;
      goto Finish;
    end if;
  end either;
Record:
  Reply(reply);
  WriteResult(CallOf[self], "drv", reply);
  EndFlight(IF reply = "ok" THEN "ok" ELSE "err");
  if reply = "ok" then outcome := "done"; else outcome := "error"; end if;
  goto Finish;
NotStarted:
  \* recordNotStarted -> Journal.notStarted, which remembers the id when the write fails.
  Reply(reply);
  WriteNS(CallOf[self], g, cid[self], reply);
  if reply # "ok" /\ Bug # "NoMemoAfterCall" then
    Remember(g, cid[self]);
  end if;
Finish:
  \* A drive that did not end with the call's outcome is driven again (Recover).
  if lease = self then lease := None; end if;
  if outcome # "done" then goto Open; end if;
end process;

fair process resolver \in ResolverSet
variables rc = None, rg = 0, rreply = "", rclaimed = FALSE;
begin
RCheck:-
  \* ResolveHaltRef on a halted call: checkNoLiveDriver, then History and the age check.
  with c \in Calls, x \in Gens do
    await Kind[c] # "flow" /\ result[c] = None /\ Live(c, x);
    await LiveCheck = "lease" => lease = None;
    await LiveCheck = "lease" /\ PlainRunIdleAtCheck =>
            \A d \in Drivers : ~(CallOf[d] = c /\ cid[d] = marker[c][x] /\ pc[d] \in Window);
    \* WithMinHaltAge, as an assumption: a drive holding the live claim is over before the
    \* minimum age has passed. A remembered claim (pendingClaims) is not a drive: its process
    \* may retry its not-started record at any later time.
    await LiveCheck = "minAge" =>
            \A d \in Drivers : ~(CallOf[d] = c /\ cid[d] = marker[c][x] /\ pc[d] \in Window);
    if LiveCheck = "lease" then lease := self; end if;
    rc := c; rg := x;
  end with;
  if ~ResolveClaim then goto RWrite; end if;
RClaim:
  \* F2's fix: ClaimAttempt on the attempt after the live one (on the lease path too: F4's fix). Through a Journal in a driver's
  \* process, the claim first retries the ids that process remembers for that key.
  await rg + 1 <= MaxGen;
  if ResolverProc # "none" /\ pending[ResolverProc][rc][rg + 1] # {} then
    toRetry[self] := pending[ResolverProc][rc][rg + 1];
    pending[ResolverProc][rc][rg + 1] := {};
  else
    goto RInsert;
  end if;
RRetry:
  with i = Min(toRetry[self]) do
    Reply(rreply);
    WriteNS(rc, rg + 1, i, rreply);
    if rreply # "ok" then RememberIn(ResolverProc, rc, rg + 1, i); end if;
    toRetry[self] := toRetry[self] \ {i};
    if toRetry[self] \ {i} = {} then goto RInsert; else goto RRetry; end if;
  end with;
RInsert:
  await Free # {};
  with i = MinFree do
    rcid := i; rids := rids \cup {i};
    Reply(rreply);
    WriteMarker(rc, rg + 1, i, rreply);
    if rreply # "ok" then
      goto RClaimNS;
    elsif marker[rc][rg + 1] \notin {0, i} then
      goto RRelease;                        \* a driver holds it: HaltInFlight
    else
      rclaimed := TRUE; goto RWrite;
    end if;
  end with;
RClaimNS:
  Reply(rreply);
  WriteNS(rc, rg + 1, rcid, rreply);
  if rreply # "ok" /\ ResolverProc # "none" then RememberIn(ResolverProc, rc, rg + 1, rcid); end if;
  goto RRelease;
RWrite:
  \* store.Do on the result key: in a driver's process, shareFlight joins or leads that
  \* process's in-flight call of the key.
  if ResolverProc # "none" /\ fl[ResolverProc][rc] # None then
    waiters[ResolverProc][rc] := waiters[ResolverProc][rc] \cup {self};
    goto RWait;
  elsif ResolverProc # "none" then
    fl[ResolverProc][rc] := self;
  end if;
RRecord:
  \* The operator's verdict is true when written: charged if the effect has fired.
  Reply(rreply);
  WriteResult(rc, IF fired[rc] > 0 THEN "res_ok" ELSE "res_err", rreply);
  if ResolverProc # "none" then
    EndFlightIn(ResolverProc, rc, IF rreply = "ok" THEN "ok" ELSE "err");
  end if;
  if rreply # "ok" /\ rclaimed /\ ResolveVoidOnError then goto RNotStarted; else goto RRelease; end if;
RWait:
  await jres[self] # None;
  if jres[self] # "ok" /\ rclaimed /\ ResolveVoidOnError then
    jres[self] := None; goto RNotStarted;
  else
    jres[self] := None; goto RRelease;
  end if;
RNotStarted:
  \* "Nothing was resolved": the resolution's own attempt is recorded as never started. But the
  \* errored result write may have committed, and a driver in its claim loop then re-attempts
  \* past the voided attempt and fires under a recorded resolution (finding F3).
  Reply(rreply);
  WriteNS(rc, rg + 1, rcid, rreply);
  if rreply # "ok" /\ ResolverProc # "none" then RememberIn(ResolverProc, rc, rg + 1, rcid); end if;
RRelease:
  if lease = self then lease := None; end if;
end process;
end algorithm; *)
\* BEGIN TRANSLATION
VARIABLES marker, nsSet, heldSet, result, toolFail, lateMarker, lateNS, 
          lateResult, pending, fl, waiters, jres, cid, oldId, toRetry, lease, 
          rids, rcid, ambig, crashes, cancels, evictions, fired, firedAt, 
          lost, evicted, pc

(* define statement *)
Voided(c, x)  == marker[c][x] # 0 /\ <<c, x, marker[c][x]>> \in nsSet
Live(c, x)    == marker[c][x] # 0 /\ ~Voided(c, x)
AnyLive(c)    == \E x \in Gens : Live(c, x)
NSTaken(c, x, i) == <<c, x, i>> \in nsSet \/ <<c, x, i>> \in heldSet

Recorded(c)   == result[c] # None \/ toolFail[c]

Issued(c)     == c \in FirstCalls
                 \/ \E c0 \in Calls : NextOf[c0] = c /\ (result[c0] = "res_err" \/ toolFail[c0])

Used == {marker[c][x] : c \in Calls, x \in Gens}
        \cup {k[3] : k \in nsSet} \cup {k[3] : k \in heldSet}
        \cup UNION {pending[p][c][x] : p \in Procs, c \in Calls, x \in Gens}
        \cup {cid[d] : d \in Drivers} \cup {oldId[d] : d \in Drivers}
        \cup {k[3] : k \in lateMarker} \cup {k[3] : k \in lateNS} \cup lost \cup rids
        \cup {rcid} \cup UNION {toRetry[d] : d \in DOMAIN toRetry} \cup evicted
Free    == Ids \ Used
Min(S)  == CHOOSE i \in S : \A j \in S : i <= j
MinFree == Min(Free)


Window  == {"ClaimNS", "Hold", "Win", "WinnerWait", "Call", "Record", "NotStarted"}

VARIABLES g, gg, reply, outcome, won, reused, rc, rg, rreply, rclaimed

vars == << marker, nsSet, heldSet, result, toolFail, lateMarker, lateNS, 
           lateResult, pending, fl, waiters, jres, cid, oldId, toRetry, lease, 
           rids, rcid, ambig, crashes, cancels, evictions, fired, firedAt, 
           lost, evicted, pc, g, gg, reply, outcome, won, reused, rc, rg, 
           rreply, rclaimed >>

ProcSet == (Drivers) \cup (ResolverSet)

Init == (* Global variables *)
        /\ marker = [c \in Calls |-> [x \in Gens |-> 0]]
        /\ nsSet = {}
        /\ heldSet = {}
        /\ result = [c \in Calls |-> None]
        /\ toolFail = [c \in Calls |-> FALSE]
        /\ lateMarker = {}
        /\ lateNS = {}
        /\ lateResult = {}
        /\ pending = [p \in Procs |-> [c \in Calls |-> [x \in Gens |-> {}]]]
        /\ fl = [p \in Procs |-> [c \in Calls |-> None]]
        /\ waiters = [p \in Procs |-> [c \in Calls |-> {}]]
        /\ jres = [d \in Drivers \cup ResolverSet |-> None]
        /\ cid = [d \in Drivers |-> 0]
        /\ oldId = [d \in Drivers |-> 0]
        /\ toRetry = [d \in Drivers \cup ResolverSet |-> {}]
        /\ lease = None
        /\ rids = {}
        /\ rcid = 0
        /\ ambig = 0
        /\ crashes = 0
        /\ cancels = 0
        /\ evictions = 0
        /\ fired = [c \in Calls |-> 0]
        /\ firedAt = [c \in Calls |-> [x \in Gens |-> 0]]
        /\ lost = {}
        /\ evicted = {}
        (* Process driver *)
        /\ g = [self \in Drivers |-> 0]
        /\ gg = [self \in Drivers |-> 0]
        /\ reply = [self \in Drivers |-> ""]
        /\ outcome = [self \in Drivers |-> None]
        /\ won = [self \in Drivers |-> FALSE]
        /\ reused = [self \in Drivers |-> FALSE]
        (* Process resolver *)
        /\ rc = [self \in ResolverSet |-> None]
        /\ rg = [self \in ResolverSet |-> 0]
        /\ rreply = [self \in ResolverSet |-> ""]
        /\ rclaimed = [self \in ResolverSet |-> FALSE]
        /\ pc = [self \in ProcSet |-> CASE self \in Drivers -> "Start"
                                        [] self \in ResolverSet -> "RCheck"]

Start(self) == /\ pc[self] = "Start"
               /\ Issued(CallOf[self])
               /\ pc' = [pc EXCEPT ![self] = "Open"]
               /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                               lateMarker, lateNS, lateResult, pending, fl, 
                               waiters, jres, cid, oldId, toRetry, lease, rids, 
                               rcid, ambig, crashes, cancels, evictions, fired, 
                               firedAt, lost, evicted, g, gg, reply, outcome, 
                               won, reused, rc, rg, rreply, rclaimed >>

Open(self) == /\ pc[self] = "Open"
              /\ IF self \in LeasedDrivers
                    THEN /\ lease \in {None, self}
                         /\ lease' = self
                    ELSE /\ TRUE
                         /\ lease' = lease
              /\ won' = [won EXCEPT ![self] = FALSE]
              /\ reused' = [reused EXCEPT ![self] = FALSE]
              /\ g' = [g EXCEPT ![self] = 0]
              /\ cid' = [cid EXCEPT ![self] = 0]
              /\ IF Recorded(CallOf[self])
                    THEN /\ outcome' = [outcome EXCEPT ![self] = "done"]
                         /\ pc' = [pc EXCEPT ![self] = "Finish"]
                         /\ UNCHANGED << oldId, gg >>
                    ELSE /\ IF Kind[CallOf[self]] = "flow" /\ marker[CallOf[self]][0] # 0
                               THEN /\ outcome' = [outcome EXCEPT ![self] = "halt_crashed"]
                                    /\ pc' = [pc EXCEPT ![self] = "Finish"]
                                    /\ UNCHANGED << oldId, gg >>
                               ELSE /\ IF Kind[CallOf[self]] = "tool" /\ AnyLive(CallOf[self])
                                          THEN /\ LET x == CHOOSE x \in Gens : Live(CallOf[self], x) IN
                                                    /\ gg' = [gg EXCEPT ![self] = x]
                                                    /\ oldId' = [oldId EXCEPT ![self] = marker[CallOf[self]][x]]
                                               /\ outcome' = [outcome EXCEPT ![self] = None]
                                               /\ pc' = [pc EXCEPT ![self] = "GateTake"]
                                          ELSE /\ outcome' = [outcome EXCEPT ![self] = None]
                                               /\ pc' = [pc EXCEPT ![self] = "Claim"]
                                               /\ UNCHANGED << oldId, gg >>
              /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                              lateMarker, lateNS, lateResult, pending, fl, 
                              waiters, jres, toRetry, rids, rcid, ambig, 
                              crashes, cancels, evictions, fired, firedAt, 
                              lost, evicted, reply, rc, rg, rreply, rclaimed >>

GateTake(self) == /\ pc[self] = "GateTake"
                  /\ IF Bug # "GateNoRetry" /\ oldId[self] \in pending[ProcOf[self]][CallOf[self]][gg[self]]
                        THEN /\ pending' = [pending EXCEPT ![ProcOf[self]][CallOf[self]][gg[self]] = pending[ProcOf[self]][CallOf[self]][gg[self]] \ {oldId[self]}]
                             /\ pc' = [pc EXCEPT ![self] = "GateWrite"]
                             /\ UNCHANGED << oldId, outcome >>
                        ELSE /\ oldId' = [oldId EXCEPT ![self] = 0]
                             /\ outcome' = [outcome EXCEPT ![self] = "halt_crashed"]
                             /\ pc' = [pc EXCEPT ![self] = "Finish"]
                             /\ UNCHANGED pending
                  /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                  lateMarker, lateNS, lateResult, fl, waiters, 
                                  jres, cid, toRetry, lease, rids, rcid, ambig, 
                                  crashes, cancels, evictions, fired, firedAt, 
                                  lost, evicted, g, gg, reply, won, reused, rc, 
                                  rg, rreply, rclaimed >>

GateWrite(self) == /\ pc[self] = "GateWrite"
                   /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                         /\ ambig' = ambig
                      \/ /\ ambig < MaxAmbig
                         /\ ambig' = ambig + 1
                         /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                      \/ /\ ambig < MaxAmbig
                         /\ ambig' = ambig + 1
                         /\ reply' = [reply EXCEPT ![self] = "err_c"]
                      \/ /\ LateCommit /\ ambig < MaxAmbig
                         /\ ambig' = ambig + 1
                         /\ reply' = [reply EXCEPT ![self] = "err_late"]
                   /\ IF reply'[self] \in {"ok", "err_c"}
                         THEN /\ IF ~NSTaken((CallOf[self]), gg[self], (oldId[self]))
                                    THEN /\ nsSet' = (nsSet \cup {<<(CallOf[self]), gg[self], (oldId[self])>>})
                                    ELSE /\ TRUE
                                         /\ nsSet' = nsSet
                              /\ UNCHANGED lateNS
                         ELSE /\ IF reply'[self] = "err_late"
                                    THEN /\ lateNS' = (lateNS \cup {<<(CallOf[self]), gg[self], (oldId[self])>>})
                                    ELSE /\ TRUE
                                         /\ UNCHANGED lateNS
                              /\ nsSet' = nsSet
                   /\ IF reply'[self] # "ok"
                         THEN /\ pending' = [pending EXCEPT ![(ProcOf[self])][(CallOf[self])][gg[self]] = IF Bug = "MemoOverwrite" THEN {(oldId[self])} ELSE pending[(ProcOf[self])][(CallOf[self])][gg[self]] \cup {(oldId[self])}]
                              /\ oldId' = [oldId EXCEPT ![self] = 0]
                              /\ outcome' = [outcome EXCEPT ![self] = "halt_crashed"]
                              /\ pc' = [pc EXCEPT ![self] = "Finish"]
                         ELSE /\ oldId' = [oldId EXCEPT ![self] = 0]
                              /\ pc' = [pc EXCEPT ![self] = "Claim"]
                              /\ UNCHANGED << pending, outcome >>
                   /\ UNCHANGED << marker, heldSet, result, toolFail, 
                                   lateMarker, lateResult, fl, waiters, jres, 
                                   cid, toRetry, lease, rids, rcid, crashes, 
                                   cancels, evictions, fired, firedAt, lost, 
                                   evicted, g, gg, won, reused, rc, rg, rreply, 
                                   rclaimed >>

Claim(self) == /\ pc[self] = "Claim"
               /\ g[self] <= MaxGen
               /\ IF pending[ProcOf[self]][CallOf[self]][g[self]] # {}
                     THEN /\ IF Bug \in {"ReuseNoHold", "HeldPin"}
                                THEN /\ LET i == Min(pending[ProcOf[self]][CallOf[self]][g[self]]) IN
                                          /\ pending' = [pending EXCEPT ![ProcOf[self]][CallOf[self]][g[self]] = pending[ProcOf[self]][CallOf[self]][g[self]] \ {i}]
                                          /\ cid' = [cid EXCEPT ![self] = i]
                                          /\ reused' = [reused EXCEPT ![self] = TRUE]
                                     /\ pc' = [pc EXCEPT ![self] = "ClaimInsert"]
                                     /\ UNCHANGED toRetry
                                ELSE /\ toRetry' = [toRetry EXCEPT ![self] = pending[ProcOf[self]][CallOf[self]][g[self]]]
                                     /\ pending' = [pending EXCEPT ![ProcOf[self]][CallOf[self]][g[self]] = {}]
                                     /\ pc' = [pc EXCEPT ![self] = "ClaimRetry"]
                                     /\ UNCHANGED << cid, reused >>
                     ELSE /\ reused' = [reused EXCEPT ![self] = FALSE]
                          /\ pc' = [pc EXCEPT ![self] = "ClaimInsert"]
                          /\ UNCHANGED << pending, cid, toRetry >>
               /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                               lateMarker, lateNS, lateResult, fl, waiters, 
                               jres, oldId, lease, rids, rcid, ambig, crashes, 
                               cancels, evictions, fired, firedAt, lost, 
                               evicted, g, gg, reply, outcome, won, rc, rg, 
                               rreply, rclaimed >>

ClaimRetry(self) == /\ pc[self] = "ClaimRetry"
                    /\ LET i == Min(toRetry[self]) IN
                         /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                               /\ ambig' = ambig
                            \/ /\ ambig < MaxAmbig
                               /\ ambig' = ambig + 1
                               /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                            \/ /\ ambig < MaxAmbig
                               /\ ambig' = ambig + 1
                               /\ reply' = [reply EXCEPT ![self] = "err_c"]
                            \/ /\ LateCommit /\ ambig < MaxAmbig
                               /\ ambig' = ambig + 1
                               /\ reply' = [reply EXCEPT ![self] = "err_late"]
                         /\ IF reply'[self] \in {"ok", "err_c"}
                               THEN /\ IF ~NSTaken((CallOf[self]), g[self], i)
                                          THEN /\ nsSet' = (nsSet \cup {<<(CallOf[self]), g[self], i>>})
                                          ELSE /\ TRUE
                                               /\ nsSet' = nsSet
                                    /\ UNCHANGED lateNS
                               ELSE /\ IF reply'[self] = "err_late"
                                          THEN /\ lateNS' = (lateNS \cup {<<(CallOf[self]), g[self], i>>})
                                          ELSE /\ TRUE
                                               /\ UNCHANGED lateNS
                                    /\ nsSet' = nsSet
                         /\ IF reply'[self] # "ok"
                               THEN /\ pending' = [pending EXCEPT ![(ProcOf[self])][(CallOf[self])][g[self]] = IF Bug = "MemoOverwrite" THEN {i} ELSE pending[(ProcOf[self])][(CallOf[self])][g[self]] \cup {i}]
                               ELSE /\ TRUE
                                    /\ UNCHANGED pending
                         /\ toRetry' = [toRetry EXCEPT ![self] = toRetry[self] \ {i}]
                         /\ IF toRetry'[self] \ {i} = {}
                               THEN /\ pc' = [pc EXCEPT ![self] = "ClaimInsert"]
                               ELSE /\ pc' = [pc EXCEPT ![self] = "ClaimRetry"]
                    /\ UNCHANGED << marker, heldSet, result, toolFail, 
                                    lateMarker, lateResult, fl, waiters, jres, 
                                    cid, oldId, lease, rids, rcid, crashes, 
                                    cancels, evictions, fired, firedAt, lost, 
                                    evicted, g, gg, outcome, won, reused, rc, 
                                    rg, rreply, rclaimed >>

ClaimInsert(self) == /\ pc[self] = "ClaimInsert"
                     /\ reused[self] \/ Free # {}
                     /\ LET i == IF reused[self] THEN cid[self] ELSE MinFree IN
                          /\ cid' = [cid EXCEPT ![self] = i]
                          /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                /\ ambig' = ambig
                             \/ /\ ambig < MaxAmbig
                                /\ ambig' = ambig + 1
                                /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                             \/ /\ ambig < MaxAmbig
                                /\ ambig' = ambig + 1
                                /\ reply' = [reply EXCEPT ![self] = "err_c"]
                             \/ /\ LateCommit /\ ambig < MaxAmbig
                                /\ ambig' = ambig + 1
                                /\ reply' = [reply EXCEPT ![self] = "err_late"]
                          /\ IF reply'[self] \in {"ok", "err_c"}
                                THEN /\ IF marker[(CallOf[self])][g[self]] = 0
                                           THEN /\ marker' = [marker EXCEPT ![(CallOf[self])][g[self]] = i]
                                           ELSE /\ TRUE
                                                /\ UNCHANGED marker
                                     /\ UNCHANGED lateMarker
                                ELSE /\ IF reply'[self] = "err_late"
                                           THEN /\ lateMarker' = (lateMarker \cup {<<(CallOf[self]), g[self], i>>})
                                           ELSE /\ TRUE
                                                /\ UNCHANGED lateMarker
                                     /\ UNCHANGED marker
                          /\ IF reply'[self] # "ok"
                                THEN /\ pc' = [pc EXCEPT ![self] = "ClaimNS"]
                                     /\ won' = won
                                ELSE /\ IF marker'[CallOf[self]][g[self]] \in {0, i}
                                           THEN /\ IF reused[self] /\ Bug = "HeldPin"
                                                      THEN /\ pc' = [pc EXCEPT ![self] = "Hold"]
                                                           /\ won' = won
                                                      ELSE /\ won' = [won EXCEPT ![self] = TRUE]
                                                           /\ pc' = [pc EXCEPT ![self] = "Win"]
                                           ELSE /\ pc' = [pc EXCEPT ![self] = "Lost"]
                                                /\ won' = won
                     /\ UNCHANGED << nsSet, heldSet, result, toolFail, lateNS, 
                                     lateResult, pending, fl, waiters, jres, 
                                     oldId, toRetry, lease, rids, rcid, 
                                     crashes, cancels, evictions, fired, 
                                     firedAt, lost, evicted, g, gg, outcome, 
                                     reused, rc, rg, rreply, rclaimed >>

ClaimNS(self) == /\ pc[self] = "ClaimNS"
                 /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                       /\ ambig' = ambig
                    \/ /\ ambig < MaxAmbig
                       /\ ambig' = ambig + 1
                       /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                    \/ /\ ambig < MaxAmbig
                       /\ ambig' = ambig + 1
                       /\ reply' = [reply EXCEPT ![self] = "err_c"]
                    \/ /\ LateCommit /\ ambig < MaxAmbig
                       /\ ambig' = ambig + 1
                       /\ reply' = [reply EXCEPT ![self] = "err_late"]
                 /\ IF reply'[self] \in {"ok", "err_c"}
                       THEN /\ IF ~NSTaken((CallOf[self]), g[self], (cid[self]))
                                  THEN /\ nsSet' = (nsSet \cup {<<(CallOf[self]), g[self], (cid[self])>>})
                                  ELSE /\ TRUE
                                       /\ nsSet' = nsSet
                            /\ UNCHANGED lateNS
                       ELSE /\ IF reply'[self] = "err_late"
                                  THEN /\ lateNS' = (lateNS \cup {<<(CallOf[self]), g[self], (cid[self])>>})
                                  ELSE /\ TRUE
                                       /\ UNCHANGED lateNS
                            /\ nsSet' = nsSet
                 /\ IF reply'[self] # "ok"
                       THEN /\ pending' = [pending EXCEPT ![(ProcOf[self])][(CallOf[self])][g[self]] = IF Bug = "MemoOverwrite" THEN {(cid[self])} ELSE pending[(ProcOf[self])][(CallOf[self])][g[self]] \cup {(cid[self])}]
                       ELSE /\ TRUE
                            /\ UNCHANGED pending
                 /\ outcome' = [outcome EXCEPT ![self] = "error"]
                 /\ pc' = [pc EXCEPT ![self] = "Finish"]
                 /\ UNCHANGED << marker, heldSet, result, toolFail, lateMarker, 
                                 lateResult, fl, waiters, jres, cid, oldId, 
                                 toRetry, lease, rids, rcid, crashes, cancels, 
                                 evictions, fired, firedAt, lost, evicted, g, 
                                 gg, won, reused, rc, rg, rreply, rclaimed >>

Hold(self) == /\ pc[self] = "Hold"
              /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                    /\ ambig' = ambig
                 \/ /\ ambig < MaxAmbig
                    /\ ambig' = ambig + 1
                    /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                 \/ /\ ambig < MaxAmbig
                    /\ ambig' = ambig + 1
                    /\ reply' = [reply EXCEPT ![self] = "err_c"]
                 \/ /\ LateCommit /\ ambig < MaxAmbig
                    /\ ambig' = ambig + 1
                    /\ reply' = [reply EXCEPT ![self] = "err_late"]
              /\ IF reply'[self] \in {"ok", "err_c"} /\ ~NSTaken(CallOf[self], g[self], cid[self])
                    THEN /\ heldSet' = (heldSet \cup {<<CallOf[self], g[self], cid[self]>>})
                    ELSE /\ TRUE
                         /\ UNCHANGED heldSet
              /\ IF reply'[self] # "ok"
                    THEN /\ pending' = [pending EXCEPT ![(ProcOf[self])][(CallOf[self])][g[self]] = IF Bug = "MemoOverwrite" THEN {(cid[self])} ELSE pending[(ProcOf[self])][(CallOf[self])][g[self]] \cup {(cid[self])}]
                         /\ outcome' = [outcome EXCEPT ![self] = "error"]
                         /\ pc' = [pc EXCEPT ![self] = "Finish"]
                         /\ won' = won
                    ELSE /\ IF <<CallOf[self], g[self], cid[self]>> \in nsSet
                               THEN /\ pc' = [pc EXCEPT ![self] = "Lost"]
                                    /\ won' = won
                               ELSE /\ won' = [won EXCEPT ![self] = TRUE]
                                    /\ pc' = [pc EXCEPT ![self] = "Win"]
                         /\ UNCHANGED << pending, outcome >>
              /\ UNCHANGED << marker, nsSet, result, toolFail, lateMarker, 
                              lateNS, lateResult, fl, waiters, jres, cid, 
                              oldId, toRetry, lease, rids, rcid, crashes, 
                              cancels, evictions, fired, firedAt, lost, 
                              evicted, g, gg, reused, rc, rg, rreply, rclaimed >>

Lost(self) == /\ pc[self] = "Lost"
              /\ IF Kind[CallOf[self]] = "flow"
                    THEN /\ outcome' = [outcome EXCEPT ![self] = "halt_contended"]
                         /\ pc' = [pc EXCEPT ![self] = "Finish"]
                         /\ g' = g
                    ELSE /\ IF Voided(CallOf[self], g[self])
                               THEN /\ g' = [g EXCEPT ![self] = g[self] + 1]
                                    /\ pc' = [pc EXCEPT ![self] = "Claim"]
                                    /\ UNCHANGED outcome
                               ELSE /\ IF Kind[CallOf[self]] = "tool"
                                          THEN /\ outcome' = [outcome EXCEPT ![self] = "halt_contended"]
                                               /\ pc' = [pc EXCEPT ![self] = "Finish"]
                                          ELSE /\ pc' = [pc EXCEPT ![self] = "Join"]
                                               /\ UNCHANGED outcome
                                    /\ g' = g
              /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                              lateMarker, lateNS, lateResult, pending, fl, 
                              waiters, jres, cid, oldId, toRetry, lease, rids, 
                              rcid, ambig, crashes, cancels, evictions, fired, 
                              firedAt, lost, evicted, gg, reply, won, reused, 
                              rc, rg, rreply, rclaimed >>

Join(self) == /\ pc[self] = "Join"
              /\ IF fl[ProcOf[self]][CallOf[self]] # None
                    THEN /\ waiters' = [waiters EXCEPT ![ProcOf[self]][CallOf[self]] = waiters[ProcOf[self]][CallOf[self]] \cup {self}]
                         /\ pc' = [pc EXCEPT ![self] = "LoserWait"]
                         /\ fl' = fl
                    ELSE /\ IF Bug = "LoserLeads"
                               THEN /\ fl' = [fl EXCEPT ![ProcOf[self]][CallOf[self]] = self]
                                    /\ pc' = [pc EXCEPT ![self] = "LoserLead"]
                               ELSE /\ pc' = [pc EXCEPT ![self] = "LoserRead"]
                                    /\ fl' = fl
                         /\ UNCHANGED waiters
              /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                              lateMarker, lateNS, lateResult, pending, jres, 
                              cid, oldId, toRetry, lease, rids, rcid, ambig, 
                              crashes, cancels, evictions, fired, firedAt, 
                              lost, evicted, g, gg, reply, outcome, won, 
                              reused, rc, rg, rreply, rclaimed >>

LoserRead(self) == /\ pc[self] = "LoserRead"
                   /\ IF result[CallOf[self]] # None
                         THEN /\ outcome' = [outcome EXCEPT ![self] = "done"]
                         ELSE /\ outcome' = [outcome EXCEPT ![self] = "halt_crashed"]
                   /\ pc' = [pc EXCEPT ![self] = "Finish"]
                   /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                   lateMarker, lateNS, lateResult, pending, fl, 
                                   waiters, jres, cid, oldId, toRetry, lease, 
                                   rids, rcid, ambig, crashes, cancels, 
                                   evictions, fired, firedAt, lost, evicted, g, 
                                   gg, reply, won, reused, rc, rg, rreply, 
                                   rclaimed >>

LoserLead(self) == /\ pc[self] = "LoserLead"
                   /\ fl' = [fl EXCEPT ![(ProcOf[self])][(CallOf[self])] = None]
                   /\ jres' = [x \in DOMAIN jres |-> IF x \in waiters[(ProcOf[self])][(CallOf[self])] THEN (IF result[CallOf[self]] # None THEN "ok" ELSE "halt") ELSE jres[x]]
                   /\ waiters' = [waiters EXCEPT ![(ProcOf[self])][(CallOf[self])] = {}]
                   /\ IF result[CallOf[self]] # None
                         THEN /\ outcome' = [outcome EXCEPT ![self] = "done"]
                         ELSE /\ outcome' = [outcome EXCEPT ![self] = "halt_contended"]
                   /\ pc' = [pc EXCEPT ![self] = "Finish"]
                   /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                   lateMarker, lateNS, lateResult, pending, 
                                   cid, oldId, toRetry, lease, rids, rcid, 
                                   ambig, crashes, cancels, evictions, fired, 
                                   firedAt, lost, evicted, g, gg, reply, won, 
                                   reused, rc, rg, rreply, rclaimed >>

LoserWait(self) == /\ pc[self] = "LoserWait"
                   /\ jres[self] # None
                   /\ IF jres[self] = "ok"
                         THEN /\ outcome' = [outcome EXCEPT ![self] = "done"]
                         ELSE /\ outcome' = [outcome EXCEPT ![self] = "halt_contended"]
                   /\ jres' = [jres EXCEPT ![self] = None]
                   /\ pc' = [pc EXCEPT ![self] = "Finish"]
                   /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                   lateMarker, lateNS, lateResult, pending, fl, 
                                   waiters, cid, oldId, toRetry, lease, rids, 
                                   rcid, ambig, crashes, cancels, evictions, 
                                   fired, firedAt, lost, evicted, g, gg, reply, 
                                   won, reused, rc, rg, rreply, rclaimed >>

Win(self) == /\ pc[self] = "Win"
             /\ IF fl[ProcOf[self]][CallOf[self]] # None
                   THEN /\ waiters' = [waiters EXCEPT ![ProcOf[self]][CallOf[self]] = waiters[ProcOf[self]][CallOf[self]] \cup {self}]
                        /\ pc' = [pc EXCEPT ![self] = "WinnerWait"]
                        /\ fl' = fl
                   ELSE /\ fl' = [fl EXCEPT ![ProcOf[self]][CallOf[self]] = self]
                        /\ pc' = [pc EXCEPT ![self] = "Call"]
                        /\ UNCHANGED waiters
             /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                             lateMarker, lateNS, lateResult, pending, jres, 
                             cid, oldId, toRetry, lease, rids, rcid, ambig, 
                             crashes, cancels, evictions, fired, firedAt, lost, 
                             evicted, g, gg, reply, outcome, won, reused, rc, 
                             rg, rreply, rclaimed >>

WinnerWait(self) == /\ pc[self] = "WinnerWait"
                    /\ jres[self] # None
                    /\ IF jres[self] = "ok"
                          THEN /\ outcome' = [outcome EXCEPT ![self] = "done"]
                               /\ jres' = [jres EXCEPT ![self] = None]
                               /\ pc' = [pc EXCEPT ![self] = "Finish"]
                          ELSE /\ outcome' = [outcome EXCEPT ![self] = IF jres[self] = "halt" THEN "halt_contended" ELSE "error"]
                               /\ jres' = [jres EXCEPT ![self] = None]
                               /\ pc' = [pc EXCEPT ![self] = "NotStarted"]
                    /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                    lateMarker, lateNS, lateResult, pending, 
                                    fl, waiters, cid, oldId, toRetry, lease, 
                                    rids, rcid, ambig, crashes, cancels, 
                                    evictions, fired, firedAt, lost, evicted, 
                                    g, gg, reply, won, reused, rc, rg, rreply, 
                                    rclaimed >>

Call(self) == /\ pc[self] = "Call"
              /\ \/ /\ cancels < MaxCancel
                    /\ cancels' = cancels + 1
                    /\ fl' = [fl EXCEPT ![(ProcOf[self])][(CallOf[self])] = None]
                    /\ jres' = [x \in DOMAIN jres |-> IF x \in waiters[(ProcOf[self])][(CallOf[self])] THEN "err" ELSE jres[x]]
                    /\ waiters' = [waiters EXCEPT ![(ProcOf[self])][(CallOf[self])] = {}]
                    /\ outcome' = [outcome EXCEPT ![self] = "error"]
                    /\ IF Kind[CallOf[self]] = "flow"
                          THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                          ELSE /\ pc' = [pc EXCEPT ![self] = "NotStarted"]
                    /\ UNCHANGED <<toolFail, fired, firedAt>>
                 \/ /\ fired' = [fired EXCEPT ![CallOf[self]] = fired[CallOf[self]] + 1]
                    /\ firedAt' = [firedAt EXCEPT ![CallOf[self]][g[self]] = cid[self]]
                    /\ IF CallOf[self] \in PauseCalls
                          THEN /\ fl' = [fl EXCEPT ![(ProcOf[self])][(CallOf[self])] = None]
                               /\ jres' = [x \in DOMAIN jres |-> IF x \in waiters[(ProcOf[self])][(CallOf[self])] THEN "err" ELSE jres[x]]
                               /\ waiters' = [waiters EXCEPT ![(ProcOf[self])][(CallOf[self])] = {}]
                               /\ outcome' = [outcome EXCEPT ![self] = "pause"]
                               /\ IF Bug = "PauseAsFailure"
                                     THEN /\ toolFail' = [toolFail EXCEPT ![CallOf[self]] = TRUE]
                                     ELSE /\ TRUE
                                          /\ UNCHANGED toolFail
                               /\ pc' = [pc EXCEPT ![self] = "Finish"]
                          ELSE /\ pc' = [pc EXCEPT ![self] = "Record"]
                               /\ UNCHANGED << toolFail, fl, waiters, jres, 
                                               outcome >>
                    /\ UNCHANGED cancels
              /\ UNCHANGED << marker, nsSet, heldSet, result, lateMarker, 
                              lateNS, lateResult, pending, cid, oldId, toRetry, 
                              lease, rids, rcid, ambig, crashes, evictions, 
                              lost, evicted, g, gg, reply, won, reused, rc, rg, 
                              rreply, rclaimed >>

Record(self) == /\ pc[self] = "Record"
                /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                      /\ ambig' = ambig
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ reply' = [reply EXCEPT ![self] = "err_c"]
                   \/ /\ LateCommit /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ reply' = [reply EXCEPT ![self] = "err_late"]
                /\ IF reply'[self] \in {"ok", "err_c"}
                      THEN /\ IF result[(CallOf[self])] = None
                                 THEN /\ result' = [result EXCEPT ![(CallOf[self])] = "drv"]
                                 ELSE /\ TRUE
                                      /\ UNCHANGED result
                           /\ UNCHANGED lateResult
                      ELSE /\ IF reply'[self] = "err_late"
                                 THEN /\ lateResult' = (lateResult \cup {<<(CallOf[self]), "drv">>})
                                 ELSE /\ TRUE
                                      /\ UNCHANGED lateResult
                           /\ UNCHANGED result
                /\ fl' = [fl EXCEPT ![(ProcOf[self])][(CallOf[self])] = None]
                /\ jres' = [x \in DOMAIN jres |-> IF x \in waiters[(ProcOf[self])][(CallOf[self])] THEN (IF reply'[self] = "ok" THEN "ok" ELSE "err") ELSE jres[x]]
                /\ waiters' = [waiters EXCEPT ![(ProcOf[self])][(CallOf[self])] = {}]
                /\ IF reply'[self] = "ok"
                      THEN /\ outcome' = [outcome EXCEPT ![self] = "done"]
                      ELSE /\ outcome' = [outcome EXCEPT ![self] = "error"]
                /\ pc' = [pc EXCEPT ![self] = "Finish"]
                /\ UNCHANGED << marker, nsSet, heldSet, toolFail, lateMarker, 
                                lateNS, pending, cid, oldId, toRetry, lease, 
                                rids, rcid, crashes, cancels, evictions, fired, 
                                firedAt, lost, evicted, g, gg, won, reused, rc, 
                                rg, rreply, rclaimed >>

NotStarted(self) == /\ pc[self] = "NotStarted"
                    /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                          /\ ambig' = ambig
                       \/ /\ ambig < MaxAmbig
                          /\ ambig' = ambig + 1
                          /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                       \/ /\ ambig < MaxAmbig
                          /\ ambig' = ambig + 1
                          /\ reply' = [reply EXCEPT ![self] = "err_c"]
                       \/ /\ LateCommit /\ ambig < MaxAmbig
                          /\ ambig' = ambig + 1
                          /\ reply' = [reply EXCEPT ![self] = "err_late"]
                    /\ IF reply'[self] \in {"ok", "err_c"}
                          THEN /\ IF ~NSTaken((CallOf[self]), g[self], (cid[self]))
                                     THEN /\ nsSet' = (nsSet \cup {<<(CallOf[self]), g[self], (cid[self])>>})
                                     ELSE /\ TRUE
                                          /\ nsSet' = nsSet
                               /\ UNCHANGED lateNS
                          ELSE /\ IF reply'[self] = "err_late"
                                     THEN /\ lateNS' = (lateNS \cup {<<(CallOf[self]), g[self], (cid[self])>>})
                                     ELSE /\ TRUE
                                          /\ UNCHANGED lateNS
                               /\ nsSet' = nsSet
                    /\ IF reply'[self] # "ok" /\ Bug # "NoMemoAfterCall"
                          THEN /\ pending' = [pending EXCEPT ![(ProcOf[self])][(CallOf[self])][g[self]] = IF Bug = "MemoOverwrite" THEN {(cid[self])} ELSE pending[(ProcOf[self])][(CallOf[self])][g[self]] \cup {(cid[self])}]
                          ELSE /\ TRUE
                               /\ UNCHANGED pending
                    /\ pc' = [pc EXCEPT ![self] = "Finish"]
                    /\ UNCHANGED << marker, heldSet, result, toolFail, 
                                    lateMarker, lateResult, fl, waiters, jres, 
                                    cid, oldId, toRetry, lease, rids, rcid, 
                                    crashes, cancels, evictions, fired, 
                                    firedAt, lost, evicted, g, gg, outcome, 
                                    won, reused, rc, rg, rreply, rclaimed >>

Finish(self) == /\ pc[self] = "Finish"
                /\ IF lease = self
                      THEN /\ lease' = None
                      ELSE /\ TRUE
                           /\ lease' = lease
                /\ IF outcome[self] # "done"
                      THEN /\ pc' = [pc EXCEPT ![self] = "Open"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "Done"]
                /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                lateMarker, lateNS, lateResult, pending, fl, 
                                waiters, jres, cid, oldId, toRetry, rids, rcid, 
                                ambig, crashes, cancels, evictions, fired, 
                                firedAt, lost, evicted, g, gg, reply, outcome, 
                                won, reused, rc, rg, rreply, rclaimed >>

driver(self) == Start(self) \/ Open(self) \/ GateTake(self)
                   \/ GateWrite(self) \/ Claim(self) \/ ClaimRetry(self)
                   \/ ClaimInsert(self) \/ ClaimNS(self) \/ Hold(self)
                   \/ Lost(self) \/ Join(self) \/ LoserRead(self)
                   \/ LoserLead(self) \/ LoserWait(self) \/ Win(self)
                   \/ WinnerWait(self) \/ Call(self) \/ Record(self)
                   \/ NotStarted(self) \/ Finish(self)

RCheck(self) == /\ pc[self] = "RCheck"
                /\ \E c \in Calls:
                     \E x \in Gens:
                       /\ Kind[c] # "flow" /\ result[c] = None /\ Live(c, x)
                       /\ LiveCheck = "lease" => lease = None
                       /\ LiveCheck = "lease" /\ PlainRunIdleAtCheck =>
                            \A d \in Drivers : ~(CallOf[d] = c /\ cid[d] = marker[c][x] /\ pc[d] \in Window)
                       /\ LiveCheck = "minAge" =>
                            \A d \in Drivers : ~(CallOf[d] = c /\ cid[d] = marker[c][x] /\ pc[d] \in Window)
                       /\ IF LiveCheck = "lease"
                             THEN /\ lease' = self
                             ELSE /\ TRUE
                                  /\ lease' = lease
                       /\ rc' = [rc EXCEPT ![self] = c]
                       /\ rg' = [rg EXCEPT ![self] = x]
                /\ IF ~ResolveClaim
                      THEN /\ pc' = [pc EXCEPT ![self] = "RWrite"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "RClaim"]
                /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                lateMarker, lateNS, lateResult, pending, fl, 
                                waiters, jres, cid, oldId, toRetry, rids, rcid, 
                                ambig, crashes, cancels, evictions, fired, 
                                firedAt, lost, evicted, g, gg, reply, outcome, 
                                won, reused, rreply, rclaimed >>

RClaim(self) == /\ pc[self] = "RClaim"
                /\ rg[self] + 1 <= MaxGen
                /\ IF ResolverProc # "none" /\ pending[ResolverProc][rc[self]][rg[self] + 1] # {}
                      THEN /\ toRetry' = [toRetry EXCEPT ![self] = pending[ResolverProc][rc[self]][rg[self] + 1]]
                           /\ pending' = [pending EXCEPT ![ResolverProc][rc[self]][rg[self] + 1] = {}]
                           /\ pc' = [pc EXCEPT ![self] = "RRetry"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "RInsert"]
                           /\ UNCHANGED << pending, toRetry >>
                /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                lateMarker, lateNS, lateResult, fl, waiters, 
                                jres, cid, oldId, lease, rids, rcid, ambig, 
                                crashes, cancels, evictions, fired, firedAt, 
                                lost, evicted, g, gg, reply, outcome, won, 
                                reused, rc, rg, rreply, rclaimed >>

RRetry(self) == /\ pc[self] = "RRetry"
                /\ LET i == Min(toRetry[self]) IN
                     /\ \/ /\ rreply' = [rreply EXCEPT ![self] = "ok"]
                           /\ ambig' = ambig
                        \/ /\ ambig < MaxAmbig
                           /\ ambig' = ambig + 1
                           /\ rreply' = [rreply EXCEPT ![self] = "err_nc"]
                        \/ /\ ambig < MaxAmbig
                           /\ ambig' = ambig + 1
                           /\ rreply' = [rreply EXCEPT ![self] = "err_c"]
                        \/ /\ LateCommit /\ ambig < MaxAmbig
                           /\ ambig' = ambig + 1
                           /\ rreply' = [rreply EXCEPT ![self] = "err_late"]
                     /\ IF rreply'[self] \in {"ok", "err_c"}
                           THEN /\ IF ~NSTaken(rc[self], (rg[self] + 1), i)
                                      THEN /\ nsSet' = (nsSet \cup {<<rc[self], (rg[self] + 1), i>>})
                                      ELSE /\ TRUE
                                           /\ nsSet' = nsSet
                                /\ UNCHANGED lateNS
                           ELSE /\ IF rreply'[self] = "err_late"
                                      THEN /\ lateNS' = (lateNS \cup {<<rc[self], (rg[self] + 1), i>>})
                                      ELSE /\ TRUE
                                           /\ UNCHANGED lateNS
                                /\ nsSet' = nsSet
                     /\ IF rreply'[self] # "ok"
                           THEN /\ pending' = [pending EXCEPT ![ResolverProc][rc[self]][(rg[self] + 1)] = IF Bug = "MemoOverwrite" THEN {i} ELSE pending[ResolverProc][rc[self]][(rg[self] + 1)] \cup {i}]
                           ELSE /\ TRUE
                                /\ UNCHANGED pending
                     /\ toRetry' = [toRetry EXCEPT ![self] = toRetry[self] \ {i}]
                     /\ IF toRetry'[self] \ {i} = {}
                           THEN /\ pc' = [pc EXCEPT ![self] = "RInsert"]
                           ELSE /\ pc' = [pc EXCEPT ![self] = "RRetry"]
                /\ UNCHANGED << marker, heldSet, result, toolFail, lateMarker, 
                                lateResult, fl, waiters, jres, cid, oldId, 
                                lease, rids, rcid, crashes, cancels, evictions, 
                                fired, firedAt, lost, evicted, g, gg, reply, 
                                outcome, won, reused, rc, rg, rclaimed >>

RInsert(self) == /\ pc[self] = "RInsert"
                 /\ Free # {}
                 /\ LET i == MinFree IN
                      /\ rcid' = i
                      /\ rids' = (rids \cup {i})
                      /\ \/ /\ rreply' = [rreply EXCEPT ![self] = "ok"]
                            /\ ambig' = ambig
                         \/ /\ ambig < MaxAmbig
                            /\ ambig' = ambig + 1
                            /\ rreply' = [rreply EXCEPT ![self] = "err_nc"]
                         \/ /\ ambig < MaxAmbig
                            /\ ambig' = ambig + 1
                            /\ rreply' = [rreply EXCEPT ![self] = "err_c"]
                         \/ /\ LateCommit /\ ambig < MaxAmbig
                            /\ ambig' = ambig + 1
                            /\ rreply' = [rreply EXCEPT ![self] = "err_late"]
                      /\ IF rreply'[self] \in {"ok", "err_c"}
                            THEN /\ IF marker[rc[self]][(rg[self] + 1)] = 0
                                       THEN /\ marker' = [marker EXCEPT ![rc[self]][(rg[self] + 1)] = i]
                                       ELSE /\ TRUE
                                            /\ UNCHANGED marker
                                 /\ UNCHANGED lateMarker
                            ELSE /\ IF rreply'[self] = "err_late"
                                       THEN /\ lateMarker' = (lateMarker \cup {<<rc[self], (rg[self] + 1), i>>})
                                       ELSE /\ TRUE
                                            /\ UNCHANGED lateMarker
                                 /\ UNCHANGED marker
                      /\ IF rreply'[self] # "ok"
                            THEN /\ pc' = [pc EXCEPT ![self] = "RClaimNS"]
                                 /\ UNCHANGED rclaimed
                            ELSE /\ IF marker'[rc[self]][rg[self] + 1] \notin {0, i}
                                       THEN /\ pc' = [pc EXCEPT ![self] = "RRelease"]
                                            /\ UNCHANGED rclaimed
                                       ELSE /\ rclaimed' = [rclaimed EXCEPT ![self] = TRUE]
                                            /\ pc' = [pc EXCEPT ![self] = "RWrite"]
                 /\ UNCHANGED << nsSet, heldSet, result, toolFail, lateNS, 
                                 lateResult, pending, fl, waiters, jres, cid, 
                                 oldId, toRetry, lease, crashes, cancels, 
                                 evictions, fired, firedAt, lost, evicted, g, 
                                 gg, reply, outcome, won, reused, rc, rg >>

RClaimNS(self) == /\ pc[self] = "RClaimNS"
                  /\ \/ /\ rreply' = [rreply EXCEPT ![self] = "ok"]
                        /\ ambig' = ambig
                     \/ /\ ambig < MaxAmbig
                        /\ ambig' = ambig + 1
                        /\ rreply' = [rreply EXCEPT ![self] = "err_nc"]
                     \/ /\ ambig < MaxAmbig
                        /\ ambig' = ambig + 1
                        /\ rreply' = [rreply EXCEPT ![self] = "err_c"]
                     \/ /\ LateCommit /\ ambig < MaxAmbig
                        /\ ambig' = ambig + 1
                        /\ rreply' = [rreply EXCEPT ![self] = "err_late"]
                  /\ IF rreply'[self] \in {"ok", "err_c"}
                        THEN /\ IF ~NSTaken(rc[self], (rg[self] + 1), rcid)
                                   THEN /\ nsSet' = (nsSet \cup {<<rc[self], (rg[self] + 1), rcid>>})
                                   ELSE /\ TRUE
                                        /\ nsSet' = nsSet
                             /\ UNCHANGED lateNS
                        ELSE /\ IF rreply'[self] = "err_late"
                                   THEN /\ lateNS' = (lateNS \cup {<<rc[self], (rg[self] + 1), rcid>>})
                                   ELSE /\ TRUE
                                        /\ UNCHANGED lateNS
                             /\ nsSet' = nsSet
                  /\ IF rreply'[self] # "ok" /\ ResolverProc # "none"
                        THEN /\ pending' = [pending EXCEPT ![ResolverProc][rc[self]][(rg[self] + 1)] = IF Bug = "MemoOverwrite" THEN {rcid} ELSE pending[ResolverProc][rc[self]][(rg[self] + 1)] \cup {rcid}]
                        ELSE /\ TRUE
                             /\ UNCHANGED pending
                  /\ pc' = [pc EXCEPT ![self] = "RRelease"]
                  /\ UNCHANGED << marker, heldSet, result, toolFail, 
                                  lateMarker, lateResult, fl, waiters, jres, 
                                  cid, oldId, toRetry, lease, rids, rcid, 
                                  crashes, cancels, evictions, fired, firedAt, 
                                  lost, evicted, g, gg, reply, outcome, won, 
                                  reused, rc, rg, rclaimed >>

RWrite(self) == /\ pc[self] = "RWrite"
                /\ IF ResolverProc # "none" /\ fl[ResolverProc][rc[self]] # None
                      THEN /\ waiters' = [waiters EXCEPT ![ResolverProc][rc[self]] = waiters[ResolverProc][rc[self]] \cup {self}]
                           /\ pc' = [pc EXCEPT ![self] = "RWait"]
                           /\ fl' = fl
                      ELSE /\ IF ResolverProc # "none"
                                 THEN /\ fl' = [fl EXCEPT ![ResolverProc][rc[self]] = self]
                                 ELSE /\ TRUE
                                      /\ fl' = fl
                           /\ pc' = [pc EXCEPT ![self] = "RRecord"]
                           /\ UNCHANGED waiters
                /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                lateMarker, lateNS, lateResult, pending, jres, 
                                cid, oldId, toRetry, lease, rids, rcid, ambig, 
                                crashes, cancels, evictions, fired, firedAt, 
                                lost, evicted, g, gg, reply, outcome, won, 
                                reused, rc, rg, rreply, rclaimed >>

RRecord(self) == /\ pc[self] = "RRecord"
                 /\ \/ /\ rreply' = [rreply EXCEPT ![self] = "ok"]
                       /\ ambig' = ambig
                    \/ /\ ambig < MaxAmbig
                       /\ ambig' = ambig + 1
                       /\ rreply' = [rreply EXCEPT ![self] = "err_nc"]
                    \/ /\ ambig < MaxAmbig
                       /\ ambig' = ambig + 1
                       /\ rreply' = [rreply EXCEPT ![self] = "err_c"]
                    \/ /\ LateCommit /\ ambig < MaxAmbig
                       /\ ambig' = ambig + 1
                       /\ rreply' = [rreply EXCEPT ![self] = "err_late"]
                 /\ IF rreply'[self] \in {"ok", "err_c"}
                       THEN /\ IF result[rc[self]] = None
                                  THEN /\ result' = [result EXCEPT ![rc[self]] = IF fired[rc[self]] > 0 THEN "res_ok" ELSE "res_err"]
                                  ELSE /\ TRUE
                                       /\ UNCHANGED result
                            /\ UNCHANGED lateResult
                       ELSE /\ IF rreply'[self] = "err_late"
                                  THEN /\ lateResult' = (lateResult \cup {<<rc[self], (IF fired[rc[self]] > 0 THEN "res_ok" ELSE "res_err")>>})
                                  ELSE /\ TRUE
                                       /\ UNCHANGED lateResult
                            /\ UNCHANGED result
                 /\ IF ResolverProc # "none"
                       THEN /\ fl' = [fl EXCEPT ![ResolverProc][rc[self]] = None]
                            /\ jres' = [x \in DOMAIN jres |-> IF x \in waiters[ResolverProc][rc[self]] THEN (IF rreply'[self] = "ok" THEN "ok" ELSE "err") ELSE jres[x]]
                            /\ waiters' = [waiters EXCEPT ![ResolverProc][rc[self]] = {}]
                       ELSE /\ TRUE
                            /\ UNCHANGED << fl, waiters, jres >>
                 /\ IF rreply'[self] # "ok" /\ rclaimed[self] /\ ResolveVoidOnError
                       THEN /\ pc' = [pc EXCEPT ![self] = "RNotStarted"]
                       ELSE /\ pc' = [pc EXCEPT ![self] = "RRelease"]
                 /\ UNCHANGED << marker, nsSet, heldSet, toolFail, lateMarker, 
                                 lateNS, pending, cid, oldId, toRetry, lease, 
                                 rids, rcid, crashes, cancels, evictions, 
                                 fired, firedAt, lost, evicted, g, gg, reply, 
                                 outcome, won, reused, rc, rg, rclaimed >>

RWait(self) == /\ pc[self] = "RWait"
               /\ jres[self] # None
               /\ IF jres[self] # "ok" /\ rclaimed[self] /\ ResolveVoidOnError
                     THEN /\ jres' = [jres EXCEPT ![self] = None]
                          /\ pc' = [pc EXCEPT ![self] = "RNotStarted"]
                     ELSE /\ jres' = [jres EXCEPT ![self] = None]
                          /\ pc' = [pc EXCEPT ![self] = "RRelease"]
               /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                               lateMarker, lateNS, lateResult, pending, fl, 
                               waiters, cid, oldId, toRetry, lease, rids, rcid, 
                               ambig, crashes, cancels, evictions, fired, 
                               firedAt, lost, evicted, g, gg, reply, outcome, 
                               won, reused, rc, rg, rreply, rclaimed >>

RNotStarted(self) == /\ pc[self] = "RNotStarted"
                     /\ \/ /\ rreply' = [rreply EXCEPT ![self] = "ok"]
                           /\ ambig' = ambig
                        \/ /\ ambig < MaxAmbig
                           /\ ambig' = ambig + 1
                           /\ rreply' = [rreply EXCEPT ![self] = "err_nc"]
                        \/ /\ ambig < MaxAmbig
                           /\ ambig' = ambig + 1
                           /\ rreply' = [rreply EXCEPT ![self] = "err_c"]
                        \/ /\ LateCommit /\ ambig < MaxAmbig
                           /\ ambig' = ambig + 1
                           /\ rreply' = [rreply EXCEPT ![self] = "err_late"]
                     /\ IF rreply'[self] \in {"ok", "err_c"}
                           THEN /\ IF ~NSTaken(rc[self], (rg[self] + 1), rcid)
                                      THEN /\ nsSet' = (nsSet \cup {<<rc[self], (rg[self] + 1), rcid>>})
                                      ELSE /\ TRUE
                                           /\ nsSet' = nsSet
                                /\ UNCHANGED lateNS
                           ELSE /\ IF rreply'[self] = "err_late"
                                      THEN /\ lateNS' = (lateNS \cup {<<rc[self], (rg[self] + 1), rcid>>})
                                      ELSE /\ TRUE
                                           /\ UNCHANGED lateNS
                                /\ nsSet' = nsSet
                     /\ IF rreply'[self] # "ok" /\ ResolverProc # "none"
                           THEN /\ pending' = [pending EXCEPT ![ResolverProc][rc[self]][(rg[self] + 1)] = IF Bug = "MemoOverwrite" THEN {rcid} ELSE pending[ResolverProc][rc[self]][(rg[self] + 1)] \cup {rcid}]
                           ELSE /\ TRUE
                                /\ UNCHANGED pending
                     /\ pc' = [pc EXCEPT ![self] = "RRelease"]
                     /\ UNCHANGED << marker, heldSet, result, toolFail, 
                                     lateMarker, lateResult, fl, waiters, jres, 
                                     cid, oldId, toRetry, lease, rids, rcid, 
                                     crashes, cancels, evictions, fired, 
                                     firedAt, lost, evicted, g, gg, reply, 
                                     outcome, won, reused, rc, rg, rclaimed >>

RRelease(self) == /\ pc[self] = "RRelease"
                  /\ IF lease = self
                        THEN /\ lease' = None
                        ELSE /\ TRUE
                             /\ lease' = lease
                  /\ pc' = [pc EXCEPT ![self] = "Done"]
                  /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                  lateMarker, lateNS, lateResult, pending, fl, 
                                  waiters, jres, cid, oldId, toRetry, rids, 
                                  rcid, ambig, crashes, cancels, evictions, 
                                  fired, firedAt, lost, evicted, g, gg, reply, 
                                  outcome, won, reused, rc, rg, rreply, 
                                  rclaimed >>

resolver(self) == RCheck(self) \/ RClaim(self) \/ RRetry(self)
                     \/ RInsert(self) \/ RClaimNS(self) \/ RWrite(self)
                     \/ RRecord(self) \/ RWait(self) \/ RNotStarted(self)
                     \/ RRelease(self)

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == (\E self \in Drivers: driver(self))
           \/ (\E self \in ResolverSet: resolver(self))
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ \A self \in Drivers : WF_vars(driver(self))
        /\ \A self \in ResolverSet : WF_vars((pc[self] # "RCheck") /\ resolver(self))

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION
-----------------------------------------------------------------------------
(***************************************************************************)
(* Faults that are not steps of a driver.                                   *)
(***************************************************************************)

\* A process dies: its drives restart from Open with empty locals, its pendingClaims and
\* flights are gone, a lease it held lapses, and the claim ids it knew become lost.
Crash(p) ==
  LET ds == {d \in Drivers : ProcOf[d] = p /\ pc[d] \notin {"Start", "Done"}}
      \* A resolver running in p dies with it (one that has not started its check is not running).
      rs == {r \in ResolverSet : ResolverProc = p /\ pc[r] \notin {"RCheck", "Done"}} IN
  /\ crashes < MaxCrash
  /\ ds \cup rs # {}
  /\ crashes' = crashes + 1
  /\ lost' = (lost \cup {cid[d] : d \in ds} \cup {oldId[d] : d \in ds}
                   \cup UNION {pending[p][c][x] : c \in Calls, x \in Gens}
                   \cup UNION {toRetry[d] : d \in ds \cup rs}
                   \cup (IF rs # {} THEN {rcid} ELSE {})) \ {0}
  /\ pc' = [d \in DOMAIN pc |-> IF d \in ds THEN "Open" ELSE IF d \in rs THEN "Done" ELSE pc[d]]
  /\ cid' = [d \in Drivers |-> IF d \in ds THEN 0 ELSE cid[d]]
  /\ oldId' = [d \in Drivers |-> IF d \in ds THEN 0 ELSE oldId[d]]
  /\ jres' = [d \in DOMAIN jres |-> IF d \in ds \cup rs THEN None ELSE jres[d]]
  /\ pending' = [pending EXCEPT ![p] = [c \in Calls |-> [x \in Gens |-> {}]]]
  /\ fl' = [fl EXCEPT ![p] = [c \in Calls |-> None]]
  /\ waiters' = [waiters EXCEPT ![p] = [c \in Calls |-> {}]]
  /\ lease' = IF lease \in ds \cup rs THEN None ELSE lease
  /\ rcid' = IF rs # {} THEN 0 ELSE rcid
  /\ toRetry' = [d \in DOMAIN toRetry |-> IF d \in ds \cup rs THEN {} ELSE toRetry[d]]
  /\ g' = [d \in DOMAIN g |-> IF d \in ds THEN 0 ELSE g[d]]
  /\ gg' = [d \in DOMAIN gg |-> IF d \in ds THEN 0 ELSE gg[d]]
  /\ reply' = [d \in DOMAIN reply |-> IF d \in ds THEN "" ELSE reply[d]]
  /\ outcome' = [d \in DOMAIN outcome |-> IF d \in ds THEN None ELSE outcome[d]]
  /\ won' = [d \in DOMAIN won |-> IF d \in ds THEN FALSE ELSE won[d]]
  /\ reused' = [d \in DOMAIN reused |-> IF d \in ds THEN FALSE ELSE reused[d]]
  /\ UNCHANGED <<marker, nsSet, heldSet, result, toolFail, lateMarker, lateNS, lateResult,
                 ambig, cancels, evictions, fired, firedAt, rids, evicted, rc, rg, rreply,
                 rclaimed>>

\* Weak A3 (LateCommit): a write that returned an error commits now, if its key is still free.
LateApply ==
  \/ \E w \in lateMarker :
       /\ lateMarker' = lateMarker \ {w}
       /\ marker' = IF marker[w[1]][w[2]] = 0 THEN [marker EXCEPT ![w[1]][w[2]] = w[3]] ELSE marker
       /\ UNCHANGED <<nsSet, lateNS, result, lateResult>>
  \/ \E w \in lateNS :
       /\ lateNS' = lateNS \ {w}
       /\ nsSet' = IF NSTaken(w[1], w[2], w[3]) THEN nsSet ELSE nsSet \cup {w}
       /\ UNCHANGED <<marker, lateMarker, result, lateResult>>
  \/ \E w \in lateResult :
       /\ lateResult' = lateResult \ {w}
       /\ result' = IF result[w[1]] = None THEN [result EXCEPT ![w[1]] = w[2]] ELSE result
       /\ UNCHANGED <<marker, lateMarker, nsSet, lateNS>>

LateVars == <<heldSet, toolFail, pending, fl, waiters, jres, cid, oldId, lease, rids, rcid, toRetry,
              ambig, crashes, cancels, evictions, fired, firedAt, lost, evicted, pc, g, gg, reply,
              outcome, won, reused, rc, rg, rreply, rclaimed>>

\* pendingClaims past maxPendingClaims drops its oldest marker keys: here, any one key's ids.
Evict(p) ==
  /\ evictions < MaxEvict
  /\ \E c \in Calls, x \in Gens :
       /\ pending[p][c][x] # {}
       /\ evicted' = evicted \cup pending[p][c][x]
       /\ pending' = [pending EXCEPT ![p][c][x] = {}]
  /\ evictions' = evictions + 1
  /\ UNCHANGED <<marker, nsSet, heldSet, result, toolFail, lateMarker, lateNS, lateResult, fl,
                 waiters, jres, cid, oldId, lease, rids, rcid, toRetry, ambig, crashes, cancels,
                 fired, firedAt, lost, pc, g, gg, reply, outcome, won, reused, rc, rg, rreply,
                 rclaimed>>

FullNext == Next \/ (\E p \in Procs : Crash(p) \/ Evict(p)) \/ (LateApply /\ UNCHANGED LateVars)

\* The translation's fairness: every driver step is weakly fair; the resolver's check is not
\* (resolution is never assumed), but once it has checked, it writes.
Fairness ==
  /\ \A self \in Drivers : WF_vars(driver(self))
  /\ \A self \in ResolverSet : WF_vars((pc[self] # "RCheck") /\ resolver(self))

FullSpec == Init /\ [][FullNext]_vars /\ Fairness

(***************************************************************************)
(* Properties                                                               *)
(***************************************************************************)

\* At most one fire per logical call.
AtMostOnce == \A c \in Calls : fired[c] <= 1

\* At most one fire across a call and the call the caller issued after reading it failed.
AtMostOncePerIntent == \A c \in Calls : NextOf[c] # None => fired[c] + fired[NextOf[c]] <= 1

\* A not-started record never coexists with a fired effect for that claim.
NotStartedExclusive ==
  \A c \in Calls, x \in Gens : firedAt[c][x] # 0 => <<c, x, firedAt[c][x]>> \notin nsSet

\* Every won claim's not-started key is empty (the rule NotStartedExclusive rests on).
WonKeyEmpty ==
  \A d \in Drivers : pc[d] \in {"Win", "Call"} => ~NSTaken(CallOf[d], g[d], cid[d])

\* Resolution never overrides a live driver's outcome: a driver that called the effect never
\* finds a resolution in place when it records its own.
NoLiveOverride ==
  \A d \in Drivers : pc[d] = "Record" => result[CallOf[d]] \notin {"res_ok", "res_err"}

\* A driver that won a claim never returns a halt (#92 first review, finding 3).
WinnerNeverHalts ==
  \A d \in Drivers : (pc[d] = "Finish" /\ won[d]) => outcome[d] \notin Halts

\* At most one attempt of a call claimed by a driver is live at a time (liveAttempts keeps one per
\* call).
AtMostOneLive ==
  \A c \in Calls : Cardinality({x \in Gens : Live(c, x) /\ marker[c][x] \notin rids}) <= 1

\* The bounds never block a driver, so no result depends on a bound being too small.
BoundNotHit ==
  \A d \in Drivers : /\ ~(pc[d] = "Claim" /\ g[d] > MaxGen)
                     /\ ~(pc[d] = "ClaimInsert" /\ ~reused[d] /\ Free = {})
  /\ \A r \in ResolverSet : ~(pc[r] = "RClaim" /\ rg[r] + 1 > MaxGen) /\ ~(pc[r] = "RInsert" /\ Free = {})

\* A recorded result is never replaced.
ResultStable == [][\A c \in Calls : result[c] # None => result'[c] = result[c]]_vars

\* Vacuity check, expected to be violated: the effect is reachable.
EffectNotReachable == \A c \in Calls : fired[c] = 0

\* A call that is left without an outcome for ever has a live attempt that may have fired: the
\* effect was called, or a crash erased the only knowledge that its claim never called it.
Excused(c) == fired[c] > 0 \/ \E x \in Gens : Live(c, x) /\ marker[c][x] \in lost

\* Liveness: a provably unstarted effect does not halt for ever.
Progress == \A c \in Calls : <>[](~Issued(c) \/ Recorded(c) \/ Excused(c))

\* The same, also excusing a live attempt whose claim id an eviction forgot: shows that an
\* eviction costs nothing but those halts.
ProgressModuloEviction ==
  \A c \in Calls : <>[](~Issued(c) \/ Recorded(c) \/ Excused(c)
                        \/ \E x \in Gens : Live(c, x) /\ marker[c][x] \in evicted)
=============================================================================
