-------------------------------- MODULE Flows --------------------------------
(***************************************************************************)
(* Model 7 of docs/design/formal-models.md: the semantics of a lowered plan *)
(* flow (P5b, #103). Each node runs as an agent.Step under its node key;    *)
(* a Switch's choice is journaled under switch:<over>; a bounded loop's     *)
(* body runs under iteration keys (node:iter:<i>:<node>, and                *)
(* switch:iter:<i>:<over> for the loop's Switch); a Step a node's body runs *)
(* is recorded under the node's key; the terminal's output is recorded as   *)
(* run:complete; Flow.ResolveHalt resolves only a halted node of a run of   *)
(* this flow. spec/tla/README.md maps each label to the Go it abstracts.    *)
(*                                                                          *)
(* The flow modelled (all a flow's routing shapes in one small graph):      *)
(*                                                                          *)
(*   E --Switch(E): v = 1--> loop { H -> S; Switch(S): v = 1 back to H,     *)
(*   |                             else exit to T }                         *)
(*   +--Else--> Q                                                           *)
(*                                                                          *)
(* E, Q and T are terminal or entry nodes; H runs a nested Step N in its    *)
(* body. The Step's own claim protocol is model 1's; here a node that is    *)
(* not retry-safe claims one marker (first writer wins), fires, and records *)
(* its result, and a drive that finds a marker with no result halts until   *)
(* the halt is resolved.                                                    *)
(*                                                                          *)
(* A node in Gated carries an approval gate (Need of the Approvers), held   *)
(* before the node's Step under the node's key, per iteration in the loop:  *)
(* the gate reads a recorded terminal tally, or counts the approvers'       *)
(* decisions (each approver's first decision; model 1b checks the counting  *)
(* rule over signed records) and records a terminal tally (first writer     *)
(* wins), or pauses. Approvers decide at any time, a driver up or not.      *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, Sequences, TLC

CONSTANTS
  Drivers,     \* drives of the run (model values)
  SideEffects, \* names of the nodes that are not retry-safe (claim a marker), a subset of Names
  MaxIter,     \* the loop's bound (LoopBack max)
  MaxAmbig,    \* budget of error replies
  MaxCrash,    \* budget of driver crashes
  HasResolver, \* whether an operator resolves node halts
  RFlows,      \* the flows an operator may resolve through: "F" (this flow), "G" (another flow
               \* or another topology digest)
  Gated,       \* names of the nodes that carry an approval gate, a subset of Names \ {"N"}
  Approvers,   \* the gates' approvers (one policy for every gate in the model)
  Need,        \* approvals a gate needs
  Early,       \* whether a decision can be recorded before its gate's request (a record written
               \* directly, which Approve and SubmitDecision refuse)
  Bug          \* "none" or a reverted rule, see regress/

None == "none"
Names == {"E", "Q", "T", "H", "S", "N"}
Iters == 0..(MaxIter - 1)
LoopNames == {"H", "S", "N"}
\* Journal keys: <<name, iteration>>; nodes outside the loop use iteration 0.
Keys == {<<n, 0>> : n \in {"E", "Q", "T"}} \cup {<<n, i>> : n \in LoopNames, i \in Iters}
NodeKeys == {k \in Keys : k[1] # "N"}
SideKeys == {k \in NodeKeys : k[1] \in SideEffects}
ChoiceKeys == {<<"E", 0>>} \cup {<<"S", i>> : i \in Iters}
NoRes == [rec |-> FALSE, v |-> 0, by |-> None, ty |-> None]
ResolverSet == IF HasResolver THEN {"resolver"} ELSE {}
GateKeys == {k \in NodeKeys : k[1] \in Gated}
ApproverSet == IF Gated = {} THEN {} ELSE Approvers

\* The Switch predicates, pure over the switched node's value (When): E routes to the loop on 1,
\* else to Q; S loops back on 1, else exits to T.
Pred(name, v) == IF name = "E" THEN (IF v = 1 THEN "loop" ELSE "Q")
                 ELSE (IF v = 1 THEN "back" ELSE "exit")

ASSUME SideEffects \subseteq {"E", "Q", "T", "H", "S"}
ASSUME MaxIter \in Nat \ {0} /\ MaxAmbig \in Nat /\ MaxCrash \in Nat
ASSUME RFlows \subseteq {"F", "G"}
ASSUME Gated \subseteq {"E", "Q", "T", "H", "S"}
ASSUME Need \in 1..Cardinality(Approvers) \/ Gated = {}
ASSUME Early \in BOOLEAN
ASSUME Bug \in {"none", "NestedUnscoped", "LoopSwitchUnscoped", "ResolveAnyNode", "ResolveAnyFlow",
                "NoCompletion", "NoGate", "GateUnscoped", "NoRequestOrder"}

(* --algorithm flows
variables
  \* The journal.
  started = FALSE,                         \* run:start of this flow (with this input)
  res     = [k \in Keys |-> NoRes],        \* node:... results: [rec, v, by (drv|res), ty (F|G)]
  mk      = [k \in NodeKeys |-> None],     \* attempt:step:node:... markers: the claiming driver
  sw      = [k \in ChoiceKeys |-> None],   \* switch:... choices
  done    = [rec |-> FALSE, v |-> 0, term |-> None],  \* run:complete
  tally   = [k \in GateKeys |-> None],      \* approval-tally:<node key>: "pass" or "deny"
  req     = [k \in GateKeys |-> FALSE],     \* approval-request:<node key>: the gate's request
  \* Each approver's decision: "yes"/"no" recorded after the gate's request, or "eyes"/"eno"
  \* recorded before it (a record written directly, before the run reached the gate). An early
  \* decision is replaced by the approver's decision after the request, re-signed at a new time:
  \* the early record never counts, so the model keeps only the one that can.
  dec     = [k \in GateKeys |-> [a \in Approvers |-> None]],
  \* Per-driver results of the Node and Choose procedures.
  nval    = [d \in Drivers |-> 0],
  choice  = [d \in Drivers |-> None],
  failed  = [d \in Drivers |-> None],
  reply   = [d \in Drivers \cup ResolverSet |-> ""],
  ambig = 0, crashes = 0,
  \* Ghosts: effect calls per node key, nested Step calls per iteration, and a route that did not
  \* follow the declared predicate over this iteration's recorded value.
  fired   = [k \in NodeKeys |-> 0],
  firedN  = [i \in Iters |-> 0],
  badRoute = FALSE,
  \* Ghost: a gated node's body ran with fewer than Need approvals recorded for its key.
  badFire = FALSE;

define
  \* The loop's switch key for iteration i (historically unscoped: LoopSwitchUnscoped).
  SKey(i) == IF Bug = "LoopSwitchUnscoped" THEN <<"S", 0>> ELSE <<"S", i>>
  \* The nested Step's key inside H's body in iteration i (historically unscoped: F7).
  NKey(i) == IF Bug = "NestedUnscoped" THEN <<"N", 0>> ELSE <<"N", i>>
  \* The gate's key for a node key (per iteration; GateUnscoped keys every iteration as the first).
  GKey(k) == IF Bug = "GateUnscoped" THEN <<k[1], 0>> ELSE k
  \* Approvals and denials recorded for a gate key after its request (the ones that count).
  Yes(k) == Cardinality({a \in Approvers : dec[k][a] = "yes"})
  No(k)  == Cardinality({a \in Approvers : dec[k][a] = "no"})
  \* What the gate counts: only decisions after the request (NoRequestOrder: any decision).
  GYes(k) == IF Bug = "NoRequestOrder" THEN Cardinality({a \in Approvers : dec[k][a] \in {"yes", "eyes"}}) ELSE Yes(k)
  GNo(k)  == IF Bug = "NoRequestOrder" THEN Cardinality({a \in Approvers : dec[k][a] \in {"no", "eno"}}) ELSE No(k)

  \* Conform: the journal holds a declared path. Which node keys the recorded choices reach:
  LoopOn == sw[<<"E", 0>>] = "loop"
  IterReached(i) == LoopOn /\ \A j \in Iters : j < i => sw[<<"S", j>>] = "back"
  ExitAt(i) == IterReached(i) /\ sw[<<"S", i>>] = "exit"
  Allowed(k) ==
    CASE k[1] = "E" -> TRUE
      [] k[1] = "Q" -> sw[<<"E", 0>>] = "Q"
      [] k[1] = "T" -> \E i \in Iters : ExitAt(i)
      [] OTHER      -> IterReached(k[2])
  \* The terminal the recorded choices reach, if any.
  Reached == IF sw[<<"E", 0>>] = "Q" THEN <<"Q", 0>>
             ELSE IF \E i \in Iters : ExitAt(i) THEN <<"T", 0>> ELSE <<"none", 0>>
end define;

macro Reply(r) begin
  either r := "ok";
  or await ambig < MaxAmbig; ambig := ambig + 1; r := "err_nc";
  or await ambig < MaxAmbig; ambig := ambig + 1; r := "err_c";
  end either;
end macro;

\* runNode: one node as an agent.Step under its key. It sets nval[self] to the recorded output, or
\* failed[self] to "halt" or "error".
procedure Node(key)
variables bodyVal = 0;
begin
GRead:
  \* runGate, then flowGate: a node with no gate goes to its Step; a recorded tally decides.
  if key[1] \notin Gated \/ Bug = "NoGate" then goto NGet;
  elsif tally[GKey(key)] # None then goto GPause;
  else req[GKey(key)] := TRUE;    \* the request, recorded once, before any count
  end if;
GCount:
  \* tallyGate: count the decisions; a terminal count is recorded (first writer wins), else the
  \* drive pauses (an ApprovalPending) and is driven again later.
  with y = GYes(GKey(key)), n = GNo(GKey(key)) do
    if y >= Need \/ Cardinality(Approvers) - n < Need then
      Reply(reply[self]);
      if reply[self] # "err_nc" /\ tally[GKey(key)] = None then
        tally[GKey(key)] := IF y >= Need THEN "pass" ELSE "deny";
      end if;
      if reply[self] # "ok" then failed[self] := "error"; return; end if;
    else
      failed[self] := "pause"; return;
    end if;
  end with;
GPause:
  \* A denying tally stops the run (ErrApprovalDenied); a passing one lets the node run.
  if tally[GKey(key)] = "deny" then failed[self] := "denied"; return; end if;
NGet:
  \* A recorded result replays without running the body (one point read).
  if res[key].rec then
    nval[self] := res[key].v; return;
  elsif key[1] \notin SideEffects then
    goto NBody;
  end if;
NClaim:
  \* The Step's attempt claim: the first marker wins; a loser, or a drive that finds a marker
  \* with no result, halts (model 1 checks the claim protocol itself).
  \* A claim is won only by the Insert that stores the marker: a marker already there (another
  \* driver's, or this driver's from a drive a crash cut off) is lost to.
  with was = mk[key] do
    Reply(reply[self]);
    if reply[self] # "err_nc" /\ was = None then mk[key] := self; end if;
    if reply[self] # "ok" then
      failed[self] := "error"; return;
    elsif was # None then
      if res[key].rec then nval[self] := res[key].v; else failed[self] := "halt"; end if;
      return;
    end if;
  end with;
NBody:
  \* The body: its effect fires (a retry-safe node's may fire again on a re-run), and it
  \* produces an output.
  with x \in {0, 1} do bodyVal := x; end with;
  if key[1] \in SideEffects then fired[key] := fired[key] + 1; end if;
  if key[1] \in Gated /\ Yes(key) < Need then badFire := TRUE; end if;
  if key[1] = "H" then goto NNested; else goto NRecord; end if;
NNested:
  \* H's body runs a Step N, recorded under node:iter:<i>:H:step:N: it runs once per iteration,
  \* and a re-run of the body replays it.
  if ~res[NKey(key[2])].rec then
    firedN[key[2]] := firedN[key[2]] + 1;
    res[NKey(key[2])] := [rec |-> TRUE, v |-> 0, by |-> "drv", ty |-> "F"];
  end if;
NRecord:
  Reply(reply[self]);
  if reply[self] # "err_nc" /\ ~res[key].rec then
    res[key] := [rec |-> TRUE, v |-> bodyVal, by |-> "drv", ty |-> "F"];
  end if;
  if reply[self] # "ok" then
    failed[self] := "error";
  else
    nval[self] := IF res[key].rec THEN res[key].v ELSE bodyVal;
  end if;
  return;
end procedure;

\* chooseArm: the Switch over the switched node's recorded value, journaled under key. Sets
\* choice[self] to the recorded choice, or failed[self].
procedure Choose(ckey, val, name)
begin
CDo:
  if sw[ckey] = None then
    Reply(reply[self]);
    if reply[self] # "err_nc" then sw[ckey] := Pred(name, val); end if;
    if reply[self] # "ok" then failed[self] := "error"; return; end if;
  end if;
CRoute:
  choice[self] := sw[ckey];
  \* The route must follow the predicate over this iteration's recorded value.
  if sw[ckey] # Pred(name, val) then badRoute := TRUE; end if;
  return;
end procedure;

fair process driver \in Drivers
variables it = 0, out = 0, term = None;
begin
Begin:
  \* journalhook.Begin: run:start (first writer), or the recorded completion.
  failed[self] := None; it := 0; out := 0; term := None;
  nval[self] := 0; choice[self] := None; reply[self] := "";
  if done.rec then goto Finish; end if;
BeginStart:
  Reply(reply[self]);
  if reply[self] # "err_nc" then started := TRUE; end if;
  if reply[self] # "ok" then failed[self] := "error"; goto Finish; end if;
RunE:
  call Node(<<"E", 0>>);
AfterE:
  if failed[self] # None then goto Finish; end if;
ChooseE:
  call Choose(<<"E", 0>>, nval[self], "E");
AfterChooseE:
  if failed[self] # None then goto Finish;
  elsif choice[self] = "Q" then goto RunQ;
  end if;
LoopH:
  call Node(<<"H", it>>);
AfterH:
  if failed[self] # None then goto Finish; end if;
RunS:
  call Node(<<"S", it>>);
AfterS:
  if failed[self] # None then goto Finish; end if;
ChooseS:
  call Choose(SKey(it), nval[self], "S");
AfterChooseS:
  if failed[self] # None then goto Finish;
  elsif choice[self] = "back" then
    if it + 1 >= MaxIter then
      failed[self] := "runaway"; goto Finish;     \* the loop's bound: a runaway-loop error
    else
      it := it + 1; goto LoopH;
    end if;
  end if;
RunT:
  call Node(<<"T", 0>>);
AfterT:
  if failed[self] # None then goto Finish; end if;
SetT:
  term := "T"; out := nval[self]; goto Complete;
RunQ:
  call Node(<<"Q", 0>>);
AfterQ:
  if failed[self] # None then goto Finish; end if;
SetQ:
  term := "Q"; out := nval[self];
Complete:
  \* run:complete with the terminal's output (first writer wins).
  if Bug # "NoCompletion" then
    Reply(reply[self]);
    if reply[self] # "err_nc" /\ ~done.rec then done := [rec |-> TRUE, v |-> out, term |-> term]; end if;
    if reply[self] # "ok" then failed[self] := "error"; end if;
  end if;
Finish:
  \* A drive that did not finish is driven again; a halt waits for its resolution.
  if failed[self] \notin {None, "runaway", "denied"} then goto Begin; end if;
end process;

fair process resolver \in ResolverSet
variables rk = <<"E", 0>>, rv = 0, rf = "F";
begin
RPick:-
  \* Flow.ResolveHalt: a node of this flow, in a run of this flow, with a live attempt and no
  \* result (ResolveHaltRef's ErrNoLiveAttempt); historically any node (ResolveAnyNode) or any
  \* flow (ResolveAnyFlow).
  with k \in NodeKeys, fl \in RFlows, v \in {0, 1} do
    await ~res[k].rec;
    await Bug = "ResolveAnyNode" \/ mk[k] # None;
    await Bug = "ResolveAnyFlow" \/ fl = "F";
    rk := k; rv := v; rf := fl;
  end with;
RWrite:
  Reply(reply[self]);
  if reply[self] # "err_nc" /\ ~res[rk].rec then
    res[rk] := [rec |-> TRUE, v |-> rv, by |-> "res", ty |-> rf];
  end if;
end process;

fair process approver \in ApproverSet
begin
ADecide:
  \* SubmitDecision: the approver's decision on a gate (its first valid one counts), whether a
  \* driver is running, paused or down.
  \* Before the gate's request a decision is an early one (refused by SubmitDecision; possible only
  \* as a record written directly, with Early), and it never counts. Since bide.approval.v3 it does
  \* not take the approver's seat: their identical decision after the request has its name and is
  \* not recorded, but a decision signed again at a new decided_at is a new record, so after the
  \* request an approver with only an early decision decides again, and that decision counts.
  with k \in {k \in GateKeys : (dec[k][self] = None /\ (req[k] \/ Early)) \/ (dec[k][self] \in {"eyes", "eno"} /\ req[k])},
       v \in IF req[k] THEN {"yes", "no"} ELSE {"eyes", "eno"} do
    dec[k][self] := v;
  end with;
  if \E k \in GateKeys : dec[k][self] \in {None, "eyes", "eno"} then goto ADecide; end if;
end process;
end algorithm; *)
\* BEGIN TRANSLATION
CONSTANT defaultInitValue
VARIABLES started, res, mk, sw, done, tally, req, dec, nval, choice, failed, 
          reply, ambig, crashes, fired, firedN, badRoute, badFire, pc, stack

(* define statement *)
SKey(i) == IF Bug = "LoopSwitchUnscoped" THEN <<"S", 0>> ELSE <<"S", i>>

NKey(i) == IF Bug = "NestedUnscoped" THEN <<"N", 0>> ELSE <<"N", i>>

GKey(k) == IF Bug = "GateUnscoped" THEN <<k[1], 0>> ELSE k

Yes(k) == Cardinality({a \in Approvers : dec[k][a] = "yes"})
No(k)  == Cardinality({a \in Approvers : dec[k][a] = "no"})

GYes(k) == IF Bug = "NoRequestOrder" THEN Cardinality({a \in Approvers : dec[k][a] \in {"yes", "eyes"}}) ELSE Yes(k)
GNo(k)  == IF Bug = "NoRequestOrder" THEN Cardinality({a \in Approvers : dec[k][a] \in {"no", "eno"}}) ELSE No(k)


LoopOn == sw[<<"E", 0>>] = "loop"
IterReached(i) == LoopOn /\ \A j \in Iters : j < i => sw[<<"S", j>>] = "back"
ExitAt(i) == IterReached(i) /\ sw[<<"S", i>>] = "exit"
Allowed(k) ==
  CASE k[1] = "E" -> TRUE
    [] k[1] = "Q" -> sw[<<"E", 0>>] = "Q"
    [] k[1] = "T" -> \E i \in Iters : ExitAt(i)
    [] OTHER      -> IterReached(k[2])

Reached == IF sw[<<"E", 0>>] = "Q" THEN <<"Q", 0>>
           ELSE IF \E i \in Iters : ExitAt(i) THEN <<"T", 0>> ELSE <<"none", 0>>

VARIABLES key, bodyVal, ckey, val, name, it, out, term, rk, rv, rf

vars == << started, res, mk, sw, done, tally, req, dec, nval, choice, failed, 
           reply, ambig, crashes, fired, firedN, badRoute, badFire, pc, stack, 
           key, bodyVal, ckey, val, name, it, out, term, rk, rv, rf >>

ProcSet == (Drivers) \cup (ResolverSet) \cup (ApproverSet)

Init == (* Global variables *)
        /\ started = FALSE
        /\ res = [k \in Keys |-> NoRes]
        /\ mk = [k \in NodeKeys |-> None]
        /\ sw = [k \in ChoiceKeys |-> None]
        /\ done = [rec |-> FALSE, v |-> 0, term |-> None]
        /\ tally = [k \in GateKeys |-> None]
        /\ req = [k \in GateKeys |-> FALSE]
        /\ dec = [k \in GateKeys |-> [a \in Approvers |-> None]]
        /\ nval = [d \in Drivers |-> 0]
        /\ choice = [d \in Drivers |-> None]
        /\ failed = [d \in Drivers |-> None]
        /\ reply = [d \in Drivers \cup ResolverSet |-> ""]
        /\ ambig = 0
        /\ crashes = 0
        /\ fired = [k \in NodeKeys |-> 0]
        /\ firedN = [i \in Iters |-> 0]
        /\ badRoute = FALSE
        /\ badFire = FALSE
        (* Procedure Node *)
        /\ key = [ self \in ProcSet |-> defaultInitValue]
        /\ bodyVal = [ self \in ProcSet |-> 0]
        (* Procedure Choose *)
        /\ ckey = [ self \in ProcSet |-> defaultInitValue]
        /\ val = [ self \in ProcSet |-> defaultInitValue]
        /\ name = [ self \in ProcSet |-> defaultInitValue]
        (* Process driver *)
        /\ it = [self \in Drivers |-> 0]
        /\ out = [self \in Drivers |-> 0]
        /\ term = [self \in Drivers |-> None]
        (* Process resolver *)
        /\ rk = [self \in ResolverSet |-> <<"E", 0>>]
        /\ rv = [self \in ResolverSet |-> 0]
        /\ rf = [self \in ResolverSet |-> "F"]
        /\ stack = [self \in ProcSet |-> << >>]
        /\ pc = [self \in ProcSet |-> CASE self \in Drivers -> "Begin"
                                        [] self \in ResolverSet -> "RPick"
                                        [] self \in ApproverSet -> "ADecide"]

GRead(self) == /\ pc[self] = "GRead"
               /\ IF key[self][1] \notin Gated \/ Bug = "NoGate"
                     THEN /\ pc' = [pc EXCEPT ![self] = "NGet"]
                          /\ req' = req
                     ELSE /\ IF tally[GKey(key[self])] # None
                                THEN /\ pc' = [pc EXCEPT ![self] = "GPause"]
                                     /\ req' = req
                                ELSE /\ req' = [req EXCEPT ![GKey(key[self])] = TRUE]
                                     /\ pc' = [pc EXCEPT ![self] = "GCount"]
               /\ UNCHANGED << started, res, mk, sw, done, tally, dec, nval, 
                               choice, failed, reply, ambig, crashes, fired, 
                               firedN, badRoute, badFire, stack, key, bodyVal, 
                               ckey, val, name, it, out, term, rk, rv, rf >>

GCount(self) == /\ pc[self] = "GCount"
                /\ LET y == GYes(GKey(key[self])) IN
                     LET n == GNo(GKey(key[self])) IN
                       IF y >= Need \/ Cardinality(Approvers) - n < Need
                          THEN /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                     /\ ambig' = ambig
                                  \/ /\ ambig < MaxAmbig
                                     /\ ambig' = ambig + 1
                                     /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                  \/ /\ ambig < MaxAmbig
                                     /\ ambig' = ambig + 1
                                     /\ reply' = [reply EXCEPT ![self] = "err_c"]
                               /\ IF reply'[self] # "err_nc" /\ tally[GKey(key[self])] = None
                                     THEN /\ tally' = [tally EXCEPT ![GKey(key[self])] = IF y >= Need THEN "pass" ELSE "deny"]
                                     ELSE /\ TRUE
                                          /\ tally' = tally
                               /\ IF reply'[self] # "ok"
                                     THEN /\ failed' = [failed EXCEPT ![self] = "error"]
                                          /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                          /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                                          /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                                          /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                     ELSE /\ pc' = [pc EXCEPT ![self] = "GPause"]
                                          /\ UNCHANGED << failed, stack, key, 
                                                          bodyVal >>
                          ELSE /\ failed' = [failed EXCEPT ![self] = "pause"]
                               /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                               /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                               /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                               /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                               /\ UNCHANGED << tally, reply, ambig >>
                /\ UNCHANGED << started, res, mk, sw, done, req, dec, nval, 
                                choice, crashes, fired, firedN, badRoute, 
                                badFire, ckey, val, name, it, out, term, rk, 
                                rv, rf >>

GPause(self) == /\ pc[self] = "GPause"
                /\ IF tally[GKey(key[self])] = "deny"
                      THEN /\ failed' = [failed EXCEPT ![self] = "denied"]
                           /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                           /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                           /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                           /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "NGet"]
                           /\ UNCHANGED << failed, stack, key, bodyVal >>
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, reply, ambig, crashes, fired, 
                                firedN, badRoute, badFire, ckey, val, name, it, 
                                out, term, rk, rv, rf >>

NGet(self) == /\ pc[self] = "NGet"
              /\ IF res[key[self]].rec
                    THEN /\ nval' = [nval EXCEPT ![self] = res[key[self]].v]
                         /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                         /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                         /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                         /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                    ELSE /\ IF key[self][1] \notin SideEffects
                               THEN /\ pc' = [pc EXCEPT ![self] = "NBody"]
                               ELSE /\ pc' = [pc EXCEPT ![self] = "NClaim"]
                         /\ UNCHANGED << nval, stack, key, bodyVal >>
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              choice, failed, reply, ambig, crashes, fired, 
                              firedN, badRoute, badFire, ckey, val, name, it, 
                              out, term, rk, rv, rf >>

NClaim(self) == /\ pc[self] = "NClaim"
                /\ LET was == mk[key[self]] IN
                     /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                           /\ ambig' = ambig
                        \/ /\ ambig < MaxAmbig
                           /\ ambig' = ambig + 1
                           /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                        \/ /\ ambig < MaxAmbig
                           /\ ambig' = ambig + 1
                           /\ reply' = [reply EXCEPT ![self] = "err_c"]
                     /\ IF reply'[self] # "err_nc" /\ was = None
                           THEN /\ mk' = [mk EXCEPT ![key[self]] = self]
                           ELSE /\ TRUE
                                /\ mk' = mk
                     /\ IF reply'[self] # "ok"
                           THEN /\ failed' = [failed EXCEPT ![self] = "error"]
                                /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                                /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                                /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                /\ nval' = nval
                           ELSE /\ IF was # None
                                      THEN /\ IF res[key[self]].rec
                                                 THEN /\ nval' = [nval EXCEPT ![self] = res[key[self]].v]
                                                      /\ UNCHANGED failed
                                                 ELSE /\ failed' = [failed EXCEPT ![self] = "halt"]
                                                      /\ nval' = nval
                                           /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                           /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                                           /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                                           /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                      ELSE /\ pc' = [pc EXCEPT ![self] = "NBody"]
                                           /\ UNCHANGED << nval, failed, stack, 
                                                           key, bodyVal >>
                /\ UNCHANGED << started, res, sw, done, tally, req, dec, 
                                choice, crashes, fired, firedN, badRoute, 
                                badFire, ckey, val, name, it, out, term, rk, 
                                rv, rf >>

NBody(self) == /\ pc[self] = "NBody"
               /\ \E x \in {0, 1}:
                    bodyVal' = [bodyVal EXCEPT ![self] = x]
               /\ IF key[self][1] \in SideEffects
                     THEN /\ fired' = [fired EXCEPT ![key[self]] = fired[key[self]] + 1]
                     ELSE /\ TRUE
                          /\ fired' = fired
               /\ IF key[self][1] \in Gated /\ Yes(key[self]) < Need
                     THEN /\ badFire' = TRUE
                     ELSE /\ TRUE
                          /\ UNCHANGED badFire
               /\ IF key[self][1] = "H"
                     THEN /\ pc' = [pc EXCEPT ![self] = "NNested"]
                     ELSE /\ pc' = [pc EXCEPT ![self] = "NRecord"]
               /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                               nval, choice, failed, reply, ambig, crashes, 
                               firedN, badRoute, stack, key, ckey, val, name, 
                               it, out, term, rk, rv, rf >>

NNested(self) == /\ pc[self] = "NNested"
                 /\ IF ~res[NKey(key[self][2])].rec
                       THEN /\ firedN' = [firedN EXCEPT ![key[self][2]] = firedN[key[self][2]] + 1]
                            /\ res' = [res EXCEPT ![NKey(key[self][2])] = [rec |-> TRUE, v |-> 0, by |-> "drv", ty |-> "F"]]
                       ELSE /\ TRUE
                            /\ UNCHANGED << res, firedN >>
                 /\ pc' = [pc EXCEPT ![self] = "NRecord"]
                 /\ UNCHANGED << started, mk, sw, done, tally, req, dec, nval, 
                                 choice, failed, reply, ambig, crashes, fired, 
                                 badRoute, badFire, stack, key, bodyVal, ckey, 
                                 val, name, it, out, term, rk, rv, rf >>

NRecord(self) == /\ pc[self] = "NRecord"
                 /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                       /\ ambig' = ambig
                    \/ /\ ambig < MaxAmbig
                       /\ ambig' = ambig + 1
                       /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                    \/ /\ ambig < MaxAmbig
                       /\ ambig' = ambig + 1
                       /\ reply' = [reply EXCEPT ![self] = "err_c"]
                 /\ IF reply'[self] # "err_nc" /\ ~res[key[self]].rec
                       THEN /\ res' = [res EXCEPT ![key[self]] = [rec |-> TRUE, v |-> bodyVal[self], by |-> "drv", ty |-> "F"]]
                       ELSE /\ TRUE
                            /\ res' = res
                 /\ IF reply'[self] # "ok"
                       THEN /\ failed' = [failed EXCEPT ![self] = "error"]
                            /\ nval' = nval
                       ELSE /\ nval' = [nval EXCEPT ![self] = IF res'[key[self]].rec THEN res'[key[self]].v ELSE bodyVal[self]]
                            /\ UNCHANGED failed
                 /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                 /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                 /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                 /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                 /\ UNCHANGED << started, mk, sw, done, tally, req, dec, 
                                 choice, crashes, fired, firedN, badRoute, 
                                 badFire, ckey, val, name, it, out, term, rk, 
                                 rv, rf >>

Node(self) == GRead(self) \/ GCount(self) \/ GPause(self) \/ NGet(self)
                 \/ NClaim(self) \/ NBody(self) \/ NNested(self)
                 \/ NRecord(self)

CDo(self) == /\ pc[self] = "CDo"
             /\ IF sw[ckey[self]] = None
                   THEN /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                              /\ ambig' = ambig
                           \/ /\ ambig < MaxAmbig
                              /\ ambig' = ambig + 1
                              /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                           \/ /\ ambig < MaxAmbig
                              /\ ambig' = ambig + 1
                              /\ reply' = [reply EXCEPT ![self] = "err_c"]
                        /\ IF reply'[self] # "err_nc"
                              THEN /\ sw' = [sw EXCEPT ![ckey[self]] = Pred(name[self], val[self])]
                              ELSE /\ TRUE
                                   /\ sw' = sw
                        /\ IF reply'[self] # "ok"
                              THEN /\ failed' = [failed EXCEPT ![self] = "error"]
                                   /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                   /\ ckey' = [ckey EXCEPT ![self] = Head(stack[self]).ckey]
                                   /\ val' = [val EXCEPT ![self] = Head(stack[self]).val]
                                   /\ name' = [name EXCEPT ![self] = Head(stack[self]).name]
                                   /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                              ELSE /\ pc' = [pc EXCEPT ![self] = "CRoute"]
                                   /\ UNCHANGED << failed, stack, ckey, val, 
                                                   name >>
                   ELSE /\ pc' = [pc EXCEPT ![self] = "CRoute"]
                        /\ UNCHANGED << sw, failed, reply, ambig, stack, ckey, 
                                        val, name >>
             /\ UNCHANGED << started, res, mk, done, tally, req, dec, nval, 
                             choice, crashes, fired, firedN, badRoute, badFire, 
                             key, bodyVal, it, out, term, rk, rv, rf >>

CRoute(self) == /\ pc[self] = "CRoute"
                /\ choice' = [choice EXCEPT ![self] = sw[ckey[self]]]
                /\ IF sw[ckey[self]] # Pred(name[self], val[self])
                      THEN /\ badRoute' = TRUE
                      ELSE /\ TRUE
                           /\ UNCHANGED badRoute
                /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                /\ ckey' = [ckey EXCEPT ![self] = Head(stack[self]).ckey]
                /\ val' = [val EXCEPT ![self] = Head(stack[self]).val]
                /\ name' = [name EXCEPT ![self] = Head(stack[self]).name]
                /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, failed, reply, ambig, crashes, fired, 
                                firedN, badFire, key, bodyVal, it, out, term, 
                                rk, rv, rf >>

Choose(self) == CDo(self) \/ CRoute(self)

Begin(self) == /\ pc[self] = "Begin"
               /\ failed' = [failed EXCEPT ![self] = None]
               /\ it' = [it EXCEPT ![self] = 0]
               /\ out' = [out EXCEPT ![self] = 0]
               /\ term' = [term EXCEPT ![self] = None]
               /\ nval' = [nval EXCEPT ![self] = 0]
               /\ choice' = [choice EXCEPT ![self] = None]
               /\ reply' = [reply EXCEPT ![self] = ""]
               /\ IF done.rec
                     THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                     ELSE /\ pc' = [pc EXCEPT ![self] = "BeginStart"]
               /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                               ambig, crashes, fired, firedN, badRoute, 
                               badFire, stack, key, bodyVal, ckey, val, name, 
                               rk, rv, rf >>

BeginStart(self) == /\ pc[self] = "BeginStart"
                    /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                          /\ ambig' = ambig
                       \/ /\ ambig < MaxAmbig
                          /\ ambig' = ambig + 1
                          /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                       \/ /\ ambig < MaxAmbig
                          /\ ambig' = ambig + 1
                          /\ reply' = [reply EXCEPT ![self] = "err_c"]
                    /\ IF reply'[self] # "err_nc"
                          THEN /\ started' = TRUE
                          ELSE /\ TRUE
                               /\ UNCHANGED started
                    /\ IF reply'[self] # "ok"
                          THEN /\ failed' = [failed EXCEPT ![self] = "error"]
                               /\ pc' = [pc EXCEPT ![self] = "Finish"]
                          ELSE /\ pc' = [pc EXCEPT ![self] = "RunE"]
                               /\ UNCHANGED failed
                    /\ UNCHANGED << res, mk, sw, done, tally, req, dec, nval, 
                                    choice, crashes, fired, firedN, badRoute, 
                                    badFire, stack, key, bodyVal, ckey, val, 
                                    name, it, out, term, rk, rv, rf >>

RunE(self) == /\ pc[self] = "RunE"
              /\ /\ key' = [key EXCEPT ![self] = <<"E", 0>>]
                 /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Node",
                                                          pc        |->  "AfterE",
                                                          bodyVal   |->  bodyVal[self],
                                                          key       |->  key[self] ] >>
                                                      \o stack[self]]
              /\ bodyVal' = [bodyVal EXCEPT ![self] = 0]
              /\ pc' = [pc EXCEPT ![self] = "GRead"]
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, failed, reply, ambig, crashes, 
                              fired, firedN, badRoute, badFire, ckey, val, 
                              name, it, out, term, rk, rv, rf >>

AfterE(self) == /\ pc[self] = "AfterE"
                /\ IF failed[self] # None
                      THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "ChooseE"]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, failed, reply, ambig, crashes, 
                                fired, firedN, badRoute, badFire, stack, key, 
                                bodyVal, ckey, val, name, it, out, term, rk, 
                                rv, rf >>

ChooseE(self) == /\ pc[self] = "ChooseE"
                 /\ /\ ckey' = [ckey EXCEPT ![self] = <<"E", 0>>]
                    /\ name' = [name EXCEPT ![self] = "E"]
                    /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Choose",
                                                             pc        |->  "AfterChooseE",
                                                             ckey      |->  ckey[self],
                                                             val       |->  val[self],
                                                             name      |->  name[self] ] >>
                                                         \o stack[self]]
                    /\ val' = [val EXCEPT ![self] = nval[self]]
                 /\ pc' = [pc EXCEPT ![self] = "CDo"]
                 /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                 nval, choice, failed, reply, ambig, crashes, 
                                 fired, firedN, badRoute, badFire, key, 
                                 bodyVal, it, out, term, rk, rv, rf >>

AfterChooseE(self) == /\ pc[self] = "AfterChooseE"
                      /\ IF failed[self] # None
                            THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                            ELSE /\ IF choice[self] = "Q"
                                       THEN /\ pc' = [pc EXCEPT ![self] = "RunQ"]
                                       ELSE /\ pc' = [pc EXCEPT ![self] = "LoopH"]
                      /\ UNCHANGED << started, res, mk, sw, done, tally, req, 
                                      dec, nval, choice, failed, reply, ambig, 
                                      crashes, fired, firedN, badRoute, 
                                      badFire, stack, key, bodyVal, ckey, val, 
                                      name, it, out, term, rk, rv, rf >>

LoopH(self) == /\ pc[self] = "LoopH"
               /\ /\ key' = [key EXCEPT ![self] = <<"H", it[self]>>]
                  /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Node",
                                                           pc        |->  "AfterH",
                                                           bodyVal   |->  bodyVal[self],
                                                           key       |->  key[self] ] >>
                                                       \o stack[self]]
               /\ bodyVal' = [bodyVal EXCEPT ![self] = 0]
               /\ pc' = [pc EXCEPT ![self] = "GRead"]
               /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                               nval, choice, failed, reply, ambig, crashes, 
                               fired, firedN, badRoute, badFire, ckey, val, 
                               name, it, out, term, rk, rv, rf >>

AfterH(self) == /\ pc[self] = "AfterH"
                /\ IF failed[self] # None
                      THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "RunS"]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, failed, reply, ambig, crashes, 
                                fired, firedN, badRoute, badFire, stack, key, 
                                bodyVal, ckey, val, name, it, out, term, rk, 
                                rv, rf >>

