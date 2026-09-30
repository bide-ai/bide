-------------------------------- MODULE Spend --------------------------------
(***************************************************************************)
(* Model 8: spend accounting of model calls (P9, #104). Every model request *)
(* a drive sends is billed; the journal must hold that spend exactly once:  *)
(* in the turn's @llm/<n> record (its answer and the discarded usage of the *)
(* other requests the turn took), in a failed call's @spend/<id>, or in a   *)
(* late record @spend-late/<id> for requests that ended after their turn    *)
(* was recorded or whose turn another driver recorded. Result.Spend and     *)
(* Replay read it from the journal.                                         *)
(*                                                                          *)
(* Units: every request bills one unit. The primary request of a turn is    *)
(* its answer; an extra request (a hedge loser, a retried attempt) ends     *)
(* before the turn's record is built (discarded usage in it), or stays in   *)
(* flight and ends later (late spend), or ignores its cancellation past the *)
(* drive's bounded wait (billed, not journaled: a documented limit).        *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS
  Drivers,       \* drivers of the run, each in its own process (model values)
  MaxTurns,      \* model turns before the run completes
  MaxExtra,      \* budget of extra requests (hedge losers, retried attempts)
  MaxFail,       \* budget of model calls that fail for good
  MaxAmbig,      \* budget of error replies on writes
  MaxLookupErr,  \* budget of failed reads of a turn's record after its write errored
  MaxIgnore,     \* budget of requests that outlive the drive's bounded wait
  MaxCrash,      \* budget of process crashes
  MaxIds,        \* bound on spend record ids
  Bug            \* "none" or a reverted rule, see regress/

None == "none"
Turns == 0..(MaxTurns - 1)
NoLLM == [rec |-> FALSE, amt |-> 0, by |-> None]

RECURSIVE SumAmt(_)
SumAmt(S) == IF S = {} THEN 0 ELSE LET x == CHOOSE y \in S : TRUE IN x.amt + SumAmt(S \ {x})

ASSUME Bug \in {"none", "SharedLateKey", "LostTurnNotLate", "NoLandedLookup", "NoSettle"}

(* --algorithm spend
variables
  \* The journal: @llm/<n> (its answer plus discarded usage, and who recorded it), the spend
  \* records @spend/<id> and @spend-late/<id> ([key, amt]), and run:complete.
  llm      = [n \in Turns |-> NoLLM],
  spendSet = {},
  complete = FALSE,
  nextId   = 0,
  \* Per process: the meter's pending spend, requests in flight, and spend kept for the next drive.
  meter    = [d \in Drivers |-> 0],
  inflight = [d \in Drivers |-> 0],
  pend     = [d \in Drivers |-> {}],
  reply    = [d \in Drivers |-> ""],
  \* Budgets and ghosts: billed units, units lost to a documented limit, and each drive's
  \* Result.Spend at its return.
  extras = 0, fails = 0, ambig = 0, lookupErrs = 0, ignores = 0, crashes = 0,
  billed = 0,
  lost   = 0,
  result = [d \in Drivers |-> -1];

define
  \* The spend the journal holds (what Replay and a later drive's Result.Spend read).
  J == SumAmt({[k |-> n, amt |-> llm[n].amt] : n \in {m \in Turns : llm[m].rec}}) + SumAmt(spendSet)
  Landed(k) == \E r \in spendSet : r.key = k
  FreshId == nextId + 1
end define;

macro Reply(r) begin
  either r := "ok";
  or await ambig < MaxAmbig; ambig := ambig + 1; r := "err_nc";
  or await ambig < MaxAmbig; ambig := ambig + 1; r := "err_c";
  end either;
end macro;

\* recordSpend: a StepValue under key, first writer wins (the journal's Do). A write that fails
\* keeps the spend for the run's next drive in this process.
macro WriteSpend(key, amt, base) begin
  Reply(reply[self]);
  if reply[self] # "err_nc" /\ ~Landed(key) then
    spendSet := spendSet \cup {[key |-> key, amt |-> amt]};
  end if;
  if reply[self] # "ok" then
    pend[self] := base \cup {[turn |-> FALSE, key |-> key, n |-> 0, amt |-> amt]};
  elsif ~Landed(key) then
    pend[self] := base;
    tot := tot + amt;
  else
    \* The key was another write's: Do returns that record, which this drive then counts.
    pend[self] := base;
    tot := tot + (CHOOSE r \in spendSet : r.key = key).amt;
  end if;
end macro;

fair process driver \in Drivers
variables seq = 0, taken = 0, built = FALSE, tot = 0, lateCnt = 0, spent = 0;
begin
Open:
  \* The Load: a completed run returns; else the drive continues after the recorded turns.
  reply[self] := "";
  if complete then
    goto Done;
  else
    tot := J; lateCnt := 0; built := FALSE; taken := 0;
    seq := Cardinality({n \in Turns : llm[n].rec});
  end if;
SettlePending:
  \* settlePending: spend a drive of this process could not journal, decided from the journal.
  if pend[self] # {} then
    with p \in pend[self] do
      if ~p.turn /\ Landed(p.key) then
        pend[self] := pend[self] \ {p};                \* the write landed after all
      elsif p.turn /\ llm[p.n].rec /\ llm[p.n].by = self then
        pend[self] := pend[self] \ {p};                \* the turn's record is this drive's own
      elsif p.turn then
        \* another driver's record holds the turn (late spend), or none does (a failed call)
        nextId := FreshId;
        WriteSpend(FreshId, p.amt, pend[self] \ {p});
      else
        WriteSpend(p.key, p.amt, pend[self] \ {p});
      end if;
    end with;
    if reply[self] # "ok" /\ reply[self] # "" then goto Open;   \* the drive fails
    else goto SettlePending;
    end if;
  end if;
Turn:
  reply[self] := "";
  if seq >= MaxTurns then
    goto End;
  else
    \* One model call: its primary request, and possibly an extra one (hedge loser or retry)
    \* that ends before the record is built or stays in flight.
    either
      skip;
    or
      await extras < MaxExtra; extras := extras + 1;
      either billed := billed + 1; meter[self] := meter[self] + 1;
      or inflight[self] := inflight[self] + 1;
      end either;
    end either;
  end if;
Call:
  billed := billed + 1;                                 \* the primary request
  either
    \* The call fails for good: its requests were billed.
    await fails < MaxFail; fails := fails + 1;
    meter[self] := meter[self] + 1; built := FALSE;
    goto FailPath;
  or
    \* The step builds its record: the answer, and the spend the meter took as discarded usage.
    taken := meter[self] + 1; meter[self] := 0; built := TRUE;
  end either;
Insert:
  Reply(reply[self]);
  if reply[self] # "err_nc" /\ ~llm[seq].rec then
    llm[seq] := [rec |-> TRUE, amt |-> taken, by |-> self];
  end if;
  if reply[self] # "ok" then goto FailPath; end if;
Recorded:
  \* The journal's record is this drive's (its answer functions run), or another driver's: this
  \* drive's requests were billed all the same, so their spend is late.
  if llm[seq].by # self /\ Bug # "LostTurnNotLate" then meter[self] := meter[self] + taken; end if;
  tot := tot + llm[seq].amt;
  seq := seq + 1; built := FALSE; taken := 0;
  goto Turn;
FailPath:
  \* waitEnd: requests in flight end (their spend metered), or outlive the bounded wait.
  either
    meter[self] := meter[self] + inflight[self]; billed := billed + inflight[self];
  or
    await ignores < MaxIgnore /\ inflight[self] > 0; ignores := ignores + 1;
    billed := billed + inflight[self]; lost := lost + inflight[self];
  end either;
  inflight[self] := 0;
FailLookup:
  if built /\ Bug # "NoLandedLookup" then
    either
      \* The read fails: the turn's spend is kept for the next drive in this process.
      await lookupErrs < MaxLookupErr; lookupErrs := lookupErrs + 1;
      pend[self] := pend[self] \cup {[turn |-> TRUE, key |-> 0, n |-> seq, amt |-> taken]};
      built := FALSE; taken := 0;
      goto Leave;
    or
      if llm[seq].rec then
        \* The record landed: the turn's (its rest is late), or another driver's (all late).
        if llm[seq].by # self then meter[self] := meter[self] + taken; end if;
        built := FALSE; taken := 0;
        goto Leave;
      end if;
    end either;
  end if;
FailSpend:
  \* A failed call's spend: what the meter holds, and what the unrecorded turn took.
  spent := meter[self] + taken; meter[self] := 0; built := FALSE; taken := 0;
  if spent > 0 then
    nextId := FreshId;
    WriteSpend(FreshId, spent, pend[self]);
  end if;
Leave:
  \* settle, then the drive ends with its error and the run is driven again.
  if Bug # "NoSettle" then
    either
      meter[self] := meter[self] + inflight[self]; billed := billed + inflight[self];
    or
      await ignores < MaxIgnore /\ inflight[self] > 0; ignores := ignores + 1;
      billed := billed + inflight[self]; lost := lost + inflight[self];
    end either;
    inflight[self] := 0;
  end if;
LeaveLate:
  if meter[self] > 0 then
    spent := meter[self]; meter[self] := 0;
    if Bug = "SharedLateKey" then
      \* Historically keyed by a sequence number each drive counted from its own view.
      lateCnt := lateCnt + 1;
      WriteSpend(100 + lateCnt, spent, pend[self]);
    else
      nextId := FreshId;
      WriteSpend(FreshId, spent, pend[self]);
    end if;
  end if;
LeaveDone:
  result[self] := tot;
  goto Open;
End:
  \* Terminal: settle (requests in flight waited for, late spend journaled), then run:complete.
  if Bug # "NoSettle" then
    either
      meter[self] := meter[self] + inflight[self]; billed := billed + inflight[self];
    or
      await ignores < MaxIgnore /\ inflight[self] > 0; ignores := ignores + 1;
      billed := billed + inflight[self]; lost := lost + inflight[self];
    end either;
    inflight[self] := 0;
  end if;
EndLate:
  if meter[self] > 0 then
    spent := meter[self]; meter[self] := 0;
    if Bug = "SharedLateKey" then
      lateCnt := lateCnt + 1;
      WriteSpend(100 + lateCnt, spent, pend[self]);
    else
      nextId := FreshId;
      WriteSpend(FreshId, spent, pend[self]);
    end if;
    if reply[self] # "ok" then goto LeaveDone; end if;
  end if;
Complete:
  Reply(reply[self]);
  if reply[self] # "err_nc" then complete := TRUE; end if;
  result[self] := tot;
  if reply[self] # "ok" then goto Open; end if;
end process;
end algorithm; *)
\* BEGIN TRANSLATION
VARIABLES llm, spendSet, complete, nextId, meter, inflight, pend, reply, 
          extras, fails, ambig, lookupErrs, ignores, crashes, billed, lost, 
          result, pc

(* define statement *)
J == SumAmt({[k |-> n, amt |-> llm[n].amt] : n \in {m \in Turns : llm[m].rec}}) + SumAmt(spendSet)
Landed(k) == \E r \in spendSet : r.key = k
FreshId == nextId + 1

VARIABLES seq, taken, built, tot, lateCnt, spent

vars == << llm, spendSet, complete, nextId, meter, inflight, pend, reply, 
           extras, fails, ambig, lookupErrs, ignores, crashes, billed, lost, 
           result, pc, seq, taken, built, tot, lateCnt, spent >>

ProcSet == (Drivers)

Init == (* Global variables *)
        /\ llm = [n \in Turns |-> NoLLM]
        /\ spendSet = {}
        /\ complete = FALSE
        /\ nextId = 0
        /\ meter = [d \in Drivers |-> 0]
        /\ inflight = [d \in Drivers |-> 0]
        /\ pend = [d \in Drivers |-> {}]
        /\ reply = [d \in Drivers |-> ""]
        /\ extras = 0
        /\ fails = 0
        /\ ambig = 0
        /\ lookupErrs = 0
        /\ ignores = 0
        /\ crashes = 0
        /\ billed = 0
        /\ lost = 0
        /\ result = [d \in Drivers |-> -1]
        (* Process driver *)
        /\ seq = [self \in Drivers |-> 0]
        /\ taken = [self \in Drivers |-> 0]
        /\ built = [self \in Drivers |-> FALSE]
        /\ tot = [self \in Drivers |-> 0]
        /\ lateCnt = [self \in Drivers |-> 0]
        /\ spent = [self \in Drivers |-> 0]
        /\ pc = [self \in ProcSet |-> "Open"]

Open(self) == /\ pc[self] = "Open"
              /\ reply' = [reply EXCEPT ![self] = ""]
              /\ IF complete
                    THEN /\ pc' = [pc EXCEPT ![self] = "Done"]
                         /\ UNCHANGED << seq, taken, built, tot, lateCnt >>
                    ELSE /\ tot' = [tot EXCEPT ![self] = J]
                         /\ lateCnt' = [lateCnt EXCEPT ![self] = 0]
                         /\ built' = [built EXCEPT ![self] = FALSE]
                         /\ taken' = [taken EXCEPT ![self] = 0]
                         /\ seq' = [seq EXCEPT ![self] = Cardinality({n \in Turns : llm[n].rec})]
                         /\ pc' = [pc EXCEPT ![self] = "SettlePending"]
              /\ UNCHANGED << llm, spendSet, complete, nextId, meter, inflight, 
                              pend, extras, fails, ambig, lookupErrs, ignores, 
                              crashes, billed, lost, result, spent >>

SettlePending(self) == /\ pc[self] = "SettlePending"
                       /\ IF pend[self] # {}
                             THEN /\ \E p \in pend[self]:
                                       IF ~p.turn /\ Landed(p.key)
                                          THEN /\ pend' = [pend EXCEPT ![self] = pend[self] \ {p}]
                                               /\ UNCHANGED << spendSet, 
                                                               nextId, reply, 
                                                               ambig, tot >>
                                          ELSE /\ IF p.turn /\ llm[p.n].rec /\ llm[p.n].by = self
                                                     THEN /\ pend' = [pend EXCEPT ![self] = pend[self] \ {p}]
                                                          /\ UNCHANGED << spendSet, 
                                                                          nextId, 
                                                                          reply, 
                                                                          ambig, 
                                                                          tot >>
                                                     ELSE /\ IF p.turn
                                                                THEN /\ nextId' = FreshId
                                                                     /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                                                           /\ ambig' = ambig
                                                                        \/ /\ ambig < MaxAmbig
                                                                           /\ ambig' = ambig + 1
                                                                           /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                                                        \/ /\ ambig < MaxAmbig
                                                                           /\ ambig' = ambig + 1
                                                                           /\ reply' = [reply EXCEPT ![self] = "err_c"]
                                                                     /\ IF reply'[self] # "err_nc" /\ ~Landed(FreshId)
                                                                           THEN /\ spendSet' = (spendSet \cup {[key |-> FreshId, amt |-> (p.amt)]})
                                                                           ELSE /\ TRUE
                                                                                /\ UNCHANGED spendSet
                                                                     /\ IF reply'[self] # "ok"
                                                                           THEN /\ pend' = [pend EXCEPT ![self] = (pend[self] \ {p}) \cup {[turn |-> FALSE, key |-> FreshId, n |-> 0, amt |-> (p.amt)]}]
                                                                                /\ tot' = tot
                                                                           ELSE /\ IF ~Landed(FreshId)
                                                                                      THEN /\ pend' = [pend EXCEPT ![self] = pend[self] \ {p}]
                                                                                           /\ tot' = [tot EXCEPT ![self] = tot[self] + (p.amt)]
                                                                                      ELSE /\ pend' = [pend EXCEPT ![self] = pend[self] \ {p}]
                                                                                           /\ tot' = [tot EXCEPT ![self] = tot[self] + (CHOOSE r \in spendSet' : r.key = FreshId).amt]
                                                                ELSE /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                                                           /\ ambig' = ambig
                                                                        \/ /\ ambig < MaxAmbig
                                                                           /\ ambig' = ambig + 1
                                                                           /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                                                        \/ /\ ambig < MaxAmbig
                                                                           /\ ambig' = ambig + 1
                                                                           /\ reply' = [reply EXCEPT ![self] = "err_c"]
                                                                     /\ IF reply'[self] # "err_nc" /\ ~Landed((p.key))
                                                                           THEN /\ spendSet' = (spendSet \cup {[key |-> (p.key), amt |-> (p.amt)]})
                                                                           ELSE /\ TRUE
                                                                                /\ UNCHANGED spendSet
                                                                     /\ IF reply'[self] # "ok"
                                                                           THEN /\ pend' = [pend EXCEPT ![self] = (pend[self] \ {p}) \cup {[turn |-> FALSE, key |-> (p.key), n |-> 0, amt |-> (p.amt)]}]
                                                                                /\ tot' = tot
                                                                           ELSE /\ IF ~Landed((p.key))
                                                                                      THEN /\ pend' = [pend EXCEPT ![self] = pend[self] \ {p}]
                                                                                           /\ tot' = [tot EXCEPT ![self] = tot[self] + (p.amt)]
                                                                                      ELSE /\ pend' = [pend EXCEPT ![self] = pend[self] \ {p}]
                                                                                           /\ tot' = [tot EXCEPT ![self] = tot[self] + (CHOOSE r \in spendSet' : r.key = (p.key)).amt]
                                                                     /\ UNCHANGED nextId
                                  /\ IF reply'[self] # "ok" /\ reply'[self] # ""
                                        THEN /\ pc' = [pc EXCEPT ![self] = "Open"]
                                        ELSE /\ pc' = [pc EXCEPT ![self] = "SettlePending"]
                             ELSE /\ pc' = [pc EXCEPT ![self] = "Turn"]
                                  /\ UNCHANGED << spendSet, nextId, pend, 
                                                  reply, ambig, tot >>
                       /\ UNCHANGED << llm, complete, meter, inflight, extras, 
                                       fails, lookupErrs, ignores, crashes, 
                                       billed, lost, result, seq, taken, built, 
                                       lateCnt, spent >>

Turn(self) == /\ pc[self] = "Turn"
              /\ reply' = [reply EXCEPT ![self] = ""]
              /\ IF seq[self] >= MaxTurns
                    THEN /\ pc' = [pc EXCEPT ![self] = "End"]
                         /\ UNCHANGED << meter, inflight, extras, billed >>
                    ELSE /\ \/ /\ TRUE
                               /\ UNCHANGED <<meter, inflight, extras, billed>>
                            \/ /\ extras < MaxExtra
                               /\ extras' = extras + 1
                               /\ \/ /\ billed' = billed + 1
                                     /\ meter' = [meter EXCEPT ![self] = meter[self] + 1]
                                     /\ UNCHANGED inflight
                                  \/ /\ inflight' = [inflight EXCEPT ![self] = inflight[self] + 1]
                                     /\ UNCHANGED <<meter, billed>>
                         /\ pc' = [pc EXCEPT ![self] = "Call"]
              /\ UNCHANGED << llm, spendSet, complete, nextId, pend, fails, 
                              ambig, lookupErrs, ignores, crashes, lost, 
                              result, seq, taken, built, tot, lateCnt, spent >>

Call(self) == /\ pc[self] = "Call"
              /\ billed' = billed + 1
              /\ \/ /\ fails < MaxFail
                    /\ fails' = fails + 1
                    /\ meter' = [meter EXCEPT ![self] = meter[self] + 1]
                    /\ built' = [built EXCEPT ![self] = FALSE]
                    /\ pc' = [pc EXCEPT ![self] = "FailPath"]
                    /\ taken' = taken
                 \/ /\ taken' = [taken EXCEPT ![self] = meter[self] + 1]
                    /\ meter' = [meter EXCEPT ![self] = 0]
                    /\ built' = [built EXCEPT ![self] = TRUE]
                    /\ pc' = [pc EXCEPT ![self] = "Insert"]
                    /\ fails' = fails
              /\ UNCHANGED << llm, spendSet, complete, nextId, inflight, pend, 
                              reply, extras, ambig, lookupErrs, ignores, 
                              crashes, lost, result, seq, tot, lateCnt, spent >>

Insert(self) == /\ pc[self] = "Insert"
                /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                      /\ ambig' = ambig
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ reply' = [reply EXCEPT ![self] = "err_c"]
                /\ IF reply'[self] # "err_nc" /\ ~llm[seq[self]].rec
                      THEN /\ llm' = [llm EXCEPT ![seq[self]] = [rec |-> TRUE, amt |-> taken[self], by |-> self]]
                      ELSE /\ TRUE
                           /\ llm' = llm
                /\ IF reply'[self] # "ok"
                      THEN /\ pc' = [pc EXCEPT ![self] = "FailPath"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "Recorded"]
                /\ UNCHANGED << spendSet, complete, nextId, meter, inflight, 
                                pend, extras, fails, lookupErrs, ignores, 
                                crashes, billed, lost, result, seq, taken, 
                                built, tot, lateCnt, spent >>

Recorded(self) == /\ pc[self] = "Recorded"
                  /\ IF llm[seq[self]].by # self /\ Bug # "LostTurnNotLate"
                        THEN /\ meter' = [meter EXCEPT ![self] = meter[self] + taken[self]]
                        ELSE /\ TRUE
                             /\ meter' = meter
                  /\ tot' = [tot EXCEPT ![self] = tot[self] + llm[seq[self]].amt]
                  /\ seq' = [seq EXCEPT ![self] = seq[self] + 1]
                  /\ built' = [built EXCEPT ![self] = FALSE]
                  /\ taken' = [taken EXCEPT ![self] = 0]
                  /\ pc' = [pc EXCEPT ![self] = "Turn"]
                  /\ UNCHANGED << llm, spendSet, complete, nextId, inflight, 
                                  pend, reply, extras, fails, ambig, 
                                  lookupErrs, ignores, crashes, billed, lost, 
                                  result, lateCnt, spent >>

FailPath(self) == /\ pc[self] = "FailPath"
                  /\ \/ /\ meter' = [meter EXCEPT ![self] = meter[self] + inflight[self]]
                        /\ billed' = billed + inflight[self]
                        /\ UNCHANGED <<ignores, lost>>
                     \/ /\ ignores < MaxIgnore /\ inflight[self] > 0
                        /\ ignores' = ignores + 1
                        /\ billed' = billed + inflight[self]
                        /\ lost' = lost + inflight[self]
                        /\ meter' = meter
                  /\ inflight' = [inflight EXCEPT ![self] = 0]
                  /\ pc' = [pc EXCEPT ![self] = "FailLookup"]
                  /\ UNCHANGED << llm, spendSet, complete, nextId, pend, reply, 
                                  extras, fails, ambig, lookupErrs, crashes, 
                                  result, seq, taken, built, tot, lateCnt, 
                                  spent >>

FailLookup(self) == /\ pc[self] = "FailLookup"
                    /\ IF built[self] /\ Bug # "NoLandedLookup"
                          THEN /\ \/ /\ lookupErrs < MaxLookupErr
                                     /\ lookupErrs' = lookupErrs + 1
                                     /\ pend' = [pend EXCEPT ![self] = pend[self] \cup {[turn |-> TRUE, key |-> 0, n |-> seq[self], amt |-> taken[self]]}]
                                     /\ built' = [built EXCEPT ![self] = FALSE]
                                     /\ taken' = [taken EXCEPT ![self] = 0]
                                     /\ pc' = [pc EXCEPT ![self] = "Leave"]
                                     /\ meter' = meter
                                  \/ /\ IF llm[seq[self]].rec
                                           THEN /\ IF llm[seq[self]].by # self
                                                      THEN /\ meter' = [meter EXCEPT ![self] = meter[self] + taken[self]]
                                                      ELSE /\ TRUE
                                                           /\ meter' = meter
                                                /\ built' = [built EXCEPT ![self] = FALSE]
                                                /\ taken' = [taken EXCEPT ![self] = 0]
                                                /\ pc' = [pc EXCEPT ![self] = "Leave"]
                                           ELSE /\ pc' = [pc EXCEPT ![self] = "FailSpend"]
                                                /\ UNCHANGED << meter, taken, 
                                                                built >>
                                     /\ UNCHANGED <<pend, lookupErrs>>
                          ELSE /\ pc' = [pc EXCEPT ![self] = "FailSpend"]
                               /\ UNCHANGED << meter, pend, lookupErrs, taken, 
                                               built >>
                    /\ UNCHANGED << llm, spendSet, complete, nextId, inflight, 
                                    reply, extras, fails, ambig, ignores, 
                                    crashes, billed, lost, result, seq, tot, 
                                    lateCnt, spent >>

FailSpend(self) == /\ pc[self] = "FailSpend"
                   /\ spent' = [spent EXCEPT ![self] = meter[self] + taken[self]]
                   /\ meter' = [meter EXCEPT ![self] = 0]
                   /\ built' = [built EXCEPT ![self] = FALSE]
                   /\ taken' = [taken EXCEPT ![self] = 0]
                   /\ IF spent'[self] > 0
                         THEN /\ nextId' = FreshId
                              /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                    /\ ambig' = ambig
                                 \/ /\ ambig < MaxAmbig
                                    /\ ambig' = ambig + 1
                                    /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                 \/ /\ ambig < MaxAmbig
                                    /\ ambig' = ambig + 1
                                    /\ reply' = [reply EXCEPT ![self] = "err_c"]
                              /\ IF reply'[self] # "err_nc" /\ ~Landed(FreshId)
                                    THEN /\ spendSet' = (spendSet \cup {[key |-> FreshId, amt |-> spent'[self]]})
                                    ELSE /\ TRUE
                                         /\ UNCHANGED spendSet
                              /\ IF reply'[self] # "ok"
                                    THEN /\ pend' = [pend EXCEPT ![self] = (pend[self]) \cup {[turn |-> FALSE, key |-> FreshId, n |-> 0, amt |-> spent'[self]]}]
                                         /\ tot' = tot
                                    ELSE /\ IF ~Landed(FreshId)
                                               THEN /\ pend' = [pend EXCEPT ![self] = pend[self]]
                                                    /\ tot' = [tot EXCEPT ![self] = tot[self] + spent'[self]]
                                               ELSE /\ pend' = [pend EXCEPT ![self] = pend[self]]
                                                    /\ tot' = [tot EXCEPT ![self] = tot[self] + (CHOOSE r \in spendSet' : r.key = FreshId).amt]
                         ELSE /\ TRUE
                              /\ UNCHANGED << spendSet, nextId, pend, reply, 
                                              ambig, tot >>
                   /\ pc' = [pc EXCEPT ![self] = "Leave"]
                   /\ UNCHANGED << llm, complete, inflight, extras, fails, 
                                   lookupErrs, ignores, crashes, billed, lost, 
                                   result, seq, lateCnt >>

