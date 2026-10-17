-------------------------------- MODULE Stop --------------------------------
(***************************************************************************)
(* Model 16: the kill switch (docs/design/kill-switch.md). Stops, lifts,   *)
(* policy transitions and mode records are records of a control log. The   *)
(* effective state is a fold of the log (Eff), which the Go code implements *)
(* in StopControl.State. Calls claim an attempt, read the log after the    *)
(* claim, and start only if no covering stop is active in what they read;  *)
(* a stop that is read makes the attempt not started, so it can be claimed *)
(* again. A call that crashed with a marker and no result is halted for    *)
(* good: nothing in the model, a stop and a lift included, starts it       *)
(* again.                                                                  *)
(*                                                                          *)
(* The mode is the process's configuration (Cfg), never the store's. A     *)
(* basic process trusts the store: any set is a stop, one lift lifts it,   *)
(* policy and mode records change nothing. An audited process counts a     *)
(* record only if it is signed (sig) by an approver the policy in force    *)
(* enrols; a signature is made only by an enrolled key (Unforgeable). The  *)
(* policy in force starts as the process's own (policy 1) and advances by  *)
(* transitions approved by a quorum of lifters of the one in force; each   *)
(* record is judged under the policy in force at its position, so a stop   *)
(* set under policy 1 stays in force after a transition. Without the chain *)
(* (Chain = FALSE) the process is pinned to the last policy and drops the  *)
(* stops of the earlier ones.                                              *)
(*                                                                          *)
(* Scope matching is a pure function tested in Go: each stop covers a set  *)
(* of calls (CoverOf). The reader's staleness bound in time is not         *)
(* modelled; ReadFails models a failed read, which the design holds closed *)
(* (FailClosed). The model is the case of MaxStaleness = 0.                *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, Sequences

CONSTANTS
  Calls, StopIds, CoverOf, \* the calls; the stops; the calls each stop covers
  MaxPol, SettersOf, LiftersOf, QuorumOf, \* policies 1..MaxPol: roles and lift quorum
  NeedsOther,        \* the fold's lift rule: one lifter is not the setter
  WantOther,         \* what the invariant demands of a lift (equal to NeedsOther unless a regress)
  People, Forger,    \* the writers of records; the one who holds no enrolled key
  Cfg,               \* the process's mode: "basic" or "audited"
  Pin,               \* the policy (1..MaxPol) an audited process has pinned; the chain is walked from policy 1
  Deps, CheckDep,    \* the deployments records name ("mine" and perhaps "other"); whether the fold checks it
  BasicHonoursPlainLift, \* regress: a basic process lets a plain lift lift a stop set under a signed record
  Chain,             \* TRUE: follow policy transitions; FALSE (regress): pinned to the last policy
  ModeFromStore,     \* regress: a mode record in the store flips the process to audited
  AcceptPlain,       \* regress: an audited process accepts a plain record
  MaxLog, MaxAtt, MaxCrash,
  Check,             \* TRUE: a call reads the control log after its claim; FALSE: it starts at once
  SyncRead,          \* TRUE: that read sees every record committed before the claim; FALSE: a cache
  FailClosed,        \* TRUE: a read that fails holds the call; FALSE: it proceeds
  ReadFails,         \* a read may fail
  LiftRefires        \* regress: a lift starts a halted call again

Pols == 1..MaxPol
Enrolled == UNION {SettersOf[p] \cup LiftersOf[p] : p \in Pols}
Kinds == {"set", "lift", "pol", "mode"}
None == "none"

\* vocabulary: begin
\* The control-run records: a stop, one lifter's lift, one lifter's approval of a policy transition,
\* evidence of a change of mode, the root policy (the chain's start; the model always walks from it).
RecordKinds == {"set", "lift", "pol", "mode"}
\* vocabulary: end

\* Unforgeable: a signed record is by an enrolled approver; the forger signs nothing.
Records == { r \in [k : Kinds, by : People, sig : BOOLEAN, stop : StopIds, to : Pols, dep : Deps] :
               /\ r.sig => r.by \in Enrolled
               /\ ~r.sig => r.by = Forger
               /\ r.k = "pol" => r.to \in 2..MaxPol
               /\ r.k /= "pol" => r.to = 1
               /\ r.k = "mode" => ~r.sig }

Stop0 == [phase |-> "none", setter |-> None, lifts |-> {}, how |-> None, signed |-> FALSE]
Init0C(ch) == [audited |-> (Cfg = "audited"), pol |-> IF ch THEN 1 ELSE MaxPol, tps |-> {},
               st |-> [i \in StopIds |-> Stop0]]

\* The lifters of the policy in force that approved the lift, and whether the rule is met.
ValidLifts(s, i) == s.st[i].lifts \cap LiftersOf[s.pol]
RuleMet(s, i) ==
  LET V == ValidLifts(s, i) IN
  Cardinality(V) >= QuorumOf[s.pol] /\ (~NeedsOther \/ \E x \in V : x /= s.st[i].setter)
Relift(s) ==
  [s EXCEPT !.st = [i \in StopIds |->
     IF s.st[i].phase = "active" /\ RuleMet(s, i)
       THEN [s.st[i] EXCEPT !.phase = "lifted", !.how = "audited"] ELSE s.st[i]]]

ApplyC(ch, s, r) ==
  IF ~s.audited THEN
    CASE r.k = "set" /\ s.st[r.stop].phase = "none"
           -> [s EXCEPT !.st[r.stop] = [Stop0 EXCEPT !.phase = "active", !.setter = r.by, !.signed = r.sig]]
      [] r.k = "lift" /\ s.st[r.stop].phase = "active" /\ (r.sig \/ ~s.st[r.stop].signed \/ BasicHonoursPlainLift)
           -> [s EXCEPT !.st[r.stop].phase = "lifted", !.st[r.stop].how = IF r.sig THEN "signedlift" ELSE "basic"]
      [] r.k = "mode" /\ ModeFromStore -> [s EXCEPT !.audited = TRUE]
      [] OTHER -> s
  ELSE
    CASE r.dep /= "mine" /\ CheckDep /\ r.k /= "mode" -> s
      [] r.k = "set" /\ s.st[r.stop].phase = "none" /\ (r.sig \/ AcceptPlain) /\ (r.by \in SettersOf[s.pol] \/ AcceptPlain)
           -> [s EXCEPT !.st[r.stop] = [Stop0 EXCEPT !.phase = "active", !.setter = r.by, !.signed = r.sig]]
      [] r.k = "lift" /\ s.st[r.stop].phase = "active" /\ r.sig /\ r.by \in LiftersOf[s.pol]
           -> LET s2 == [s EXCEPT !.st[r.stop].lifts = @ \cup {r.by}] IN
              IF RuleMet(s2, r.stop)
                THEN [s2 EXCEPT !.st[r.stop].phase = "lifted", !.st[r.stop].how = "audited"] ELSE s2
      [] r.k = "pol" /\ ch /\ r.sig /\ r.by \in LiftersOf[s.pol] /\ r.to = s.pol + 1
           -> LET T == s.tps \cup {r.by} IN
              IF Cardinality(T \cap LiftersOf[s.pol]) >= QuorumOf[s.pol]
                THEN Relift([s EXCEPT !.pol = r.to, !.tps = {}])
                ELSE [s EXCEPT !.tps = T]
      [] OTHER -> s

RECURSIVE EffC(_, _, _)
EffC(ch, l, n) == IF n = 0 THEN Init0C(ch) ELSE ApplyC(ch, EffC(ch, l, n - 1), l[n])
Eff(l, n) == EffC(Chain, l, n)

\* The process cannot tell which stops are in force: an audited process whose pin the chain has not
\* reached, or a basic process holding an active stop that was set under a signed record.
Unk(s) == IF s.audited THEN Pin > s.pol ELSE \E i \in StopIds : s.st[i].phase = "active" /\ s.st[i].signed

Active(s, c) == \E i \in StopIds : s.st[i].phase = "active" /\ c \in CoverOf[i]
Covers(s, c) == Active(s, c)

VARIABLES
  log,        \* the control log
  calls,      \* c -> "idle", "claimed", "cleared", "started", "done", "halted"
  att,        \* c -> attempts made
  claimLen,   \* c -> length of the log when the latest attempt was claimed
  rd,         \* c -> length of the log prefix the latest attempt's read saw (-1: none)
  fired,      \* c -> times the effect started
  crashes,
  blocked     \* a call has been held back by a stop that was read

vars == <<log, calls, att, claimLen, rd, fired, crashes, blocked>>

TypeOK ==
  /\ log \in Seq(Records) /\ Len(log) <= MaxLog
  /\ calls \in [Calls -> {"idle", "claimed", "cleared", "started", "done", "halted"}]
  /\ att \in [Calls -> 0..MaxAtt] /\ crashes \in 0..MaxCrash /\ blocked \in BOOLEAN

Init ==
  /\ log = <<>> /\ calls = [c \in Calls |-> "idle"] /\ att = [c \in Calls |-> 0]
  /\ claimLen = [c \in Calls |-> 0] /\ rd = [c \in Calls |-> -1] /\ fired = [c \in Calls |-> 0]
  /\ crashes = 0 /\ blocked = FALSE

Write(r) ==
  /\ Len(log) < MaxLog
  /\ log' = Append(log, r)
  /\ UNCHANGED <<calls, att, claimLen, rd, fired, crashes, blocked>>

Claim(c) ==
  /\ calls[c] = "idle" /\ att[c] < MaxAtt
  /\ att' = [att EXCEPT ![c] = @ + 1]
  /\ claimLen' = [claimLen EXCEPT ![c] = Len(log)]
  /\ rd' = [rd EXCEPT ![c] = IF Check THEN -1 ELSE 0]
  /\ calls' = [calls EXCEPT ![c] = IF Check THEN "claimed" ELSE "cleared"]
  /\ UNCHANGED <<log, fired, crashes, blocked>>

\* The check after the claim: it reads the control log (everything committed so far, or, without
\* SyncRead, any earlier prefix), and the attempt is cleared to start, or voided (not started) when a
\* covering stop is read, or held when the read fails and FailClosed.
ReadOk(c) ==
  /\ calls[c] = "claimed"
  /\ \E n \in (IF SyncRead THEN {Len(log)} ELSE 0..Len(log)) :
       IF Covers(Eff(log, n), c)
         THEN /\ calls' = [calls EXCEPT ![c] = "idle"] /\ blocked' = TRUE /\ rd' = rd
         ELSE /\ calls' = [calls EXCEPT ![c] = "cleared"] /\ rd' = [rd EXCEPT ![c] = n] /\ blocked' = blocked
  /\ UNCHANGED <<log, att, claimLen, fired, crashes>>

ReadFail(c) ==
  /\ ReadFails /\ calls[c] = "claimed"
  /\ IF FailClosed
       THEN /\ calls' = [calls EXCEPT ![c] = "idle"] /\ rd' = rd
       ELSE /\ calls' = [calls EXCEPT ![c] = "cleared"] /\ rd' = [rd EXCEPT ![c] = 0]
  /\ UNCHANGED <<log, att, claimLen, fired, crashes, blocked>>

\* A stop may be committed between the read and the start: the call is in flight and finishes.
Begin(c) ==
  /\ calls[c] = "cleared"
  /\ calls' = [calls EXCEPT ![c] = "started"] /\ fired' = [fired EXCEPT ![c] = @ + 1]
  /\ UNCHANGED <<log, att, claimLen, rd, crashes, blocked>>

Finish(c) ==
  /\ calls[c] = "started"
  /\ calls' = [calls EXCEPT ![c] = "done"]
  /\ UNCHANGED <<log, att, claimLen, rd, fired, crashes, blocked>>

\* A crash with a marker and no result: the outcome is unknown, the call halts, for good.
Crash(c) ==
  /\ crashes < MaxCrash /\ calls[c] \in {"claimed", "cleared", "started"}
  /\ calls' = [calls EXCEPT ![c] = "halted"] /\ crashes' = crashes + 1
  /\ UNCHANGED <<log, att, claimLen, rd, fired, blocked>>

\* Regress only: a lift starts a halted call that had fired again.
Refire(c) ==
  /\ LiftRefires /\ calls[c] = "halted" /\ \E i \in StopIds : Eff(log, Len(log)).st[i].phase = "lifted"
  /\ calls' = [calls EXCEPT ![c] = "cleared"] /\ rd' = [rd EXCEPT ![c] = Len(log)]
  /\ UNCHANGED <<log, att, claimLen, fired, crashes, blocked>>

Next == \/ \E r \in Records : Write(r)
        \/ \E c \in Calls : Claim(c) \/ ReadOk(c) \/ ReadFail(c) \/ Begin(c) \/ Finish(c) \/ Crash(c) \/ Refire(c)

Spec == Init /\ [][Next]_vars

----------------------------------------------------------------------------
\* The effect starts at most once, whatever the stop, the lift and the crashes do.
AtMostOnce == \A c \in Calls : fired[c] <= 1

\* A started call read the log after its claim and saw no covering stop. (Without Check, without
\* SyncRead, or on a failed read that is not held, the read is missing or older than the claim.)
StartedSawNoStop ==
  \A c \in Calls : calls[c] \in {"started", "done"} \/ fired[c] > 0 =>
    /\ rd[c] >= claimLen[c]
    /\ ~Covers(Eff(log, rd[c]), c)

\* In an audited process a record that is not signed changes nothing; a mode record changes nothing
\* in any process: the mode is configuration, the store's mode records are evidence.
ForgedNoEffect ==
  \A n \in 0..(Len(log) - 1) :
    (Eff(log, n).audited /\ ~log[n + 1].sig) => Eff(log, n + 1) = Eff(log, n)

ModeRecordsEvidence ==
  \A n \in 0..(Len(log) - 1) : log[n + 1].k = "mode" => Eff(log, n + 1) = Eff(log, n)

\* An audited lift is by a quorum of distinct lifters of the policy in force and, when WantOther, one
\* who is not the setter.
LiftNeedsQuorum ==
  \A n \in 1..Len(log) : \A i \in StopIds :
    (Eff(log, n - 1).st[i].phase = "active" /\ Eff(log, n).st[i].how = "audited") =>
      LET s == Eff(log, n) V == s.st[i].lifts \cap LiftersOf[s.pol] IN
      Cardinality(V) >= QuorumOf[s.pol] /\ (~WantOther \/ \E x \in V : x /= s.st[i].setter)

\* The fold that follows the transitions is the reference: a process that cannot follow them (pinned
\* to the last policy) drops stops set under the earlier ones.
\* A process never silently drops a stop the chain holds: its stops agree with the fold that follows
\* every transition from the root, or it says they are unknowable.
AgreesWithChain ==
  Cfg = "audited" =>
    (Unk(Eff(log, Len(log))) \/ \A i \in StopIds : Eff(log, Len(log)).st[i].phase = EffC(TRUE, log, Len(log)).st[i].phase)

\* A record for another deployment changes nothing in an audited process.
ForeignNoEffect ==
  \A n \in 0..(Len(log) - 1) :
    (Eff(log, n).audited /\ log[n + 1].dep /= "mine" /\ log[n + 1].k /= "mode") => Eff(log, n + 1) = Eff(log, n)

\* A basic process does not lift a stop set under a signed record on a plain lift.
BasicSignedStopNeedsSignedLift ==
  Cfg = "basic" => \A i \in StopIds :
    (Eff(log, Len(log)).st[i].signed /\ Eff(log, Len(log)).st[i].phase = "lifted") => Eff(log, Len(log)).st[i].how = "signedlift"

\* A stop never goes back: none, then active, then lifted.
Rank(p) == IF p = "none" THEN 0 ELSE IF p = "active" THEN 1 ELSE 2
StopsNeverDropped ==
  [][\A i \in StopIds : Rank(Eff(log', Len(log')).st[i].phase) >= Rank(Eff(log, Len(log)).st[i].phase)]_vars

\* Only Begin starts the effect: a stop, a lift or a change of mode never does.
OnlyBeginFires == [][\A c \in Calls : fired'[c] /= fired[c] => (calls[c] = "cleared" /\ calls'[c] = "started")]_vars

\* Vacuity and reachability: each is violated when the effect is reached.
EffectNotReachable == \A c \in Calls : fired[c] = 0
BlockedNotReachable == ~blocked
AuditedLiftNotReachable == ~(\E i \in StopIds : Eff(log, Len(log)).st[i].how = "audited")
RotatedNotReachable == ~(Eff(log, Len(log)).pol = 2 /\ \E i \in StopIds : Eff(log, Len(log)).st[i].phase = "active")
=============================================================================