RunS(self) == /\ pc[self] = "RunS"
              /\ /\ key' = [key EXCEPT ![self] = <<"S", it[self]>>]
                 /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Node",
                                                          pc        |->  "AfterS",
                                                          bodyVal   |->  bodyVal[self],
                                                          key       |->  key[self] ] >>
                                                      \o stack[self]]
              /\ bodyVal' = [bodyVal EXCEPT ![self] = 0]
              /\ pc' = [pc EXCEPT ![self] = "GRead"]
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, failed, reply, ambig, crashes, 
                              fired, firedN, badRoute, badFire, ckey, val, 
                              name, it, out, term, rk, rv, rf >>

AfterS(self) == /\ pc[self] = "AfterS"
                /\ IF failed[self] # None
                      THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "ChooseS"]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, failed, reply, ambig, crashes, 
                                fired, firedN, badRoute, badFire, stack, key, 
                                bodyVal, ckey, val, name, it, out, term, rk, 
                                rv, rf >>

ChooseS(self) == /\ pc[self] = "ChooseS"
                 /\ /\ ckey' = [ckey EXCEPT ![self] = SKey(it[self])]
                    /\ name' = [name EXCEPT ![self] = "S"]
                    /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Choose",
                                                             pc        |->  "AfterChooseS",
                                                             ckey      |->  ckey[self],
                                                             val       |->  val[self],
                                                             name      |->  name[self] ] >>
                                                         \o stack[self]]
                    /\ val' = [val EXCEPT ![self] = nval[self]]
                 /\ pc' = [pc EXCEPT ![self] = "CDo"]
                 /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                 nval, choice, failed, reply, ambig, crashes, 
                                 fired, firedN, badRoute, badFire, key, 
                                 bodyVal, it, out, term, rk, rv, rf >>