Leave(self) == /\ pc[self] = "Leave"
               /\ IF Bug # "NoSettle"
                     THEN /\ \/ /\ meter' = [meter EXCEPT ![self] = meter[self] + inflight[self]]
                                /\ billed' = billed + inflight[self]
                                /\ UNCHANGED <<ignores, lost>>
                             \/ /\ ignores < MaxIgnore /\ inflight[self] > 0
                                /\ ignores' = ignores + 1
                                /\ billed' = billed + inflight[self]
                                /\ lost' = lost + inflight[self]
                                /\ meter' = meter
                          /\ inflight' = [inflight EXCEPT ![self] = 0]
                     ELSE /\ TRUE
                          /\ UNCHANGED << meter, inflight, ignores, billed, 
                                          lost >>
               /\ pc' = [pc EXCEPT ![self] = "LeaveLate"]
               /\ UNCHANGED << llm, spendSet, complete, nextId, pend, reply, 
                               extras, fails, ambig, lookupErrs, crashes, 
                               result, seq, taken, built, tot, lateCnt, spent >>

LeaveLate(self) == /\ pc[self] = "LeaveLate"
                   /\ IF meter[self] > 0
                         THEN /\ spent' = [spent EXCEPT ![self] = meter[self]]
                              /\ meter' = [meter EXCEPT ![self] = 0]
                              /\ IF Bug = "SharedLateKey"
                                    THEN /\ lateCnt' = [lateCnt EXCEPT ![self] = lateCnt[self] + 1]
                                         /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                               /\ ambig' = ambig
                                            \/ /\ ambig < MaxAmbig
                                               /\ ambig' = ambig + 1
                                               /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                            \/ /\ ambig < MaxAmbig
                                               /\ ambig' = ambig + 1
                                               /\ reply' = [reply EXCEPT ![self] = "err_c"]
                                         /\ IF reply'[self] # "err_nc" /\ ~Landed((100 + lateCnt'[self]))
                                               THEN /\ spendSet' = (spendSet \cup {[key |-> (100 + lateCnt'[self]), amt |-> spent'[self]]})
                                               ELSE /\ TRUE
                                                    /\ UNCHANGED spendSet
                                         /\ IF reply'[self] # "ok"
                                               THEN /\ pend' = [pend EXCEPT ![self] = (pend[self]) \cup {[turn |-> FALSE, key |-> (100 + lateCnt'[self]), n |-> 0, amt |-> spent'[self]]}]
                                                    /\ tot' = tot
                                               ELSE /\ IF ~Landed((100 + lateCnt'[self]))
                                                          THEN /\ pend' = [pend EXCEPT ![self] = pend[self]]
                                                               /\ tot' = [tot EXCEPT ![self] = tot[self] + spent'[self]]
                                                          ELSE /\ pend' = [pend EXCEPT ![self] = pend[self]]
                                                               /\ tot' = [tot EXCEPT ![self] = tot[self] + (CHOOSE r \in spendSet' : r.key = (100 + lateCnt'[self])).amt]
                                         /\ UNCHANGED nextId
                                    ELSE /\ nextId' = FreshId
                                         /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                               /\ ambig' = ambig
                                            \/ /\ ambig < MaxAmbig
                                               /\ ambig' = ambig + 1
                                               /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                            \/ /\ ambig < MaxAmbig
                                               /\ ambig' = ambig + 1
                                               /\ reply' = [reply EXCEPT ![self] = "err_c"]
                                         /\ IF reply'[self] # "err_nc" /\ ~Landed(FreshId)
                                               THEN /\ spendSet' = (spendSet \cup {[key |-> FreshId, amt |-> spent'[self]]})
                                               ELSE /\ TRUE
                                                    /\ UNCHANGED spendSet
                                         /\ IF reply'[self] # "ok"
                                               THEN /\ pend' = [pend EXCEPT ![self] = (pend[self]) \cup {[turn |-> FALSE, key |-> FreshId, n |-> 0, amt |-> spent'[self]]}]
                                                    /\ tot' = tot
                                               ELSE /\ IF ~Landed(FreshId)
                                                          THEN /\ pend' = [pend EXCEPT ![self] = pend[self]]
                                                               /\ tot' = [tot EXCEPT ![self] = tot[self] + spent'[self]]
                                                          ELSE /\ pend' = [pend EXCEPT ![self] = pend[self]]
                                                               /\ tot' = [tot EXCEPT ![self] = tot[self] + (CHOOSE r \in spendSet' : r.key = FreshId).amt]
                                         /\ UNCHANGED lateCnt
                         ELSE /\ TRUE
                              /\ UNCHANGED << spendSet, nextId, meter, pend, 
                                              reply, ambig, tot, lateCnt, 
                                              spent >>
                   /\ pc' = [pc EXCEPT ![self] = "LeaveDone"]
                   /\ UNCHANGED << llm, complete, inflight, extras, fails, 
                                   lookupErrs, ignores, crashes, billed, lost, 
                                   result, seq, taken, built >>

