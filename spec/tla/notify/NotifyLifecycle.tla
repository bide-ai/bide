--------------------------- MODULE NotifyLifecycle ---------------------------
(***************************************************************************)
(* Model 14 composed with model 10 (property 6, no interference). Model    *)
(* 10's run lifecycle, unchanged (LifecycleMC, from spec/tla/lifecycle,    *)
(* which the configurations name in a LIBRARY line), runs beside the       *)
(* notify protocol, whose input cur is read from the lifecycle's own       *)
(* state: a run halted on a live attempt with no drive, a run waiting for  *)
(* an approval, a run aborted. Every step is a lifecycle step that leaves  *)
(* the notify variables as they are, or a notify step that leaves the      *)
(* lifecycle's as they are. The configurations check model 10's invariants *)
(* and the notify safety properties together, and LifeStepsOnly: seen on  *)
(* the lifecycle's variables, every step is a lifecycle step or no change. *)
(*                                                                          *)
(* NJournal = TRUE is the counterexample: the notifier records its claim   *)
(* in the run's journal (a notification run as a Step of the run, or the   *)
(* first draft's notify: records), here an attempt marker on the run's     *)
(* last call, which a drive then reads as a halt.                          *)
(***************************************************************************)
EXTENDS LifecycleMC

CONSTANTS NProcs, NSlots, NEvents, NReqs, NFailAfter, NFailCount, NMaxRounds, NMaxCrash,
          NMaxLost, NMaxAmbig, NMaxFail, NMaxSlow, NLeased, NHolder, NJournal, p1, p2

VARIABLES nclaim, ndeliv, trail, lapsed, past, dpast, broken, opened, nlease, inc, progressed, npc,
          ev, len, act, claim, nreq, ok, fails, memFS, memD, deliv, stale, late, failed, lateFail,
          ncrashes, lost, nambig, sfail, slow, boundHit

nvars == <<nclaim, ndeliv, trail, lapsed, past, dpast, broken, opened, nlease, inc, progressed, npc,
           ev, len, act, claim, nreq, ok, fails, memFS, memD, deliv, stale, late, failed, lateFail,
           ncrashes, lost, nambig, sfail, slow, boundHit>>

\* The events the journal holds, read from model 10's state (the notifier's step 3 reads them).
Standing(e) ==
  CASE e = "halt1" -> HaltedRun(1)
    [] e = "halt2" -> HaltedRun(2)
    [] e = "approval1" -> 1 \in Paused0 /\ ~approved[1] /\ ~Ended(1)
    [] e = "aborted1" -> "aborted" \in Range(ends[1])
    [] OTHER -> FALSE

N == INSTANCE Notify WITH
       Procs <- NProcs, Slots <- NSlots, Events <- NEvents, Episodes <- 0, Reqs <- NReqs,
       FailAfter <- NFailAfter, FailCount <- NFailCount, MaxRounds <- NMaxRounds,
       MaxCrash <- NMaxCrash, MaxLost <- NMaxLost, MaxAmbig <- NMaxAmbig, MaxFail <- NMaxFail,
       MaxSlow <- NMaxSlow, Resolve <- FALSE, Notice <- FALSE, LongClaims <- FALSE,
       UseLease <- NLeased, Holder <- NHolder, Bug <- "none",
       cur <- [e \in NEvents |-> Standing(e)],
       lease <- nlease, pc <- npc, crashes <- ncrashes, ambig <- nambig

\* NJournal: a notify step that also writes the run's journal.
\* (The lifecycle's primed variables are fixed first: the notify step's UNCHANGED cur reads them.)
InJournal ==
  /\ NJournal
  /\ marker' = [marker EXCEPT ![PrimRun][NCalls] = IF @ = NoClaim THEN Unknown ELSE @]
  /\ UNCHANGED <<result, ends, approved, start, lims, creq, lease, job, jr, holds, cc, outc, ret,
                 reply, ctxDead, stalled, copt, eo, why, srun, sk, sseen, sround, sret, nsRep,
                 listed, pri, cand, tk, fire, orun, ocall, cret, ambig, crashes, stalls, fired,
                 seenEnds, fireAfterCancel, resumedEnded, recErr, crashLost, orphan, age, rolled,
                 filterBroken, optBroken, overLimit, sOpen, nsCnt, pc, spent>>
  /\ \E a \in N!Dlv : act[a] = "claimed" /\ N!Write(a)

CInit == FullInit /\ N!InitProto
CNext ==
  \/ FullNext /\ UNCHANGED nvars
  \/ UNCHANGED allVars /\ (N!ProtoNext \/ N!TimeNext)
  \/ InJournal
CSpec == CInit /\ [][CNext]_<<allVars, nvars>>

\* Property 6: on the lifecycle's variables every step is a lifecycle step or no change, so the
\* lifecycle's behaviors, and its reachable states, are those of model 10 alone (its actions read
\* no notify variable, so none of them is disabled by the notifier either).
LifeStepsOnly == [][FullNext \/ UNCHANGED allVars]_<<allVars, nvars>>

\* The notify properties, checked in the composition too.
NTypeOK == N!TypeOK
NAckTrue == N!AckTrue
NDupBound == N!DupBound
NNoConcurrentDouble == N!NoConcurrentDouble
NClaimWon == N!ClaimWon
NTerminal == N!Terminal
NOnce == N!Once
NObservedFirst == N!ObservedFirst
NBoundNotHit == N!BoundNotHit
\* Vacuity of the notifier in the composition: a notification is delivered (expected violated).
NotifyNotReachable == N!EffectNotReachable
=============================================================================