AfterChooseS(self) == /\ pc[self] = "AfterChooseS"
                      /\ IF failed[self] # None
                            THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                                 /\ UNCHANGED << failed, it >>
                            ELSE /\ IF choice[self] = "back"
                                       THEN /\ IF it[self] + 1 >= MaxIter
                                                  THEN /\ failed' = [failed EXCEPT ![self] = "runaway"]
                                                       /\ pc' = [pc EXCEPT ![self] = "Finish"]
                                                       /\ it' = it
                                                  ELSE /\ it' = [it EXCEPT ![self] = it[self] + 1]
                                                       /\ pc' = [pc EXCEPT ![self] = "LoopH"]
                                                       /\ UNCHANGED failed
                                       ELSE /\ pc' = [pc EXCEPT ![self] = "RunT"]
                                            /\ UNCHANGED << failed, it >>
                      /\ UNCHANGED << started, res, mk, sw, done, tally, req, 
                                      dec, nval, choice, reply, ambig, crashes, 
                                      fired, firedN, badRoute, badFire, stack, 
                                      key, bodyVal, ckey, val, name, out, term, 
                                      rk, rv, rf >>

RunT(self) == /\ pc[self] = "RunT"
              /\ /\ key' = [key EXCEPT ![self] = <<"T", 0>>]
                 /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Node",
                                                          pc        |->  "AfterT",
                                                          bodyVal   |->  bodyVal[self],
                                                          key       |->  key[self] ] >>
                                                      \o stack[self]]
              /\ bodyVal' = [bodyVal EXCEPT ![self] = 0]
              /\ pc' = [pc EXCEPT ![self] = "GRead"]
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, failed, reply, ambig, crashes, 
                              fired, firedN, badRoute, badFire, ckey, val, 
                              name, it, out, term, rk, rv, rf >>

