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
EXTENDS Naturals, FiniteSets, Sequences, TLC

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
  ResolveClaim,  \* TRUE: the resolver first claims the attempt after the live one (F2's and F4's
                 \* fix; #92 at 2c8d2db claims on the lease and the min-age paths)
  ResolverProc,  \* the process the resolver runs in (sharing its pendingClaims and flights), or "none"
  PlainRunIdleAtCheck, \* TRUE: under the lease check, assume no unleased driver holds the live
                       \* claim when the resolution checks (isolates finding F4 from #90's known limit)
  ResolveVoidOnError, \* TRUE: a resolution whose result write errored records its own attempt as not
                      \* started (#92 at 06408db); FALSE: it leaves that attempt live (finding F3's fix)
  \* The approval gate (model 1b). A call's policy is "none", "one" (Approve) or the name of an
  \* m-of-n policy in Policies; a redeploy may change it between drives.
  Policy0,       \* call -> the policy its tool has at the start
  Policies,      \* name -> [need |-> k, apprs |-> the eligible approver ids]
  PolicyChoices, \* the policies a redeploy may switch a call's tool to
  ApproverIds,   \* every approver id a decision can name (eligible or not)
  Actors,        \* the people who submit decisions (an adversary holds no approver's key)
  KeyOf,         \* approver id -> the set of signing keys its verifier accepts (KeyIDs, #109)
  KeyOf2,        \* the resolver's answer after a change (a rotation, a redeploy of the key file)
  MaxResolverChange, \* budget of resolver changes (0 or 1)
  Holder,        \* key -> the person holding it
  FoldSame,      \* pairs of approver ids that differ only by case or normalization
  Subjects,      \* "this" (signed over this exact call) and optionally "other" (another call or args)
  MaxApprove1,   \* budget of Approve calls (1-of-1)
  MaxSubmit,     \* budget of SubmitDecision calls (m-of-n)
  MaxRedeploy,   \* budget of redeploys that change a tool's gate
  KeyCheck,      \* TRUE: the gate refuses a policy two of whose approvers' key sets meet, or one
                 \* with an empty key set (F5's fix, #109)
  Bug            \* "none" or a historical rule, see the regression configs

None == "none"
NoTally == [rec |-> FALSE, passed |-> FALSE, pol |-> "none", signers |-> {}, excl |-> 0, denials |-> 0]
Gens == 0..MaxGen
Ids  == 1..MaxIds
ResolverSet == IF HasResolver THEN {"resolver"} ELSE {}
Halts == {"halt_crashed", "halt_contended"}
\* vocabulary: begin
\* The kinds of journal record the claim protocol reads and writes, named by key: an attempt's
\* first marker (attempt:tool:<id>, attempt:step:<name>), a numbered re-attempt's marker
\* (attempt:retry:<n>:...), a not-started record (attempt:not-started:<claim>:<marker>), and the
\* result of a tool call (tool:<id>) or of a Step (its name). The model's marker, nsSet and
\* result variables hold them. TestProtocolVocabulary (package agent) maps every Go key
\* constructor and record kind of the claim code to one of these and fails if the two differ.
RecordKinds == {"marker", "retry_marker", "not_started", "result_tool", "result_step"}
\* vocabulary: end

ASSUME MaxGen \in Nat /\ MaxIds \in Nat /\ MaxAmbig \in Nat /\ MaxCrash \in Nat /\ MaxCancel \in Nat
ASSUME LiveCheck \in {"lease", "minAge", "none"}
ASSUME ResolveClaim \in BOOLEAN /\ LateCommit \in BOOLEAN /\ HasResolver \in BOOLEAN
ASSUME Bug \in {"none", "ReuseNoHold", "HeldPin", "GateNoRetry", "NoMemoAfterCall",
                "MemoOverwrite", "LoserLeads", "PauseAsFailure",
                "DenialNotFinal", "UnboundSubject", "SlotPerApprover", "NoFoldCheck",
                "SetEqualityCheck", "NoCountExclusion"}
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
  evicted    = {},
  \* The approval gate. one[c]: the 1-of-1 decision (approval:<id>): "none", "yes" or "no".
  one        = [c \in Calls |-> "none"],
  \* dlog[c]: the m-of-n decision records in journal order, each [a (approver id), h (who
  \* signed), ok (approved), valid (the signature verifies: h holds a's key), subj].
  dlog       = [c \in Calls |-> <<>>],
  \* tally[c]: the terminal tally (approval-tally:<id>): [rec (recorded), passed, pol].
  tally      = [c \in Calls |-> NoTally],
  \* policy[p][c]: the gate process p's deployment gives the call's tool.
  policy     = [p \in Procs |-> Policy0],
  approves1 = 0, submits = 0, redeploys = 0,
  \* Ghosts: a fire without a recorded sufficient approval under a gate; an approval pause
  \* while the valid decisions already met the policy.
  badFire    = FALSE,
  badPause   = FALSE,
  \* Ghost: a drive whose Load read a recorded denial fired the effect.
  badDeny    = FALSE,
  \* The verifier resolver as the gate sees it now (verifierFor), and its changes.
  keys       = KeyOf,
  resChanges = 0;

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
  \* The m-of-n counting rule (TallyApprovals), with the resolver as it is at the count: an
  \* approver's decision is their first record that counts (its signing key is one the approver's
  \* verifier accepts now, and it was signed over this call). Historically (SlotPerApprover), their
  \* first record, valid or not, took their only place; and (UnboundSubject) a signature did not
  \* bind the call.
  Known(a)      == a \in DOMAIN keys
  CountsFor(c, i) == LET r == dlog[c][i] IN
                     Known(r.a) /\ r.k \in keys[r.a] /\ (r.subj = "this" \/ Bug = "UnboundSubject")
  Deciding(c, a) == {i \in DOMAIN dlog[c] :
                      dlog[c][i].a = a /\ (Bug = "SlotPerApprover" \/ CountsFor(c, i))}
  Dec(c, a)     == IF Deciding(c, a) = {} THEN 0 ELSE Min(Deciding(c, a))
  Counted(c, a) == Dec(c, a) # 0 /\ CountsFor(c, Dec(c, a))
  \* The count never seats an approver whose key set is empty or meets another eligible
  \* approver's (#109), whatever the resolver answered the gate's check: it may have changed.
  Excluded(n, a) == /\ Bug \notin {"NoCountExclusion", "SetEqualityCheck"}
                    /\ Known(a)
                    /\ \/ keys[a] = {}
                       \/ \E b \in Policies[n].apprs \ {a} : Known(b) /\ keys[a] \cap keys[b] # {}
  Seated(c, n, v) == {a \in Policies[n].apprs : ~Excluded(n, a) /\ Counted(c, a) /\ dlog[c][Dec(c, a)].ok = v}
  NVotes(c, n, v) == Cardinality(Seated(c, n, v))
  NExcl(n)      == Cardinality({a \in Policies[n].apprs : Excluded(n, a)})
  Passed(c, n)  == NVotes(c, n, TRUE) >= Policies[n].need
  Unreach(c, n) == Cardinality(Policies[n].apprs) - NExcl(n) - NVotes(c, n, FALSE) < Policies[n].need
  \* Ghost for TallySound: the people who signed the seated approvals over this exact call.
  Signers(c, n) == {dlog[c][Dec(c, a)].h : a \in {x \in Seated(c, n, TRUE) : dlog[c][Dec(c, x)].subj = "this"}}
  \* ApprovalPolicy.Validate and ValidateKeys: no two eligible ids that fold to one approver, and
  \* (KeyCheck) no eligible approver the resolver knows with no key, and no two whose key sets
  \* meet (historically SetEqualityCheck: only equal sets refused, and the count seated every
  \* approver).
  PolValid(n)   == /\ ~\E pr \in FoldSame : pr[1] \in Policies[n].apprs /\ pr[2] \in Policies[n].apprs
                   /\ KeyCheck => /\ \A x \in Policies[n].apprs \cap DOMAIN keys : keys[x] # {}
                                  /\ \A x, y \in Policies[n].apprs \cap DOMAIN keys :
                                       x # y => IF Bug = "SetEqualityCheck"
                                                THEN keys[x] # keys[y]
                                                ELSE keys[x] \cap keys[y] = {}
  \* A recorded sufficient approval: an Approve(true), or a recorded tally that passed.
  Sufficient(c) == one[c] = "yes" \/ (tally[c].rec /\ tally[c].passed)
  \* A recorded denial: an Approve(false), or a recorded tally that did not pass.
  Denied(c)     == one[c] = "no" \/ (tally[c].rec /\ ~tally[c].passed)
  \* The approvals already in, by the counting rule without its history: every seated approver's
  \* first valid decision over this call (for NoStuckPause).
  TrueDeciding(c, a) == {i \in DOMAIN dlog[c] : dlog[c][i].a = a /\ Known(a) /\ dlog[c][i].k \in keys[a]
                                                /\ dlog[c][i].subj = "this"}
  TrueDec(c, a) == IF TrueDeciding(c, a) = {} THEN 0 ELSE Min(TrueDeciding(c, a))
  TrueVoters(c, n, v) == {a \in Policies[n].apprs : ~Excluded(n, a) /\ TrueDec(c, a) # 0
                                                  /\ dlog[c][TrueDec(c, a)].ok = v}
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
variables g = 0, gg = 0, reply = "", outcome = None, won = FALSE, reused = FALSE,
          snapOne = "none", snapTally = NoTally, gp = "none", qt = NoTally, loadDenied = FALSE;
begin
Start:
  await Issued(CallOf[self]);
Open:
  \* One Load. A leased driver holds the run's lease for the whole drive.
  if self \in LeasedDrivers then
    await lease \in {None, self};
    lease := self;
  end if;
  won := FALSE; reused := FALSE; g := 0; cid[self] := 0; gp := "none";
  \* The approval gate reads the 1-of-1 decision and a recorded tally from this Load.
  snapOne := one[CallOf[self]]; snapTally := tally[CallOf[self]];
  loadDenied := Denied(CallOf[self]);
  if Recorded(CallOf[self]) then
    outcome := "done"; goto Finish;
  elsif Kind[CallOf[self]] = "flow" /\ marker[CallOf[self]][0] # 0 then
    \* A flow node that is not retry-safe halts on any marker of its step (plan's case 2).
    outcome := "halt_crashed"; goto Finish;
  elsif Kind[CallOf[self]] = "tool" /\ AnyLive(CallOf[self]) then
    \* The resume gate: a live marker halts, unless this process remembers its claim.
    with x = CHOOSE y \in Gens : Live(CallOf[self], y) do
      gg := x; oldId[self] := marker[CallOf[self]][x];
    end with;
    outcome := None; goto GateTake;
  elsif Kind[CallOf[self]] = "tool" then
    outcome := None; goto ApGate;
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
    oldId[self] := 0; goto ApGate;
  end if;
ApGate:
  \* The loop's pre-pass for the call. A recorded denial is final whatever the gate is now (#70).
  if (snapOne = "no" \/ (snapTally.rec /\ ~snapTally.passed)) /\ Bug # "DenialNotFinal" then
    goto Deny;
  elsif policy[ProcOf[self]][CallOf[self]] = "none" then
    goto Claim;
  elsif policy[ProcOf[self]][CallOf[self]] = "one" then
    gp := "one";
    if snapOne = "none" then
      outcome := "pause_approval"; goto Finish;
    elsif snapOne = "yes" then
      goto Claim;
    else
      goto Deny;
    end if;
  else
    gp := policy[ProcOf[self]][CallOf[self]]; goto QTally;
  end if;
QTally:
  \* quorumTally: the policy and key checks (every evaluation, before a recorded tally is read),
  \* then a fresh History read; a recorded tally stands.
  if ~PolValid(gp) /\ Bug # "NoFoldCheck" then
    outcome := "error"; goto Finish;          \* ErrConfig: the policy is refused
  elsif tally[CallOf[self]].rec then
    if tally[CallOf[self]].passed then goto Claim; else goto Deny; end if;
  end if;
QCount:
  \* TallyApprovals: the count, with the resolver as it is now (it may have changed since the
  \* check); pause unless the count is final.
  if ~Passed(CallOf[self], gp) /\ ~Unreach(CallOf[self], gp) then
    if Cardinality(TrueVoters(CallOf[self], gp, TRUE)) >= Policies[gp].need then
      badPause := TRUE;
    end if;
    outcome := "pause_approval"; goto Finish;
  else
    qt := [rec |-> TRUE, passed |-> Passed(CallOf[self], gp), pol |-> gp,
           signers |-> Signers(CallOf[self], gp), excl |-> NExcl(gp),
           denials |-> NVotes(CallOf[self], gp, FALSE)];
  end if;
QRecord:
  \* The terminal tally, journaled before the tool runs (a retry-safe step: first writer wins, and
  \* the gate goes by the record the journal holds).
  Reply(reply);
  if reply \in {"ok", "err_c"} /\ ~tally[CallOf[self]].rec then
    tally[CallOf[self]] := qt;
  end if;
  if reply # "ok" then
    outcome := "error"; goto Finish;
  elsif tally[CallOf[self]].passed then
    goto Claim;
  else
    goto Deny;
  end if;
Deny:
  \* The denial is recorded as the call's result, and the model reads it.
  Reply(reply);
  WriteResult(CallOf[self], "denied", reply);
  if reply = "ok" then outcome := "done"; else outcome := "error"; end if;
  goto Finish;
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
    if gp # "none" /\ ~Sufficient(CallOf[self]) then badFire := TRUE; end if;
    if loadDenied then badDeny := TRUE; end if;
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
  \* WithoutLiveDriverCheck skips the claim as well as the check.
  if ~ResolveClaim \/ LiveCheck = "none" then goto RWrite; end if;
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
          lost, evicted, one, dlog, tally, policy, approves1, submits, 
          redeploys, badFire, badPause, badDeny, keys, resChanges, pc

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





Known(a)      == a \in DOMAIN keys
CountsFor(c, i) == LET r == dlog[c][i] IN
                   Known(r.a) /\ r.k \in keys[r.a] /\ (r.subj = "this" \/ Bug = "UnboundSubject")
Deciding(c, a) == {i \in DOMAIN dlog[c] :
                    dlog[c][i].a = a /\ (Bug = "SlotPerApprover" \/ CountsFor(c, i))}
Dec(c, a)     == IF Deciding(c, a) = {} THEN 0 ELSE Min(Deciding(c, a))
Counted(c, a) == Dec(c, a) # 0 /\ CountsFor(c, Dec(c, a))


Excluded(n, a) == /\ Bug \notin {"NoCountExclusion", "SetEqualityCheck"}
                  /\ Known(a)
                  /\ \/ keys[a] = {}
                     \/ \E b \in Policies[n].apprs \ {a} : Known(b) /\ keys[a] \cap keys[b] # {}
Seated(c, n, v) == {a \in Policies[n].apprs : ~Excluded(n, a) /\ Counted(c, a) /\ dlog[c][Dec(c, a)].ok = v}
NVotes(c, n, v) == Cardinality(Seated(c, n, v))
NExcl(n)      == Cardinality({a \in Policies[n].apprs : Excluded(n, a)})
Passed(c, n)  == NVotes(c, n, TRUE) >= Policies[n].need
Unreach(c, n) == Cardinality(Policies[n].apprs) - NExcl(n) - NVotes(c, n, FALSE) < Policies[n].need

Signers(c, n) == {dlog[c][Dec(c, a)].h : a \in {x \in Seated(c, n, TRUE) : dlog[c][Dec(c, x)].subj = "this"}}




PolValid(n)   == /\ ~\E pr \in FoldSame : pr[1] \in Policies[n].apprs /\ pr[2] \in Policies[n].apprs
                 /\ KeyCheck => /\ \A x \in Policies[n].apprs \cap DOMAIN keys : keys[x] # {}
                                /\ \A x, y \in Policies[n].apprs \cap DOMAIN keys :
                                     x # y => IF Bug = "SetEqualityCheck"
                                              THEN keys[x] # keys[y]
                                              ELSE keys[x] \cap keys[y] = {}

Sufficient(c) == one[c] = "yes" \/ (tally[c].rec /\ tally[c].passed)

Denied(c)     == one[c] = "no" \/ (tally[c].rec /\ ~tally[c].passed)


TrueDeciding(c, a) == {i \in DOMAIN dlog[c] : dlog[c][i].a = a /\ Known(a) /\ dlog[c][i].k \in keys[a]
                                              /\ dlog[c][i].subj = "this"}
TrueDec(c, a) == IF TrueDeciding(c, a) = {} THEN 0 ELSE Min(TrueDeciding(c, a))
TrueVoters(c, n, v) == {a \in Policies[n].apprs : ~Excluded(n, a) /\ TrueDec(c, a) # 0
                                                /\ dlog[c][TrueDec(c, a)].ok = v}

VARIABLES g, gg, reply, outcome, won, reused, snapOne, snapTally, gp, qt, 
          loadDenied, rc, rg, rreply, rclaimed

vars == << marker, nsSet, heldSet, result, toolFail, lateMarker, lateNS, 
           lateResult, pending, fl, waiters, jres, cid, oldId, toRetry, lease, 
           rids, rcid, ambig, crashes, cancels, evictions, fired, firedAt, 
           lost, evicted, one, dlog, tally, policy, approves1, submits, 
           redeploys, badFire, badPause, badDeny, keys, resChanges, pc, g, gg, 
           reply, outcome, won, reused, snapOne, snapTally, gp, qt, 
           loadDenied, rc, rg, rreply, rclaimed >>

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
        /\ one = [c \in Calls |-> "none"]
        /\ dlog = [c \in Calls |-> <<>>]
        /\ tally = [c \in Calls |-> NoTally]
        /\ policy = [p \in Procs |-> Policy0]
        /\ approves1 = 0
        /\ submits = 0
        /\ redeploys = 0
        /\ badFire = FALSE
        /\ badPause = FALSE
        /\ badDeny = FALSE
        /\ keys = KeyOf
        /\ resChanges = 0
        (* Process driver *)
        /\ g = [self \in Drivers |-> 0]
        /\ gg = [self \in Drivers |-> 0]
        /\ reply = [self \in Drivers |-> ""]
        /\ outcome = [self \in Drivers |-> None]
        /\ won = [self \in Drivers |-> FALSE]
        /\ reused = [self \in Drivers |-> FALSE]
        /\ snapOne = [self \in Drivers |-> "none"]
        /\ snapTally = [self \in Drivers |-> NoTally]
        /\ gp = [self \in Drivers |-> "none"]
        /\ qt = [self \in Drivers |-> NoTally]
        /\ loadDenied = [self \in Drivers |-> FALSE]
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
                               firedAt, lost, evicted, one, dlog, tally, 
                               policy, approves1, submits, redeploys, badFire, 
                               badPause, badDeny, keys, resChanges, g, gg, 
                               reply, outcome, won, reused, snapOne, snapTally, 
                               gp, qt, loadDenied, rc, rg, rreply, rclaimed >>

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
              /\ gp' = [gp EXCEPT ![self] = "none"]
              /\ snapOne' = [snapOne EXCEPT ![self] = one[CallOf[self]]]
              /\ snapTally' = [snapTally EXCEPT ![self] = tally[CallOf[self]]]
              /\ loadDenied' = [loadDenied EXCEPT ![self] = Denied(CallOf[self])]
              /\ IF Recorded(CallOf[self])
                    THEN /\ outcome' = [outcome EXCEPT ![self] = "done"]
                         /\ pc' = [pc EXCEPT ![self] = "Finish"]
                         /\ UNCHANGED << oldId, gg >>
                    ELSE /\ IF Kind[CallOf[self]] = "flow" /\ marker[CallOf[self]][0] # 0
                               THEN /\ outcome' = [outcome EXCEPT ![self] = "halt_crashed"]
                                    /\ pc' = [pc EXCEPT ![self] = "Finish"]
                                    /\ UNCHANGED << oldId, gg >>
                               ELSE /\ IF Kind[CallOf[self]] = "tool" /\ AnyLive(CallOf[self])
                                          THEN /\ LET x == CHOOSE y \in Gens : Live(CallOf[self], y) IN
                                                    /\ gg' = [gg EXCEPT ![self] = x]
                                                    /\ oldId' = [oldId EXCEPT ![self] = marker[CallOf[self]][x]]
                                               /\ outcome' = [outcome EXCEPT ![self] = None]
                                               /\ pc' = [pc EXCEPT ![self] = "GateTake"]
                                          ELSE /\ IF Kind[CallOf[self]] = "tool"
                                                     THEN /\ outcome' = [outcome EXCEPT ![self] = None]
                                                          /\ pc' = [pc EXCEPT ![self] = "ApGate"]
                                                     ELSE /\ outcome' = [outcome EXCEPT ![self] = None]
                                                          /\ pc' = [pc EXCEPT ![self] = "Claim"]
                                               /\ UNCHANGED << oldId, gg >>
              /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                              lateMarker, lateNS, lateResult, pending, fl, 
                              waiters, jres, toRetry, rids, rcid, ambig, 
                              crashes, cancels, evictions, fired, firedAt, 
                              lost, evicted, one, dlog, tally, policy, 
                              approves1, submits, redeploys, badFire, badPause, 
                              badDeny, keys, resChanges, reply, qt, rc, rg, 
                              rreply, rclaimed >>

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
                                  lost, evicted, one, dlog, tally, policy, 
                                  approves1, submits, redeploys, badFire, 
                                  badPause, badDeny, keys, resChanges, g, gg, 
                                  reply, won, reused, snapOne, snapTally, gp, 
                                  qt, loadDenied, rc, rg, rreply, rclaimed >>

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
                              /\ pc' = [pc EXCEPT ![self] = "ApGate"]
                              /\ UNCHANGED << pending, outcome >>
                   /\ UNCHANGED << marker, heldSet, result, toolFail, 
                                   lateMarker, lateResult, fl, waiters, jres, 
                                   cid, toRetry, lease, rids, rcid, crashes, 
                                   cancels, evictions, fired, firedAt, lost, 
                                   evicted, one, dlog, tally, policy, 
                                   approves1, submits, redeploys, badFire, 
                                   badPause, badDeny, keys, resChanges, g, gg, 
                                   won, reused, snapOne, snapTally, gp, qt, 
                                   loadDenied, rc, rg, rreply, rclaimed >>

ApGate(self) == /\ pc[self] = "ApGate"
                /\ IF (snapOne[self] = "no" \/ (snapTally[self].rec /\ ~snapTally[self].passed)) /\ Bug # "DenialNotFinal"
                      THEN /\ pc' = [pc EXCEPT ![self] = "Deny"]
                           /\ UNCHANGED << outcome, gp >>
                      ELSE /\ IF policy[ProcOf[self]][CallOf[self]] = "none"
                                 THEN /\ pc' = [pc EXCEPT ![self] = "Claim"]
                                      /\ UNCHANGED << outcome, gp >>
                                 ELSE /\ IF policy[ProcOf[self]][CallOf[self]] = "one"
                                            THEN /\ gp' = [gp EXCEPT ![self] = "one"]
                                                 /\ IF snapOne[self] = "none"
                                                       THEN /\ outcome' = [outcome EXCEPT ![self] = "pause_approval"]
                                                            /\ pc' = [pc EXCEPT ![self] = "Finish"]
                                                       ELSE /\ IF snapOne[self] = "yes"
                                                                  THEN /\ pc' = [pc EXCEPT ![self] = "Claim"]
                                                                  ELSE /\ pc' = [pc EXCEPT ![self] = "Deny"]
                                                            /\ UNCHANGED outcome
                                            ELSE /\ gp' = [gp EXCEPT ![self] = policy[ProcOf[self]][CallOf[self]]]
                                                 /\ pc' = [pc EXCEPT ![self] = "QTally"]
                                                 /\ UNCHANGED outcome
                /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                lateMarker, lateNS, lateResult, pending, fl, 
                                waiters, jres, cid, oldId, toRetry, lease, 
                                rids, rcid, ambig, crashes, cancels, evictions, 
                                fired, firedAt, lost, evicted, one, dlog, 
                                tally, policy, approves1, submits, redeploys, 
                                badFire, badPause, badDeny, keys, resChanges, 
                                g, gg, reply, won, reused, snapOne, snapTally, 
                                qt, loadDenied, rc, rg, rreply, rclaimed >>

QTally(self) == /\ pc[self] = "QTally"
                /\ IF ~PolValid(gp[self]) /\ Bug # "NoFoldCheck"
                      THEN /\ outcome' = [outcome EXCEPT ![self] = "error"]
                           /\ pc' = [pc EXCEPT ![self] = "Finish"]
                      ELSE /\ IF tally[CallOf[self]].rec
                                 THEN /\ IF tally[CallOf[self]].passed
                                            THEN /\ pc' = [pc EXCEPT ![self] = "Claim"]
                                            ELSE /\ pc' = [pc EXCEPT ![self] = "Deny"]
                                 ELSE /\ pc' = [pc EXCEPT ![self] = "QCount"]
                           /\ UNCHANGED outcome
                /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                lateMarker, lateNS, lateResult, pending, fl, 
                                waiters, jres, cid, oldId, toRetry, lease, 
                                rids, rcid, ambig, crashes, cancels, evictions, 
                                fired, firedAt, lost, evicted, one, dlog, 
                                tally, policy, approves1, submits, redeploys, 
                                badFire, badPause, badDeny, keys, resChanges, 
                                g, gg, reply, won, reused, snapOne, snapTally, 
                                gp, qt, loadDenied, rc, rg, rreply, rclaimed >>

QCount(self) == /\ pc[self] = "QCount"
                /\ IF ~Passed(CallOf[self], gp[self]) /\ ~Unreach(CallOf[self], gp[self])
                      THEN /\ IF Cardinality(TrueVoters(CallOf[self], gp[self], TRUE)) >= Policies[gp[self]].need
                                 THEN /\ badPause' = TRUE
                                 ELSE /\ TRUE
                                      /\ UNCHANGED badPause
                           /\ outcome' = [outcome EXCEPT ![self] = "pause_approval"]
                           /\ pc' = [pc EXCEPT ![self] = "Finish"]
                           /\ qt' = qt
                      ELSE /\ qt' = [qt EXCEPT ![self] = [rec |-> TRUE, passed |-> Passed(CallOf[self], gp[self]), pol |-> gp[self],
                                                          signers |-> Signers(CallOf[self], gp[self]), excl |-> NExcl(gp[self]),
                                                          denials |-> NVotes(CallOf[self], gp[self], FALSE)]]
                           /\ pc' = [pc EXCEPT ![self] = "QRecord"]
                           /\ UNCHANGED << badPause, outcome >>
                /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                lateMarker, lateNS, lateResult, pending, fl, 
                                waiters, jres, cid, oldId, toRetry, lease, 
                                rids, rcid, ambig, crashes, cancels, evictions, 
                                fired, firedAt, lost, evicted, one, dlog, 
                                tally, policy, approves1, submits, redeploys, 
                                badFire, badDeny, keys, resChanges, g, gg, 
                                reply, won, reused, snapOne, snapTally, gp, 
                                loadDenied, rc, rg, rreply, rclaimed >>

QRecord(self) == /\ pc[self] = "QRecord"
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
                 /\ IF reply'[self] \in {"ok", "err_c"} /\ ~tally[CallOf[self]].rec
                       THEN /\ tally' = [tally EXCEPT ![CallOf[self]] = qt[self]]
                       ELSE /\ TRUE
                            /\ tally' = tally
                 /\ IF reply'[self] # "ok"
                       THEN /\ outcome' = [outcome EXCEPT ![self] = "error"]
                            /\ pc' = [pc EXCEPT ![self] = "Finish"]
                       ELSE /\ IF tally'[CallOf[self]].passed
                                  THEN /\ pc' = [pc EXCEPT ![self] = "Claim"]
                                  ELSE /\ pc' = [pc EXCEPT ![self] = "Deny"]
                            /\ UNCHANGED outcome
                 /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                 lateMarker, lateNS, lateResult, pending, fl, 
                                 waiters, jres, cid, oldId, toRetry, lease, 
                                 rids, rcid, crashes, cancels, evictions, 
                                 fired, firedAt, lost, evicted, one, dlog, 
                                 policy, approves1, submits, redeploys, 
                                 badFire, badPause, badDeny, keys, resChanges, 
                                 g, gg, won, reused, snapOne, snapTally, gp, 
                                 qt, loadDenied, rc, rg, rreply, rclaimed >>

Deny(self) == /\ pc[self] = "Deny"
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
                               THEN /\ result' = [result EXCEPT ![(CallOf[self])] = "denied"]
                               ELSE /\ TRUE
                                    /\ UNCHANGED result
                         /\ UNCHANGED lateResult
                    ELSE /\ IF reply'[self] = "err_late"
                               THEN /\ lateResult' = (lateResult \cup {<<(CallOf[self]), "denied">>})
                               ELSE /\ TRUE
                                    /\ UNCHANGED lateResult
                         /\ UNCHANGED result
              /\ IF reply'[self] = "ok"
                    THEN /\ outcome' = [outcome EXCEPT ![self] = "done"]
                    ELSE /\ outcome' = [outcome EXCEPT ![self] = "error"]
              /\ pc' = [pc EXCEPT ![self] = "Finish"]
              /\ UNCHANGED << marker, nsSet, heldSet, toolFail, lateMarker, 
                              lateNS, pending, fl, waiters, jres, cid, oldId, 
                              toRetry, lease, rids, rcid, crashes, cancels, 
                              evictions, fired, firedAt, lost, evicted, one, 
                              dlog, tally, policy, approves1, submits, 
                              redeploys, badFire, badPause, badDeny, keys, 
                              resChanges, g, gg, won, reused, snapOne, 
                              snapTally, gp, qt, loadDenied, rc, rg, rreply, 
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
                               evicted, one, dlog, tally, policy, approves1, 
                               submits, redeploys, badFire, badPause, badDeny, 
                               keys, resChanges, g, gg, reply, outcome, won, 
                               snapOne, snapTally, gp, qt, loadDenied, rc, rg, 
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
                                    evicted, one, dlog, tally, policy, 
                                    approves1, submits, redeploys, badFire, 
                                    badPause, badDeny, keys, resChanges, g, gg, 
                                    outcome, won, reused, snapOne, snapTally, 
                                    gp, qt, loadDenied, rc, rg, rreply, 
                                    rclaimed >>

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
                                     firedAt, lost, evicted, one, dlog, tally, 
                                     policy, approves1, submits, redeploys, 
                                     badFire, badPause, badDeny, keys, 
                                     resChanges, g, gg, outcome, reused, 
                                     snapOne, snapTally, gp, qt, loadDenied, 
                                     rc, rg, rreply, rclaimed >>

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
                                 evictions, fired, firedAt, lost, evicted, one, 
                                 dlog, tally, policy, approves1, submits, 
                                 redeploys, badFire, badPause, badDeny, keys, 
                                 resChanges, g, gg, won, reused, snapOne, 
                                 snapTally, gp, qt, loadDenied, rc, rg, rreply, 
                                 rclaimed >>

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
                              evicted, one, dlog, tally, policy, approves1, 
                              submits, redeploys, badFire, badPause, badDeny, 
                              keys, resChanges, g, gg, reused, snapOne, 
                              snapTally, gp, qt, loadDenied, rc, rg, rreply, 
                              rclaimed >>

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
                              firedAt, lost, evicted, one, dlog, tally, policy, 
                              approves1, submits, redeploys, badFire, badPause, 
                              badDeny, keys, resChanges, gg, reply, won, 
                              reused, snapOne, snapTally, gp, qt, loadDenied, 
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
                              lost, evicted, one, dlog, tally, policy, 
                              approves1, submits, redeploys, badFire, badPause, 
                              badDeny, keys, resChanges, g, gg, reply, outcome, 
                              won, reused, snapOne, snapTally, gp, qt, 
                              loadDenied, rc, rg, rreply, rclaimed >>

LoserRead(self) == /\ pc[self] = "LoserRead"
                   /\ IF result[CallOf[self]] # None
                         THEN /\ outcome' = [outcome EXCEPT ![self] = "done"]
                         ELSE /\ outcome' = [outcome EXCEPT ![self] = "halt_crashed"]
                   /\ pc' = [pc EXCEPT ![self] = "Finish"]
                   /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                   lateMarker, lateNS, lateResult, pending, fl, 
                                   waiters, jres, cid, oldId, toRetry, lease, 
                                   rids, rcid, ambig, crashes, cancels, 
                                   evictions, fired, firedAt, lost, evicted, 
                                   one, dlog, tally, policy, approves1, 
                                   submits, redeploys, badFire, badPause, 
                                   badDeny, keys, resChanges, g, gg, reply, 
                                   won, reused, snapOne, snapTally, gp, qt, 
                                   loadDenied, rc, rg, rreply, rclaimed >>

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
                                   firedAt, lost, evicted, one, dlog, tally, 
                                   policy, approves1, submits, redeploys, 
                                   badFire, badPause, badDeny, keys, 
                                   resChanges, g, gg, reply, won, reused, 
                                   snapOne, snapTally, gp, qt, loadDenied, rc, 
                                   rg, rreply, rclaimed >>

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
                                   fired, firedAt, lost, evicted, one, dlog, 
                                   tally, policy, approves1, submits, 
                                   redeploys, badFire, badPause, badDeny, keys, 
                                   resChanges, g, gg, reply, won, reused, 
                                   snapOne, snapTally, gp, qt, loadDenied, rc, 
                                   rg, rreply, rclaimed >>

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
                             evicted, one, dlog, tally, policy, approves1, 
                             submits, redeploys, badFire, badPause, badDeny, 
                             keys, resChanges, g, gg, reply, outcome, won, 
                             reused, snapOne, snapTally, gp, qt, loadDenied, 
                             rc, rg, rreply, rclaimed >>

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
                                    one, dlog, tally, policy, approves1, 
                                    submits, redeploys, badFire, badPause, 
                                    badDeny, keys, resChanges, g, gg, reply, 
                                    won, reused, snapOne, snapTally, gp, qt, 
                                    loadDenied, rc, rg, rreply, rclaimed >>

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
                    /\ UNCHANGED <<toolFail, fired, firedAt, badFire, badDeny>>
                 \/ /\ fired' = [fired EXCEPT ![CallOf[self]] = fired[CallOf[self]] + 1]
                    /\ firedAt' = [firedAt EXCEPT ![CallOf[self]][g[self]] = cid[self]]
                    /\ IF gp[self] # "none" /\ ~Sufficient(CallOf[self])
                          THEN /\ badFire' = TRUE
                          ELSE /\ TRUE
                               /\ UNCHANGED badFire
                    /\ IF loadDenied[self]
                          THEN /\ badDeny' = TRUE
                          ELSE /\ TRUE
                               /\ UNCHANGED badDeny
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
                              lost, evicted, one, dlog, tally, policy, 
                              approves1, submits, redeploys, badPause, keys, 
                              resChanges, g, gg, reply, won, reused, snapOne, 
                              snapTally, gp, qt, loadDenied, rc, rg, rreply, 
                              rclaimed >>

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
                                firedAt, lost, evicted, one, dlog, tally, 
                                policy, approves1, submits, redeploys, badFire, 
                                badPause, badDeny, keys, resChanges, g, gg, 
                                won, reused, snapOne, snapTally, gp, qt, 
                                loadDenied, rc, rg, rreply, rclaimed >>

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
                                    firedAt, lost, evicted, one, dlog, tally, 
                                    policy, approves1, submits, redeploys, 
                                    badFire, badPause, badDeny, keys, 
                                    resChanges, g, gg, outcome, won, reused, 
                                    snapOne, snapTally, gp, qt, loadDenied, rc, 
                                    rg, rreply, rclaimed >>

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
                                firedAt, lost, evicted, one, dlog, tally, 
                                policy, approves1, submits, redeploys, badFire, 
                                badPause, badDeny, keys, resChanges, g, gg, 
                                reply, outcome, won, reused, snapOne, 
                                snapTally, gp, qt, loadDenied, rc, rg, rreply, 
                                rclaimed >>

driver(self) == Start(self) \/ Open(self) \/ GateTake(self)
                   \/ GateWrite(self) \/ ApGate(self) \/ QTally(self)
                   \/ QCount(self) \/ QRecord(self) \/ Deny(self)
                   \/ Claim(self) \/ ClaimRetry(self) \/ ClaimInsert(self)
                   \/ ClaimNS(self) \/ Hold(self) \/ Lost(self)
                   \/ Join(self) \/ LoserRead(self) \/ LoserLead(self)
                   \/ LoserWait(self) \/ Win(self) \/ WinnerWait(self)
                   \/ Call(self) \/ Record(self) \/ NotStarted(self)
                   \/ Finish(self)

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
                /\ IF ~ResolveClaim \/ LiveCheck = "none"
                      THEN /\ pc' = [pc EXCEPT ![self] = "RWrite"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "RClaim"]
                /\ UNCHANGED << marker, nsSet, heldSet, result, toolFail, 
                                lateMarker, lateNS, lateResult, pending, fl, 
                                waiters, jres, cid, oldId, toRetry, rids, rcid, 
                                ambig, crashes, cancels, evictions, fired, 
                                firedAt, lost, evicted, one, dlog, tally, 
                                policy, approves1, submits, redeploys, badFire, 
                                badPause, badDeny, keys, resChanges, g, gg, 
                                reply, outcome, won, reused, snapOne, 
                                snapTally, gp, qt, loadDenied, rreply, 
                                rclaimed >>

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
                                lost, evicted, one, dlog, tally, policy, 
                                approves1, submits, redeploys, badFire, 
                                badPause, badDeny, keys, resChanges, g, gg, 
                                reply, outcome, won, reused, snapOne, 
                                snapTally, gp, qt, loadDenied, rc, rg, rreply, 
                                rclaimed >>

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
                                fired, firedAt, lost, evicted, one, dlog, 
                                tally, policy, approves1, submits, redeploys, 
                                badFire, badPause, badDeny, keys, resChanges, 
                                g, gg, reply, outcome, won, reused, snapOne, 
                                snapTally, gp, qt, loadDenied, rc, rg, 
                                rclaimed >>

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
                                 evictions, fired, firedAt, lost, evicted, one, 
                                 dlog, tally, policy, approves1, submits, 
                                 redeploys, badFire, badPause, badDeny, keys, 
                                 resChanges, g, gg, reply, outcome, won, 
                                 reused, snapOne, snapTally, gp, qt, 
                                 loadDenied, rc, rg >>

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
                                  lost, evicted, one, dlog, tally, policy, 
                                  approves1, submits, redeploys, badFire, 
                                  badPause, badDeny, keys, resChanges, g, gg, 
                                  reply, outcome, won, reused, snapOne, 
                                  snapTally, gp, qt, loadDenied, rc, rg, 
                                  rclaimed >>

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
                                lost, evicted, one, dlog, tally, policy, 
                                approves1, submits, redeploys, badFire, 
                                badPause, badDeny, keys, resChanges, g, gg, 
                                reply, outcome, won, reused, snapOne, 
                                snapTally, gp, qt, loadDenied, rc, rg, rreply, 
                                rclaimed >>

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
                                 fired, firedAt, lost, evicted, one, dlog, 
                                 tally, policy, approves1, submits, redeploys, 
                                 badFire, badPause, badDeny, keys, resChanges, 
                                 g, gg, reply, outcome, won, reused, snapOne, 
                                 snapTally, gp, qt, loadDenied, rc, rg, 
                                 rclaimed >>

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
                               firedAt, lost, evicted, one, dlog, tally, 
                               policy, approves1, submits, redeploys, badFire, 
                               badPause, badDeny, keys, resChanges, g, gg, 
                               reply, outcome, won, reused, snapOne, snapTally, 
                               gp, qt, loadDenied, rc, rg, rreply, rclaimed >>

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
                                     firedAt, lost, evicted, one, dlog, tally, 
                                     policy, approves1, submits, redeploys, 
                                     badFire, badPause, badDeny, keys, 
                                     resChanges, g, gg, reply, outcome, won, 
                                     reused, snapOne, snapTally, gp, qt, 
                                     loadDenied, rc, rg, rclaimed >>

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
                                  fired, firedAt, lost, evicted, one, dlog, 
                                  tally, policy, approves1, submits, redeploys, 
                                  badFire, badPause, badDeny, keys, resChanges, 
                                  g, gg, reply, outcome, won, reused, snapOne, 
                                  snapTally, gp, qt, loadDenied, rc, rg, 
                                  rreply, rclaimed >>

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

\* The approval gate's journal and environment.
ApVars == <<one, dlog, tally, policy, approves1, submits, redeploys, badFire, badPause, badDeny,
             keys, resChanges>>

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
  /\ snapOne' = [d \in DOMAIN snapOne |-> IF d \in ds THEN "none" ELSE snapOne[d]]
  /\ snapTally' = [d \in DOMAIN snapTally |-> IF d \in ds THEN NoTally ELSE snapTally[d]]
  /\ gp' = [d \in DOMAIN gp |-> IF d \in ds THEN "none" ELSE gp[d]]
  /\ qt' = [d \in DOMAIN qt |-> IF d \in ds THEN NoTally ELSE qt[d]]
  /\ loadDenied' = [d \in DOMAIN loadDenied |-> IF d \in ds THEN FALSE ELSE loadDenied[d]]
  /\ UNCHANGED ApVars
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

LateVars == <<ApVars, snapOne, snapTally, gp, qt, loadDenied, heldSet, toolFail, pending, fl, waiters, jres, cid, oldId, lease, rids, rcid, toRetry,
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
  /\ UNCHANGED <<ApVars, snapOne, snapTally, gp, qt, loadDenied>>

\* Everything the approval environment leaves alone.
CoreVars == <<marker, nsSet, heldSet, result, toolFail, lateMarker, lateNS, lateResult, pending, fl,
              waiters, jres, cid, oldId, toRetry, lease, rids, rcid, ambig, crashes, cancels,
              evictions, fired, firedAt, lost, evicted, badFire, badPause, badDeny, pc, g, gg, reply,
              outcome, won, reused, snapOne, snapTally, gp, qt, loadDenied, rc, rg, rreply, rclaimed>>

\* Approve (1-of-1): the first decision recorded for the call wins.
Approve1(c, v) ==
  /\ approves1 < MaxApprove1
  /\ approves1' = approves1 + 1
  /\ one' = IF one[c] = "none" THEN [one EXCEPT ![c] = v] ELSE one
  /\ UNCHANGED <<dlog, tally, policy, submits, redeploys, keys, resChanges>>
  /\ UNCHANGED CoreVars

\* SubmitDecision (m-of-n): person h signs a decision naming approver a with key k. It counts only
\* if a's verifier accepts k when the gate counts, and it was signed over this exact call. An
\* identical resubmission is the same journal record, so it adds nothing. People sign as an
\* approver with a key they hold that a resolver maps to it; an actor holding no key forges.
Held(h) == {k \in DOMAIN Holder : Holder[k] = h}
Submit(c, h, a, v, sj, k) ==
  LET r == [a |-> a, h |-> h, ok |-> v, k |-> k, subj |-> sj] IN
  /\ submits < MaxSubmit
  /\ \/ /\ k \in Held(h)
        /\ \/ a \in DOMAIN KeyOf /\ k \in KeyOf[a]
           \/ a \in DOMAIN KeyOf2 /\ k \in KeyOf2[a]
     \/ Held(h) = {} /\ k = "none"
  /\ \A i \in DOMAIN dlog[c] : dlog[c][i] # r
  /\ submits' = submits + 1
  /\ dlog' = [dlog EXCEPT ![c] = Append(dlog[c], r)]
  /\ UNCHANGED <<one, tally, policy, approves1, redeploys, keys, resChanges>>
  /\ UNCHANGED CoreVars

\* A redeploy of process p changes the call's tool gate (removed, loosened, tightened or made
\* m-of-n) between p's drives; another process may still run the old deployment.
Redeploy(p, c, n) ==
  /\ redeploys < MaxRedeploy
  /\ policy[p][c] # n
  /\ \A d \in Drivers : ProcOf[d] = p => pc[d] \in {"Start", "Open", "Finish", "Done"}
  /\ redeploys' = redeploys + 1
  /\ policy' = [policy EXCEPT ![p][c] = n]
  /\ UNCHANGED <<one, dlog, tally, approves1, submits, keys, resChanges>>
  /\ UNCHANGED CoreVars

\* The verifier resolver changes (a key rotated, a key file redeployed): the gate's next check and
\* count see the new answer, a count already under way included.
ResolverChange ==
  /\ resChanges < MaxResolverChange
  /\ keys # KeyOf2
  /\ resChanges' = resChanges + 1
  /\ keys' = KeyOf2
  /\ UNCHANGED <<one, dlog, tally, policy, approves1, submits, redeploys>>
  /\ UNCHANGED CoreVars

ApprovalEnv ==
  \/ ResolverChange
  \/ \E c \in Calls :
       \/ \E v \in {"yes", "no"} : Approve1(c, v)
       \/ \E h \in Actors, a \in ApproverIds, v \in BOOLEAN, sj \in Subjects,
             k \in DOMAIN Holder \cup {"none"} : Submit(c, h, a, v, sj, k)
       \/ \E p \in Procs, n \in PolicyChoices : Redeploy(p, c, n)

FullNext == Next \/ (\E p \in Procs : Crash(p) \/ Evict(p)) \/ (LateApply /\ UNCHANGED LateVars)
            \/ ApprovalEnv

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

\* Probe, expected violated where reachable: a resolution claims past an attempt that was voided
\* after its check (a process retried a remembered claim's not-started record in between).
ResolveOnlyLiveAttempt ==
  \A r \in ResolverSet : pc[r] = "RInsert" => ~Voided(rc[r], rg[r])

\* The approval gate (model 1b).
\* No effect runs under a gate without a recorded sufficient approval.
NoUnapprovedFire == ~badFire

\* A recorded denial is final: a drive whose Load read it never fires the effect, whatever the gate
\* is now.
DenialFinal == ~badDeny

\* A recorded passing tally rests on enough valid, call-bound approvals signed by distinct
\* people (the holders of the keys that signed the seated approvals).
TallySound ==
  \A c \in Calls : (tally[c].rec /\ tally[c].passed)
                     => Cardinality(tally[c].signers) >= Policies[tally[c].pol].need

\* A recorded failing tally rests on valid, call-bound denials and seats the count excluded: an
\* invalid record cannot force a denial.
DenialSound ==
  \A c \in Calls : (tally[c].rec /\ ~tally[c].passed)
                     => Cardinality(Policies[tally[c].pol].apprs) - tally[c].excl - tally[c].denials
                          < Policies[tally[c].pol].need

\* Probe, violated where reachable: a failing tally rests on denials alone. A resolver change that
\* makes two approvers share a key excludes both seats, and can make the gate deny.
NoDenyWithoutDenials ==
  \A c \in Calls : (tally[c].rec /\ ~tally[c].passed)
                     => Cardinality(Policies[tally[c].pol].apprs) - tally[c].denials
                          < Policies[tally[c].pol].need

\* The gate never waits for approvals that are already in (no lockout).
NoStuckPause == ~badPause

\* Vacuity for the gate: an approved effect fires (expected violated).
GatedFireNotReachable == \A c \in Calls : ~(fired[c] > 0 /\ Policy0[c] # "none")

\* Liveness: a provably unstarted effect does not halt for ever.
Progress == \A c \in Calls : <>[](~Issued(c) \/ Recorded(c) \/ Excused(c))

\* The same, also excusing a live attempt whose claim id an eviction forgot: shows that an
\* eviction costs nothing but those halts.
ProgressModuloEviction ==
  \A c \in Calls : <>[](~Issued(c) \/ Recorded(c) \/ Excused(c)
                        \/ \E x \in Gens : Live(c, x) /\ marker[c][x] \in evicted)
=============================================================================
