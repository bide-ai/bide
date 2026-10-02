-------------------------- MODULE ClaimsInductive --------------------------
(***************************************************************************)
(* An inductive invariant for AtMostOnce and NotStartedExclusive of model  *)
(* 1, checked by Apalache (spec/tla/README.md, "Apalache").                *)
(*                                                                          *)
(* IndInv holds initially, every step of FullNext from any state that      *)
(* satisfies it leads to a state that satisfies it, and it implies the     *)
(* properties. So the properties hold in every reachable state, at any     *)
(* depth, and for every fault budget: IndInv says nothing about the fault  *)
(* counters (ambig, crashes, cancels, evictions), so a state that          *)
(* satisfies it does so with each counter reset to 0, where every fault is *)
(* enabled; a smaller budget only removes steps.                           *)
(*                                                                          *)
(* Scope: the current protocol (Bug = "none"), no halt resolution, no      *)
(* approval gate, and the drivers, processes, calls, attempts and claim    *)
(* ids of the configuration (apalache/inductive*.cfg). Every placement of  *)
(* the drivers, every kind of call, LateCommit and the lease are left to   *)
(* the solver (ClaimsApalache's CInitPlacements).                          *)
(***************************************************************************)
EXTENDS ClaimsApalache

DriverLabels == {"Start", "Open", "GateTake", "GateWrite", "ApGate", "QTally", "QCount", "QRecord",
                 "Deny", "Claim", "ClaimRetry", "ClaimInsert", "ClaimNS", "Hold", "Lost", "Join",
                 "LoserRead", "LoserLead", "LoserWait", "Win", "WinnerWait", "Call", "Record",
                 "NotStarted", "Finish", "Done"}

\* The labels at which g[d] is the attempt the driver works on.
AttemptLabels == {"Claim", "ClaimRetry", "ClaimInsert", "ClaimNS", "Lost", "Join", "LoserRead",
                  "LoserWait", "Win", "WinnerWait", "Call", "Record", "NotStarted"}

\* A driver that holds a won claim it has not finished with.
Holding(d) == pc[d] \in {"Win", "WinnerWait", "Call"}

\* An id the state could still write a not-started record for, or hand to a driver to run.
InCustody(i) ==
  \/ \E p \in Procs, c \in Calls, x \in Gens : i \in pending[p][c][x]
  \/ \E d \in Drivers : i \in toRetry[d]
  \/ \E w \in lateNS : w[3] = i

\* The shape of the state: every variable in its range. The fault counters are any natural
\* number; the variables of the resolver, the approval gate and the historical bugs keep their
\* initial values, since this scope never changes them. TypeGen generates the states (for
\* IndInit), TypeOK checks them: Apalache can enumerate the flattened nested functions but not
\* test membership in them.
TypeGen ==
  /\ marker \in [Calls -> [Gens -> 0..MaxIds]]
  /\ nsSet \in SUBSET (Calls \X Gens \X Ids)
  /\ heldSet = {}
  /\ result \in [Calls -> {None, "drv"}]
  /\ toolFail = [c \in Calls |-> FALSE]
  /\ lateMarker \in SUBSET (Calls \X Gens \X Ids)
  /\ lateNS \in SUBSET (Calls \X Gens \X Ids)
  /\ lateResult \in SUBSET (Calls \X {"drv"})
  /\ \E pf \in [Procs \X Calls \X Gens -> SUBSET Ids] :
       pending = [p \in Procs |-> [c \in Calls |-> [x \in Gens |-> pf[<<p, c, x>>]]]]
  /\ fl \in [Procs -> [Calls -> Drivers \cup {None}]]
  /\ \E wf \in [Procs \X Calls -> SUBSET Drivers] :
       waiters = [p \in Procs |-> [c \in Calls |-> wf[<<p, c>>]]]
  /\ jres \in [Drivers -> {None, "ok", "err"}]
  /\ cid \in [Drivers -> 0..MaxIds]
  /\ oldId \in [Drivers -> 0..MaxIds]
  /\ toRetry \in [Drivers -> SUBSET Ids]
  /\ lease \in Drivers \cup {None}
  /\ rids = {}
  /\ rcid = 0
  /\ ambig \in Nat
  /\ crashes \in Nat
  /\ cancels \in Nat
  /\ evictions \in Nat
  /\ fired \in [Calls -> 0..1]
  /\ firedAt \in [Calls -> [Gens -> 0..MaxIds]]
  /\ lost \in SUBSET Ids
  /\ evicted \in SUBSET Ids
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
  /\ pc \in [Drivers -> DriverLabels \ {"QTally", "QCount", "QRecord", "Deny", "Hold", "LoserLead"}]
  /\ g \in [Drivers -> 0..(MaxGen + 1)]
  /\ gg \in [Drivers -> Gens]
  /\ reply \in [Drivers -> {"", "ok", "err_nc", "err_c", "err_late"}]
  /\ outcome \in [Drivers -> {None, "done", "error", "halt_crashed", "halt_contended", "pause"}]
  /\ won \in [Drivers -> BOOLEAN]
  /\ reused = [d \in Drivers |-> FALSE]
  /\ snapOne = [d \in Drivers |-> "none"]
  /\ snapTally = [d \in Drivers |-> NoTally]
  /\ gp = [d \in Drivers |-> "none"]
  /\ qt = [d \in Drivers |-> NoTally]
  /\ loadDenied = [d \in Drivers |-> FALSE]
  /\ rc = [r \in ResolverSet |-> None]
  /\ rg = [r \in ResolverSet |-> 0]
  /\ rreply = [r \in ResolverSet |-> ""]
  /\ rclaimed = [r \in ResolverSet |-> FALSE]

TypeOK ==
  /\ marker \in [Calls -> [Gens -> 0..MaxIds]]
  /\ nsSet \in SUBSET (Calls \X Gens \X Ids)
  /\ heldSet = {}
  /\ result \in [Calls -> {None, "drv"}]
  /\ toolFail = [c \in Calls |-> FALSE]
  /\ lateMarker \in SUBSET (Calls \X Gens \X Ids)
  /\ lateNS \in SUBSET (Calls \X Gens \X Ids)
  /\ lateResult \in SUBSET (Calls \X {"drv"})
  /\ DOMAIN pending = Procs
  /\ \A p \in Procs : DOMAIN pending[p] = Calls /\ \A c \in Calls :
       DOMAIN pending[p][c] = Gens /\ \A x \in Gens : pending[p][c][x] \subseteq Ids
  /\ fl \in [Procs -> [Calls -> Drivers \cup {None}]]
  /\ DOMAIN waiters = Procs
  /\ \A p \in Procs : DOMAIN waiters[p] = Calls /\ \A c \in Calls : waiters[p][c] \subseteq Drivers
  /\ jres \in [Drivers -> {None, "ok", "err"}]
  /\ cid \in [Drivers -> 0..MaxIds]
  /\ oldId \in [Drivers -> 0..MaxIds]
  /\ toRetry \in [Drivers -> SUBSET Ids]
  /\ lease \in Drivers \cup {None}
  /\ rids = {}
  /\ rcid = 0
  /\ ambig \in Nat
  /\ crashes \in Nat
  /\ cancels \in Nat
  /\ evictions \in Nat
  /\ fired \in [Calls -> 0..1]
  /\ firedAt \in [Calls -> [Gens -> 0..MaxIds]]
  /\ lost \in SUBSET Ids
  /\ evicted \in SUBSET Ids
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
  /\ pc \in [Drivers -> DriverLabels \ {"QTally", "QCount", "QRecord", "Deny", "Hold", "LoserLead"}]
  /\ g \in [Drivers -> 0..(MaxGen + 1)]
  /\ gg \in [Drivers -> Gens]
  /\ reply \in [Drivers -> {"", "ok", "err_nc", "err_c", "err_late"}]
  /\ outcome \in [Drivers -> {None, "done", "error", "halt_crashed", "halt_contended", "pause"}]
  /\ won \in [Drivers -> BOOLEAN]
  /\ reused = [d \in Drivers |-> FALSE]
  /\ snapOne = [d \in Drivers |-> "none"]
  /\ snapTally = [d \in Drivers |-> NoTally]
  /\ gp = [d \in Drivers |-> "none"]
  /\ qt = [d \in Drivers |-> NoTally]
  /\ loadDenied = [d \in Drivers |-> FALSE]
  /\ rc = [r \in ResolverSet |-> None]
  /\ rg = [r \in ResolverSet |-> 0]
  /\ rreply = [r \in ResolverSet |-> ""]
  /\ rclaimed = [r \in ResolverSet |-> FALSE]

\* What each driver's position says about its own variables.
Locals ==
  \A d \in Drivers :
    LET c == CallOf[d] IN
    /\ toRetry[d] # {} <=> pc[d] = "ClaimRetry"
    /\ pc[d] \in {"GateTake", "GateWrite", "ApGate"} => g[d] = 0
    /\ pc[d] \in {"GateTake", "GateWrite"} => Kind[c] = "tool" /\ oldId[d] \in Ids
    /\ pc[d] = "GateTake" => marker[c][gg[d]] = oldId[d]
    /\ pc[d] \in {"ClaimNS", "Lost", "Win", "WinnerWait", "Call", "Record", "NotStarted"} =>
         cid[d] \in Ids /\ g[d] \in Gens
    /\ pc[d] = "Lost" => marker[c][g[d]] \notin {0, cid[d]}
    /\ pc[d] = "NotStarted" => marker[c][g[d]] = cid[d]
    /\ pc[d] = "Record" => firedAt[c][g[d]] = cid[d]

\* Attempts are claimed in order: an attempt is claimed (a marker, a late marker write, or a
\* driver working on it) only once every earlier attempt of the call is voided.
Chain ==
  /\ \A c \in Calls, x, y \in Gens : y < x /\ marker[c][x] # 0 => Voided(c, y)
  /\ \A w \in lateMarker : \A y \in Gens : y < w[2] => Voided(w[1], y)
  /\ \A d \in Drivers : pc[d] \in AttemptLabels =>
       /\ pc[d] # "Claim" => g[d] \in Gens
       /\ \A y \in Gens : y < g[d] => Voided(CallOf[d], y)

\* A fired attempt: its marker holds the claim that fired, which no not-started record (written
\* or in flight) names and nothing holds that could write one; only the driver that fired still
\* knows the id, and the resume gate may read it from the live marker.
Fired ==
  /\ \A c \in Calls : fired[c] = 0 <=> \A x \in Gens : firedAt[c][x] = 0
  /\ \A c \in Calls, x, y \in Gens : firedAt[c][x] # 0 /\ firedAt[c][y] # 0 => x = y
  /\ \A c \in Calls, x \in Gens : firedAt[c][x] # 0 =>
       LET i == firedAt[c][x] IN
       /\ marker[c][x] = i
       /\ <<c, x, i>> \notin nsSet
       /\ ~InCustody(i)
       /\ \A d \in Drivers :
            /\ oldId[d] = i => pc[d] = "GateTake"
            /\ cid[d] = i => /\ pc[d] \in {"Record", "Finish", "Open", "Done"}
                             /\ CallOf[d] = c /\ g[d] = x

\* A driver holding a won claim: the attempt's marker is its claim, which no not-started record
\* names and nothing else holds, and the call has not fired.
Holders ==
  \A d \in Drivers : Holding(d) =>
    LET c == CallOf[d]
        x == g[d]
        i == cid[d] IN
    /\ i \in Ids
    /\ marker[c][x] = i
    /\ <<c, x, i>> \notin nsSet
    /\ ~InCustody(i)
    /\ \A d2 \in Drivers : /\ oldId[d2] = i => pc[d2] = "GateTake"
                           /\ d2 # d => cid[d2] # i
    /\ \A y \in Gens : firedAt[c][y] = 0

Core == Locals /\ Chain /\ Fired /\ Holders

IndInv == TypeOK /\ Core

\* The properties IndInv must imply.
IndProps == AtMostOnce /\ NotStartedExclusive /\ NoLiveOverride

\* For the induction step: any state of the invariant.
IndInit == TypeGen /\ Core
=============================================================================