AfterT(self) == /\ pc[self] = "AfterT"
                /\ IF failed[self] # None
                      THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "SetT"]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, failed, reply, ambig, crashes, 
                                fired, firedN, badRoute, badFire, stack, key, 
                                bodyVal, ckey, val, name, it, out, term, rk, 
                                rv, rf >>

SetT(self) == /\ pc[self] = "SetT"
              /\ term' = [term EXCEPT ![self] = "T"]
              /\ out' = [out EXCEPT ![self] = nval[self]]
              /\ pc' = [pc EXCEPT ![self] = "Complete"]
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, failed, reply, ambig, crashes, 
                              fired, firedN, badRoute, badFire, stack, key, 
                              bodyVal, ckey, val, name, it, rk, rv, rf >>

RunQ(self) == /\ pc[self] = "RunQ"
              /\ /\ key' = [key EXCEPT ![self] = <<"Q", 0>>]
                 /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Node",
                                                          pc        |->  "AfterQ",
                                                          bodyVal   |->  bodyVal[self],
                                                          key       |->  key[self] ] >>
                                                      \o stack[self]]
              /\ bodyVal' = [bodyVal EXCEPT ![self] = 0]
              /\ pc' = [pc EXCEPT ![self] = "GRead"]
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, failed, reply, ambig, crashes, 
                              fired, firedN, badRoute, badFire, ckey, val, 
                              name, it, out, term, rk, rv, rf >>

