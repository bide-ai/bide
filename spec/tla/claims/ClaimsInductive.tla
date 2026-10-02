-------------------------- MODULE ClaimsInductive --------------------------
(***************************************************************************)
(* An inductive invariant for the safety properties of model 1, checked by *)
(* Apalache (spec/tla/README.md, "Apalache").                              *)
(*                                                                          *)
(* IndInv holds initially, every step of FullNext from any state that      *)
(* satisfies it leads to a state that satisfies it, and it implies         *)
(* AtMostOnce, NotStartedExclusive, NoLiveOverride and AtMostOncePerIntent. *)
(* So the properties hold in every reachable state, at any depth, and for  *)
(* every fault budget: IndInv says nothing about the fault counters        *)
(* (ambig, crashes, cancels, evictions), so a state that satisfies it does *)
(* so with each counter reset to 0, where every fault is enabled; a        *)
(* smaller budget only removes steps.                                       *)
(*                                                                          *)
(* Scope: the current protocol (Bug = "none"), with or without halt        *)
(* resolution as the current protocol does it (ResolveClaim, the lease or  *)
(* the minimum-age check, ResolveVoidOnError FALSE, and under the lease    *)
(* check no plain run holding the live attempt: PlainRunIdleAtCheck), no   *)
(* approval gate, and the drivers, processes, calls, attempts and claim    *)
(* ids of the configuration (apalache/inductive-*.cfg). Every placement of *)
(* the drivers, every kind of call, LateCommit, the lease holders, the     *)
(* resolver's check and process are left to the solver.                   *)
(***************************************************************************)
EXTENDS ClaimsApalache

DriverLabels == {"Start", "Open", "GateTake", "GateWrite", "ApGate", "Claim", "ClaimRetry",
                 "ClaimInsert", "ClaimNS", "Lost", "Join", "LoserRead", "LoserWait", "Win",
                 "WinnerWait", "Call", "Record", "NotStarted", "Finish", "Done"}
ResolverLabels == {"RCheck", "RClaim", "RRetry", "RInsert", "RClaimNS", "RWrite", "RRecord",
                   "RWait", "RNotStarted", "RRelease", "Done"}
Results == {None, "drv", "res_ok", "res_err"}
Resolutions == {"res_ok", "res_err"}
Replies == {"", "ok", "err_nc", "err_c", "err_late"}

\* The labels at which g[d] is the attempt the driver works on.
AttemptLabels == {"Claim", "ClaimRetry", "ClaimInsert", "ClaimNS", "Lost", "Join", "LoserRead",
                  "LoserWait", "Win", "WinnerWait", "Call", "Record", "NotStarted"}

\* A driver that holds a won claim it has not finished with.
Holding(d) == pc[d] \in {"Win", "WinnerWait", "Call"}

\* An id the state could still write a not-started record for, or hand to a driver to run.
InCustody(i) ==
  \/ \E p \in Procs, c \in Calls, x \in Gens : i \in pending[p][c][x]
  \/ \E d \in DOMAIN toRetry : i \in toRetry[d]
  \/ \E w \in lateNS : w[3] = i
  \/ \E d \in Drivers : pc[d] = "GateWrite" /\ oldId[d] = i

\* A resolution of call c is recorded, or written and may still commit.
ResolvedOrLate(c) ==
  \/ result[c] \in Resolutions
  \/ \E w \in lateResult : w[1] = c /\ w[2] \in Resolutions

\* The shape of the state: every variable in its range. The fault counters are any natural
\* number; the variables of the approval gate and of the historical bugs keep their initial
\* values, since this scope never changes them. TypeGen generates the states (for IndInit),
\* TypeOK checks them: Apalache can enumerate the flattened nested functions but not test
\* membership in them.
TypeCommon ==
  /\ marker \in [Calls -> [Gens -> 0..MaxIds]]
  /\ nsSet \in SUBSET (Calls \X Gens \X Ids)
  /\ heldSet = {}
  /\ result \in [Calls -> Results]
  /\ toolFail = [c \in Calls |-> FALSE]
  /\ lateMarker \in SUBSET (Calls \X Gens \X Ids)
  /\ lateNS \in SUBSET (Calls \X Gens \X Ids)
  /\ lateResult \in SUBSET (Calls \X (Results \ {None}))
  /\ fl \in [Procs -> [Calls -> Drivers \cup ResolverSet \cup {None}]]
  /\ jres \in [Drivers \cup ResolverSet -> {None, "ok", "err"}]
  /\ cid \in [Drivers -> 0..MaxIds]
  /\ oldId \in [Drivers -> 0..MaxIds]
  /\ toRetry \in [Drivers \cup ResolverSet -> SUBSET Ids]
  /\ lease \in Drivers \cup ResolverSet \cup {None}
  /\ rids \in SUBSET Ids
  /\ rcid \in 0..MaxIds
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
  /\ pc \in [Drivers \cup ResolverSet -> DriverLabels \cup ResolverLabels]
  /\ \A d \in Drivers : pc[d] \in DriverLabels
  /\ \A r \in ResolverSet : pc[r] \in ResolverLabels
  /\ g \in [Drivers -> 0..(MaxGen + 1)]
  /\ gg \in [Drivers -> Gens]
  /\ reply \in [Drivers -> Replies]
  /\ outcome \in [Drivers -> {None, "done", "error", "halt_crashed", "halt_contended", "pause"}]
  /\ won \in [Drivers -> BOOLEAN]
  /\ reused = [d \in Drivers |-> FALSE]
  /\ snapOne = [d \in Drivers |-> "none"]
  /\ snapTally = [d \in Drivers |-> NoTally]
  /\ gp = [d \in Drivers |-> "none"]
  /\ qt = [d \in Drivers |-> NoTally]
  /\ loadDenied = [d \in Drivers |-> FALSE]
  /\ rc \in [ResolverSet -> Calls \cup {None}]
  /\ rg \in [ResolverSet -> Gens]
  /\ rreply \in [ResolverSet -> Replies]
  /\ rclaimed \in [ResolverSet -> BOOLEAN]

TypeGen ==
  /\ TypeCommon
  /\ \E pf \in [Procs \X Calls \X Gens -> SUBSET Ids] :
       pending = [p \in Procs |-> [c \in Calls |-> [x \in Gens |-> pf[<<p, c, x>>]]]]
  /\ \E wf \in [Procs \X Calls -> SUBSET (Drivers \cup ResolverSet)] :
       waiters = [p \in Procs |-> [c \in Calls |-> wf[<<p, c>>]]]

TypeOK ==
  /\ TypeCommon
  /\ DOMAIN pending = Procs
  /\ \A p \in Procs : DOMAIN pending[p] = Calls /\ \A c \in Calls :
       DOMAIN pending[p][c] = Gens /\ \A x \in Gens : pending[p][c][x] \subseteq Ids
  /\ DOMAIN waiters = Procs
  /\ \A p \in Procs : DOMAIN waiters[p] = Calls /\ \A c \in Calls :
       waiters[p][c] \subseteq Drivers \cup ResolverSet

\* What each driver's and the resolver's position says about their own variables.
Locals ==
  /\ \A d \in Drivers :
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
  /\ \A r \in ResolverSet :
       /\ toRetry[r] # {} <=> pc[r] = "RRetry"
       /\ pc[r] = "RCheck" <=> rc[r] = None
       /\ pc[r] = "RCheck" => rids = {} /\ rcid = 0 /\ ~rclaimed[r]
       /\ pc[r] \in {"RClaim", "RRetry", "RInsert"} =>
            /\ rids = {} /\ rcid = 0 /\ ~rclaimed[r]
            /\ pc[r] # "RClaim" => rg[r] + 1 \in Gens
       /\ pc[r] \in {"RClaimNS", "RWrite", "RRecord", "RWait", "RNotStarted", "RRelease"} =>
            rcid \in Ids /\ rids = {rcid} /\ rg[r] + 1 \in Gens
       /\ pc[r] \in {"RWrite", "RRecord", "RWait", "RNotStarted"} => rclaimed[r]
       /\ pc[r] = "RClaimNS" => ~rclaimed[r]
       /\ Cardinality(rids) <= 1
       /\ rclaimed[r] => rids # {}

\* Attempts are claimed in order: an attempt a driver claims (a marker, a late marker write, or a
\* driver working on it) only once every earlier attempt of the call is voided. The resolver claims
\* the attempt after the live one it checked: every earlier attempt holds a driver's marker.
Chain ==
  /\ \A c \in Calls, x, y \in Gens :
       y < x /\ marker[c][x] # 0 /\ marker[c][x] \notin rids => Voided(c, y)
  /\ \A c \in Calls, x, y \in Gens :
       y < x /\ marker[c][x] \in rids => marker[c][y] # 0 /\ marker[c][y] \notin rids
  /\ \A w \in lateMarker : \A y \in Gens : y < w[2] =>
       IF w[3] \in rids THEN marker[w[1]][y] # 0 ELSE Voided(w[1], y)
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
       /\ i \notin rids
       /\ <<c, x, i>> \notin nsSet
       /\ ~InCustody(i)
       /\ \A d \in Drivers :
            /\ oldId[d] = i => pc[d] = "GateTake"
            /\ cid[d] = i => /\ pc[d] \in {"Record", "Finish", "Open", "Done"}
                             /\ CallOf[d] = c /\ g[d] = x

\* A driver holding a won claim: the attempt's marker is its claim, which no not-started record
\* names and nothing else holds, the call has not fired, and no resolution of the call is
\* recorded or claimed. A driver that fired and is recording its result finds no resolution.
Holders ==
  /\ \A d \in Drivers : Holding(d) =>
       LET c == CallOf[d]
           x == g[d]
           i == cid[d] IN
       /\ i \in Ids
       /\ i \notin rids
       /\ marker[c][x] = i
       /\ <<c, x, i>> \notin nsSet
       /\ ~InCustody(i)
       /\ \A d2 \in Drivers : /\ oldId[d2] = i => pc[d2] = "GateTake"
                              /\ d2 # d => cid[d2] # i
       /\ \A y \in Gens : firedAt[c][y] = 0
  /\ \A d \in Drivers : Holding(d) \/ pc[d] = "Record" =>
       /\ ~ResolvedOrLate(CallOf[d])
       /\ \A r \in ResolverSet : ~(rclaimed[r] /\ rc[r] = CallOf[d])

\* Halt resolution. Once the resolver has checked the live attempt rg of call rc, no driver holds
\* that attempt's claim while running (the check's assumption, which nothing breaks later: no
\* driver takes up an existing claim id again). Once it has claimed the next attempt, its marker
\* stays live, nothing could void it, and so no driver can win an attempt of the call again. A
\* recorded resolution, written or still to commit, comes from such a resolver; and "not charged"
\* is written only for a call that never fired.
Resolver ==
  /\ \A r \in ResolverSet :
       /\ rc[r] # None =>
            LET c == rc[r] IN
            /\ marker[c][rg[r]] # 0
            /\ marker[c][rg[r]] \notin rids
            /\ \A d \in Drivers :
                 ~(CallOf[d] = c /\ cid[d] = marker[c][rg[r]] /\ pc[d] \in Window)
       /\ rclaimed[r] =>
            LET c == rc[r]
                x == rg[r] + 1 IN
            /\ x \in Gens
            /\ marker[c][x] \in rids
            /\ ~NSTaken(c, x, marker[c][x])
            /\ ~InCustody(marker[c][x])
       /\ pc[r] = "RRecord" => rclaimed[r]
  /\ \A c \in Calls : ResolvedOrLate(c) => \E r \in ResolverSet : rc[r] = c /\ rclaimed[r]
  /\ \A c \in Calls :
       (result[c] = "res_err" \/ <<c, "res_err">> \in lateResult) => fired[c] = 0

\* The caller issues a call that is not a first call only after reading the one before it as not
\* charged; until then no driver of it has moved and it has not fired.
Intent ==
  \A c \in Calls \ FirstCalls :
    (fired[c] > 0 \/ \E d \in Drivers : CallOf[d] = c /\ pc[d] # "Start") => Issued(c)

Core == Locals /\ Chain /\ Fired /\ Holders /\ Resolver /\ Intent

IndInv == TypeOK /\ Core

\* The properties IndInv must imply.
IndProps == AtMostOnce /\ NotStartedExclusive /\ NoLiveOverride /\ AtMostOncePerIntent

\* For the induction step: any state of the invariant.
IndInit == TypeGen /\ Core
=============================================================================