LeaveDone(self) == /\ pc[self] = "LeaveDone"
                   /\ result' = [result EXCEPT ![self] = tot[self]]
                   /\ pc' = [pc EXCEPT ![self] = "Open"]
                   /\ UNCHANGED << llm, spendSet, complete, nextId, meter, 
                                   inflight, pend, reply, extras, fails, ambig, 
                                   lookupErrs, ignores, crashes, billed, lost, 
                                   seq, taken, built, tot, lateCnt, spent >>

End(self) == /\ pc[self] = "End"
             /\ IF Bug # "NoSettle"
                   THEN /\ \/ /\ meter' = [meter EXCEPT ![self] = meter[self] + inflight[self]]
                              /\ billed' = billed + inflight[self]
                              /\ UNCHANGED <<ignores, lost>>
                           \/ /\ ignores < MaxIgnore /\ inflight[self] > 0
                              /\ ignores' = ignores + 1
                              /\ billed' = billed + inflight[self]
                              /\ lost' = lost + inflight[self]
                              /\ meter' = meter
                        /\ inflight' = [inflight EXCEPT ![self] = 0]
                   ELSE /\ TRUE
                        /\ UNCHANGED << meter, inflight, ignores, billed, lost >>
             /\ pc' = [pc EXCEPT ![self] = "EndLate"]
             /\ UNCHANGED << llm, spendSet, complete, nextId, pend, reply, 
                             extras, fails, ambig, lookupErrs, crashes, result, 
                             seq, taken, built, tot, lateCnt, spent >>