AfterQ(self) == /\ pc[self] = "AfterQ"
                /\ IF failed[self] # None
                      THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "SetQ"]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, failed, reply, ambig, crashes, 
                                fired, firedN, badRoute, badFire, stack, key, 
                                bodyVal, ckey, val, name, it, out, term, rk, 
                                rv, rf >>

SetQ(self) == /\ pc[self] = "SetQ"
              /\ term' = [term EXCEPT ![self] = "Q"]
              /\ out' = [out EXCEPT ![self] = nval[self]]
              /\ pc' = [pc EXCEPT ![self] = "Complete"]
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, failed, reply, ambig, crashes, 
                              fired, firedN, badRoute, badFire, stack, key, 
                              bodyVal, ckey, val, name, it, rk, rv, rf >>

Complete(self) == /\ pc[self] = "Complete"
                  /\ IF Bug # "NoCompletion"
                        THEN /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                   /\ ambig' = ambig
                                \/ /\ ambig < MaxAmbig
                                   /\ ambig' = ambig + 1
                                   /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                \/ /\ ambig < MaxAmbig
                                   /\ ambig' = ambig + 1
                                   /\ reply' = [reply EXCEPT ![self] = "err_c"]
                             /\ IF reply'[self] # "err_nc" /\ ~done.rec
                                   THEN /\ done' = [rec |-> TRUE, v |-> out[self], term |-> term[self]]
                                   ELSE /\ TRUE
                                        /\ done' = done
                             /\ IF reply'[self] # "ok"
                                   THEN /\ failed' = [failed EXCEPT ![self] = "error"]
                                   ELSE /\ TRUE
                                        /\ UNCHANGED failed
                        ELSE /\ TRUE
                             /\ UNCHANGED << done, failed, reply, ambig >>
                  /\ pc' = [pc EXCEPT ![self] = "Finish"]
                  /\ UNCHANGED << started, res, mk, sw, tally, req, dec, nval, 
                                  choice, crashes, fired, firedN, badRoute, 
                                  badFire, stack, key, bodyVal, ckey, val, 
                                  name, it, out, term, rk, rv, rf >>

