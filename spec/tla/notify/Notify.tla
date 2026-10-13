------------------------------- MODULE Notify -------------------------------
(***************************************************************************)
(* Model 14: notifications (docs/design/notifier.md, "Notify once, across  *)
(* crashes"). A design model with no Go code yet, so it has no map and no  *)
(* markers. One run's notify trail (notify:<run>, an audit.EventStore      *)
(* trail): an append-only sequence of records, each appended at the next   *)
(* position or refused (audit.ErrSeqTaken) when another writer took it.    *)
(* Deliverers in processes run the protocol for each event key:            *)
(*   1. load the trail and fold; acked or cleared: stop;                   *)
(*   2. no observed: append it;                                            *)
(*   3. the event is no longer current (the journal, or for                *)
(*      recovery_failing the trail): append cleared;                       *)
(*      past the notify deadline with no overdue: append overdue;          *)
(*      enough failures with no failing: append failing;                   *)
(*   4. another holder's claim is live: stop; else append claimed;         *)
(*   5. a round of up to Reqs requests; on success append acked (retried   *)
(*      in place until the fold shows acked or cleared); on failure count  *)
(*      it and begin a new round at step 1.                                *)
(* Every append is one store round trip that may also error, committed or  *)
(* not (a lost reply of the trail); ErrSeqTaken reloads and folds again.   *)
(* A recovery pass of each process appends failing_since when it fails and *)
(* no episode is open, observes recovery_failing once D has passed since   *)
(* the episode's failing_since, and appends recovered when it succeeds.    *)
(*                                                                          *)
(* Time is abstract. A claim's until passing (ClaimLapse), an event's      *)
(* deadline passing (Deadline) and D passing since failing_since (Elapse)  *)
(* are steps of the environment, weakly fair as time is. A claim lapses    *)
(* while its holder is still in its round (a stalled or slow POST) only    *)
(* within the budget MaxSlow.                                              *)
(*                                                                          *)
(* The lifecycle is an input: cur[e] is whether event e still stands in    *)
(* the run's journal (a halt not resolved, an approval not decided, a run  *)
(* aborted). No action of the protocol writes it; NotifyLifecycle.tla      *)
(* drives it from model 10's own state.                                    *)
(***************************************************************************)
EXTENDS Integers, Sequences, FiniteSets

CONSTANTS
  Procs,      \* processes with the notifier configured (model values)
  Slots,      \* deliverers per process (the drive's delivery goroutine, the sweep)
  Events,     \* keys of the halts, approvals and aborts the journal holds (strings)
  Episodes,   \* recovery_failing episodes the environment may open
  Reqs,       \* requests per delivery round (3 in the design)
  FailAfter,  \* failed requests (FailCount "memory") or failed rounds ("trail") before failing
  FailCount,  \* "memory": counted in the deliverer's memory; "trail": the claimed records
  MaxRounds,  \* claimed records per key the model allows (BoundNotHit)
  MaxCrash,   \* budget of process crashes; -1: no budget (a crash loop)
  MaxLost,    \* budget of lost sink replies (the sink accepted, the reply never came)
  MaxAmbig,   \* budget of trail appends that error, committed or not
  MaxFail,    \* budget of failed requests (5xx, timeouts); -1: the sink never accepts
  MaxSlow,    \* budget of claims that lapse while their holder is still in its round
  Resolve,    \* the environment may end an event (ResolveHalt, a decision, the run ended)
  UseLease,     \* a deliverer starts only under the run's lease (the usual deliverer)
  Holder,     \* what a claim names: "deliverer" (a deliverer of a process incarnation) or
              \* "process" (the process incarnation)
  Bug         \* "none" or a broken rule, see regress/

ASSUME FailCount \in {"memory", "trail"} /\ Holder \in {"deliverer", "process"}
ASSUME Bug \in {"none", "AckFirst", "NoUntil", "NoLapse", "NoSeq", "NoOverdue", "MemFailingSince",
                "NoStaleCheck", "NoDoneCheck", "AckNoRetry"}
ASSUME Reqs >= 1 /\ FailAfter >= 1 /\ MaxRounds >= 1 /\ Slots >= 1 /\ Episodes \in 0..2

None == "none"
NoHolder == <<"none", 0, 0>>
RecKeys == <<"rec0", "rec1">>
RecKey(k) == RecKeys[k + 1]
Keys == Events \cup {RecKey(k) : k \in 0..Episodes - 1}
Dlv == Procs \X (1..Slots)
\* vocabulary: begin
\* The trail's leaf kinds (docs/design/notifier.md, "Records").
RecordKinds == {"observed", "claimed", "acked", "failing", "overdue", "failing_since", "recovered",
                "cleared"}
\* vocabulary: end
Rec(k, e, h) == [k |-> k, e |-> e, h |-> h]

VARIABLES
  \* The trail and its environment.
  trail,      \* the records, in position order
  cur,        \* input: event e still stands in the run's journal
  lapsed,     \* positions of claims whose until has passed
  past,       \* keys whose deadline_at has passed
  dpast,      \* D has passed since the open episode's failing_since
  broken,     \* environment: recovery passes of the run fail
  opened,     \* environment: episodes opened so far
  lease,      \* the run's lease: the holding process, or None
  inc,        \* each process's incarnation: a restart is a new one, with a new holder token
  progressed, \* the process ended a delivery round or a recovery pass in this incarnation
  \* The deliverers.
  pc,         \* "idle", "decide", "append", "send"
  ev,         \* the key it works on
  len,        \* the trail length it last loaded (it appends at len + 1)
  act,        \* the record it is appending
  claim,      \* the position of its claim in this round, or 0
  nreq,       \* requests made in this round
  ok,         \* the sink accepted, acked not yet in the trail
  fails,      \* FailCount "memory": failed requests per key, lost on a crash
  \* Bug "MemFailingSince": the episode's start and D, in each process's memory.
  memFS, memD,
  \* Ghosts and budgets.
  deliv,      \* requests the sink accepted, per key
  stale,      \* requests the sink accepted while the event no longer stood
  late,       \* claims appended past the deadline with no overdue in the trail
  failed,     \* failed requests (lost replies included), per key, over all processes
  lateFail,   \* claims appended once failed reached FailAfter, with no failing in the trail
  crashes, lost, ambig, sfail, slow,
  boundHit

vars == <<trail, cur, lapsed, past, dpast, broken, opened, lease, inc, progressed, pc, ev, len, act,
          claim, nreq, ok, fails, memFS, memD, deliv, stale, late, failed, lateFail, crashes, lost,
          ambig, sfail, slow, boundHit>>
procVars == <<inc, progressed>>
envVars == <<cur, lapsed, past, dpast, broken, opened>>
dlvVars == <<pc, ev, len, act, claim, nreq, ok, fails>>
ghostVars == <<deliv, stale, late, failed, lateFail, crashes, lost, ambig, sfail, slow, boundHit>>

(***************************************************************************)
(* The fold                                                                *)
(***************************************************************************)
Pos(s) == 1..Len(s)
Has(s, k, e) == \E i \in Pos(s) : s[i].k = k /\ s[i].e = e
Count(s, k, e) == Cardinality({i \in Pos(s) : s[i].k = k /\ s[i].e = e})
ClaimsOf(s, e) == {i \in Pos(s) : s[i].k = "claimed" /\ s[i].e = e}
LastClaim(s, e) == IF ClaimsOf(s, e) = {} THEN 0
                   ELSE CHOOSE i \in ClaimsOf(s, e) : \A j \in ClaimsOf(s, e) : j <= i
\* Step 1's stop: acked (Bug "NoDoneCheck": not read) or cleared.
Done(s, e) == (Bug # "NoDoneCheck" /\ Has(s, "acked", e)) \/ Has(s, "cleared", e)
Acked(e) == Has(trail, "acked", e)
Open(e) == Has(trail, "observed", e) /\ ~Has(trail, "acked", e) /\ ~Has(trail, "cleared", e)
\* Recovery episodes: one failing_since opens one, recovered closes it; k counts recovered.
NRec(s) == Count(s, "recovered", "run")
EpisodeOpen(s) == Count(s, "failing_since", "run") > NRec(s)
Durable == Bug # "MemFailingSince"

\* The holder a claim names: a token of the process incarnation, and of the deliverer in it.
Holds(a) == <<a[1], IF Holder = "process" THEN 0 ELSE a[2], inc[a[1]]>>
\* A claim is live until its until passes; Bug "NoUntil": a claim never holds anyone off.
ClaimLive(i) == i # 0 /\ i \notin lapsed
LiveOther(a, s, e) == LET l == LastClaim(s, e) IN
                        l # 0 /\ s[l].h # Holds(a) /\ ClaimLive(l) /\ Bug # "NoUntil"

\* Step 3's check, read at the step: the journal for an event of the run, the trail (or, Bug
\* "MemFailingSince", the failing passes) for recovery_failing.
Current(e) ==
  IF e \in Events THEN cur[e]
  ELSE IF Durable THEN EpisodeOpen(trail) /\ e = RecKey(NRec(trail))
  ELSE broken /\ e = RecKey(opened - 1)

\* A failed round is Reqs failed requests (a round ends early only on success), so FailAfter
\* failures are FailRounds rounds.
FailRounds == (FailAfter + Reqs - 1) \div Reqs
FailingDue(a, s, e) ==
  IF FailCount = "trail" THEN Cardinality(ClaimsOf(s, e)) >= FailRounds
  ELSE fails[a][e] >= FailAfter

\* A deliverer is in its round: sending under its claim, or holding the sink's acceptance.
InRound(a) == claim[a] # 0 /\ (pc[a] = "send" \/ ok[a])

\* The protocol's initial state; the input cur is the journal's (Init, or model 10's state).
InitProto ==
  /\ trail = <<>>
  /\ lapsed = {} /\ past = {} /\ dpast = FALSE /\ broken = FALSE /\ opened = 0
  /\ lease = None
  /\ inc = [p \in Procs |-> 0]
  /\ progressed = [p \in Procs |-> FALSE]
  /\ pc = [a \in Dlv |-> "idle"]
  /\ ev = [a \in Dlv |-> None]
  /\ len = [a \in Dlv |-> 0]
  /\ act = [a \in Dlv |-> None]
  /\ claim = [a \in Dlv |-> 0]
  /\ nreq = [a \in Dlv |-> 0]
  /\ ok = [a \in Dlv |-> FALSE]
  /\ fails = [a \in Dlv |-> [e \in Keys |-> 0]]
  /\ memFS = [p \in Procs |-> FALSE] /\ memD = [p \in Procs |-> FALSE]
  /\ deliv = [e \in Keys |-> 0] /\ stale = [e \in Keys |-> 0] /\ late = [e \in Keys |-> 0]
  /\ failed = [e \in Keys |-> 0] /\ lateFail = [e \in Keys |-> 0]
  /\ crashes = 0 /\ lost = 0 /\ ambig = 0 /\ sfail = 0 /\ slow = 0
  /\ boundHit = FALSE
\* Standalone, every event stands in the journal when the model starts (the drive that saw it
\* may already be dead, so only a catch-up path knows of it).
Init == InitProto /\ cur = [e \in Events |-> TRUE]

(***************************************************************************)
(* The deliverers                                                          *)
(***************************************************************************)

\* A pass starts (the drive-end handoff, a RecoverLoop pass, a NotifyPending sweep) for a key the
\* journal shows as standing, or one the trail holds open; it loads the trail. Under UseLease the
\* process takes the run's lease, unless another process holds it.
Start(a, e) ==
  /\ pc[a] = "idle"
  /\ (e \in Events /\ cur[e]) \/ Open(e)
  /\ ~Done(trail, e)
  /\ UseLease => lease \in {None, a[1]}
  /\ lease' = IF UseLease THEN a[1] ELSE lease
  /\ ev' = [ev EXCEPT ![a] = e]
  /\ len' = [len EXCEPT ![a] = Len(trail)]
  /\ pc' = [pc EXCEPT ![a] = "decide"]
  /\ UNCHANGED <<trail, envVars, procVars, act, claim, nreq, ok, fails, memFS, memD, ghostVars>>

\* The pass ends: the deliverer forgets the key; the process keeps its memory (fails). The
\* lease is released when no other deliverer of the process is busy.
FinishVars(a) ==
  /\ pc' = [pc EXCEPT ![a] = "idle"]
  /\ ev' = [ev EXCEPT ![a] = None]
  /\ ok' = [ok EXCEPT ![a] = FALSE]
  /\ claim' = [claim EXCEPT ![a] = 0]
  /\ act' = [act EXCEPT ![a] = None]
  /\ lease' = IF lease = a[1] /\ \A b \in Dlv : b[1] = a[1] /\ b # a => pc[b] = "idle"
              THEN None ELSE lease

\* Steps 1 to 4 on the loaded prefix: a local decision, made with the journal (step 3) and the
\* clock read at this step.
Decide(a) ==
  /\ pc[a] = "decide"
  /\ LET s == SubSeq(trail, 1, len[a])
         e == ev[a]
         To(r) == /\ act' = [act EXCEPT ![a] = r] /\ pc' = [pc EXCEPT ![a] = "append"]
                  /\ UNCHANGED <<lease, ev, ok, claim, boundHit, len>>
     IN
     IF Done(s, e) THEN FinishVars(a) /\ UNCHANGED <<boundHit, len>>
     ELSE IF ok[a] THEN To("acked")
     ELSE IF ~Has(s, "observed", e) THEN To("observed")
     ELSE IF Bug # "NoStaleCheck" /\ ~Current(e) THEN To("cleared")
     ELSE IF Bug # "NoOverdue" /\ e \in past /\ ~Has(s, "overdue", e) THEN To("overdue")
     ELSE IF FailingDue(a, s, e) /\ ~Has(s, "failing", e) THEN To("failing")
     ELSE IF LiveOther(a, s, e) THEN FinishVars(a) /\ UNCHANGED <<boundHit, len>>
     \* At the configuration's bound the pass ends without a claim (BoundNotHit fails); a later
     \* pass still raises overdue, failing and cleared, so a sink that never accepts is checked.
     \* On a stale prefix the claim's append would be refused (ErrSeqTaken): reload instead.
     ELSE IF Cardinality(ClaimsOf(s, e)) >= MaxRounds THEN
       IF Len(trail) # len[a]
       THEN /\ len' = [len EXCEPT ![a] = Len(trail)]
            /\ UNCHANGED <<pc, lease, ev, ok, claim, act, boundHit>>
       ELSE FinishVars(a) /\ boundHit' = TRUE /\ UNCHANGED len
     ELSE To("claimed")
  /\ UNCHANGED <<trail, envVars, procVars, nreq, fails, memFS, memD, deliv, stale, late,
                 failed, lateFail, crashes, lost, ambig, sfail, slow>>

\* Write: append the record at len + 1. Another record there: ErrSeqTaken, reload and fold again
\* (Bug "NoSeq": the store keeps the last writer's leaf at a position, and both writers think
\* they won). Otherwise ok, or an error that did or did not commit; on an error the deliverer
\* reloads and folds again (an acked append is retried in place: ok stays set), so a committed
\* claimed record it does not know of names it, and it claims again.
Write(a) ==
  /\ pc[a] = "append"
  /\ LET e == ev[a]
         r == Rec(IF act[a] = "ackfirst" THEN "acked" ELSE act[a], e,
                  IF act[a] = "claimed" THEN Holds(a) ELSE NoHolder)
         at == len[a] + 1
         taken == Len(trail) # len[a]
         put == IF taken THEN [trail EXCEPT ![at] = r] ELSE Append(trail, r)
         isClaim == act[a] = "claimed"
         lateNow == isClaim /\ e \in past /\ ~Has(trail, "overdue", e)
         failNow == isClaim /\ failed[e] >= FailAfter /\ ~Has(trail, "failing", e)
         \* What the deliverer does once its record is in at position at.
         Won == IF isClaim THEN
                  /\ claim' = [claim EXCEPT ![a] = at] /\ nreq' = [nreq EXCEPT ![a] = 0]
                  /\ IF Bug = "AckFirst"
                     THEN /\ act' = [act EXCEPT ![a] = "ackfirst"]
                          /\ pc' = [pc EXCEPT ![a] = "append"]
                     ELSE pc' = [pc EXCEPT ![a] = "send"] /\ UNCHANGED act
                  /\ UNCHANGED ok
                ELSE IF act[a] = "ackfirst" THEN
                  pc' = [pc EXCEPT ![a] = "send"] /\ UNCHANGED <<act, claim, nreq, ok>>
                ELSE pc' = [pc EXCEPT ![a] = "decide"] /\ UNCHANGED <<act, claim, nreq, ok>>
         Reload(l) == /\ len' = [len EXCEPT ![a] = l] /\ pc' = [pc EXCEPT ![a] = "decide"]
                      \* A deliverer holding the sink's acceptance keeps its round's claim.
                      /\ claim' = IF ok[a] /\ Bug # "AckNoRetry" THEN claim
                                   ELSE [claim EXCEPT ![a] = 0]
                      /\ ok' = IF Bug = "AckNoRetry" THEN [ok EXCEPT ![a] = FALSE] ELSE ok
                      /\ UNCHANGED <<act, nreq>>
     IN
     IF taken /\ Bug # "NoSeq" THEN
       /\ Reload(Len(trail)) /\ UNCHANGED <<trail, ambig, late, lateFail>>
     ELSE
       \/ /\ trail' = put /\ len' = [len EXCEPT ![a] = at] /\ Won
          /\ late' = IF lateNow THEN [late EXCEPT ![e] = @ + 1] ELSE late
          /\ lateFail' = IF failNow THEN [lateFail EXCEPT ![e] = @ + 1] ELSE lateFail
          /\ UNCHANGED ambig
       \/ /\ ambig < MaxAmbig /\ ambig' = ambig + 1
          /\ Reload(Len(trail)) /\ UNCHANGED <<trail, late, lateFail>>
       \/ /\ ambig < MaxAmbig /\ ambig' = ambig + 1
          /\ trail' = put /\ Reload(Len(put))
          /\ late' = IF lateNow THEN [late EXCEPT ![e] = @ + 1] ELSE late
          /\ lateFail' = IF failNow THEN [lateFail EXCEPT ![e] = @ + 1] ELSE lateFail
  /\ UNCHANGED <<envVars, lease, procVars, ev, fails, memFS, memD, deliv, stale, failed, crashes,
                 lost, sfail, slow, boundHit>>

\* One request of the round. The sink accepts (2xx), accepts with the reply lost, or fails. An
\* acceptance is appended as acked; anything else is a failed request, and after Reqs of them
\* the round ends and the next begins at step 1 (the backoff is time, abstracted).
Send(a) ==
  /\ pc[a] = "send"
  /\ LET e == ev[a]
         accepted == /\ deliv' = [deliv EXCEPT ![e] = @ + 1]
                     /\ stale' = IF Current(e) THEN stale ELSE [stale EXCEPT ![e] = @ + 1]
         Failed == /\ fails' = [fails EXCEPT ![a][e] = IF @ < FailAfter THEN @ + 1 ELSE @]
                   /\ failed' = [failed EXCEPT ![e] = IF @ < FailAfter THEN @ + 1 ELSE @]
                   /\ IF nreq[a] + 1 < Reqs
                      THEN /\ nreq' = [nreq EXCEPT ![a] = @ + 1]
                           /\ UNCHANGED <<pc, len, claim, progressed>>
                      ELSE /\ nreq' = [nreq EXCEPT ![a] = 0] /\ claim' = [claim EXCEPT ![a] = 0]
                           /\ len' = [len EXCEPT ![a] = Len(trail)]
                           /\ pc' = [pc EXCEPT ![a] = "decide"]
                           /\ progressed' = [progressed EXCEPT ![a[1]] = TRUE]
                   /\ UNCHANGED <<ok, act>>
     IN
     \/ /\ MaxFail # -1 /\ accepted
        /\ IF act[a] = "ackfirst"
           THEN /\ pc' = [pc EXCEPT ![a] = "decide"] /\ len' = [len EXCEPT ![a] = Len(trail)]
                /\ claim' = [claim EXCEPT ![a] = 0] /\ UNCHANGED <<ok, act>>
           ELSE /\ ok' = [ok EXCEPT ![a] = TRUE] /\ act' = [act EXCEPT ![a] = "acked"]
                /\ pc' = [pc EXCEPT ![a] = "append"] /\ UNCHANGED <<len, claim>>
        /\ UNCHANGED <<nreq, fails, failed, lost, sfail, progressed>>
     \/ /\ MaxFail # -1 /\ lost < MaxLost /\ lost' = lost + 1 /\ accepted /\ Failed
        /\ UNCHANGED sfail
     \/ /\ MaxFail = -1 \/ sfail < MaxFail
        /\ sfail' = IF MaxFail = -1 THEN sfail ELSE sfail + 1
        /\ Failed /\ UNCHANGED <<deliv, stale, lost>>
  /\ UNCHANGED <<trail, envVars, lease, inc, ev, memFS, memD, late, lateFail, crashes, ambig, slow,
                 boundHit>>

\* A process dies and restarts with no memory: its deliverers' passes, their failure counts and
\* (Bug "MemFailingSince") the episode it remembered are gone; the run's lease it held lapses
\* later (LeaseLapse). Its requests already sent may still be accepted (they were, in Send).
Crash(p) ==
  \* With no budget (a crash loop), each incarnation first ends a round or a recovery pass: a
  \* process that dies before doing anything can alert no one, whatever the design.
  /\ IF MaxCrash = -1 THEN progressed[p] ELSE crashes < MaxCrash
  /\ (\E a \in Dlv : a[1] = p /\ pc[a] # "idle") \/ memFS[p] \/ progressed[p]
  /\ progressed' = [progressed EXCEPT ![p] = FALSE]
  /\ crashes' = IF MaxCrash = -1 THEN crashes ELSE crashes + 1
  \* With no crash budget the incarnations alternate, so the state stays finite; a token then
  \* recurs only two restarts later.
  /\ inc' = [inc EXCEPT ![p] = IF MaxCrash = -1 THEN 1 - @ ELSE @ + 1]
  /\ pc' = [a \in Dlv |-> IF a[1] = p THEN "idle" ELSE pc[a]]
  /\ ev' = [a \in Dlv |-> IF a[1] = p THEN None ELSE ev[a]]
  /\ act' = [a \in Dlv |-> IF a[1] = p THEN None ELSE act[a]]
  /\ claim' = [a \in Dlv |-> IF a[1] = p THEN 0 ELSE claim[a]]
  /\ nreq' = [a \in Dlv |-> IF a[1] = p THEN 0 ELSE nreq[a]]
  /\ ok' = [a \in Dlv |-> IF a[1] = p THEN FALSE ELSE ok[a]]
  /\ fails' = [a \in Dlv |-> IF a[1] = p THEN [e \in Keys |-> 0] ELSE fails[a]]
  /\ memFS' = [memFS EXCEPT ![p] = FALSE] /\ memD' = [memD EXCEPT ![p] = FALSE]
  /\ UNCHANGED <<trail, envVars, lease, len, deliv, stale, late, failed, lateFail, lost, ambig,
                 sfail, slow, boundHit>>

(***************************************************************************)
(* Recovery passes and recovery_failing                                    *)
(***************************************************************************)

\* A recovery pass of process p fails (a Resumer error, not resumable, a store error). With no
\* open episode it appends failing_since; once D has passed since it, it observes the episode's
\* recovery_failing event (protocol step 2; the deliverers take it from there). One atomic
\* load-and-append: two passes racing for a position leave one record, the loser's reload
\* finding it (the same race as Write's).
RecFail(p) ==
  /\ broken
  /\ IF Durable THEN
       LET k == NRec(trail) IN
       /\ \/ /\ ~EpisodeOpen(trail)
             /\ trail' = Append(trail, Rec("failing_since", "run", NoHolder))
          \/ /\ EpisodeOpen(trail) /\ dpast /\ ~Has(trail, "observed", RecKey(k))
             /\ trail' = Append(trail, Rec("observed", RecKey(k), NoHolder))
       /\ UNCHANGED <<memFS, memD>>
     ELSE
       \/ /\ ~memFS[p] /\ memFS' = [memFS EXCEPT ![p] = TRUE] /\ UNCHANGED <<trail, memD>>
       \/ /\ memD[p] /\ ~Has(trail, "observed", RecKey(opened - 1))
          /\ trail' = Append(trail, Rec("observed", RecKey(opened - 1), NoHolder))
          /\ UNCHANGED <<memFS, memD>>
  /\ progressed' = [progressed EXCEPT ![p] = TRUE]
  /\ UNCHANGED <<envVars, lease, inc, dlvVars, ghostVars>>

\* A recovery pass succeeds: an open episode gets recovered (or, Bug "MemFailingSince", the
\* process forgets the one it remembered).
RecOK(p) ==
  /\ ~broken
  /\ IF Durable THEN
       /\ EpisodeOpen(trail)
       /\ trail' = Append(trail, Rec("recovered", "run", NoHolder))
       /\ dpast' = FALSE
       /\ UNCHANGED <<memFS, memD>>
     ELSE
       /\ memFS[p]
       /\ memFS' = [memFS EXCEPT ![p] = FALSE] /\ memD' = [memD EXCEPT ![p] = FALSE]
       /\ UNCHANGED <<trail, dpast>>
  /\ UNCHANGED <<cur, lapsed, past, broken, opened, lease, procVars, dlvVars, ghostVars>>

(***************************************************************************)
(* The environment: time, the run's journal, the sink's faults             *)
(***************************************************************************)

\* A claim's until passes. Claims lapse in the order they were made. One whose holder is still
\* in its round (a slow POST, a stall) lapses only within MaxSlow. Bug "NoLapse": a claim has no
\* until and never lapses.
ClaimLapse(i) ==
  /\ Bug # "NoLapse"
  /\ i \in Pos(trail) /\ trail[i].k = "claimed" /\ i \notin lapsed
  /\ \A j \in Pos(trail) : j < i /\ trail[j].k = "claimed" => j \in lapsed
  /\ LET mid == \E a \in Dlv : claim[a] = i /\ InRound(a) IN
     /\ mid => slow < MaxSlow
     /\ slow' = IF mid THEN slow + 1 ELSE slow
  /\ lapsed' = lapsed \cup {i}
  /\ UNCHANGED <<trail, cur, past, dpast, broken, opened, lease, procVars, dlvVars, memFS, memD,
                 deliv, stale, late, failed, lateFail, crashes, lost, ambig, sfail, boundHit>>

\* An observed event's deadline_at passes.
Deadline(e) ==
  /\ Has(trail, "observed", e) /\ e \notin past
  /\ past' = past \cup {e}
  /\ UNCHANGED <<trail, cur, lapsed, dpast, broken, opened, lease, procVars, dlvVars, memFS, memD,
                 ghostVars>>

\* D passes since the open episode's failing_since (or, Bug "MemFailingSince", since the
\* failing pass process p remembers).
Elapse ==
  /\ Durable /\ EpisodeOpen(trail) /\ ~dpast
  /\ dpast' = TRUE
  /\ UNCHANGED <<trail, cur, lapsed, past, broken, opened, lease, procVars, dlvVars, memFS, memD,
                 ghostVars>>
ElapseMem(p) ==
  /\ ~Durable /\ memFS[p] /\ ~memD[p]
  /\ memD' = [memD EXCEPT ![p] = TRUE]
  /\ UNCHANGED <<trail, envVars, lease, procVars, dlvVars, memFS, ghostVars>>

\* The run's journal moves on: a halt is resolved, a decision submitted, the run ends.
ResolveEvent(e) ==
  /\ Resolve /\ cur[e]
  /\ cur' = [cur EXCEPT ![e] = FALSE]
  /\ UNCHANGED <<trail, lapsed, past, dpast, broken, opened, lease, procVars, dlvVars, memFS, memD,
                 ghostVars>>

\* Recovery of the run starts failing (a new episode, once the last one is closed) or works again.
Break ==
  /\ ~broken /\ opened < Episodes
  /\ Durable => ~EpisodeOpen(trail)
  /\ broken' = TRUE /\ opened' = opened + 1
  /\ UNCHANGED <<trail, cur, lapsed, past, dpast, lease, procVars, dlvVars, memFS, memD, ghostVars>>
Fix ==
  /\ broken /\ broken' = FALSE
  /\ UNCHANGED <<trail, cur, lapsed, past, dpast, opened, lease, procVars, dlvVars, memFS, memD,
                 ghostVars>>

\* The run's lease lapses (its holder stalled, died or stopped renewing); the holder keeps running.
LeaseLapse ==
  /\ lease # None /\ lease' = None
  /\ UNCHANGED <<trail, envVars, procVars, dlvVars, memFS, memD, ghostVars>>

\* The protocol's own steps: none writes cur, the run's journal.
DlvNext(a) == (\E e \in Keys : Start(a, e)) \/ Decide(a) \/ Write(a) \/ Send(a)
ProtoNext ==
  \/ \E a \in Dlv : DlvNext(a)
  \/ \E p \in Procs : Crash(p) \/ RecFail(p) \/ RecOK(p)
\* Time and the faults of the world, which the lifecycle (cur) is not part of.
TimeNext ==
  \/ \E i \in Pos(trail) : ClaimLapse(i)
  \/ \E e \in Keys : Deadline(e)
  \/ Elapse \/ \E p \in Procs : ElapseMem(p)
  \/ Break \/ Fix \/ LeaseLapse
EnvNext == \E e \in Events : ResolveEvent(e)

\* The longest trail: per key an observed, its claims, an overdue, a failing and an acked or
\* cleared; per episode a failing_since and a recovered.
MaxLen == (MaxRounds + 4) * Cardinality(Keys) + 2 * Episodes

Next == ProtoNext \/ TimeNext \/ EnvNext
Spec == Init /\ [][Next]_vars

\* Fairness: every deliverer step and every recovery pass is weakly fair (a process that keeps
\* recovering or sweeping), and so is time (a claim lapses, a deadline and D pass, a lease
\* lapses). Crashes, the sink's faults, ResolveEvent, Break and Fix are not.
Fairness ==
  /\ \A a \in Dlv : WF_vars(DlvNext(a))
  /\ \A p \in Procs : WF_vars(RecFail(p)) /\ WF_vars(RecOK(p)) /\ WF_vars(ElapseMem(p))
  /\ \A i \in 1..MaxLen : WF_vars(ClaimLapse(i))
  /\ \A e \in Keys : WF_vars(Deadline(e))
  /\ WF_vars(Elapse) /\ WF_vars(LeaseLapse)
LiveSpec == Spec /\ Fairness

(***************************************************************************)
(* Properties                                                               *)
(***************************************************************************)

TypeOK ==
  /\ trail \in Seq([k : RecordKinds, e : Keys \cup {"run"},
                    h : (Procs \X (0..Slots) \X Nat) \cup {NoHolder}])
  /\ cur \in [Events -> BOOLEAN]
  /\ lapsed \subseteq Pos(trail) /\ past \subseteq Keys
  /\ pc \in [Dlv -> {"idle", "decide", "append", "send"}]
  /\ ev \in [Dlv -> Keys \cup {None}]
  /\ \A a \in Dlv : len[a] \in 0..Len(trail) /\ claim[a] \in 0..Len(trail)
  /\ lease \in Procs \cup {None}

\* No silent ack: an event is acked only after the sink accepted a request for it.
AckTrue == \A e \in Keys : Acked(e) => deliv[e] >= 1

\* 1. No silent drop: an event that stays current is acked (a sink that accepts in the end).
NoSilentDrop == \A e \in Events : <>[]~cur[e] \/ <>Acked(e)
\* 1. With a sink that never accepts: failing is raised, and (AckTrue) the event stays open.
FailingRaised == \A e \in Events : <>[]~cur[e] \/ <>Has(trail, "failing", e)

\* 1. failing is raised in time: no round of a key is claimed once FailAfter of its requests have
\* failed (over all processes and restarts) unless failing is in the trail.
FailingBy == \A e \in Keys : lateFail[e] = 0

\* 2. Duplicates are bounded: a key's accepted requests never exceed 1 + the crashes, the lost
\* replies and the claims that lapsed mid-round, so they are 1 in a history with none.
DupBound == \A e \in Keys : deliv[e] <= 1 + crashes + lost + slow

\* 3. No concurrent double while a claim is live: no two deliverers are in a round of one key
\* both under a live claim; and every claim a deliverer works under is its own record, at the
\* position it appended (of two appends at one position, exactly one won).
NoConcurrentDouble ==
  \A a, b \in Dlv : a # b /\ InRound(a) /\ InRound(b) /\ ev[a] = ev[b]
                    => ~(ClaimLive(claim[a]) /\ ClaimLive(claim[b]))
ClaimWon ==
  \A a \in Dlv : claim[a] # 0 =>
    /\ claim[a] \in Pos(trail)
    /\ trail[claim[a]] = Rec("claimed", ev[a], Holds(a))
    /\ \A b \in Dlv : b # a /\ ev[b] = ev[a] /\ Holder = "deliverer" => claim[b] # claim[a]

\* 4. Overdue is raised by the deadline: at most one round of a key is claimed past its deadline
\* before overdue is in the trail (the one whose check raced the deadline); and, under fairness,
\* an open event past its deadline gets overdue.
OverdueBounded == \A e \in Keys : late[e] <= 1
OverdueRaised == \A e \in Keys : (e \in past /\ Open(e)) ~> (~Open(e) \/ Has(trail, "overdue", e))

\* 5. A crash loop still alerts: while recovery keeps failing, the recovery_failing event of the
\* open episode (k recovered records so far; Bug "MemFailingSince": the environment's count) is
\* observed, however many restarts lose their memory; and a successful pass closes the episode.
EpisodeKey == IF Durable THEN RecKey(NRec(trail)) ELSE RecKey(opened - 1)
CrashLoopAlerts == broken ~> (~broken \/ Has(trail, "observed", EpisodeKey))
EpisodeCloses == (~broken /\ EpisodeOpen(trail)) ~> (broken \/ ~EpisodeOpen(trail))

\* 7. done or acked is idempotent: once a key is acked or cleared nothing more is appended for
\* it; observed, overdue, failing, acked and cleared are each appended at most once per key.
Terminal ==
  \A i, j \in Pos(trail) : i < j /\ trail[i].k \in {"acked", "cleared"} => trail[j].e # trail[i].e
Once ==
  \A e \in Keys, k \in {"observed", "overdue", "failing", "acked", "cleared"} :
    Count(trail, k, e) <= 1
\* The verifier's rules: every record of a key follows its observed.
ObservedFirst ==
  \A i \in Pos(trail) : trail[i].e # "run" /\ trail[i].k # "observed" =>
    \E j \in 1..i - 1 : trail[j] = Rec("observed", trail[i].e, NoHolder)

\* Stale suppression, what holds: no round is claimed for a key once it is cleared (Terminal),
\* and a resolved event that was observed stops being worked on (cleared or acked). One resolved
\* before any process observed it is never notified, and needs nothing in the trail.
StaleStops ==
  \A e \in Events : (~cur[e] /\ Has(trail, "observed", e)) ~> (Acked(e) \/ Has(trail, "cleared", e))
\* What does not hold (limits/stale-alert): the check and the send are not atomic.
NoStaleDelivery == \A e \in Keys : stale[e] = 0

\* What the design does not claim (limits/not-exactly-once): exactly once.
ExactlyOnce == \A e \in Keys : deliv[e] <= 1

\* The configuration's bound on rounds never blocks a deliverer.
BoundNotHit == ~boundHit

\* Vacuity (check.sh): a notification is delivered.
EffectNotReachable == \A e \in Keys : deliv[e] = 0
=============================================================================