EndLate(self) == /\ pc[self] = "EndLate"
                 /\ IF meter[self] > 0
                       THEN /\ spent' = [spent EXCEPT ![self] = meter[self]]
                            /\ meter' = [meter EXCEPT ![self] = 0]
                            /\ IF Bug = "SharedLateKey"
                                  THEN /\ lateCnt' = [lateCnt EXCEPT ![self] = lateCnt[self] + 1]
                                       /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                             /\ ambig' = ambig
                                          \/ /\ ambig < MaxAmbig
                                             /\ ambig' = ambig + 1
                                             /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                          \/ /\ ambig < MaxAmbig
                                             /\ ambig' = ambig + 1
                                             /\ reply' = [reply EXCEPT ![self] = "err_c"]
                                       /\ IF reply'[self] # "err_nc" /\ ~Landed((100 + lateCnt'[self]))
                                             THEN /\ spendSet' = (spendSet \cup {[key |-> (100 + lateCnt'[self]), amt |-> spent'[self]]})
                                             ELSE /\ TRUE
                                                  /\ UNCHANGED spendSet
                                       /\ IF reply'[self] # "ok"
                                             THEN /\ pend' = [pend EXCEPT ![self] = (pend[self]) \cup {[turn |-> FALSE, key |-> (100 + lateCnt'[self]), n |-> 0, amt |-> spent'[self]]}]
                                                  /\ tot' = tot
                                             ELSE /\ IF ~Landed((100 + lateCnt'[self]))
                                                        THEN /\ pend' = [pend EXCEPT ![self] = pend[self]]
                                                             /\ tot' = [tot EXCEPT ![self] = tot[self] + spent'[self]]
                                                        ELSE /\ pend' = [pend EXCEPT ![self] = pend[self]]
                                                             /\ tot' = [tot EXCEPT ![self] = tot[self] + (CHOOSE r \in spendSet' : r.key = (100 + lateCnt'[self])).amt]
                                       /\ UNCHANGED nextId
                                  ELSE /\ nextId' = FreshId
                                       /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                             /\ ambig' = ambig
                                          \/ /\ ambig < MaxAmbig
                                             /\ ambig' = ambig + 1
                                             /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                          \/ /\ ambig < MaxAmbig
                                             /\ ambig' = ambig + 1
                                             /\ reply' = [reply EXCEPT ![self] = "err_c"]
                                       /\ IF reply'[self] # "err_nc" /\ ~Landed(FreshId)
                                             THEN /\ spendSet' = (spendSet \cup {[key |-> FreshId, amt |-> spent'[self]]})
                                             ELSE /\ TRUE
                                                  /\ UNCHANGED spendSet
                                       /\ IF reply'[self] # "ok"
                                             THEN /\ pend' = [pend EXCEPT ![self] = (pend[self]) \cup {[turn |-> FALSE, key |-> FreshId, n |-> 0, amt |-> spent'[self]]}]
                                                  /\ tot' = tot
                                             ELSE /\ IF ~Landed(FreshId)
                                                        THEN /\ pend' = [pend EXCEPT ![self] = pend[self]]
                                                             /\ tot' = [tot EXCEPT ![self] = tot[self] + spent'[self]]
                                                        ELSE /\ pend' = [pend EXCEPT ![self] = pend[self]]
                                                             /\ tot' = [tot EXCEPT ![self] = tot[self] + (CHOOSE r \in spendSet' : r.key = FreshId).amt]
                                       /\ UNCHANGED lateCnt
                            /\ IF reply'[self] # "ok"
                                  THEN /\ pc' = [pc EXCEPT ![self] = "LeaveDone"]
                                  ELSE /\ pc' = [pc EXCEPT ![self] = "Complete"]
                       ELSE /\ pc' = [pc EXCEPT ![self] = "Complete"]
                            /\ UNCHANGED << spendSet, nextId, meter, pend, 
                                            reply, ambig, tot, lateCnt, spent >>
                 /\ UNCHANGED << llm, complete, inflight, extras, fails, 
                                 lookupErrs, ignores, crashes, billed, lost, 
                                 result, seq, taken, built >>