Finish(self) == /\ pc[self] = "Finish"
                /\ IF failed[self] \notin {None, "runaway", "denied"}
                      THEN /\ pc' = [pc EXCEPT ![self] = "Begin"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "Done"]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, failed, reply, ambig, crashes, 
                                fired, firedN, badRoute, badFire, stack, key, 
                                bodyVal, ckey, val, name, it, out, term, rk, 
                                rv, rf >>

driver(self) == Begin(self) \/ BeginStart(self) \/ RunE(self)
                   \/ AfterE(self) \/ ChooseE(self) \/ AfterChooseE(self)
                   \/ LoopH(self) \/ AfterH(self) \/ RunS(self)
                   \/ AfterS(self) \/ ChooseS(self) \/ AfterChooseS(self)
                   \/ RunT(self) \/ AfterT(self) \/ SetT(self)
                   \/ RunQ(self) \/ AfterQ(self) \/ SetQ(self)
                   \/ Complete(self) \/ Finish(self)

RPick(self) == /\ pc[self] = "RPick"
               /\ \E k \in NodeKeys:
                    \E fl \in RFlows:
                      \E v \in {0, 1}:
                        /\ ~res[k].rec
                        /\ Bug = "ResolveAnyNode" \/ mk[k] # None
                        /\ Bug = "ResolveAnyFlow" \/ fl = "F"
                        /\ rk' = [rk EXCEPT ![self] = k]
                        /\ rv' = [rv EXCEPT ![self] = v]
                        /\ rf' = [rf EXCEPT ![self] = fl]
               /\ pc' = [pc EXCEPT ![self] = "RWrite"]
               /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                               nval, choice, failed, reply, ambig, crashes, 
                               fired, firedN, badRoute, badFire, stack, key, 
                               bodyVal, ckey, val, name, it, out, term >>

