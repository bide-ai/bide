------------------------------- MODULE Probes -------------------------------
(***************************************************************************)
(* Model 13: probes for unknown outcomes (docs/design/probes.md). A design *)
(* model with no Go code yet, so it has no map and no markers. It restates *)
(* the attempt-claim rules it needs (numbered attempts, first writer wins, *)
(* an attempt marker that records whether the call's requests carry a      *)
(* fence) and adds:                                                        *)
(*  - a provider that applies a call's requests, which stay in flight      *)
(*    until it does (a stalled or dead driver's request can land later);   *)
(*  - a probe: a pure lookup any driver may make, and acting on its        *)
(*    verdict, which needs the claim on the next attempt, the run's lease  *)
(*    (so the original driver's lease has lapsed) and the attempt's        *)
(*    minimum age;                                                         *)
(*  - attempt-scoped results: the call's outcome is the result recorded    *)
(*    for its latest attempt, so a result from an older attempt never      *)
(*    stands;                                                              *)
(*  - a cap on Unknown probes, and a person granting more.                 *)
(*                                                                          *)
(* Mode selects the probe and the provider's fence:                        *)
(*   "off"     no probe: a live attempt with no result halts (today).      *)
(*   "observe" Happened or Unknown only (a lookup, no fence).              *)
(*   "void"    a lookup, then an atomic check-and-void at the provider: a  *)
(*             watermark below which fenced attempts' requests are         *)
(*             rejected (F-void).                                          *)
(*   "dedup"   the provider applies one request per call key; acting is a  *)
(*             replay under the next attempt (F-dedup).                    *)
(*   "lookup"  NotHappened from a lookup alone, relying on the lease and   *)
(*             the minimum age as a fence: the documented counterexample.  *)
(* Leases are not fenced: a lease lapses at any time, and its holder keeps *)
(* running. Time is abstract: Tick marks an attempt old enough to act on.  *)
(***************************************************************************)
EXTENDS Integers, FiniteSets

CONSTANTS
  Drivers,          \* drives of the run (model values)
  MaxAtt,           \* highest attempt number
  MaxCrash,         \* budget of driver crashes
  Mode,             \* see above
  PerAttempt,       \* TRUE: results are keyed per attempt; FALSE: one result key per call
  UnfencedAttempts, \* attempts made by a tool version whose requests carried no fence (no key)
  CheckStamp,       \* TRUE: act on an absent lookup only if every attempt's marker records Fenced
  Cap,              \* Unknown probes allowed per grant
  MaxGrant,         \* budget of fresh probes a person grants
  MonotoneVoid      \* TRUE: the provider's void only ever raises its watermark

ASSUME Mode \in {"off", "observe", "void", "dedup", "lookup"}
ASSUME MaxAtt \in Nat /\ MaxCrash \in Nat /\ Cap \in Nat /\ MaxGrant \in Nat
ASSUME PerAttempt \in BOOLEAN /\ CheckStamp \in BOOLEAN /\ MonotoneVoid \in BOOLEAN
ASSUME UnfencedAttempts \subseteq 0..MaxAtt

None == "none"
Att == 0..MaxAtt
\* vocabulary: begin
\* The journal records the model reads and writes: an attempt marker (with its Fenced stamp), a
\* probe verdict, an Unknown probe count, a grant of fresh probes, and a result (per attempt, or
\* per call when PerAttempt is FALSE).
RecordKinds == {"marker", "verdict", "probe_unknown", "probe_grant", "result"}
\* vocabulary: end

VARIABLES
  \* The journal.
  marker,    \* attempt -> the driver that claimed it, or None
  fenced,    \* attempt -> whether its marker records Fenced
  verdict,   \* attempt -> None | "happened" | "not"
  res,       \* result key -> None | "ok" | "err"
  unk,       \* Unknown probes recorded
  grants,    \* fresh probes granted by a person
  \* Time and the lease.
  aged,      \* attempt -> old enough to act on (MinAge)
  lease,     \* the run's lease holder, or None
  \* The provider.
  wire,      \* attempts whose request is in flight
  reply,     \* attempt -> None | "ok" | "rejected": what the provider answered its request
  pfloor,    \* void: fenced requests of attempts below it are rejected
  applied,   \* dedup: a request under the call key has been applied
  fired,     \* ghost: the effect's landings
  landedAt,  \* ghost: attempt -> its request landed
  \* The drivers.
  pc, tgt, act, crashes

vars == <<marker, fenced, verdict, res, unk, grants, aged, lease, wire, reply, pfloor, applied,
          fired, landedAt, pc, tgt, act, crashes>>

Max(a, b) == IF a >= b THEN a ELSE b
RK(x) == IF PerAttempt THEN x ELSE 0
Claimed == {x \in Att : marker[x] # None}
\* The live attempt: the latest one claimed, -1 when none is.
Latest == IF Claimed = {} THEN -1 ELSE CHOOSE x \in Claimed : \A y \in Claimed : y <= x
\* The call's outcome as a reader sees it: the result recorded for its latest attempt.
Outcome == IF Latest = -1 THEN None ELSE res[RK(Latest)]
AllStamped(l) == \A y \in 0..l : marker[y] # None => fenced[y]
StampOK(l) == ~CheckStamp \/ AllStamped(l)
FencedMode == Mode \in {"void", "dedup", "lookup"}

Init ==
  /\ marker = [x \in Att |-> None]
  /\ fenced = [x \in Att |-> FALSE]
  /\ verdict = [x \in Att |-> None]
  /\ res = [x \in Att |-> None]
  /\ unk = 0 /\ grants = 0
  /\ aged = [x \in Att |-> FALSE]
  /\ lease = None
  /\ wire = {}
  /\ reply = [x \in Att |-> None]
  /\ pfloor = 0
  /\ applied = FALSE
  /\ fired = 0
  /\ landedAt = [x \in Att |-> FALSE]
  /\ pc = [d \in Drivers |-> "idle"]
  /\ tgt = [d \in Drivers |-> 0]
  /\ act = [d \in Drivers |-> None]
  /\ crashes = 0

Goto(d, l) == pc' = [pc EXCEPT ![d] = l]

\* A drive starts under the run's lease (Lease, Recover, RecoverLoop).
Start(d) ==
  /\ pc[d] = "idle" /\ lease = None
  /\ lease' = d
  /\ Goto(d, "gate")
  /\ UNCHANGED <<marker, fenced, verdict, res, unk, grants, aged, wire, reply, pfloor, applied,
                 fired, landedAt, tgt, act, crashes>>

\* The resume gate reads the journal: no attempt, claim the first; the latest attempt has a
\* result, the call is done; otherwise its outcome is unknown, so probe it (or halt).
Gate(d) ==
  /\ pc[d] = "gate"
  /\ IF Latest = -1 THEN /\ tgt' = [tgt EXCEPT ![d] = 0] /\ Goto(d, "claim")
     ELSE IF Outcome # None THEN /\ Goto(d, "finish") /\ UNCHANGED tgt
     ELSE IF Mode = "off" THEN /\ Goto(d, "finish") /\ UNCHANGED tgt
     ELSE /\ tgt' = [tgt EXCEPT ![d] = Latest] /\ Goto(d, "probe")
  /\ UNCHANGED <<marker, fenced, verdict, res, unk, grants, aged, lease, wire, reply, pfloor,
                 applied, fired, landedAt, act, crashes>>

\* Claim an attempt: insert its marker, first writer wins. The marker records whether this
\* tool version's requests carry the fence. A loser halts (HaltContended).
Claim(d) ==
  /\ pc[d] = "claim"
  /\ LET x == tgt[d] IN
     IF x > MaxAtt \/ marker[x] # None THEN
       /\ Goto(d, "finish") /\ UNCHANGED <<marker, fenced>>
     ELSE
       /\ marker' = [marker EXCEPT ![x] = d]
       /\ fenced' = [fenced EXCEPT ![x] = x \notin UnfencedAttempts]
       /\ Goto(d, "send")
  /\ UNCHANGED <<verdict, res, unk, grants, aged, lease, wire, reply, pfloor, applied, fired,
                 landedAt, tgt, act, crashes>>

\* Call the tool: its request is now in flight, and may land whatever becomes of this driver.
Send(d) ==
  /\ pc[d] = "send"
  /\ wire' = wire \cup {tgt[d]}
  /\ Goto(d, "await")
  /\ UNCHANGED <<marker, fenced, verdict, res, unk, grants, aged, lease, reply, pfloor, applied,
                 fired, landedAt, tgt, act, crashes>>

\* The provider applies a request. Under void, a fenced request of an attempt below the
\* watermark is rejected; under dedup, a fenced request (one carrying the call key) after one
\* was applied returns the applied outcome without a second effect. An unfenced request is
\* applied whatever the fence.
Land(x) ==
  /\ x \in wire
  /\ wire' = wire \ {x}
  /\ IF Mode = "void" /\ fenced[x] /\ x < pfloor THEN
       /\ reply' = [reply EXCEPT ![x] = "rejected"]
       /\ UNCHANGED <<fired, landedAt, applied>>
     ELSE IF Mode = "dedup" /\ fenced[x] /\ applied THEN
       /\ reply' = [reply EXCEPT ![x] = "ok"]
       /\ UNCHANGED <<fired, landedAt, applied>>
     ELSE
       /\ reply' = [reply EXCEPT ![x] = "ok"]
       /\ fired' = fired + 1
       /\ landedAt' = [landedAt EXCEPT ![x] = TRUE]
       /\ applied' = (applied \/ fenced[x])
  /\ UNCHANGED <<marker, fenced, verdict, res, unk, grants, aged, lease, pfloor, pc, tgt, act,
                 crashes>>

\* The tool returns what the provider answered, and the driver records it (first write wins).
\* A rejected request is recorded as a failure: with attempt-scoped results it is the result of
\* an older attempt, which no reader takes for the call's outcome.
Record(d) ==
  /\ pc[d] = "await"
  /\ reply[tgt[d]] # None
  /\ LET k == RK(tgt[d]) IN
     res' = IF res[k] = None
              THEN [res EXCEPT ![k] = IF reply[tgt[d]] = "ok" THEN "ok" ELSE "err"]
              ELSE res
  /\ Goto(d, "finish")
  /\ UNCHANGED <<marker, fenced, verdict, unk, grants, aged, lease, wire, reply, pfloor, applied,
                 fired, landedAt, tgt, act, crashes>>

\* The probe's pure lookup, which needs no claim: Happened if the effect has landed (a
\* linearizable read), absent otherwise, or Unknown (an error, a timeout) at any time. Unknown is
\* recorded and counted; past the cap the gate halts until a person grants fresh probes. Under
\* observe an absent lookup is Unknown. Under the other modes it leads to acting, if every
\* attempt's marker records Fenced (CheckStamp).
Probe(d) ==
  /\ pc[d] = "probe"
  /\ IF unk >= Cap * (1 + grants) THEN
       /\ Goto(d, "finish") /\ UNCHANGED <<unk, act>>
     ELSE
       \/ /\ fired > 0
          /\ act' = [act EXCEPT ![d] = "happened"]
          /\ Goto(d, "actclaim") /\ UNCHANGED unk
       \/ /\ fired = 0 /\ FencedMode /\ StampOK(tgt[d])
          /\ act' = [act EXCEPT ![d] = "absent"]
          /\ Goto(d, "actclaim") /\ UNCHANGED unk
       \/ /\ unk' = unk + 1
          /\ Goto(d, "finish") /\ UNCHANGED act
  /\ UNCHANGED <<marker, fenced, verdict, res, grants, aged, lease, wire, reply, pfloor, applied,
                 fired, landedAt, tgt, crashes>>

\* Acting on a verdict needs the claim on the next attempt, the run's lease (so the original
\* driver's lease has lapsed, or it is this driver's own from an earlier drive) and the attempt's
\* minimum age. Neither the lease nor the age stops the original attempt's request.
ActClaim(d) ==
  /\ pc[d] = "actclaim"
  /\ LET l == tgt[d] IN
     IF lease # d \/ ~aged[l] \/ l + 1 > MaxAtt \/ marker[l + 1] # None THEN
       /\ Goto(d, "finish") /\ UNCHANGED <<marker, fenced, tgt>>
     ELSE
       /\ marker' = [marker EXCEPT ![l + 1] = d]
       /\ fenced' = [fenced EXCEPT ![l + 1] = (l + 1) \notin UnfencedAttempts]
       /\ tgt' = [tgt EXCEPT ![d] = l + 1]
       /\ Goto(d, "act")
  /\ UNCHANGED <<verdict, res, unk, grants, aged, lease, wire, reply, pfloor, applied, fired,
                 landedAt, act, crashes>>

\* Act, holding attempt n = tgt[d] (the probed attempt is n - 1):
\*  happened: record the verdict and the result under attempt n.
\*  void:     check-and-void at the provider, atomically: Happened if the effect landed, else
\*            raise the watermark to n, then record NotHappened and call the tool as attempt n.
\*  dedup:    replay the request under the call key, as attempt n.
\*  lookup:   record NotHappened from the lookup alone and call the tool as attempt n.
Act(d) ==
  /\ pc[d] = "act"
  /\ LET n == tgt[d] IN
     IF act[d] = "happened" \/ (Mode = "void" /\ fired > 0) THEN
       /\ verdict' = [verdict EXCEPT ![n - 1] = "happened"]
       /\ res' = IF res[RK(n)] = None THEN [res EXCEPT ![RK(n)] = "ok"] ELSE res
       /\ Goto(d, "finish") /\ UNCHANGED pfloor
     ELSE IF Mode = "dedup" THEN
       /\ Goto(d, "send") /\ UNCHANGED <<verdict, res, pfloor>>
     ELSE
       /\ pfloor' = IF Mode # "void" THEN pfloor
                      ELSE IF MonotoneVoid THEN Max(pfloor, n) ELSE n
       /\ verdict' = [verdict EXCEPT ![n - 1] = "not"]
       /\ Goto(d, "send") /\ UNCHANGED res
  /\ UNCHANGED <<marker, fenced, unk, grants, aged, lease, wire, reply, applied, fired, landedAt,
                 tgt, act, crashes>>

\* The drive ends (done or halted) and releases the lease if it still holds it. RecoverLoop
\* drives a halted run again, so the driver can start over.
Finish(d) ==
  /\ pc[d] = "finish"
  /\ lease' = IF lease = d THEN None ELSE lease
  /\ act' = [act EXCEPT ![d] = None]
  /\ Goto(d, "idle")
  /\ UNCHANGED <<marker, fenced, verdict, res, unk, grants, aged, wire, reply, pfloor, applied,
                 fired, landedAt, tgt, crashes>>

\* A process dies: the driver forgets everything; its request stays in flight; its lease lapses
\* later (Lapse).
Crash(d) ==
  /\ pc[d] \notin {"idle"} /\ crashes < MaxCrash
  /\ crashes' = crashes + 1
  /\ act' = [act EXCEPT ![d] = None]
  /\ Goto(d, "idle")
  /\ UNCHANGED <<marker, fenced, verdict, res, unk, grants, aged, lease, wire, reply, pfloor,
                 applied, fired, landedAt, tgt>>

\* A lease lapses (its TTL passed): its holder may be stalled, and keeps running.
Lapse ==
  /\ lease # None /\ lease' = None
  /\ UNCHANGED <<marker, fenced, verdict, res, unk, grants, aged, wire, reply, pfloor, applied,
                 fired, landedAt, pc, tgt, act, crashes>>

\* Time passes for an attempt: it is now older than MinAge.
Tick(x) ==
  /\ marker[x] # None /\ ~aged[x]
  /\ aged' = [aged EXCEPT ![x] = TRUE]
  /\ UNCHANGED <<marker, fenced, verdict, res, unk, grants, lease, wire, reply, pfloor, applied,
                 fired, landedAt, pc, tgt, act, crashes>>

\* A person grants fresh probes after the cap (agent.Reprobe, proposed).
Grant ==
  /\ grants < MaxGrant /\ unk >= Cap * (1 + grants)
  /\ grants' = grants + 1
  /\ UNCHANGED <<marker, fenced, verdict, res, unk, aged, lease, wire, reply, pfloor, applied,
                 fired, landedAt, pc, tgt, act, crashes>>

Next ==
  \/ \E d \in Drivers : Start(d) \/ Gate(d) \/ Claim(d) \/ Send(d) \/ Record(d) \/ Probe(d)
                         \/ ActClaim(d) \/ Act(d) \/ Finish(d) \/ Crash(d)
  \/ \E x \in Att : Land(x) \/ Tick(x)
  \/ Lapse \/ Grant

Spec == Init /\ [][Next]_vars

(***************************************************************************)
(* Properties                                                               *)
(***************************************************************************)

TypeOK ==
  /\ marker \in [Att -> Drivers \cup {None}]
  /\ verdict \in [Att -> {None, "happened", "not"}]
  /\ res \in [Att -> {None, "ok", "err"}]
  /\ wire \subseteq Claimed
  /\ pfloor \in 0..MaxAtt + 1
  /\ fired \in Nat
  /\ pc \in [Drivers -> {"idle", "gate", "claim", "send", "await", "probe", "actclaim", "act", "finish"}]

\* No double effect: the effect lands at most once.
AtMostOnce == fired <= 1

\* The call's recorded outcome is true: "ok" only if the effect landed, "err" only if it never did
\* (and, the outcome being final, never will).
OutcomeTrue ==
  /\ Outcome = "ok" => fired >= 1
  /\ Outcome = "err" => fired = 0

\* A recorded verdict is true: Happened only after a landing, and NotHappened only if no attempt
\* up to the probed one has landed (checked in every later state, so it never lands later).
VerdictTrue ==
  \A x \in Att :
    /\ verdict[x] = "happened" => fired >= 1
    /\ verdict[x] = "not" => \A y \in 0..x : ~landedAt[y]

\* Vacuity (check.sh): the effect is reachable.
EffectNotReachable == fired = 0

\* Reachability, expected violated: a probe voids an attempt and the call is then run once and
\* its outcome recorded (the path probes add).
RerunNotReachable == ~(\E x \in Att : verdict[x] = "not") \/ Outcome # "ok"

\* Reachability, expected violated: a probe records Happened as the call's outcome.
HappenedNotReachable == ~(\E x \in Att : verdict[x] = "happened") \/ Outcome # "ok"
=============================================================================