Complete(self) == /\ pc[self] = "Complete"
                  /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                        /\ ambig' = ambig
                     \/ /\ ambig < MaxAmbig
                        /\ ambig' = ambig + 1
                        /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                     \/ /\ ambig < MaxAmbig
                        /\ ambig' = ambig + 1
                        /\ reply' = [reply EXCEPT ![self] = "err_c"]
                  /\ IF reply'[self] # "err_nc"
                        THEN /\ complete' = TRUE
                        ELSE /\ TRUE
                             /\ UNCHANGED complete
                  /\ result' = [result EXCEPT ![self] = tot[self]]
                  /\ IF reply'[self] # "ok"
                        THEN /\ pc' = [pc EXCEPT ![self] = "Open"]
                        ELSE /\ pc' = [pc EXCEPT ![self] = "Done"]
                  /\ UNCHANGED << llm, spendSet, nextId, meter, inflight, pend, 
                                  extras, fails, lookupErrs, ignores, crashes, 
                                  billed, lost, seq, taken, built, tot, 
                                  lateCnt, spent >>

driver(self) == Open(self) \/ SettlePending(self) \/ Turn(self)
                   \/ Call(self) \/ Insert(self) \/ Recorded(self)
                   \/ FailPath(self) \/ FailLookup(self) \/ FailSpend(self)
                   \/ Leave(self) \/ LeaveLate(self) \/ LeaveDone(self)
                   \/ End(self) \/ EndLate(self) \/ Complete(self)

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == (\E self \in Drivers: driver(self))
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ \A self \in Drivers : WF_vars(driver(self))

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION
-----------------------------------------------------------------------------