RWrite(self) == /\ pc[self] = "RWrite"
                /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                      /\ ambig' = ambig
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                   \/ /\ ambig < MaxAmbig
                      /\ ambig' = ambig + 1
                      /\ reply' = [reply EXCEPT ![self] = "err_c"]
                /\ IF reply'[self] # "err_nc" /\ ~res[rk[self]].rec
                      THEN /\ res' = [res EXCEPT ![rk[self]] = [rec |-> TRUE, v |-> rv[self], by |-> "res", ty |-> rf[self]]]
                      ELSE /\ TRUE
                           /\ res' = res
                /\ pc' = [pc EXCEPT ![self] = "Done"]
                /\ UNCHANGED << started, mk, sw, done, tally, req, dec, nval, 
                                choice, failed, crashes, fired, firedN, 
                                badRoute, badFire, stack, key, bodyVal, ckey, 
                                val, name, it, out, term, rk, rv, rf >>

resolver(self) == RPick(self) \/ RWrite(self)

ADecide(self) == /\ pc[self] = "ADecide"
                 /\ \E k \in {k \in GateKeys : (dec[k][self] = None /\ (req[k] \/ Early)) \/ (dec[k][self] \in {"eyes", "eno"} /\ req[k])}:
                      \E v \in IF req[k] THEN {"yes", "no"} ELSE {"eyes", "eno"}:
                        dec' = [dec EXCEPT ![k][self] = v]
                 /\ IF \E k \in GateKeys : dec'[k][self] \in {None, "eyes", "eno"}
                       THEN /\ pc' = [pc EXCEPT ![self] = "ADecide"]
                       ELSE /\ pc' = [pc EXCEPT ![self] = "Done"]
                 /\ UNCHANGED << started, res, mk, sw, done, tally, req, nval, 
                                 choice, failed, reply, ambig, crashes, fired, 
                                 firedN, badRoute, badFire, stack, key, 
                                 bodyVal, ckey, val, name, it, out, term, rk, 
                                 rv, rf >>

