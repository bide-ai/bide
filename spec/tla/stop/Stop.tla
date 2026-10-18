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
  TargetOf, CompOf,  \* what each stop covers, and whether it holds compensation ("allow" or "block")
  KeyIncludesComp,   \* TRUE: a stop's key is its target and its compensation; FALSE (regress): its target alone
  GenRule,           \* TRUE: a set is named by (stop key, generation) and the insert is once-only; FALSE (regress): named by its own content
  MaxGen,            \* the largest generation a set may carry (a name past it is not a generation)
  Backstop,          \* TRUE: when no generation name is free a set is written under its content-derived name; FALSE (regress): it is not written
  MaxJunk,           \* how many names a writer with store access and no key may take (entries that are not stops)
  MaxPol, SettersOf, LiftersOf, QuorumOf, \* policies 1..MaxPol: roles and lift quorum
  NeedsOther,        \* the fold's lift rule: one lifter is not the setter
  WantOther,         \* what the invariant demands of a lift (equal to NeedsOther unless a regress)
  People, Forger,    \* the writers of records; the one who holds no enrolled key
  Cfg,               \* the process's mode: "basic" or "audited"
  Pin,               \* the policy (1..MaxPol) an audited process has pinned; the chain is walked from policy 1
  Deps, CheckDep,    \* the deployments records name ("mine" and perhaps "other"); whether the fold checks it
  AttackerRoots,     \* the forger holds a key of their own and may write a root and a transition (policy 0 is theirs)
  TrustRootOnly,     \* TRUE: only the root the process trusts (policy 1) starts the chain; FALSE (regress): a stored root does
  UnkOnUnverified,   \* regress: an audited process whose pin is reached treats a forged record as making the stops unknowable
  BasicHonoursPlainLift, \* regress: a basic process lets a plain lift lift a stop set under a signed record
  Chain,             \* TRUE: follow policy transitions; FALSE (regress): pinned to the last policy
  ModeFromStore,     \* regress: a mode record in the store flips the process to audited
  AcceptPlain,       \* regress: an audited process accepts a plain record
  MaxLog, MaxAtt, MaxCrash,
  MaxWriteCrash,     \* how many writes may crash between their two steps

  Check,             \* TRUE: a call reads the control log after its claim; FALSE: it starts at once
  SyncRead,          \* TRUE: that read sees every record committed before the claim; FALSE: a cache
  FailClosed,        \* TRUE: a read that fails holds the call; FALSE: it proceeds
  ReadFails,         \* a read may fail
  LiftRefires,       \* regress: a lift starts a halted call again
  WithTrail,         \* TRUE: records are also written to the audit trail, in two steps (the order of the Go control)
  UseKinds,          \* the record kinds in play (a subset of set, lift, pol, mode: a configuration may narrow them to keep a deep check small)
  ApplySuffixOnly,   \* TRUE: a pending trail record is re-applied only while no log record follows it on the trail (ApplyPending's rule)
  CatchUp,           \* TRUE: before a record that rests on earlier ones goes to the trail, the log's records the trail lacks are appended (in log order)
  LiftStoreFirst     \* regress: a record that is not a stop is written to the control run before the trail

\* The stop key: two stops with one key are duplicates. Compensation is part of the key, so a stop that holds
\* compensation is never refused because a weaker one (that lets it run) is in force.
ScopeOf == [i \in StopIds |-> IF KeyIncludesComp THEN <<TargetOf[i], CompOf[i]>> ELSE <<TargetOf[i], "any">>]
Pols == 1..MaxPol
Enrolled == UNION {SettersOf[p] \cup LiftersOf[p] : p \in Pols}
Kinds == {"set", "lift", "pol", "mode", "root"}
None == "none"

\* vocabulary: begin
\* The control-run records: a stop, one lifter's lift, one lifter's approval of a policy transition,
\* evidence of a change of mode, the root policy (the chain's start; the model always walks from it).
RecordKinds == {"set", "lift", "pol", "mode", "root"}
\* vocabulary: end

\* Unforgeable: a signed record is by an enrolled approver; the forger signs nothing.
Records == { r \in [k : Kinds, by : People, sig : BOOLEAN, stop : StopIds, to : Pols, dep : Deps, gen : 0..MaxGen] :
               /\ r.sig => (r.by \in Enrolled \/ (AttackerRoots /\ r.by = Forger /\ r.k \in {"root", "pol"}))
               /\ ~r.sig => r.by = Forger
               /\ r.k = "root" => (AttackerRoots /\ r.sig /\ r.by = Forger)
               /\ (r.k = "pol" /\ r.by = Forger /\ r.sig) => r.to = 2
               /\ r.k = "pol" => r.to \in 2..MaxPol
               /\ r.k \notin {"pol"} => r.to = 1
               /\ r.k = "mode" => ~r.sig
               /\ r.k /= "set" => r.gen = 0
               /\ r.k = "set" => r.gen \in (IF GenRule THEN 0..MaxGen ELSE {0}) }

Stop0 == [phase |-> "none", setter |-> None, lifts |-> {}, how |-> None, signed |-> FALSE]
Init0C(ch) == [unv |-> FALSE, audited |-> (Cfg = "audited"), pol |-> IF ch THEN 1 ELSE MaxPol, tps |-> {},
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

ApplyBody(ch, tr, s, r) ==
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
      [] r.k = "root" /\ ~tr /\ r.sig -> [s EXCEPT !.pol = 0]
      [] r.k = "pol" /\ ch /\ r.sig /\ r.by \in LiftersOf[s.pol] /\ (r.to = s.pol + 1 \/ s.pol = 0)
           -> LET T == s.tps \cup {r.by} IN
              IF Cardinality(T \cap LiftersOf[s.pol]) >= QuorumOf[s.pol]
                THEN Relift([s EXCEPT !.pol = r.to, !.tps = {}])
                ELSE [s EXCEPT !.tps = T]
      [] OTHER -> s

ApplyC(ch, tr, s, r) ==
  LET t == ApplyBody(ch, tr, s, r) IN
  IF UnkOnUnverified /\ s.audited /\ ~r.sig /\ r.k \in {"set", "lift", "pol"} THEN [t EXCEPT !.unv = TRUE] ELSE t

RECURSIVE EffC(_, _, _, _)
EffC(ch, tr, l, n) == IF n = 0 THEN Init0C(ch) ELSE ApplyC(ch, tr, EffC(ch, tr, l, n - 1), l[n])
Eff(l, n) == EffC(Chain, TrustRootOnly, l, n)

\* The process cannot tell which stops are in force: an audited process whose pin the walk from the root has not
\* reached, or a basic process holding an active stop that was set under a signed record.
Unk(s) == IF s.audited THEN (Pin > s.pol \/ s.unv) ELSE \E i \in StopIds : s.st[i].phase = "active" /\ s.st[i].signed

\* A stop in force for the key sc.
HeldKey(s, sc) == \E i \in StopIds : s.st[i].phase = "active" /\ ScopeOf[i] = sc
\* The name a set has in the control run: with GenRule the stop key and the generation (so two writers that
\* chose the same generation collide), without it, or at generation 0 (the backstop), the set's own content
\* (here its stop), which never collides.
SetName(r) == IF GenRule /\ r.gen > 0 THEN <<ScopeOf[r.stop], r.gen>> ELSE <<r.stop, 0>>
\* The greatest generation among the sets of key sc in the first n records of l (0 if none).
MaxGenOf(l, n, sc) ==
  LET gs == {l[i].gen : i \in {j \in 1..n : l[j].k = "set" /\ ScopeOf[l[j].stop] = sc}} IN
  IF gs = {} THEN 0 ELSE CHOOSE g \in gs : \A h \in gs : h <= g
\* The names the sets of the first n records of l hold.
NamesOf(l, n) == {SetName(l[i]) : i \in {j \in 1..n : l[j].k = "set"}}
\* The generation a writer chooses: the first free one after the greatest of the sets that are records, free
\* of every name it saw (the records' names and the junk it saw), and within MaxGen; 0 when there is none, which
\* is the backstop's (content-derived) name. A junk name never moves the generation: it is only skipped.
NextGen(l, n, sc, jv) ==
  LET m == MaxGenOf(l, n, sc)
      taken == NamesOf(l, n) \cup jv
      free == {g \in (m + 1)..MaxGen : <<sc, g>> \notin taken} IN
  IF free = {} THEN 0 ELSE CHOOSE g \in free : \A h \in free : g <= h
\* The insert of a set record r into log l with junk jk on the store. A writer reads the log up to some n (any
\* prefix: it may be stale) and the junk it saw (any subset); if a stop is in force for the key in what it read it
\* reports "already stopped" and writes nothing; else it names the record by the key and NextGen, or, when no
\* generation is free, by its content (the backstop, if there is one); and the insert fails if that name is taken.
\* Another record kind is not a set and is not constrained.
SetInsertable(l, jk, r) ==
  r.k = "set" =>
    /\ \E n \in 0..Len(l), jv \in SUBSET jk :
         /\ ~HeldKey(Eff(l, n), ScopeOf[r.stop])
         /\ r.gen = IF GenRule THEN NextGen(l, n, ScopeOf[r.stop], jv) ELSE 0
    /\ (r.gen = 0 /\ GenRule) => Backstop
    /\ SetName(r) \notin NamesOf(l, Len(l)) \cup jk

Active(s, c) == \E i \in StopIds : s.st[i].phase = "active" /\ c \in CoverOf[i]
Covers(s, c) == Active(s, c)

VARIABLES
  log,        \* the control log
  trail,      \* the audit trail: the sequence of records written to it, in order (WithTrail)
  wcrash,     \* writes that crashed between their two steps
  pend,       \* the record being written in two steps (a set of at most one)

  calls,      \* c -> "idle", "claimed", "cleared", "started", "done", "halted"
  att,        \* c -> attempts made
  claimLen,   \* c -> length of the log when the latest attempt was claimed
  rd,         \* c -> length of the log prefix the latest attempt's read saw (-1: none)
  fired,      \* c -> times the effect started
  crashes,
  blocked,    \* a call has been held back by a stop that was read
  junk        \* the names taken by entries that are not stops (a writer with store access and no key)

vars == <<log, trail, pend, wcrash, calls, att, claimLen, rd, fired, crashes, blocked, junk>>

TypeOK ==
  /\ log \in Seq(Records) /\ Len(log) <= MaxLog
  /\ calls \in [Calls -> {"idle", "claimed", "cleared", "started", "done", "halted"}]
  /\ att \in [Calls -> 0..MaxAtt] /\ crashes \in 0..MaxCrash /\ blocked \in BOOLEAN
  /\ trail \in Seq(Records) /\ wcrash \in 0..MaxWriteCrash /\ pend \subseteq Records /\ Cardinality(pend) <= 1
  /\ junk \subseteq {<<k, g>> : k \in {ScopeOf[i] : i \in StopIds}, g \in 1..MaxGen} /\ Cardinality(junk) <= MaxJunk

Init ==
  /\ log = <<>> /\ calls = [c \in Calls |-> "idle"] /\ att = [c \in Calls |-> 0]
  /\ claimLen = [c \in Calls |-> 0] /\ rd = [c \in Calls |-> -1] /\ fired = [c \in Calls |-> 0]
  /\ crashes = 0 /\ blocked = FALSE /\ trail = <<>> /\ pend = {} /\ wcrash = 0 /\ junk = {}

\* protocol:stop begin Write StartWrite FinishWrite Reaudit (the Go regions carry the markers)
\* Without a trail a record is one atomic write to the control log.
Write(r) ==
  /\ ~WithTrail
  /\ Len(log) < MaxLog
  /\ SetInsertable(log, junk, r)
  /\ log' = Append(log, r)
  /\ UNCHANGED <<junk, trail, pend, wcrash, calls, att, claimLen, rd, fired, crashes, blocked>>

Claim(c) ==
  /\ calls[c] = "idle" /\ att[c] < MaxAtt
  /\ att' = [att EXCEPT ![c] = @ + 1]
  /\ claimLen' = [claimLen EXCEPT ![c] = Len(log)]
  /\ rd' = [rd EXCEPT ![c] = IF Check THEN -1 ELSE 0]
  /\ calls' = [calls EXCEPT ![c] = IF Check THEN "claimed" ELSE "cleared"]
  /\ UNCHANGED <<junk, log, trail, pend, wcrash, fired, crashes, blocked>>

\* The check after the claim: it reads the control log (everything committed so far, or, without
\* SyncRead, any earlier prefix), and the attempt is cleared to start, or voided (not started) when a
\* covering stop is read, or held when the read fails and FailClosed.
ReadOk(c) ==
  /\ calls[c] = "claimed"
  /\ \E n \in (IF SyncRead THEN {Len(log)} ELSE 0..Len(log)) :
       IF Covers(Eff(log, n), c)
         THEN /\ calls' = [calls EXCEPT ![c] = "idle"] /\ blocked' = TRUE /\ rd' = rd
         ELSE /\ calls' = [calls EXCEPT ![c] = "cleared"] /\ rd' = [rd EXCEPT ![c] = n] /\ blocked' = blocked
  /\ UNCHANGED <<junk, log, trail, pend, wcrash, att, claimLen, fired, crashes>>

ReadFail(c) ==
  /\ ReadFails /\ calls[c] = "claimed"
  /\ IF FailClosed
       THEN /\ calls' = [calls EXCEPT ![c] = "idle"] /\ rd' = rd
       ELSE /\ calls' = [calls EXCEPT ![c] = "cleared"] /\ rd' = [rd EXCEPT ![c] = 0]
  /\ UNCHANGED <<junk, log, trail, pend, wcrash, att, claimLen, fired, crashes, blocked>>

\* A stop may be committed between the read and the start: the call is in flight and finishes.
Begin(c) ==
  /\ calls[c] = "cleared"
  /\ calls' = [calls EXCEPT ![c] = "started"] /\ fired' = [fired EXCEPT ![c] = @ + 1]
  /\ UNCHANGED <<junk, log, trail, pend, wcrash, att, claimLen, rd, crashes, blocked>>

Finish(c) ==
  /\ calls[c] = "started"
  /\ calls' = [calls EXCEPT ![c] = "done"]
  /\ UNCHANGED <<junk, log, trail, pend, wcrash, att, claimLen, rd, fired, crashes, blocked>>

\* A crash with a marker and no result: the outcome is unknown, the call halts, for good.
Crash(c) ==
  /\ crashes < MaxCrash /\ calls[c] \in {"claimed", "cleared", "started"}
  /\ calls' = [calls EXCEPT ![c] = "halted"] /\ crashes' = crashes + 1
  /\ UNCHANGED <<junk, log, trail, pend, wcrash, att, claimLen, rd, fired, blocked>>

\* Regress only: a lift starts a halted call that had fired again.
Refire(c) ==
  /\ LiftRefires /\ calls[c] = "halted" /\ \E i \in StopIds : Eff(log, Len(log)).st[i].phase = "lifted"
  /\ calls' = [calls EXCEPT ![c] = "cleared"] /\ rd' = [rd EXCEPT ![c] = Len(log)]
  /\ UNCHANGED <<junk, log, trail, pend, wcrash, att, claimLen, fired, crashes, blocked>>

\* With a trail, a signed record is written in two steps. A stop is written to the control log first (it is
\* in force at once) and to the trail second; every other record is written to the trail first, so none is in
\* force without its evidence (LiftStoreFirst is the regress that breaks this). A crash between the two steps
\* leaves the first one, and Reaudit appends to the trail what the control log holds that it lacks.
StoreFirst(r) == r.k = "set" \/ LiftStoreFirst

Range(q) == {q[i] : i \in 1..Len(q)}
TrailSet == Range(trail)
SignedLog == {x \in Range(log) : x.sig}
\* The catch-up before a trail-first record: every signed record of the log is already on the trail.
CaughtUp == ~CatchUp \/ SignedLog \subseteq TrailSet

StartWrite(r) ==
  /\ WithTrail /\ pend = {} /\ r.sig /\ Len(log) < MaxLog
  /\ SetInsertable(log, junk, r)
  /\ r \notin Range(log)   \* a record is inserted once: the control run's names are content-derived, so the same record is not written twice
  /\ r.k = "lift" => Eff(log, Len(log)).st[r.stop].phase /= "none"   \* a lift names a stop the control run holds
  \* A record already on the trail but not in the log (a write that crashed) is only re-applied while no log
  \* record follows it on the trail: ApplyPending leaves a superseded one, so the orders cannot diverge.
  /\ ApplySuffixOnly => ((r \in TrailSet /\ r \notin Range(log)) => \A i \in 1..Len(trail) : trail[i] = r => \A j \in (i + 1)..Len(trail) : trail[j] \notin Range(log))
  /\ IF StoreFirst(r) THEN log' = Append(log, r) /\ trail' = trail
       ELSE /\ CaughtUp
            /\ log' = log
            /\ trail' = IF r \in TrailSet THEN trail ELSE Append(trail, r)
  /\ pend' = {r}
  /\ UNCHANGED <<junk, wcrash, calls, att, claimLen, rd, fired, crashes, blocked>>

FinishWrite ==
  /\ WithTrail /\ pend /= {}
  /\ LET p == CHOOSE x \in pend : TRUE IN
       IF StoreFirst(p) THEN trail' = (IF p \in TrailSet THEN trail ELSE Append(trail, p)) /\ log' = log ELSE Len(log) < MaxLog /\ log' = Append(log, p) /\ trail' = trail
  /\ pend' = {}
  /\ UNCHANGED <<junk, wcrash, calls, att, claimLen, rd, fired, crashes, blocked>>

WriteCrash ==
  /\ WithTrail /\ pend /= {} /\ wcrash < MaxWriteCrash /\ pend' = {} /\ wcrash' = wcrash + 1
  /\ UNCHANGED <<junk, log, trail, calls, att, claimLen, rd, fired, crashes, blocked>>

\* A writer with store access and no key writes an unsigned record straight into the control log.
Forge(r) ==
  /\ WithTrail /\ ~r.sig /\ Len(log) < MaxLog
  /\ r.k = "set" => SetName(r) \notin NamesOf(log, Len(log)) \cup junk   \* a forger cannot overwrite a name either
  /\ log' = Append(log, r)
  /\ UNCHANGED <<junk, trail, pend, wcrash, calls, att, claimLen, rd, fired, crashes, blocked>>

Reaudit ==
  /\ WithTrail /\ \E i \in 1..Len(log) :
       /\ log[i].sig /\ log[i] \notin TrailSet
       /\ \A j \in 1..(i - 1) : log[j].sig => log[j] \in TrailSet   \* in log order, every earlier record first
       /\ trail' = Append(trail, log[i])
  /\ UNCHANGED <<junk, log, pend, wcrash, calls, att, claimLen, rd, fired, crashes, blocked>>
\* protocol:stop end

\* A writer with store access and no key takes a name: an entry that is not a stop, under a name a writer might
\* choose (the key is a public hash). It never decodes, so it moves no generation; it only occupies a name.
Squat(k, g) ==
  /\ Cardinality(junk) < MaxJunk /\ <<k, g>> \notin junk /\ <<k, g>> \notin NamesOf(log, Len(log))
  /\ junk' = junk \cup {<<k, g>>}
  /\ UNCHANGED <<log, trail, pend, wcrash, calls, att, claimLen, rd, fired, crashes, blocked>>

Next == \/ \E r \in Records : Write(r) \/ StartWrite(r) \/ Forge(r)
        \/ \E k \in {ScopeOf[i] : i \in StopIds}, g \in 1..MaxGen : Squat(k, g)
        \/ FinishWrite \/ WriteCrash \/ Reaudit
        \/ \E c \in Calls : Claim(c) \/ ReadOk(c) \/ ReadFail(c) \/ Begin(c) \/ Finish(c) \/ Crash(c) \/ Refire(c)

Spec == Init /\ [][Next]_vars

----------------------------------------------------------------------------
\* Two stops with one key are never in force together: a concurrent duplicate is refused at the insert (the
\* generation names one winner), and the loser, reading again, finds the stop in force and writes nothing.
OneActivePerScope ==
  \A i, j \in StopIds : (i /= j /\ ScopeOf[i] = ScopeOf[j]) =>
    ~(Eff(log, Len(log)).st[i].phase = "active" /\ Eff(log, Len(log)).st[j].phase = "active")

\* A set request is refused ("already stopped") only when a stop in force for the same target is at least as
\* strict: no request is refused while every stop in force for its target is weaker. (A stop that blocks
\* compensation is stricter than one that lets it run.)
StopRank(c) == IF c = "block" THEN 1 ELSE 0
StricterNeverRefused ==
  \A i \in StopIds : \A n \in 0..Len(log) :
    HeldKey(Eff(log, n), ScopeOf[i]) =>
      \E j \in StopIds : Eff(log, n).st[j].phase = "active" /\ TargetOf[j] = TargetOf[i] /\ StopRank(CompOf[j]) >= StopRank(CompOf[i])

\* A stop can always be set when none is in force for its target: no squatter, whatever names it takes, leaves a
\* stop that was never written (and none in force for its key) without a name to be written under. (A stop
\* written under the backstop's name may then be a duplicate of one written concurrently: the safe direction.)
\* (A stop id stands for one record's content, and its content-derived name, which no one can have taken before
\* the record exists: the invariant is about a stop whose own name is unused.)
SetNeverBlocked ==
  \A i \in StopIds :
    (Eff(log, Len(log)).st[i].phase = "none" /\ ~HeldKey(Eff(log, Len(log)), ScopeOf[i]) /\ <<i, 0>> \notin NamesOf(log, Len(log))) =>
      \E r \in Records : r.k = "set" /\ r.stop = i /\ SetInsertable(log, junk, r)

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
    (Unk(Eff(log, Len(log))) \/ \A i \in StopIds : Eff(log, Len(log)).st[i].phase = EffC(TRUE, TRUE, log, Len(log)).st[i].phase)

\* An audited process whose pin the chain has reached is never made unknowable by a record that does
\* not verify: a forgery is only ignored, so a store-write attacker has no lever against audited mode.
NoSpuriousUnknowable == Cfg = "audited" => (Unk(Eff(log, Len(log))) => Pin > Eff(log, Len(log)).pol)

\* A record for another deployment changes nothing in an audited process.
ForeignNoEffect ==
  \A n \in 0..(Len(log) - 1) :
    (Eff(log, n).audited /\ log[n + 1].dep /= "mine" /\ log[n + 1].k /= "mode") => Eff(log, n + 1) = Eff(log, n)

\* A basic process does not lift a stop set under a signed record on a plain lift.
BasicSignedStopNeedsSignedLift ==
  Cfg = "basic" => \A i \in StopIds :
    (Eff(log, Len(log)).st[i].signed /\ Eff(log, Len(log)).st[i].phase = "lifted") => Eff(log, Len(log)).st[i].how = "signedlift"

\* No record that is not a stop is in force in the control log unless it is already on the trail: a lift, a
\* policy transition, the root and a mode change are evidenced first. (A stop is written to the control log
\* first, so it may be in force before its trail write: Reaudit completes it.)
EffectiveImpliesTrailed ==
  WithTrail => \A i \in 1..Len(log) : (log[i].sig /\ log[i].k /= "set") => log[i] \in TrailSet

\* Once every write has finished and the trail holds every signed record of the log and nothing else, the
\* trail's fold equals the log's: the trail shows what is in force. (A trail that holds a lift before the stop
\* it names folds differently: the lift is rejected and the stop shows in force.)
Quiescent == WithTrail /\ pend = {} /\ TrailSet = SignedLog
TrailAgreesWithLog ==
  Quiescent => EffC(Chain, TrustRootOnly, trail, Len(trail)) = Eff(log, Len(log))

\* A stop never goes back: none, then active, then lifted.
Rank(p) == IF p = "none" THEN 0 ELSE IF p = "active" THEN 1 ELSE 2
StopsNeverDropped ==
  [][\A i \in StopIds : Rank(Eff(log', Len(log')).st[i].phase) >= Rank(Eff(log, Len(log)).st[i].phase)]_vars

\* Only Begin starts the effect: a stop, a lift or a change of mode never does.
OnlyBeginFires == [][\A c \in Calls : fired'[c] /= fired[c] => (calls[c] = "cleared" /\ calls'[c] = "started")]_vars

\* Vacuity and reachability: each is violated when the effect is reached.
EffectNotReachable == \A c \in Calls : fired[c] = 0
UntrailedStopNotReachable == ~(pend = {} /\ \E i \in 1..Len(log) : log[i].sig /\ log[i].k = "set" /\ log[i] \notin TrailSet)
QuiescentAgreeNotReachable == ~(Quiescent /\ Len(trail) > 1)
BlockedNotReachable == ~blocked
\* A second stop of one key can be written once the first is lifted.
SecondSetNotReachable == ~(\E i, j \in StopIds : i /= j /\ ScopeOf[i] = ScopeOf[j] /\ Eff(log, Len(log)).st[i].phase /= "none" /\ Eff(log, Len(log)).st[j].phase /= "none")
AuditedLiftNotReachable == ~(\E i \in StopIds : Eff(log, Len(log)).st[i].how = "audited")
RotatedNotReachable == ~(Eff(log, Len(log)).pol = 2 /\ \E i \in StopIds : Eff(log, Len(log)).st[i].phase = "active")
=============================================================================