\* The spend a process keeps for its next drive: a turn's spend the journal holds in this
\* driver's own record is not outstanding.
Outstanding(d) == SumAmt({p \in pend[d] : ~(p.turn /\ llm[p.n].rec /\ llm[p.n].by = d)
                                         /\ ~(~p.turn /\ Landed(p.key))})

\* A process crash: the meter, the requests in flight (billed by the provider) and the kept spend
\* are gone; that spend is a documented limit.
Crash(d) ==
  /\ crashes < MaxCrash
  /\ pc[d] # "Done"
  /\ crashes' = crashes + 1
  /\ billed' = billed + inflight[d]
  \* The spend a turn's built record carries is lost too while its write has not landed.
  /\ lost' = lost + meter[d] + inflight[d] + Outstanding(d)
             + (IF built[d] /\ ~(llm[seq[d]].rec /\ llm[seq[d]].by = d) THEN taken[d] ELSE 0)
  /\ meter' = [meter EXCEPT ![d] = 0]
  /\ inflight' = [inflight EXCEPT ![d] = 0]
  /\ pend' = [pend EXCEPT ![d] = {}]
  /\ pc' = [pc EXCEPT ![d] = "Open"]
  /\ UNCHANGED <<llm, spendSet, complete, nextId, reply, extras, fails, ambig, lookupErrs, ignores,
                 result, seq, taken, built, tot, lateCnt, spent>>

