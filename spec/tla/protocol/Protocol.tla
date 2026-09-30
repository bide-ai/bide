------------------------------- MODULE Protocol -------------------------------
(***************************************************************************)
(* Model 2: the bide protocol's claim rules for a remote side-effect tool   *)
(* call (docs/design/protocol.md, #95, revision 2, section 18).             *)
(*                                                                          *)
(* The engine claims the call when it assigns a delivery to a polling       *)
(* worker (I1: a remote marker naming the delivery and the worker). The     *)
(* transport may lose, duplicate (to a second worker presenting the same    *)
(* delivery id) or delay the task. A worker runs the handler only after     *)
(* BeginTask answered begun: the engine inserts the begin record            *)
(* attempt:begin:<marker> naming the delivery and the worker's nonce, and   *)
(* answers from the stored record (I2). The engine abandons a delivery that *)
(* lapsed or that this engine instance does not know by inserting an        *)
(* abandon under the same key, and writes the not-started record only when  *)
(* the stored begin record is an abandon (I3); the next attempt is claimed  *)
(* afresh, up to MaxAttempts, then DELIVERY_EXHAUSTED is recorded. A begun  *)
(* delivery reports its outcome (possibly late), or unknown, or is lost; a  *)
(* resolution derives the halt's cause from the journal.                    *)
(*                                                                          *)
(* Engine instances: a crash empties the dispatch table and the engine's    *)
(* in-flight steps; the next instance decides from the journal (I7).        *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS
  Workers,       \* worker processes (model values)
  MaxAttempts,   \* attempts (claims) per call before DELIVERY_EXHAUSTED
  MaxDeliveries, \* bound on delivery ids (and claim ids)
  MaxAmbig,      \* budget of error replies on journal inserts
  MaxLoss,       \* budget of lost messages (tasks, begin answers)
  MaxDup,        \* budget of duplicated tasks (to the same or a second worker)
  MaxEngineCrash,\* budget of engine crashes (new instances)
  MaxWorkerCrash,\* budget of worker crashes
  HasResolver,   \* whether an operator resolves halts
  Kind,          \* "side_effect" (claim, begin, abandon) or "retry_safe" (re-dispatch, 10.9)
  Bug            \* "none" or a rule section 18 requires to fail, see regress/

None == "none"
Gens == 0..(MaxAttempts - 1)
DIds == 1..MaxDeliveries
NoBegin == [set |-> FALSE, ab |-> FALSE, d |-> 0, n |-> <<None, 0>>, stamp |-> 0]
ResolverSet == IF HasResolver THEN {"resolver"} ELSE {}

ASSUME Bug \in {"none", "InsertedFlagWon", "DeliveryIdOnly", "NotStartedFromBegun", "ByteEqualWon",
                "AbandonWithoutKey", "CallerCause", "CheckBeforeRead", "LostIsExhausted",
                "NewScopePerDelivery"}
ASSUME Kind \in {"side_effect", "retry_safe"}

(* --algorithm protocol
variables
  \* The journal. marker[g]: the remote marker of attempt g: [set, cid, d, w].
  marker  = [g \in Gens |-> [set |-> FALSE, cid |-> 0, d |-> 0, w |-> None]],
  \* bgn[g]: attempt:begin:<marker>: a worker's begin (d, its nonce) or an abandon.
  bgn     = [g \in Gens |-> NoBegin],
  \* nsIds[g]: claim ids holding a not-started record for attempt g's marker key.
  nsIds   = [g \in Gens |-> {}],
  unknown = [g \in Gens |-> FALSE],          \* attempt:unknown:<claim>:<marker>
  result  = None,                            \* tool:<id>: "ok", "exhausted", "resolved"
  \* Engine memory (the dispatch table), lost on an engine crash: delivery -> "assigned" | "lapsed".
  table   = [d \in DIds |-> None],
  nextD   = 0,
  \* The transport: tasks [d, g, cid, w] in flight to worker w (lost, duplicated, delayed).
  net     = {},
  \* Begin requests the engine has received and not answered: [w, d, g, cid, n, stamp].
  breq    = {},
  \* Begin answers in flight to workers: w -> [d (the delivery asked about), a: "none" | "true" |
  \* "false" | "unavailable"].
  bans    = [w \in Workers |-> [d |-> 0, a |-> None]],
  \* Outcome reports in flight to the engine: [w, d, g, kind].
  reports = {},
  ambig = 0, loss = 0, dups = 0, ecrash = 0, wcrash = 0,
  \* Ghosts: effect calls; the claim ids each attempt fired under; the workers ever in Run per
  \* attempt; workers between Run and the end of their Complete; a begun worker refused.
  fired   = 0,
  firedAt = [g \in Gens |-> 0],
  runners = [g \in Gens |-> {}],
  liveRun = {},
  \* The delivery (and nonce) each worker holds, for the resolver's floor assumption.
  hold    = [w \in Workers |-> [d |-> 0, n |-> <<None, 0>>]],
  \* Retry-safe calls: the journaled unknown and not-started reports, the halt when the retry
  \* window closes with an unknown outcome, and a ghost: the once-key scopes whose downstream
  \* effect took place (a downstream deduplicates by once key, so one scope applies once).
  unknownN = 0, nsRep = 0, halted = FALSE,
  applied  = {},
  badBegin = FALSE;

define
  Voided(g) == marker[g].set /\ marker[g].cid \in nsIds[g]
  \* The attempt the next assignment claims: the first attempt with no marker, when every earlier
  \* one is voided.
  NextAtt == IF \E g \in Gens : ~marker[g].set /\ \A h \in Gens : h < g => Voided(h)
             THEN CHOOSE g \in Gens : ~marker[g].set /\ \A h \in Gens : h < g => Voided(h)
             ELSE -1
  Exhausted == \A g \in Gens : Voided(g)
  LiveDelivery(d) == table[d] = "assigned"
  \* A worker's begin answer, from the stored record (I2) or, historically, otherwise.
  BegunFor(g, d, n, stamp) ==
    CASE Bug = "DeliveryIdOnly" -> bgn[g].set /\ ~bgn[g].ab /\ bgn[g].d = d
      [] Bug = "ByteEqualWon"   -> bgn[g].set /\ ~bgn[g].ab /\ bgn[g].d = d /\ bgn[g].n = n
                                   /\ bgn[g].stamp = stamp
      [] OTHER                  -> bgn[g].set /\ ~bgn[g].ab /\ bgn[g].d = d /\ bgn[g].n = n
end define;

macro Reply(r) begin
  either r := "ok";
  or await ambig < MaxAmbig; ambig := ambig + 1; r := "err_nc";
  or await ambig < MaxAmbig; ambig := ambig + 1; r := "err_c";
  end either;
end macro;

\* The engine: each step is one store round trip or one decision, on the run's scheduler slot.
fair process engine = "engine"
variables er = "", eg = 0, ed = 0, ecid = 0;
begin
EIdle:
  either
    \* Retry-safe: dispatch (again) under the call's once-key scope while no delivery is live.
    await Kind = "retry_safe" /\ result = None /\ ~halted /\ nextD < MaxDeliveries;
    await \A x \in DIds : table[x] # "assigned";
    with nd = nextD + 1, w \in Workers do
      nextD := nd; table[nd] := "assigned";
      net := net \cup {[d |-> nd, g |-> 0, cid |-> 0, w |-> w]};
    end with;
    goto EIdle;
  or
    \* Retry-safe: the retry window closes (deliveries exhausted) with no outcome. A journaled
    \* unknown report halts (crashed). Revision 2 records DELIVERY_EXHAUSTED when the deliveries
    \* were only lost (LostIsExhausted); but a lost delivery, or a duplicate of one whose holder
    \* refused it, may have run its handler, and nothing journaled says otherwise, so the model's
    \* rule halts (finding P2).
    await Kind = "retry_safe" /\ result = None /\ ~halted /\ nextD >= MaxDeliveries;
    await \A x \in DIds : table[x] # "assigned";
    if unknownN > 0 \/ Bug # "LostIsExhausted" then
      halted := TRUE; goto EIdle;
    else
      goto EExhausted;
    end if;
  or
    \* Assign: a worker polls; the engine claims the next attempt at assignment (I1).
    await Kind = "side_effect" /\ result = None /\ nextD < MaxDeliveries /\ NextAtt # -1;
    with nd = nextD + 1 do
      eg := NextAtt; nextD := nd; ed := nd; ecid := nd;
    end with;
    goto EAssign;
  or
    \* A delivery's lease lapses (untimed: at any time).
    with d \in {x \in DIds : table[x] = "assigned"} do table[d] := "lapsed"; end with;
    goto EIdle;
  or
    \* Abandon: a remote marker with no begin record whose delivery lapsed or is unknown here.
    await result = None;
    with g \in {x \in Gens : marker[x].set /\ ~bgn[x].set /\ ~Voided(x)
                             /\ ~LiveDelivery(marker[x].d)} do eg := g; end with;
    goto EAbandon;
  or
    \* A stored abandon whose not-started record is missing (an instance crashed in between).
    await result = None;
    with g \in {x \in Gens : marker[x].set /\ bgn[x].set /\ bgn[x].ab /\ ~Voided(x)} do
      eg := g;
    end with;
    goto ENotStarted;
  or
    \* All attempts voided: DELIVERY_EXHAUSTED, a truthful error (no effect ran).
    await Kind = "side_effect" /\ result = None /\ Exhausted;
    goto EExhausted;
  end either;
EAssign:
  \* The marker names the delivery and the worker whose poll it serves.
  with w \in Workers do
    Reply(er);
    if er # "err_nc" /\ ~marker[eg].set then
      marker[eg] := [set |-> TRUE, cid |-> ecid, d |-> ed, w |-> w];
    end if;
    if er = "ok" then
      \* Won (one engine claims; the stored marker carries this claim id): the task leaves.
      table[ed] := "assigned";
      net := net \cup {[d |-> ed, g |-> eg, cid |-> ecid, w |-> w]};
      goto EIdle;
    end if;
  end with;
EClaimNS:
  \* The claim's Insert errored: record that this claim never started (its own not-started).
  Reply(er);
  if er # "err_nc" then nsIds[eg] := nsIds[eg] \cup {ecid}; end if;
  goto EIdle;
EAbandon:
  \* Insert the abandon under the begin key, then read the stored record.
  Reply(er);
  if er # "err_nc" /\ ~bgn[eg].set then
    bgn[eg] := [set |-> TRUE, ab |-> TRUE, d |-> 0, n |-> <<None, 0>>, stamp |-> 0];
  end if;
  if er # "ok" then goto EIdle; end if;
EAbandonRead:
  if (bgn[eg].set /\ bgn[eg].ab) \/ Bug = "AbandonWithoutKey" then
    goto ENotStarted;
  else
    goto EIdle;                               \* a stored begin: the delivery began
  end if;
ENotStarted:
  \* Enabled only by a stored abandon (I2), historically also without one.
  Reply(er);
  if er # "err_nc" then nsIds[eg] := nsIds[eg] \cup {marker[eg].cid}; end if;
  goto EIdle;
EExhausted:
  Reply(er);
  if er # "err_nc" /\ result = None then result := "exhausted"; end if;
  goto EIdle;
end process;

\* The engine's RPC handlers (BeginTask, CompleteTask, FailTask): they run beside the run's drive,
\* not on its scheduler slot, and share the engine instance's dispatch table.
fair process rpc = "rpc"
variables er2 = "", ereq = [w |-> None, d |-> 0, g |-> 0, cid |-> 0, n |-> <<None, 0>>, stamp |-> 0],
          erep = [w |-> None, d |-> 0, g |-> 0, kind |-> None];
begin
RIdle:
  either
    with q \in breq do ereq := q; breq := breq \ {q}; end with;
    goto EBeginCheck;
  or
    with p \in reports do erep := p; reports := reports \ {p}; end with;
    goto EReport;
  end either;
EBeginCheck:
  \* BeginTask, engine side. A begin the stored record already holds for this delivery and nonce is
  \* answered true (a retry after a lost answer, whatever the lease or the engine instance: finding
  \* P1); otherwise the delivery must be assigned and unlapsed here, and the claim id the stored
  \* marker's. Revision 2's text checks the dispatch table first (CheckBeforeRead).
  if Bug # "CheckBeforeRead" /\ BegunFor(ereq.g, ereq.d, ereq.n, ereq.stamp) then
    bans[ereq.w] := [d |-> ereq.d, a |-> "true"];
    goto RIdle;
  elsif table[ereq.d] # "assigned" \/ ~marker[ereq.g].set \/ marker[ereq.g].cid # ereq.cid then
    bans[ereq.w] := [d |-> ereq.d, a |-> "false"];
    goto RIdle;
  end if;
EBeginInsert:
  Reply(er2);
  if er2 # "err_nc" /\ ~bgn[ereq.g].set then
    bgn[ereq.g] := [set |-> TRUE, ab |-> FALSE, d |-> ereq.d, n |-> ereq.n, stamp |-> ereq.stamp];
  end if;
EBeginRead:
  \* The answer comes from the stored record: the Insert's returned entry, or a Get after an
  \* errored Insert, which may fail too (unavailable: the worker retries with the same nonce).
  if er2 # "ok" /\ ambig < MaxAmbig /\ Bug # "InsertedFlagWon" then
    either
      bans[ereq.w] := [d |-> ereq.d, a |-> IF BegunFor(ereq.g, ereq.d, ereq.n, ereq.stamp) THEN "true" ELSE "false"];
    or
      ambig := ambig + 1; bans[ereq.w] := [d |-> ereq.d, a |-> "unavailable"];
    end either;
  elsif Bug = "InsertedFlagWon" then
    \* Historically "won" from the insert's inserted flag, shared through one flight by every
    \* begin request for this delivery the engine held: all of them are answered from it.
    with others = {q \in breq : q.d = ereq.d},
         ans = IF er2 = "ok" /\ bgn[ereq.g].set /\ ~bgn[ereq.g].ab /\ bgn[ereq.g].d = ereq.d
               THEN "true" ELSE "false" do
      bans := [w \in Workers |-> IF w = ereq.w \/ \E q \in others : q.w = w
                                  THEN [d |-> ereq.d, a |-> ans] ELSE bans[w]];
      breq := breq \ others;
    end with;
  else
    bans[ereq.w] := [d |-> ereq.d, a |-> IF BegunFor(ereq.g, ereq.d, ereq.n, ereq.stamp) THEN "true" ELSE "false"];
  end if;
  goto RIdle;
EReport:
  \* An outcome report: only the begun delivery completes (NOT_BEGUN_BY_DELIVERY otherwise).
  if Kind = "retry_safe" then
    goto SReport;
  elsif erep.kind = "not_started" then
    \* not_started only before a begin: the engine abandons that delivery (6.3); from a delivery
    \* that has begun it is rejected, historically accepted (the attempt voided).
    if Bug = "NotStartedFromBegun" /\ marker[erep.g].set /\ bgn[erep.g].set
       /\ ~bgn[erep.g].ab /\ bgn[erep.g].d = erep.d /\ ~Voided(erep.g) then
      goto RNotStarted;
    elsif marker[erep.g].set /\ ~bgn[erep.g].set then
      table[erep.d] := "lapsed"; goto RIdle;
    else
      goto RIdle;
    end if;
  elsif ~(bgn[erep.g].set /\ ~bgn[erep.g].ab /\ bgn[erep.g].d = erep.d) then
    goto RIdle;
  elsif erep.kind = "unknown" then
    goto EUnknown;
  else
    goto EComplete;
  end if;
RNotStarted:
  Reply(er2);
  if er2 # "err_nc" then nsIds[erep.g] := nsIds[erep.g] \cup {marker[erep.g].cid}; end if;
  goto RIdle;
SReport:
  \* Retry-safe: any assigned delivery may complete (the first wins); an unknown report is
  \* journaled and the call is dispatched again under the same scope; not_started too.
  if erep.kind = "complete" then
    goto EComplete;
  elsif erep.kind = "unknown" then
    Reply(er2);
    if er2 # "err_nc" then unknownN := unknownN + 1; end if;
    table[erep.d] := "reported";
    goto RIdle;
  else
    Reply(er2);
    if er2 # "err_nc" then nsRep := nsRep + 1; end if;
    table[erep.d] := "reported";
    goto RIdle;
  end if;
EComplete:
  \* CompleteTask: the result, inserted if absent (a late completion after a resolution is a
  \* conflict and never overwrites).
  Reply(er2);
  if er2 # "err_nc" /\ result = None then result := "ok"; end if;
  goto RIdle;
EUnknown:
  Reply(er2);
  if er2 # "err_nc" then unknown[erep.g] := TRUE; end if;
  goto RIdle;
end process;

\* A worker SDK process: poll, begin with its own nonce (retried with the same nonce), run only
\* when begun, report exactly one outcome.
fair process worker \in Workers
variables wd = 0, wg = 0, wcid = 0, wn = <<None, 0>>, stamp = 0, wst = "idle";
begin
WIdle:
  with t \in {x \in net : x.w = self} do
    net := net \ {t};
    wd := t.d; wg := t.g; wcid := t.cid; wn := <<self, t.d>>; stamp := 0;
    hold[self] := [d |-> t.d, n |-> <<self, t.d>>];
  end with;
  wst := "got";
  if Kind = "retry_safe" then goto WSafe; end if;
WBegin:
  \* BeginTask with this delivery's nonce; a retry rebuilds its record (a new stamp).
  stamp := stamp + 1;
  breq := breq \cup {[w |-> self, d |-> wd, g |-> wg, cid |-> wcid, n |-> wn, stamp |-> stamp]};
  bans[self] := [d |-> 0, a |-> None];
WWait:
  await bans[self].d = wd /\ bans[self].a # None;
  if bans[self].a = "true" then
    goto WRun;
  elsif bans[self].a = "unavailable" /\ stamp < 2 then
    goto WBegin;                              \* retry with the same nonce
  else
    \* Refused (or given up without a definitive answer): never run the handler. A live worker
    \* whose nonce the stored begin record holds must never be refused.
    if bgn[wg].set /\ ~bgn[wg].ab /\ bgn[wg].d = wd /\ bgn[wg].n = wn /\ bans[self].a = "false" then
      badBegin := TRUE;
    end if;
    if bans[self].a = "false" /\ ~(bgn[wg].set /\ bgn[wg].n = wn) then
      reports := reports \cup {[w |-> self, d |-> wd, g |-> wg, kind |-> "not_started"]};
    end if;
    wst := "idle"; hold[self] := [d |-> 0, n |-> <<None, 0>>]; goto WIdle;
  end if;
WSafe:
  \* A retry-safe task (pull): no begin. The worker refuses it (not_started), or runs the handler,
  \* whose downstream effect is deduplicated by the once key: every delivery of the call shares
  \* its scope (historically, a new scope per delivery: NewScopePerDelivery).
  either
    reports := reports \cup {[w |-> self, d |-> wd, g |-> 0, kind |-> "not_started"]};
    goto WReported;
  or
    applied := applied \cup {IF Bug = "NewScopePerDelivery" THEN wd ELSE 0};
    liveRun := liveRun \cup {self};
    goto WReport;
  end either;
WRun:
  \* The handler: the effect fires under this attempt's claim.
  fired := fired + 1;
  firedAt[wg] := wcid;
  runners[wg] := runners[wg] \cup {self};
  liveRun := liveRun \cup {self};
  wst := "ran";
WReport:
  \* Exactly one outcome: complete, or unknown (the handler cannot tell).
  either
    reports := reports \cup {[w |-> self, d |-> wd, g |-> wg, kind |-> "complete"]};
    liveRun := liveRun \ {self};
  or
    \* The handler returned and cannot tell whether its effect took place.
    reports := reports \cup {[w |-> self, d |-> wd, g |-> wg, kind |-> "unknown"]};
    liveRun := liveRun \ {self};
  end either;
WReported:
  \* The worker leaves Run once the engine has taken its report.
  await ~\E p \in reports : p.w = self /\ p.d = wd;
  wst := "idle"; hold[self] := [d |-> 0, n |-> <<None, 0>>]; goto WIdle;
end process;

\* ResolveHalt over the protocol: the cause and the floor come from the journal. A begun attempt
\* with no result and no unknown report is worker_lost, resolvable only after the lost-worker
\* floor, encoded as its assumption: the worker holding the begun delivery is done with it (it
\* ran and reported, or crashed) before the floor passes.
fair process resolver \in ResolverSet
begin
RPick:-
  await result = None;
  with g \in {x \in Gens : bgn[x].set /\ ~bgn[x].ab /\ ~Voided(x)} do
    await \/ unknown[g]
          \/ \A w \in Workers : ~(hold[w].d = bgn[g].d /\ hold[w].n = bgn[g].n)
          \/ Bug = "CallerCause";            \* historically the request's cause: "crashed"
  end with;
RWrite:
  if result = None then result := "resolved"; end if;
end process;
end algorithm; *)
\* BEGIN TRANSLATION
VARIABLES marker, bgn, nsIds, unknown, result, table, nextD, net, breq, bans, 
          reports, ambig, loss, dups, ecrash, wcrash, fired, firedAt, runners, 
          liveRun, hold, unknownN, nsRep, halted, applied, badBegin, pc

(* define statement *)
Voided(g) == marker[g].set /\ marker[g].cid \in nsIds[g]


NextAtt == IF \E g \in Gens : ~marker[g].set /\ \A h \in Gens : h < g => Voided(h)
           THEN CHOOSE g \in Gens : ~marker[g].set /\ \A h \in Gens : h < g => Voided(h)
           ELSE -1
Exhausted == \A g \in Gens : Voided(g)
LiveDelivery(d) == table[d] = "assigned"

BegunFor(g, d, n, stamp) ==
  CASE Bug = "DeliveryIdOnly" -> bgn[g].set /\ ~bgn[g].ab /\ bgn[g].d = d
    [] Bug = "ByteEqualWon"   -> bgn[g].set /\ ~bgn[g].ab /\ bgn[g].d = d /\ bgn[g].n = n
                                 /\ bgn[g].stamp = stamp
    [] OTHER                  -> bgn[g].set /\ ~bgn[g].ab /\ bgn[g].d = d /\ bgn[g].n = n

VARIABLES er, eg, ed, ecid, er2, ereq, erep, wd, wg, wcid, wn, stamp, wst

vars == << marker, bgn, nsIds, unknown, result, table, nextD, net, breq, bans, 
           reports, ambig, loss, dups, ecrash, wcrash, fired, firedAt, 
           runners, liveRun, hold, unknownN, nsRep, halted, applied, badBegin, 
           pc, er, eg, ed, ecid, er2, ereq, erep, wd, wg, wcid, wn, stamp, 
           wst >>

ProcSet == {"engine"} \cup {"rpc"} \cup (Workers) \cup (ResolverSet)

Init == (* Global variables *)
        /\ marker = [g \in Gens |-> [set |-> FALSE, cid |-> 0, d |-> 0, w |-> None]]
        /\ bgn = [g \in Gens |-> NoBegin]
        /\ nsIds = [g \in Gens |-> {}]
        /\ unknown = [g \in Gens |-> FALSE]
        /\ result = None
        /\ table = [d \in DIds |-> None]
        /\ nextD = 0
        /\ net = {}
        /\ breq = {}
        /\ bans = [w \in Workers |-> [d |-> 0, a |-> None]]
        /\ reports = {}
        /\ ambig = 0
        /\ loss = 0
        /\ dups = 0
        /\ ecrash = 0
        /\ wcrash = 0
        /\ fired = 0
        /\ firedAt = [g \in Gens |-> 0]
        /\ runners = [g \in Gens |-> {}]
        /\ liveRun = {}
        /\ hold = [w \in Workers |-> [d |-> 0, n |-> <<None, 0>>]]
        /\ unknownN = 0
        /\ nsRep = 0
        /\ halted = FALSE
        /\ applied = {}
        /\ badBegin = FALSE
        (* Process engine *)
        /\ er = ""
        /\ eg = 0
        /\ ed = 0
        /\ ecid = 0
        (* Process rpc *)
        /\ er2 = ""
        /\ ereq = [w |-> None, d |-> 0, g |-> 0, cid |-> 0, n |-> <<None, 0>>, stamp |-> 0]
        /\ erep = [w |-> None, d |-> 0, g |-> 0, kind |-> None]
        (* Process worker *)
        /\ wd = [self \in Workers |-> 0]
        /\ wg = [self \in Workers |-> 0]
        /\ wcid = [self \in Workers |-> 0]
        /\ wn = [self \in Workers |-> <<None, 0>>]
        /\ stamp = [self \in Workers |-> 0]
        /\ wst = [self \in Workers |-> "idle"]
        /\ pc = [self \in ProcSet |-> CASE self = "engine" -> "EIdle"
                                        [] self = "rpc" -> "RIdle"
                                        [] self \in Workers -> "WIdle"
                                        [] self \in ResolverSet -> "RPick"]

EIdle == /\ pc["engine"] = "EIdle"
         /\ \/ /\ Kind = "retry_safe" /\ result = None /\ ~halted /\ nextD < MaxDeliveries
               /\ \A x \in DIds : table[x] # "assigned"
               /\ LET nd == nextD + 1 IN
                    \E w \in Workers:
                      /\ nextD' = nd
                      /\ table' = [table EXCEPT ![nd] = "assigned"]
                      /\ net' = (net \cup {[d |-> nd, g |-> 0, cid |-> 0, w |-> w]})
               /\ pc' = [pc EXCEPT !["engine"] = "EIdle"]
               /\ UNCHANGED <<halted, eg, ed, ecid>>
            \/ /\ Kind = "retry_safe" /\ result = None /\ ~halted /\ nextD >= MaxDeliveries
               /\ \A x \in DIds : table[x] # "assigned"
               /\ IF unknownN > 0 \/ Bug # "LostIsExhausted"
                     THEN /\ halted' = TRUE
                          /\ pc' = [pc EXCEPT !["engine"] = "EIdle"]
                     ELSE /\ pc' = [pc EXCEPT !["engine"] = "EExhausted"]
                          /\ UNCHANGED halted
               /\ UNCHANGED <<table, nextD, net, eg, ed, ecid>>
            \/ /\ Kind = "side_effect" /\ result = None /\ nextD < MaxDeliveries /\ NextAtt # -1
               /\ LET nd == nextD + 1 IN
                    /\ eg' = NextAtt
                    /\ nextD' = nd
                    /\ ed' = nd
                    /\ ecid' = nd
               /\ pc' = [pc EXCEPT !["engine"] = "EAssign"]
               /\ UNCHANGED <<table, net, halted>>
            \/ /\ \E d \in {x \in DIds : table[x] = "assigned"}:
                    table' = [table EXCEPT ![d] = "lapsed"]
               /\ pc' = [pc EXCEPT !["engine"] = "EIdle"]
               /\ UNCHANGED <<nextD, net, halted, eg, ed, ecid>>
            \/ /\ result = None
               /\ \E g \in {x \in Gens : marker[x].set /\ ~bgn[x].set /\ ~Voided(x)
                                         /\ ~LiveDelivery(marker[x].d)}:
                    eg' = g
               /\ pc' = [pc EXCEPT !["engine"] = "EAbandon"]
               /\ UNCHANGED <<table, nextD, net, halted, ed, ecid>>
            \/ /\ result = None
               /\ \E g \in {x \in Gens : marker[x].set /\ bgn[x].set /\ bgn[x].ab /\ ~Voided(x)}:
                    eg' = g
               /\ pc' = [pc EXCEPT !["engine"] = "ENotStarted"]
               /\ UNCHANGED <<table, nextD, net, halted, ed, ecid>>
            \/ /\ Kind = "side_effect" /\ result = None /\ Exhausted
               /\ pc' = [pc EXCEPT !["engine"] = "EExhausted"]
               /\ UNCHANGED <<table, nextD, net, halted, eg, ed, ecid>>
         /\ UNCHANGED << marker, bgn, nsIds, unknown, result, breq, bans, 
                         reports, ambig, loss, dups, ecrash, wcrash, fired, 
                         firedAt, runners, liveRun, hold, unknownN, nsRep, 
                         applied, badBegin, er, er2, ereq, erep, wd, wg, wcid, 
                         wn, stamp, wst >>

EAssign == /\ pc["engine"] = "EAssign"
           /\ \E w \in Workers:
                /\ \/ /\ er' = "ok"
                      /\ ambig' = ambig
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ er' = "err_nc"
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ er' = "err_c"
                /\ IF er' # "err_nc" /\ ~marker[eg].set
                      THEN /\ marker' = [marker EXCEPT ![eg] = [set |-> TRUE, cid |-> ecid, d |-> ed, w |-> w]]
                      ELSE /\ TRUE
                           /\ UNCHANGED marker
                /\ IF er' = "ok"
                      THEN /\ table' = [table EXCEPT ![ed] = "assigned"]
                           /\ net' = (net \cup {[d |-> ed, g |-> eg, cid |-> ecid, w |-> w]})
                           /\ pc' = [pc EXCEPT !["engine"] = "EIdle"]
                      ELSE /\ pc' = [pc EXCEPT !["engine"] = "EClaimNS"]
                           /\ UNCHANGED << table, net >>
           /\ UNCHANGED << bgn, nsIds, unknown, result, nextD, breq, bans, 
                           reports, loss, dups, ecrash, wcrash, fired, firedAt, 
                           runners, liveRun, hold, unknownN, nsRep, halted, 
                           applied, badBegin, eg, ed, ecid, er2, ereq, erep, 
                           wd, wg, wcid, wn, stamp, wst >>

EClaimNS == /\ pc["engine"] = "EClaimNS"
            /\ \/ /\ er' = "ok"
                  /\ ambig' = ambig
               \/ /\ ambig < MaxAmbig
                  /\ ambig' = ambig + 1
                  /\ er' = "err_nc"
               \/ /\ ambig < MaxAmbig
                  /\ ambig' = ambig + 1
                  /\ er' = "err_c"
            /\ IF er' # "err_nc"
                  THEN /\ nsIds' = [nsIds EXCEPT ![eg] = nsIds[eg] \cup {ecid}]
                  ELSE /\ TRUE
                       /\ nsIds' = nsIds
            /\ pc' = [pc EXCEPT !["engine"] = "EIdle"]
            /\ UNCHANGED << marker, bgn, unknown, result, table, nextD, net, 
                            breq, bans, reports, loss, dups, ecrash, wcrash, 
                            fired, firedAt, runners, liveRun, hold, unknownN, 
                            nsRep, halted, applied, badBegin, eg, ed, ecid, 
                            er2, ereq, erep, wd, wg, wcid, wn, stamp, wst >>

EAbandon == /\ pc["engine"] = "EAbandon"
            /\ \/ /\ er' = "ok"
                  /\ ambig' = ambig
               \/ /\ ambig < MaxAmbig
                  /\ ambig' = ambig + 1
                  /\ er' = "err_nc"
               \/ /\ ambig < MaxAmbig
                  /\ ambig' = ambig + 1
                  /\ er' = "err_c"
            /\ IF er' # "err_nc" /\ ~bgn[eg].set
                  THEN /\ bgn' = [bgn EXCEPT ![eg] = [set |-> TRUE, ab |-> TRUE, d |-> 0, n |-> <<None, 0>>, stamp |-> 0]]
                  ELSE /\ TRUE
                       /\ bgn' = bgn
            /\ IF er' # "ok"
                  THEN /\ pc' = [pc EXCEPT !["engine"] = "EIdle"]
                  ELSE /\ pc' = [pc EXCEPT !["engine"] = "EAbandonRead"]
            /\ UNCHANGED << marker, nsIds, unknown, result, table, nextD, net, 
                            breq, bans, reports, loss, dups, ecrash, wcrash, 
                            fired, firedAt, runners, liveRun, hold, unknownN, 
                            nsRep, halted, applied, badBegin, eg, ed, ecid, 
                            er2, ereq, erep, wd, wg, wcid, wn, stamp, wst >>

EAbandonRead == /\ pc["engine"] = "EAbandonRead"
                /\ IF (bgn[eg].set /\ bgn[eg].ab) \/ Bug = "AbandonWithoutKey"
                      THEN /\ pc' = [pc EXCEPT !["engine"] = "ENotStarted"]
                      ELSE /\ pc' = [pc EXCEPT !["engine"] = "EIdle"]
                /\ UNCHANGED << marker, bgn, nsIds, unknown, result, table, 
                                nextD, net, breq, bans, reports, ambig, loss, 
                                dups, ecrash, wcrash, fired, firedAt, runners, 
                                liveRun, hold, unknownN, nsRep, halted, 
                                applied, badBegin, er, eg, ed, ecid, er2, ereq, 
                                erep, wd, wg, wcid, wn, stamp, wst >>

ENotStarted == /\ pc["engine"] = "ENotStarted"
               /\ \/ /\ er' = "ok"
                     /\ ambig' = ambig
                  \/ /\ ambig < MaxAmbig
                     /\ ambig' = ambig + 1
                     /\ er' = "err_nc"
                  \/ /\ ambig < MaxAmbig
                     /\ ambig' = ambig + 1
                     /\ er' = "err_c"
               /\ IF er' # "err_nc"
                     THEN /\ nsIds' = [nsIds EXCEPT ![eg] = nsIds[eg] \cup {marker[eg].cid}]
                     ELSE /\ TRUE
                          /\ nsIds' = nsIds
               /\ pc' = [pc EXCEPT !["engine"] = "EIdle"]
               /\ UNCHANGED << marker, bgn, unknown, result, table, nextD, net, 
                               breq, bans, reports, loss, dups, ecrash, wcrash, 
                               fired, firedAt, runners, liveRun, hold, 
                               unknownN, nsRep, halted, applied, badBegin, eg, 
                               ed, ecid, er2, ereq, erep, wd, wg, wcid, wn, 
                               stamp, wst >>

EExhausted == /\ pc["engine"] = "EExhausted"
              /\ \/ /\ er' = "ok"
                    /\ ambig' = ambig
                 \/ /\ ambig < MaxAmbig
                    /\ ambig' = ambig + 1
                    /\ er' = "err_nc"
                 \/ /\ ambig < MaxAmbig
                    /\ ambig' = ambig + 1
                    /\ er' = "err_c"
              /\ IF er' # "err_nc" /\ result = None
                    THEN /\ result' = "exhausted"
                    ELSE /\ TRUE
                         /\ UNCHANGED result
              /\ pc' = [pc EXCEPT !["engine"] = "EIdle"]
              /\ UNCHANGED << marker, bgn, nsIds, unknown, table, nextD, net, 
                              breq, bans, reports, loss, dups, ecrash, wcrash, 
                              fired, firedAt, runners, liveRun, hold, unknownN, 
                              nsRep, halted, applied, badBegin, eg, ed, ecid, 
                              er2, ereq, erep, wd, wg, wcid, wn, stamp, wst >>

engine == EIdle \/ EAssign \/ EClaimNS \/ EAbandon \/ EAbandonRead
             \/ ENotStarted \/ EExhausted

RIdle == /\ pc["rpc"] = "RIdle"
         /\ \/ /\ \E q \in breq:
                    /\ ereq' = q
                    /\ breq' = breq \ {q}
               /\ pc' = [pc EXCEPT !["rpc"] = "EBeginCheck"]
               /\ UNCHANGED <<reports, erep>>
            \/ /\ \E p \in reports:
                    /\ erep' = p
                    /\ reports' = reports \ {p}
               /\ pc' = [pc EXCEPT !["rpc"] = "EReport"]
               /\ UNCHANGED <<breq, ereq>>
         /\ UNCHANGED << marker, bgn, nsIds, unknown, result, table, nextD, 
                         net, bans, ambig, loss, dups, ecrash, wcrash, fired, 
                         firedAt, runners, liveRun, hold, unknownN, nsRep, 
                         halted, applied, badBegin, er, eg, ed, ecid, er2, wd, 
                         wg, wcid, wn, stamp, wst >>

EBeginCheck == /\ pc["rpc"] = "EBeginCheck"
               /\ IF Bug # "CheckBeforeRead" /\ BegunFor(ereq.g, ereq.d, ereq.n, ereq.stamp)
                     THEN /\ bans' = [bans EXCEPT ![ereq.w] = [d |-> ereq.d, a |-> "true"]]
                          /\ pc' = [pc EXCEPT !["rpc"] = "RIdle"]
                     ELSE /\ IF table[ereq.d] # "assigned" \/ ~marker[ereq.g].set \/ marker[ereq.g].cid # ereq.cid
                                THEN /\ bans' = [bans EXCEPT ![ereq.w] = [d |-> ereq.d, a |-> "false"]]
                                     /\ pc' = [pc EXCEPT !["rpc"] = "RIdle"]
                                ELSE /\ pc' = [pc EXCEPT !["rpc"] = "EBeginInsert"]
                                     /\ bans' = bans
               /\ UNCHANGED << marker, bgn, nsIds, unknown, result, table, 
                               nextD, net, breq, reports, ambig, loss, dups, 
                               ecrash, wcrash, fired, firedAt, runners, 
                               liveRun, hold, unknownN, nsRep, halted, applied, 
                               badBegin, er, eg, ed, ecid, er2, ereq, erep, wd, 
                               wg, wcid, wn, stamp, wst >>

EBeginInsert == /\ pc["rpc"] = "EBeginInsert"
                /\ \/ /\ er2' = "ok"
                      /\ ambig' = ambig
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ er2' = "err_nc"
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ er2' = "err_c"
                /\ IF er2' # "err_nc" /\ ~bgn[ereq.g].set
                      THEN /\ bgn' = [bgn EXCEPT ![ereq.g] = [set |-> TRUE, ab |-> FALSE, d |-> ereq.d, n |-> ereq.n, stamp |-> ereq.stamp]]
                      ELSE /\ TRUE
                           /\ bgn' = bgn
                /\ pc' = [pc EXCEPT !["rpc"] = "EBeginRead"]
                /\ UNCHANGED << marker, nsIds, unknown, result, table, nextD, 
                                net, breq, bans, reports, loss, dups, ecrash, 
                                wcrash, fired, firedAt, runners, liveRun, hold, 
                                unknownN, nsRep, halted, applied, badBegin, er, 
                                eg, ed, ecid, ereq, erep, wd, wg, wcid, wn, 
                                stamp, wst >>

EBeginRead == /\ pc["rpc"] = "EBeginRead"
              /\ IF er2 # "ok" /\ ambig < MaxAmbig /\ Bug # "InsertedFlagWon"
                    THEN /\ \/ /\ bans' = [bans EXCEPT ![ereq.w] = [d |-> ereq.d, a |-> IF BegunFor(ereq.g, ereq.d, ereq.n, ereq.stamp) THEN "true" ELSE "false"]]
                               /\ ambig' = ambig
                            \/ /\ ambig' = ambig + 1
                               /\ bans' = [bans EXCEPT ![ereq.w] = [d |-> ereq.d, a |-> "unavailable"]]
                         /\ breq' = breq
                    ELSE /\ IF Bug = "InsertedFlagWon"
                               THEN /\ LET others == {q \in breq : q.d = ereq.d} IN
                                         LET ans == IF er2 = "ok" /\ bgn[ereq.g].set /\ ~bgn[ereq.g].ab /\ bgn[ereq.g].d = ereq.d
                                                    THEN "true" ELSE "false" IN
                                           /\ bans' = [w \in Workers |-> IF w = ereq.w \/ \E q \in others : q.w = w
                                                                          THEN [d |-> ereq.d, a |-> ans] ELSE bans[w]]
                                           /\ breq' = breq \ others
                               ELSE /\ bans' = [bans EXCEPT ![ereq.w] = [d |-> ereq.d, a |-> IF BegunFor(ereq.g, ereq.d, ereq.n, ereq.stamp) THEN "true" ELSE "false"]]
                                    /\ breq' = breq
                         /\ ambig' = ambig
              /\ pc' = [pc EXCEPT !["rpc"] = "RIdle"]
              /\ UNCHANGED << marker, bgn, nsIds, unknown, result, table, 
                              nextD, net, reports, loss, dups, ecrash, wcrash, 
                              fired, firedAt, runners, liveRun, hold, unknownN, 
                              nsRep, halted, applied, badBegin, er, eg, ed, 
                              ecid, er2, ereq, erep, wd, wg, wcid, wn, stamp, 
                              wst >>

EReport == /\ pc["rpc"] = "EReport"
           /\ IF Kind = "retry_safe"
                 THEN /\ pc' = [pc EXCEPT !["rpc"] = "SReport"]
                      /\ table' = table
                 ELSE /\ IF erep.kind = "not_started"
                            THEN /\ IF Bug = "NotStartedFromBegun" /\ marker[erep.g].set /\ bgn[erep.g].set
                                       /\ ~bgn[erep.g].ab /\ bgn[erep.g].d = erep.d /\ ~Voided(erep.g)
                                       THEN /\ pc' = [pc EXCEPT !["rpc"] = "RNotStarted"]
                                            /\ table' = table
                                       ELSE /\ IF marker[erep.g].set /\ ~bgn[erep.g].set
                                                  THEN /\ table' = [table EXCEPT ![erep.d] = "lapsed"]
                                                       /\ pc' = [pc EXCEPT !["rpc"] = "RIdle"]
                                                  ELSE /\ pc' = [pc EXCEPT !["rpc"] = "RIdle"]
                                                       /\ table' = table
                            ELSE /\ IF ~(bgn[erep.g].set /\ ~bgn[erep.g].ab /\ bgn[erep.g].d = erep.d)
                                       THEN /\ pc' = [pc EXCEPT !["rpc"] = "RIdle"]
                                       ELSE /\ IF erep.kind = "unknown"
                                                  THEN /\ pc' = [pc EXCEPT !["rpc"] = "EUnknown"]
                                                  ELSE /\ pc' = [pc EXCEPT !["rpc"] = "EComplete"]
                                 /\ table' = table
           /\ UNCHANGED << marker, bgn, nsIds, unknown, result, nextD, net, 
                           breq, bans, reports, ambig, loss, dups, ecrash, 
                           wcrash, fired, firedAt, runners, liveRun, hold, 
                           unknownN, nsRep, halted, applied, badBegin, er, eg, 
                           ed, ecid, er2, ereq, erep, wd, wg, wcid, wn, stamp, 
                           wst >>

RNotStarted == /\ pc["rpc"] = "RNotStarted"
               /\ \/ /\ er2' = "ok"
                     /\ ambig' = ambig
                  \/ /\ ambig < MaxAmbig
                     /\ ambig' = ambig + 1
                     /\ er2' = "err_nc"
                  \/ /\ ambig < MaxAmbig
                     /\ ambig' = ambig + 1
                     /\ er2' = "err_c"
               /\ IF er2' # "err_nc"
                     THEN /\ nsIds' = [nsIds EXCEPT ![erep.g] = nsIds[erep.g] \cup {marker[erep.g].cid}]
                     ELSE /\ TRUE
                          /\ nsIds' = nsIds
               /\ pc' = [pc EXCEPT !["rpc"] = "RIdle"]
               /\ UNCHANGED << marker, bgn, unknown, result, table, nextD, net, 
                               breq, bans, reports, loss, dups, ecrash, wcrash, 
                               fired, firedAt, runners, liveRun, hold, 
                               unknownN, nsRep, halted, applied, badBegin, er, 
                               eg, ed, ecid, ereq, erep, wd, wg, wcid, wn, 
                               stamp, wst >>

SReport == /\ pc["rpc"] = "SReport"
           /\ IF erep.kind = "complete"
                 THEN /\ pc' = [pc EXCEPT !["rpc"] = "EComplete"]
                      /\ UNCHANGED << table, ambig, unknownN, nsRep, er2 >>
                 ELSE /\ IF erep.kind = "unknown"
                            THEN /\ \/ /\ er2' = "ok"
                                       /\ ambig' = ambig
                                    \/ /\ ambig < MaxAmbig
                                       /\ ambig' = ambig + 1
                                       /\ er2' = "err_nc"
                                    \/ /\ ambig < MaxAmbig
                                       /\ ambig' = ambig + 1
                                       /\ er2' = "err_c"
                                 /\ IF er2' # "err_nc"
                                       THEN /\ unknownN' = unknownN + 1
                                       ELSE /\ TRUE
                                            /\ UNCHANGED unknownN
                                 /\ table' = [table EXCEPT ![erep.d] = "reported"]
                                 /\ pc' = [pc EXCEPT !["rpc"] = "RIdle"]
                                 /\ nsRep' = nsRep
                            ELSE /\ \/ /\ er2' = "ok"
                                       /\ ambig' = ambig
                                    \/ /\ ambig < MaxAmbig
                                       /\ ambig' = ambig + 1
                                       /\ er2' = "err_nc"
                                    \/ /\ ambig < MaxAmbig
                                       /\ ambig' = ambig + 1
                                       /\ er2' = "err_c"
                                 /\ IF er2' # "err_nc"
                                       THEN /\ nsRep' = nsRep + 1
                                       ELSE /\ TRUE
                                            /\ nsRep' = nsRep
                                 /\ table' = [table EXCEPT ![erep.d] = "reported"]
                                 /\ pc' = [pc EXCEPT !["rpc"] = "RIdle"]
                                 /\ UNCHANGED unknownN
           /\ UNCHANGED << marker, bgn, nsIds, unknown, result, nextD, net, 
                           breq, bans, reports, loss, dups, ecrash, wcrash, 
                           fired, firedAt, runners, liveRun, hold, halted, 
                           applied, badBegin, er, eg, ed, ecid, ereq, erep, wd, 
                           wg, wcid, wn, stamp, wst >>

EComplete == /\ pc["rpc"] = "EComplete"
             /\ \/ /\ er2' = "ok"
                   /\ ambig' = ambig
                \/ /\ ambig < MaxAmbig
                   /\ ambig' = ambig + 1
                   /\ er2' = "err_nc"
                \/ /\ ambig < MaxAmbig
                   /\ ambig' = ambig + 1
                   /\ er2' = "err_c"
             /\ IF er2' # "err_nc" /\ result = None
                   THEN /\ result' = "ok"
                   ELSE /\ TRUE
                        /\ UNCHANGED result
             /\ pc' = [pc EXCEPT !["rpc"] = "RIdle"]
             /\ UNCHANGED << marker, bgn, nsIds, unknown, table, nextD, net, 
                             breq, bans, reports, loss, dups, ecrash, wcrash, 
                             fired, firedAt, runners, liveRun, hold, unknownN, 
                             nsRep, halted, applied, badBegin, er, eg, ed, 
                             ecid, ereq, erep, wd, wg, wcid, wn, stamp, wst >>

EUnknown == /\ pc["rpc"] = "EUnknown"
            /\ \/ /\ er2' = "ok"
                  /\ ambig' = ambig
               \/ /\ ambig < MaxAmbig
                  /\ ambig' = ambig + 1
                  /\ er2' = "err_nc"
               \/ /\ ambig < MaxAmbig
                  /\ ambig' = ambig + 1
                  /\ er2' = "err_c"
            /\ IF er2' # "err_nc"
                  THEN /\ unknown' = [unknown EXCEPT ![erep.g] = TRUE]
                  ELSE /\ TRUE
                       /\ UNCHANGED unknown
            /\ pc' = [pc EXCEPT !["rpc"] = "RIdle"]
            /\ UNCHANGED << marker, bgn, nsIds, result, table, nextD, net, 
                            breq, bans, reports, loss, dups, ecrash, wcrash, 
                            fired, firedAt, runners, liveRun, hold, unknownN, 
                            nsRep, halted, applied, badBegin, er, eg, ed, ecid, 
                            ereq, erep, wd, wg, wcid, wn, stamp, wst >>

rpc == RIdle \/ EBeginCheck \/ EBeginInsert \/ EBeginRead \/ EReport
          \/ RNotStarted \/ SReport \/ EComplete \/ EUnknown

WIdle(self) == /\ pc[self] = "WIdle"
               /\ \E t \in {x \in net : x.w = self}:
                    /\ net' = net \ {t}
                    /\ wd' = [wd EXCEPT ![self] = t.d]
                    /\ wg' = [wg EXCEPT ![self] = t.g]
                    /\ wcid' = [wcid EXCEPT ![self] = t.cid]
                    /\ wn' = [wn EXCEPT ![self] = <<self, t.d>>]
                    /\ stamp' = [stamp EXCEPT ![self] = 0]
                    /\ hold' = [hold EXCEPT ![self] = [d |-> t.d, n |-> <<self, t.d>>]]
               /\ wst' = [wst EXCEPT ![self] = "got"]
               /\ IF Kind = "retry_safe"
                     THEN /\ pc' = [pc EXCEPT ![self] = "WSafe"]
                     ELSE /\ pc' = [pc EXCEPT ![self] = "WBegin"]
               /\ UNCHANGED << marker, bgn, nsIds, unknown, result, table, 
                               nextD, breq, bans, reports, ambig, loss, dups, 
                               ecrash, wcrash, fired, firedAt, runners, 
                               liveRun, unknownN, nsRep, halted, applied, 
                               badBegin, er, eg, ed, ecid, er2, ereq, erep >>

WBegin(self) == /\ pc[self] = "WBegin"
                /\ stamp' = [stamp EXCEPT ![self] = stamp[self] + 1]
                /\ breq' = (breq \cup {[w |-> self, d |-> wd[self], g |-> wg[self], cid |-> wcid[self], n |-> wn[self], stamp |-> stamp'[self]]})
                /\ bans' = [bans EXCEPT ![self] = [d |-> 0, a |-> None]]
                /\ pc' = [pc EXCEPT ![self] = "WWait"]
                /\ UNCHANGED << marker, bgn, nsIds, unknown, result, table, 
                                nextD, net, reports, ambig, loss, dups, ecrash, 
                                wcrash, fired, firedAt, runners, liveRun, hold, 
                                unknownN, nsRep, halted, applied, badBegin, er, 
                                eg, ed, ecid, er2, ereq, erep, wd, wg, wcid, 
                                wn, wst >>

WWait(self) == /\ pc[self] = "WWait"
               /\ bans[self].d = wd[self] /\ bans[self].a # None
               /\ IF bans[self].a = "true"
                     THEN /\ pc' = [pc EXCEPT ![self] = "WRun"]
                          /\ UNCHANGED << reports, hold, badBegin, wst >>
                     ELSE /\ IF bans[self].a = "unavailable" /\ stamp[self] < 2
                                THEN /\ pc' = [pc EXCEPT ![self] = "WBegin"]
                                     /\ UNCHANGED << reports, hold, badBegin, 
                                                     wst >>
                                ELSE /\ IF bgn[wg[self]].set /\ ~bgn[wg[self]].ab /\ bgn[wg[self]].d = wd[self] /\ bgn[wg[self]].n = wn[self] /\ bans[self].a = "false"
                                           THEN /\ badBegin' = TRUE
                                           ELSE /\ TRUE
                                                /\ UNCHANGED badBegin
                                     /\ IF bans[self].a = "false" /\ ~(bgn[wg[self]].set /\ bgn[wg[self]].n = wn[self])
                                           THEN /\ reports' = (reports \cup {[w |-> self, d |-> wd[self], g |-> wg[self], kind |-> "not_started"]})
                                           ELSE /\ TRUE
                                                /\ UNCHANGED reports
                                     /\ wst' = [wst EXCEPT ![self] = "idle"]
                                     /\ hold' = [hold EXCEPT ![self] = [d |-> 0, n |-> <<None, 0>>]]
                                     /\ pc' = [pc EXCEPT ![self] = "WIdle"]
               /\ UNCHANGED << marker, bgn, nsIds, unknown, result, table, 
                               nextD, net, breq, bans, ambig, loss, dups, 
                               ecrash, wcrash, fired, firedAt, runners, 
                               liveRun, unknownN, nsRep, halted, applied, er, 
                               eg, ed, ecid, er2, ereq, erep, wd, wg, wcid, wn, 
                               stamp >>

WSafe(self) == /\ pc[self] = "WSafe"
               /\ \/ /\ reports' = (reports \cup {[w |-> self, d |-> wd[self], g |-> 0, kind |-> "not_started"]})
                     /\ pc' = [pc EXCEPT ![self] = "WReported"]
                     /\ UNCHANGED <<liveRun, applied>>
                  \/ /\ applied' = (applied \cup {IF Bug = "NewScopePerDelivery" THEN wd[self] ELSE 0})
                     /\ liveRun' = (liveRun \cup {self})
                     /\ pc' = [pc EXCEPT ![self] = "WReport"]
                     /\ UNCHANGED reports
               /\ UNCHANGED << marker, bgn, nsIds, unknown, result, table, 
                               nextD, net, breq, bans, ambig, loss, dups, 
                               ecrash, wcrash, fired, firedAt, runners, hold, 
                               unknownN, nsRep, halted, badBegin, er, eg, ed, 
                               ecid, er2, ereq, erep, wd, wg, wcid, wn, stamp, 
                               wst >>

WRun(self) == /\ pc[self] = "WRun"
              /\ fired' = fired + 1
              /\ firedAt' = [firedAt EXCEPT ![wg[self]] = wcid[self]]
              /\ runners' = [runners EXCEPT ![wg[self]] = runners[wg[self]] \cup {self}]
              /\ liveRun' = (liveRun \cup {self})
              /\ wst' = [wst EXCEPT ![self] = "ran"]
              /\ pc' = [pc EXCEPT ![self] = "WReport"]
              /\ UNCHANGED << marker, bgn, nsIds, unknown, result, table, 
                              nextD, net, breq, bans, reports, ambig, loss, 
                              dups, ecrash, wcrash, hold, unknownN, nsRep, 
                              halted, applied, badBegin, er, eg, ed, ecid, er2, 
                              ereq, erep, wd, wg, wcid, wn, stamp >>

WReport(self) == /\ pc[self] = "WReport"
                 /\ \/ /\ reports' = (reports \cup {[w |-> self, d |-> wd[self], g |-> wg[self], kind |-> "complete"]})
                       /\ liveRun' = liveRun \ {self}
                    \/ /\ reports' = (reports \cup {[w |-> self, d |-> wd[self], g |-> wg[self], kind |-> "unknown"]})
                       /\ liveRun' = liveRun \ {self}
                 /\ pc' = [pc EXCEPT ![self] = "WReported"]
                 /\ UNCHANGED << marker, bgn, nsIds, unknown, result, table, 
                                 nextD, net, breq, bans, ambig, loss, dups, 
                                 ecrash, wcrash, fired, firedAt, runners, hold, 
                                 unknownN, nsRep, halted, applied, badBegin, 
                                 er, eg, ed, ecid, er2, ereq, erep, wd, wg, 
                                 wcid, wn, stamp, wst >>

WReported(self) == /\ pc[self] = "WReported"
                   /\ ~\E p \in reports : p.w = self /\ p.d = wd[self]
                   /\ wst' = [wst EXCEPT ![self] = "idle"]
                   /\ hold' = [hold EXCEPT ![self] = [d |-> 0, n |-> <<None, 0>>]]
                   /\ pc' = [pc EXCEPT ![self] = "WIdle"]
                   /\ UNCHANGED << marker, bgn, nsIds, unknown, result, table, 
                                   nextD, net, breq, bans, reports, ambig, 
                                   loss, dups, ecrash, wcrash, fired, firedAt, 
                                   runners, liveRun, unknownN, nsRep, halted, 
                                   applied, badBegin, er, eg, ed, ecid, er2, 
                                   ereq, erep, wd, wg, wcid, wn, stamp >>

worker(self) == WIdle(self) \/ WBegin(self) \/ WWait(self) \/ WSafe(self)
                   \/ WRun(self) \/ WReport(self) \/ WReported(self)

RPick(self) == /\ pc[self] = "RPick"
               /\ result = None
               /\ \E g \in {x \in Gens : bgn[x].set /\ ~bgn[x].ab /\ ~Voided(x)}:
                    \/ unknown[g]
                    \/ \A w \in Workers : ~(hold[w].d = bgn[g].d /\ hold[w].n = bgn[g].n)
                    \/ Bug = "CallerCause"
               /\ pc' = [pc EXCEPT ![self] = "RWrite"]
               /\ UNCHANGED << marker, bgn, nsIds, unknown, result, table, 
                               nextD, net, breq, bans, reports, ambig, loss, 
                               dups, ecrash, wcrash, fired, firedAt, runners, 
                               liveRun, hold, unknownN, nsRep, halted, applied, 
                               badBegin, er, eg, ed, ecid, er2, ereq, erep, wd, 
                               wg, wcid, wn, stamp, wst >>

RWrite(self) == /\ pc[self] = "RWrite"
                /\ IF result = None
                      THEN /\ result' = "resolved"
                      ELSE /\ TRUE
                           /\ UNCHANGED result
                /\ pc' = [pc EXCEPT ![self] = "Done"]
                /\ UNCHANGED << marker, bgn, nsIds, unknown, table, nextD, net, 
                                breq, bans, reports, ambig, loss, dups, ecrash, 
                                wcrash, fired, firedAt, runners, liveRun, hold, 
                                unknownN, nsRep, halted, applied, badBegin, er, 
                                eg, ed, ecid, er2, ereq, erep, wd, wg, wcid, 
                                wn, stamp, wst >>

resolver(self) == RPick(self) \/ RWrite(self)

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == engine \/ rpc
           \/ (\E self \in Workers: worker(self))
           \/ (\E self \in ResolverSet: resolver(self))
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ WF_vars(engine)
        /\ WF_vars(rpc)
        /\ \A self \in Workers : WF_vars(worker(self))
        /\ \A self \in ResolverSet : WF_vars((pc[self] # "RPick") /\ resolver(self))

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION
-----------------------------------------------------------------------------

\* Transport faults: a task lost, or duplicated to any worker (a proxy retry, a replayed push, a
\* second worker presenting the same delivery id); a begin answer lost (the worker retries).
LoseTask ==
  /\ loss < MaxLoss
  /\ \E t \in net : net' = net \ {t}
  /\ loss' = loss + 1
  /\ UNCHANGED <<marker, bgn, nsIds, unknown, result, table, nextD, breq, bans, reports, ambig,
                 dups, ecrash, wcrash, fired, firedAt, runners, liveRun, hold, unknownN, nsRep, halted, applied, badBegin, pc, er, er2, eg, ed,
                 ecid, ereq, erep, wd, wg, wcid, wn, stamp, wst>>

DupTask ==
  /\ dups < MaxDup
  /\ \E t \in net, w \in Workers : net' = net \cup {[t EXCEPT !.w = w]} /\ [t EXCEPT !.w = w] \notin net
  /\ dups' = dups + 1
  /\ UNCHANGED <<marker, bgn, nsIds, unknown, result, table, nextD, breq, bans, reports, ambig,
                 loss, ecrash, wcrash, fired, firedAt, runners, liveRun, hold, unknownN, nsRep, halted, applied, badBegin, pc, er, er2, eg, ed,
                 ecid, ereq, erep, wd, wg, wcid, wn, stamp, wst>>

LoseAnswer ==
  /\ loss < MaxLoss
  /\ \E w \in Workers : bans[w].a \in {"true", "false"} /\ pc[w] = "WWait"
                        /\ bans' = [bans EXCEPT ![w].a = "unavailable"]
  /\ loss' = loss + 1
  /\ UNCHANGED <<marker, bgn, nsIds, unknown, result, table, nextD, net, breq, reports, ambig,
                 dups, ecrash, wcrash, fired, firedAt, runners, liveRun, hold, unknownN, nsRep, halted, applied, badBegin, pc, er, er2, eg, ed,
                 ecid, ereq, erep, wd, wg, wcid, wn, stamp, wst>>

\* An engine crash: a new instance with an empty dispatch table; begin requests and reports in
\* flight are retried by the workers (kept), the engine's step is lost.
EngineCrash ==
  /\ ecrash < MaxEngineCrash
  /\ ecrash' = ecrash + 1
  /\ table' = [d \in DIds |-> None]
  /\ pc' = [pc EXCEPT !["engine"] = "EIdle", !["rpc"] = "RIdle"]
  \* An RPC the crash cut off fails: a begin is answered unavailable (the worker retries with its
  \* nonce), a report is sent again.
  /\ bans' = IF pc["rpc"] \in {"EBeginCheck", "EBeginInsert", "EBeginRead"}
             THEN [bans EXCEPT ![ereq.w] = [d |-> ereq.d, a |-> "unavailable"]] ELSE bans
  /\ reports' = IF pc["rpc"] \in {"EReport", "RNotStarted", "EComplete", "EUnknown"}
                THEN reports \cup {erep} ELSE reports
  /\ UNCHANGED <<marker, bgn, nsIds, unknown, result, nextD, net, breq, ambig,
                 loss, dups, wcrash, fired, firedAt, runners, liveRun, hold, unknownN, nsRep, halted, applied, badBegin, er, er2, eg, ed, ecid,
                 ereq, erep, wd, wg, wcid, wn, stamp, wst>>

\* A worker crash: it forgets its task (no SDK-side durability, I6); one killed after Run leaves a
\* begun attempt with no outcome.
WorkerCrash(w) ==
  /\ wcrash < MaxWorkerCrash
  /\ pc[w] # "WIdle"
  /\ wcrash' = wcrash + 1
  /\ pc' = [pc EXCEPT ![w] = "WIdle"]
  /\ liveRun' = liveRun \ {w}
  /\ hold' = [hold EXCEPT ![w] = [d |-> 0, n |-> <<None, 0>>]]
  /\ breq' = {q \in breq : q.w # w}
  /\ bans' = [bans EXCEPT ![w] = [d |-> 0, a |-> None]]
  /\ UNCHANGED <<marker, bgn, nsIds, unknown, result, table, nextD, net, reports, ambig, loss,
                 dups, ecrash, fired, firedAt, runners, unknownN, nsRep, halted, applied, badBegin, er, er2, eg, ed, ecid, ereq, erep, wd,
                 wg, wcid, wn, stamp, wst>>

FullNext == Next \/ LoseTask \/ DupTask \/ LoseAnswer \/ EngineCrash \/ \E w \in Workers : WorkerCrash(w)
FullSpec == Init /\ [][FullNext]_vars
            /\ WF_vars(engine) /\ WF_vars(rpc) /\ \A self \in Workers : WF_vars(worker(self))

(***************************************************************************)
(* Properties                                                               *)
(***************************************************************************)

\* At most one fire per call.
AtMostOnce == fired <= 1

\* No not-started record for a claim whose effect fired (with a non-holder voider: the abandon).
NotStartedExclusive == \A g \in Gens : firedAt[g] # 0 => firedAt[g] \notin nsIds[g]

\* At most one worker is ever in Run for one marker.
BeginExclusive == \A g \in Gens : Cardinality(runners[g]) <= 1

\* A live worker whose nonce the stored begin record holds is never answered false.
BeginIdempotent == ~badBegin

\* No worker runs under a marker whose begin key holds an abandon.
NoRunAfterAbandon == \A g \in Gens : runners[g] # {} => ~(bgn[g].set /\ bgn[g].ab)

\* A resolution never lands while a begun worker's handler runs (between Run and its report), nor
\* over a completion its worker reported: a recorded "resolved" means no worker's report said
\* complete for it.
NoLiveOverride == result = "resolved" => liveRun = {}

\* The recorded outcome is never replaced.
ResultStable == [][result # None => result' = result]_vars

\* A DELIVERY_EXHAUSTED error is truthful: no effect ran (for a retry-safe call, no delivery's
\* downstream effect took place), so the caller asking again under a new call cannot repeat it.
ExhaustedTruthful == result = "exhausted" => fired = 0 /\ applied = {}

\* A retry-safe call's re-dispatches share one once-key scope, so its downstream effect applies
\* once.
DownstreamOnce == Cardinality(applied) <= 1

\* Vacuity, expected violated: the effect fires and its completion is recorded.
EffectNotReachable == ~((fired > 0 \/ applied # {}) /\ result = "ok")
=============================================================================