approver(self) == ADecide(self)

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == (\E self \in ProcSet: Node(self) \/ Choose(self))
           \/ (\E self \in Drivers: driver(self))
           \/ (\E self \in ResolverSet: resolver(self))
           \/ (\E self \in ApproverSet: approver(self))
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ \A self \in Drivers : WF_vars(driver(self)) /\ WF_vars(Node(self)) /\ WF_vars(Choose(self))
        /\ \A self \in ResolverSet : WF_vars((pc[self] # "RPick") /\ resolver(self))
        /\ \A self \in ApproverSet : WF_vars(approver(self))

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION
-----------------------------------------------------------------------------

\* A driver crash: the drive restarts from Begin with an empty stack.
Crash(d) ==
  /\ crashes < MaxCrash
  /\ pc[d] \notin {"Begin", "Done"}
  /\ crashes' = crashes + 1
  /\ pc' = [pc EXCEPT ![d] = "Begin"]
  /\ stack' = [stack EXCEPT ![d] = <<>>]
  /\ nval' = [nval EXCEPT ![d] = 0]
  /\ choice' = [choice EXCEPT ![d] = None]
  /\ failed' = [failed EXCEPT ![d] = None]
  /\ reply' = [reply EXCEPT ![d] = ""]
  /\ bodyVal' = [bodyVal EXCEPT ![d] = 0]
  /\ it' = [it EXCEPT ![d] = 0]
  /\ out' = [out EXCEPT ![d] = 0]
  /\ term' = [term EXCEPT ![d] = None]
  /\ key' = [key EXCEPT ![d] = defaultInitValue]
  /\ ckey' = [ckey EXCEPT ![d] = defaultInitValue]
  /\ val' = [val EXCEPT ![d] = defaultInitValue]
  /\ name' = [name EXCEPT ![d] = defaultInitValue]
  /\ UNCHANGED <<started, res, mk, sw, done, tally, req, dec, ambig, fired, firedN, badRoute, badFire, rk, rv, rf>>

FullNext == Next \/ \E d \in Drivers : Crash(d)

Fairness ==
  /\ \A self \in Drivers : WF_vars(driver(self)) /\ WF_vars(Node(self)) /\ WF_vars(Choose(self))
  /\ \A self \in ResolverSet : WF_vars((pc[self] # "RPick") /\ resolver(self))
  /\ \A self \in ApproverSet : WF_vars(approver(self))

FullSpec == Init /\ [][FullNext]_vars /\ Fairness

(***************************************************************************)
(* Properties                                                               *)
(***************************************************************************)

\* Each node that is not retry-safe fires at most once per iteration.
AtMostOncePerIteration == \A k \in SideKeys : fired[k] <= 1

\* The nested Step fires at most once per iteration, and an iteration the driver recorded ran it.
NestedOncePerIteration ==
  \A i \in Iters : /\ firedN[i] <= 1
                   /\ (res[<<"H", i>>].rec /\ res[<<"H", i>>].by = "drv") => firedN[i] = 1

\* Conform: the journal holds a declared path. Every recorded node result and choice is on the
\* path the recorded choices take, every choice follows its predicate over the recorded value,
\* and run:complete holds the output of the terminal that path reaches.
Conform ==
  /\ \A k \in NodeKeys : res[k].rec => Allowed(k)
  /\ \A k \in ChoiceKeys : sw[k] # None =>
        /\ Allowed(k) /\ res[k].rec /\ sw[k] = Pred(k[1], res[k].v)
  /\ done.rec => /\ Reached[1] = done.term
                 /\ res[Reached].rec /\ res[Reached].v = done.v

\* Every route a driver took followed the declared predicate over its iteration's value.
RoutesFollowDeclared == ~badRoute

\* Every recorded node output is one this flow's node types read.
ResultsTyped == \A k \in Keys : res[k].rec => res[k].ty = "F"

\* A gated node's body runs only after Need approvals are recorded for its key (its iteration's,
\* in the loop), across crashes, error replies and drivers.
GateHeld == ~badFire

\* A gated node's result is recorded only for a key with Need approvals, whoever recorded it (a
\* driver, or the resolver after a halt).
GatedResults == \A k \in GateKeys : res[k].rec => Yes(k) >= Need

\* A recorded tally says what the decisions say: a pass has Need approvals, a deny can never reach
\* them.
TallySound == \A k \in GateKeys :
  /\ tally[k] = "pass" => Yes(k) >= Need
  /\ tally[k] = "deny" => Cardinality(Approvers) - No(k) < Need

\* Vacuity, expected to be violated: the flow completes after an effect fired.
EffectNotReachable == ~(done.rec /\ \E k \in NodeKeys : fired[k] > 0)

\* Liveness: a run whose drives do not crash or halt completes (recovery skips it then).
Completes == <>(done.rec \/ \E d \in Drivers : failed[d] \in {"halt", "runaway", "denied"})
=============================================================================