\* A request in flight ends: its usage reaches the meter of its drive (a hedge loser ending after
\* the winner: the next turn's record takes it, or the drive's end journals it as late spend).
RequestEnds(d) ==
  /\ inflight[d] > 0 /\ pc[d] # "Done"
  /\ inflight' = [inflight EXCEPT ![d] = @ - 1]
  /\ meter' = [meter EXCEPT ![d] = @ + 1]
  /\ billed' = billed + 1
  /\ UNCHANGED <<llm, spendSet, complete, nextId, pend, reply, extras, fails, ambig, lookupErrs,
                 ignores, crashes, lost, result, pc, seq, taken, built, tot, lateCnt, spent>>

\* A request that outlived a drive that did not wait for it (historically, Bug = "NoSettle") ends
\* after the drive returned: the provider bills it, and no record holds it.
EndsAfterReturn(d) ==
  /\ inflight[d] > 0 /\ pc[d] = "Done"
  /\ billed' = billed + inflight[d]
  /\ inflight' = [inflight EXCEPT ![d] = 0]
  /\ UNCHANGED <<llm, spendSet, complete, nextId, meter, pend, reply, extras, fails, ambig,
                 lookupErrs, ignores, crashes, lost, result, pc, seq, taken, built, tot, lateCnt,
                 spent>>

FullNext == Next \/ \E d \in Drivers : Crash(d) \/ RequestEnds(d) \/ EndsAfterReturn(d)
FullSpec == Init /\ [][FullNext]_vars /\ \A self \in Drivers : WF_vars(driver(self))

(***************************************************************************)
(* Properties                                                               *)
(***************************************************************************)

\* The journal never counts a billed unit twice.
NoDoubleCount == J <= billed

\* Exactly once: when every drive is done and nothing is in flight, the journal holds every
\* billed unit, except those a documented limit lost (a crash, a request that outlived the wait)
\* and those a process still keeps for a drive that will not come.
Quiescent == \A d \in Drivers : pc[d] = "Done" /\ inflight[d] = 0
SpendExact == Quiescent => J + lost + SumAmt({[k |-> d, amt |-> Outstanding(d)] : d \in Drivers}) = billed

\* Result.Spend: a drive's result never exceeds the journal's spend, and with one driver it is
\* exactly the journal's.
ResultSpend ==
  \A d \in Drivers :
    /\ result[d] >= 0 => result[d] <= J
    /\ (Cardinality(Drivers) = 1 /\ pc[d] = "Done" /\ result[d] >= 0) => result[d] = J

\* Vacuity, expected violated: a run completes with discarded or late spend journaled.
EffectNotReachable == ~(complete /\ J > MaxTurns)

BoundNotHit == nextId <= MaxIds
=============================================================================
