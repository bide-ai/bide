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
(*                                                                          *)
(* Taint (docs/design/flow-taint.md, A2 to A5b). A run records its          *)
(* declarations in run:start (rdecl: the policy, the exempt nodes, the      *)
(* sources), first writer wins. Each drive compares its own declarations    *)
(* with the recorded ones exactly and refuses on any difference (a weaker   *)
(* policy included). A node's read set is a fold of the journal in journal  *)
(* order (ord): the declared sources with a result earlier than the node's  *)
(* own record (or any, if it has none), the taint-mark records of other     *)
(* nodes, and the flow input if declared untrusted; a node that is both     *)
(* source and side effect does not read itself. A side-effecting,           *)
(* non-exempt node with a non-empty read set, under a policy, first records *)
(* a gate-reason (grec), then under Refuse is refused (no gate, request,    *)
(* record or claim, every drive), else passes its own gate and then the     *)
(* second gate instance node:[iter:n:]name:taint (treq, tdec, ttally),      *)
(* before the attempt claim. SeparateApprovers excludes from the instance   *)
(* the approvers the own gate's terminal tally counted (ncnt).              *)
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
  Sources,     \* node names declared Untrusted, and "input" if the run declares its input
               \* untrusted: a subset of {"E","Q","T","H","S","input"}
  Exempt,      \* node names declared TaintExempt, a subset of SideEffects
  MarkNodes,   \* nodes whose body can run a child that learns a taint mark
  DeclNames,   \* the declaration sets a drive can run under: "none" (no policy), "gate",
               \* "refuse", "dropsrc" (Gate with the source declarations dropped), "exempt"
               \* (Gate with every side effect exempt)
  TNeed,       \* approvals the second gate instance needs
  Separate,    \* TaintPolicy.SeparateApprovers
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
ASSUME Sources \subseteq {"E", "Q", "T", "H", "S", "input"}
ASSUME Exempt \subseteq SideEffects /\ MarkNodes \subseteq {"E", "Q", "T", "H", "S"}
ASSUME DeclNames \subseteq {"none", "gate", "refuse", "dropsrc", "exempt"} /\ DeclNames # {}
ASSUME (\A n \in DeclNames : n # "gate") \/ TNeed \in 1..Cardinality(Approvers)
ASSUME Separate \in BOOLEAN
ASSUME Bug \in {"none", "NestedUnscoped", "LoopSwitchUnscoped", "ResolveAnyNode", "ResolveAnyFlow",
                "NoCompletion", "NoGate", "GateUnscoped", "NoRequestOrder",
                "NoTaintGate", "TaintGateUnscoped", "TaintReuse", "ReadSetOnResumeEmpty",
                "ReadSetIgnoresOrder", "ReadSetResetsPerIteration", "TaintMarkIgnored",
                "RefuseAsSkip", "RefuseNotDurable", "ResumeNoCompare", "TaintReusesNodeDecider",
                "NoPreExec", "PreExecIgnoresRevocation", "RunBeforeRefusal"}

\* The declarations a run records in run:start and a drive runs under (A2, "Declarations are
\* recorded"): the policy, the exempt nodes, the declared sources.
NoDecl == [pol |-> "none", ex |-> {}, src |-> {}]
DeclOf(n) ==
  CASE n = "none"    -> [pol |-> "none",   ex |-> Exempt,      src |-> Sources]
    [] n = "gate"    -> [pol |-> "gate",   ex |-> Exempt,      src |-> Sources]
    [] n = "refuse"  -> [pol |-> "refuse", ex |-> Exempt,      src |-> Sources]
    [] n = "dropsrc" -> [pol |-> "gate",   ex |-> Exempt,      src |-> {}]
    [] n = "exempt"  -> [pol |-> "gate",   ex |-> SideEffects, src |-> Sources]
Decls == {DeclOf(n) : n \in DeclNames}
\* The keys whose second gate instance exists: the side-effecting nodes, when a Gate policy is possible.
TKeys == IF \E d \in Decls : d.pol = "gate" THEN SideKeys ELSE {}
MarkKeys == {k \in NodeKeys : k[1] \in MarkNodes}
\* Whether any declaration set carries a policy: only then does the model track the journal's order.
Track == \E d \in Decls : d.pol # "none"
ApproverSet == IF Gated = {} /\ \A d \in Decls : d.pol # "gate" THEN {} ELSE Approvers


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
  badFire = FALSE,
  \* Taint. rdecl: the declarations run:start records (first writer wins); cur: the declarations
  \* a drive runs under now (an operator may have changed them); dd: the ones its fold uses.
  rdecl   = NoDecl,
  cur     = [d \in Drivers |-> NoDecl],
  dd      = [d \in Drivers |-> NoDecl],
  \* The journal's order: the next position, and the position of each node record and of each
  \* taint-mark record (taint-mark:<node key>:<source>, written by a child's mark sink).
  jn      = 1,
  ord     = [k \in Keys |-> 0],
  tm      = [k \in NodeKeys |-> FALSE],
  mord    = [k \in NodeKeys |-> 0],
  \* A drive's in-memory memory of the sources it ran (used only by ReadSetOnResumeEmpty).
  seen    = [d \in Drivers |-> {}],
  \* The gate-reason record; a refusal (ghost: the node was refused, always, in every drive).
  grec    = [k \in NodeKeys |-> FALSE],
  refused = [k \in NodeKeys |-> FALSE],
  \* The second gate instance node:...:taint of a side-effecting node: request, decisions (each
  \* approver's first valid one), terminal tally; and the approvers the node's own gate's
  \* terminal tally counted (SeparateApprovers excludes them from the instance).
  treq    = [k \in TKeys |-> FALSE],
  tdec    = [k \in TKeys |-> [a \in Approvers |-> None]],
  ttally  = [k \in TKeys |-> None],
  ncnt    = [k \in GateKeys |-> {}],
  \* Ghosts: an attempt claimed with a non-empty read set and no passed taint gate and no
  \* exemption, under a policy; a node body that ran under declarations other than the recorded.
  badTaint = FALSE,
  badResume = FALSE,
  \* PreExec (A2 step 4): an approval certificate can be revoked after the gates pass (revoked, once, by
  \* the environment); a drive's PreExec just before the claim records whether it ran (pre) and
  \* whether it saw the revocation (preRev). Ghosts: an approved attempt claimed without a PreExec
  \* that saw no revocation (badPre); an attempt claimed with no gate-reason record journaled
  \* first (badOrder).
  revoked = FALSE,
  pre     = [d \in Drivers |-> FALSE],
  preRev  = [d \in Drivers |-> FALSE],
  badPre  = FALSE,
  badOrder = FALSE;

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

  \* ---- Taint ----
  \* The second gate instance's key (per iteration, like the node's; TaintGateUnscoped keys every
  \* iteration as the first).
  TKey(k) == IF Bug = "TaintGateUnscoped" THEN <<k[1], 0>> ELSE k
  \* The approvers the node's own gate's terminal tally counted, whom SeparateApprovers excludes from
  \* the instance (TaintReusesNodeDecider: the flag ignored).
  Excl(k) == IF Separate /\ k[1] \in Gated /\ Bug # "TaintReusesNodeDecider" THEN ncnt[k] ELSE {}
  TElig(k) == Approvers \ Excl(k)
  \* The instance's counted decisions (TaintReuse: the node gate's decisions count for it).
  TYes(k) == IF Bug = "TaintReuse" THEN Yes(k)
             ELSE Cardinality({a \in TElig(k) : tdec[TKey(k)][a] = "yes"})
  TNo(k)  == IF Bug = "TaintReuse" THEN No(k)
             ELSE Cardinality({a \in TElig(k) : tdec[TKey(k)][a] = "no"})
  \* A node's place in the journal: its own record if it has one, else the end.
  Pos(k) == IF res[k].rec THEN ord[k] ELSE jn
  \* The read set of node k is non-empty, folded from the journal by drive me under declarations d:
  \* declared sources with a result earlier than k's place (k itself never counts), taint-mark
  \* records of other nodes earlier than that, and the flow input if declared untrusted.
  Reads(me, k, d) ==
    \/ "input" \in d.src
    \/ \E s \in NodeKeys \ {k} :
         /\ s[1] \in d.src /\ res[s].rec
         /\ IF Bug = "ReadSetOnResumeEmpty" THEN s \in seen[me] ELSE TRUE
         /\ IF Bug = "ReadSetIgnoresOrder" THEN ord[s] > Pos(k) ELSE ord[s] < Pos(k)
         /\ (Bug # "ReadSetResetsPerIteration" \/ s[1] \notin LoopNames \/ s[2] = k[2])
    \/ /\ Bug # "TaintMarkIgnored"
       /\ \E j \in NodeKeys \ {k} : tm[j] /\ mord[j] < Pos(k)
  \* The requirement: tracked (side effect), not exempt, a policy, a non-empty read set
  \* (RefuseNotDurable: a recorded refusal is not re-applied).
  TaintNeed(me, k) ==
    /\ Bug # "NoTaintGate"
    /\ k[1] \in SideEffects /\ dd[me].pol # "none" /\ k[1] \notin dd[me].ex
    /\ Reads(me, k, dd[me])
    /\ ~(Bug = "RefuseNotDurable" /\ dd[me].pol = "refuse" /\ grec[k])
    /\ ~(Bug = "RunBeforeRefusal" /\ dd[me].pol = "refuse")
  \* The ground truth when the attempt is claimed: the recorded declarations, and everything the
  \* journal holds then.
  TrueReads(k) ==
    \/ "input" \in rdecl.src
    \/ \E s \in NodeKeys \ {k} : s[1] \in rdecl.src /\ res[s].rec
    \/ \E j \in NodeKeys \ {k} : tm[j]
  TaintBreach(k) ==
    /\ rdecl.pol # "none" /\ k[1] \notin rdecl.ex /\ TrueReads(k) /\ ~(k \in TKeys /\ ttally[k] = "pass")

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
variables bodyVal = 0, tneed = FALSE;
begin
GRead:
  \* The taint requirement, folded from the journal, and its gate-reason record (insert-once, also
  \* written for a refused node). Under Refuse the node is refused before any gate. Then runGate,
  \* then flowGate: a node with no gate goes to its taint gate or Step; a recorded tally decides.
  tneed := TaintNeed(self, key);
  pre[self] := FALSE; preRev[self] := FALSE;
  if tneed then grec[key] := TRUE; end if;
  if tneed /\ dd[self].pol = "refuse" then goto TRefuse;
  elsif key[1] \notin Gated \/ Bug = "NoGate" then
    if tneed then goto TGate; else goto NGet; end if;
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
        ncnt[GKey(key)] := IF Separate THEN {a \in Approvers : dec[GKey(key)][a] = "yes"} ELSE {};
      end if;
      if reply[self] # "ok" then failed[self] := "error"; return; end if;
    else
      failed[self] := "pause"; return;
    end if;
  end with;
GPause:
  \* A denying tally stops the run (ErrApprovalDenied); a passing one lets the node run.
  if tally[GKey(key)] = "deny" then failed[self] := "denied"; return;
  elsif tneed then goto TGate;
  elsif Track then goto TPre;
  end if;
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
    if reply[self] # "err_nc" /\ was = None then
      mk[key] := self;
      badTaint := badTaint \/ TaintBreach(key);
      badPre := badPre \/ (Track /\ (key[1] \in Gated \/ tneed) /\ (~pre[self] \/ preRev[self]));
      badOrder := badOrder \/ (Track /\ rdecl.pol # "none" /\ key[1] \notin rdecl.ex /\ TrueReads(key) /\ ~grec[key]);
    end if;
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
  if cur[self] # rdecl then badResume := TRUE; end if;
  if key[1] \in MarkNodes /\ ~tm[key] then
    \* A child learned a taint mark: the sink writes taint-mark:<node key>:<source> before the
    \* node's result (insert-once).
    either skip;
    or tm[key] := TRUE; mord[key] := IF Track THEN jn ELSE 0; jn := IF Track THEN jn + 1 ELSE jn;
    end either;
  end if;
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
    ord[key] := IF Track THEN jn ELSE 0; jn := IF Track THEN jn + 1 ELSE jn;
    seen[self] := IF Bug = "ReadSetOnResumeEmpty" THEN seen[self] \cup {key} ELSE {};
  end if;
  if reply[self] # "ok" then
    failed[self] := "error";
  else
    nval[self] := IF res[key].rec THEN res[key].v ELSE bodyVal;
  end if;
  return;
TRefuse:
  \* Refuse: the gate-reason record is written (GRead), then the sentinel is returned before any
  \* gate: no node record, no attempt, no approval request. The run stays unfinished and every drive
  \* re-refuses. RefuseAsSkip: the node is skipped (a record with a default output) instead.
  refused[key] := TRUE;
  if Bug = "RefuseAsSkip" then
    res[key] := [rec |-> TRUE, v |-> 0, by |-> "drv", ty |-> "F"];
    ord[key] := IF Track THEN jn ELSE 0; jn := IF Track THEN jn + 1 ELSE jn;
    nval[self] := 0;
  else
    failed[self] := "refused";
  end if;
  return;
TGate:
  \* The second gate instance node:[iter:<n>:]<name>:taint, after the node's own gate and before the
  \* attempt claim: its request is recorded once, before any count.
  if ttally[TKey(key)] # None then goto TPause;
  else treq[TKey(key)] := TRUE;
  end if;
TCount:
  \* The instance's count: its own decisions (SeparateApprovers: not those the own gate counted).
  \* A terminal count is recorded; when the approvers left cannot reach the need it is a denial.
  with y = TYes(key), n = TNo(key) do
    if y >= TNeed \/ Cardinality(TElig(key)) - n < TNeed then
      Reply(reply[self]);
      if reply[self] # "err_nc" /\ ttally[TKey(key)] = None then
        ttally[TKey(key)] := IF y >= TNeed THEN "pass" ELSE "deny";
      end if;
      if reply[self] # "ok" then failed[self] := "error"; return; end if;
    else
      failed[self] := "pause"; return;
    end if;
  end with;
TPause:
  if ttally[TKey(key)] = "deny" then failed[self] := "denied"; return;
  else goto TPre;
  end if;
TPre:
  \* Both PreExecs, node gate then instance, immediately before runNode: an approval whose
  \* certificate was revoked after the gates passed must stop the node, before the claim.
  \* (NoPreExec: not run; PreExecIgnoresRevocation: run, but its result ignored.)
  if Bug # "NoPreExec" /\ revoked /\ Bug # "PreExecIgnoresRevocation" then
    pre[self] := TRUE; preRev[self] := TRUE; failed[self] := "error"; return;
  elsif Bug # "NoPreExec" then
    pre[self] := TRUE; preRev[self] := revoked; goto NGet;
  else
    goto NGet;
  end if;
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
  nval[self] := 0; choice[self] := None; reply[self] := ""; seen[self] := {};
  \* The declarations this drive runs under: the operator may have changed them since the start.
  with c \in Decls do cur[self] := c; end with;
  if done.rec then goto Finish; end if;
BeginStart:
  \* run:start records the declarations (first writer wins). A drive reads the journaled start and
  \* runs under IT; it compares the recorded declarations with its own exactly, and any difference
  \* (a weaker policy, a dropped declaration) is an ErrConfig before the first node.
  Reply(reply[self]);
  if reply[self] # "err_nc" then
    if ~started then rdecl := cur[self]; end if;
    started := TRUE;
    dd[self] := IF Bug = "ResumeNoCompare" \/ ~started THEN cur[self] ELSE rdecl;
  end if;
  if reply[self] # "ok" then failed[self] := "error"; goto Finish;
  elsif Bug # "ResumeNoCompare" /\ started /\ cur[self] # rdecl then failed[self] := "config"; goto Finish;
  end if;
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
    ord[rk] := IF Track THEN jn ELSE 0; jn := IF Track THEN jn + 1 ELSE jn;
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
  either
    with k \in {k \in GateKeys : (dec[k][self] = None /\ (req[k] \/ Early)) \/ (dec[k][self] \in {"eyes", "eno"} /\ req[k])},
         v \in IF req[k] THEN {"yes", "no"} ELSE {"eyes", "eno"} do
      dec[k][self] := v;
    end with;
  or
    \* A decision on the second gate instance (no early decisions: Approve and SubmitDecision refuse
    \* one before the request, and this model keeps Early for the node gates).
    with k \in {k \in TKeys : tdec[k][self] = None /\ treq[k]}, v \in {"yes", "no"} do
      tdec[k][self] := v;
    end with;
  end either;
  if \E k \in GateKeys : dec[k][self] \in {None, "eyes", "eno"} then goto ADecide;
  elsif \E k \in TKeys : tdec[k][self] = None then goto ADecide;
  end if;
end process;
end algorithm; *)
\* BEGIN TRANSLATION
CONSTANT defaultInitValue
VARIABLES started, res, mk, sw, done, tally, req, dec, nval, choice, failed, 
          reply, ambig, crashes, fired, firedN, badRoute, badFire, rdecl, cur, 
          dd, jn, ord, tm, mord, seen, grec, refused, treq, tdec, ttally, 
          ncnt, badTaint, badResume, revoked, pre, preRev, badPre, badOrder, 
          pc, stack

(* define statement *)
SKey(i) == IF Bug = "LoopSwitchUnscoped" THEN <<"S", 0>> ELSE <<"S", i>>

NKey(i) == IF Bug = "NestedUnscoped" THEN <<"N", 0>> ELSE <<"N", i>>

GKey(k) == IF Bug = "GateUnscoped" THEN <<k[1], 0>> ELSE k

Yes(k) == Cardinality({a \in Approvers : dec[k][a] = "yes"})
No(k)  == Cardinality({a \in Approvers : dec[k][a] = "no"})

GYes(k) == IF Bug = "NoRequestOrder" THEN Cardinality({a \in Approvers : dec[k][a] \in {"yes", "eyes"}}) ELSE Yes(k)
GNo(k)  == IF Bug = "NoRequestOrder" THEN Cardinality({a \in Approvers : dec[k][a] \in {"no", "eno"}}) ELSE No(k)




TKey(k) == IF Bug = "TaintGateUnscoped" THEN <<k[1], 0>> ELSE k


Excl(k) == IF Separate /\ k[1] \in Gated /\ Bug # "TaintReusesNodeDecider" THEN ncnt[k] ELSE {}
TElig(k) == Approvers \ Excl(k)

TYes(k) == IF Bug = "TaintReuse" THEN Yes(k)
           ELSE Cardinality({a \in TElig(k) : tdec[TKey(k)][a] = "yes"})
TNo(k)  == IF Bug = "TaintReuse" THEN No(k)
           ELSE Cardinality({a \in TElig(k) : tdec[TKey(k)][a] = "no"})

Pos(k) == IF res[k].rec THEN ord[k] ELSE jn



Reads(me, k, d) ==
  \/ "input" \in d.src
  \/ \E s \in NodeKeys \ {k} :
       /\ s[1] \in d.src /\ res[s].rec
       /\ IF Bug = "ReadSetOnResumeEmpty" THEN s \in seen[me] ELSE TRUE
       /\ IF Bug = "ReadSetIgnoresOrder" THEN ord[s] > Pos(k) ELSE ord[s] < Pos(k)
       /\ (Bug # "ReadSetResetsPerIteration" \/ s[1] \notin LoopNames \/ s[2] = k[2])
  \/ /\ Bug # "TaintMarkIgnored"
     /\ \E j \in NodeKeys \ {k} : tm[j] /\ mord[j] < Pos(k)


TaintNeed(me, k) ==
  /\ Bug # "NoTaintGate"
  /\ k[1] \in SideEffects /\ dd[me].pol # "none" /\ k[1] \notin dd[me].ex
  /\ Reads(me, k, dd[me])
  /\ ~(Bug = "RefuseNotDurable" /\ dd[me].pol = "refuse" /\ grec[k])
  /\ ~(Bug = "RunBeforeRefusal" /\ dd[me].pol = "refuse")


TrueReads(k) ==
  \/ "input" \in rdecl.src
  \/ \E s \in NodeKeys \ {k} : s[1] \in rdecl.src /\ res[s].rec
  \/ \E j \in NodeKeys \ {k} : tm[j]
TaintBreach(k) ==
  /\ rdecl.pol # "none" /\ k[1] \notin rdecl.ex /\ TrueReads(k) /\ ~(k \in TKeys /\ ttally[k] = "pass")


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

VARIABLES key, bodyVal, tneed, ckey, val, name, it, out, term, rk, rv, rf

vars == << started, res, mk, sw, done, tally, req, dec, nval, choice, failed, 
           reply, ambig, crashes, fired, firedN, badRoute, badFire, rdecl, 
           cur, dd, jn, ord, tm, mord, seen, grec, refused, treq, tdec, 
           ttally, ncnt, badTaint, badResume, revoked, pre, preRev, badPre, 
           badOrder, pc, stack, key, bodyVal, tneed, ckey, val, name, it, out, 
           term, rk, rv, rf >>

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
        /\ rdecl = NoDecl
        /\ cur = [d \in Drivers |-> NoDecl]
        /\ dd = [d \in Drivers |-> NoDecl]
        /\ jn = 1
        /\ ord = [k \in Keys |-> 0]
        /\ tm = [k \in NodeKeys |-> FALSE]
        /\ mord = [k \in NodeKeys |-> 0]
        /\ seen = [d \in Drivers |-> {}]
        /\ grec = [k \in NodeKeys |-> FALSE]
        /\ refused = [k \in NodeKeys |-> FALSE]
        /\ treq = [k \in TKeys |-> FALSE]
        /\ tdec = [k \in TKeys |-> [a \in Approvers |-> None]]
        /\ ttally = [k \in TKeys |-> None]
        /\ ncnt = [k \in GateKeys |-> {}]
        /\ badTaint = FALSE
        /\ badResume = FALSE
        /\ revoked = FALSE
        /\ pre = [d \in Drivers |-> FALSE]
        /\ preRev = [d \in Drivers |-> FALSE]
        /\ badPre = FALSE
        /\ badOrder = FALSE
        (* Procedure Node *)
        /\ key = [ self \in ProcSet |-> defaultInitValue]
        /\ bodyVal = [ self \in ProcSet |-> 0]
        /\ tneed = [ self \in ProcSet |-> FALSE]
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
               /\ tneed' = [tneed EXCEPT ![self] = TaintNeed(self, key[self])]
               /\ pre' = [pre EXCEPT ![self] = FALSE]
               /\ preRev' = [preRev EXCEPT ![self] = FALSE]
               /\ IF tneed'[self]
                     THEN /\ grec' = [grec EXCEPT ![key[self]] = TRUE]
                     ELSE /\ TRUE
                          /\ grec' = grec
               /\ IF tneed'[self] /\ dd[self].pol = "refuse"
                     THEN /\ pc' = [pc EXCEPT ![self] = "TRefuse"]
                          /\ req' = req
                     ELSE /\ IF key[self][1] \notin Gated \/ Bug = "NoGate"
                                THEN /\ IF tneed'[self]
                                           THEN /\ pc' = [pc EXCEPT ![self] = "TGate"]
                                           ELSE /\ pc' = [pc EXCEPT ![self] = "NGet"]
                                     /\ req' = req
                                ELSE /\ IF tally[GKey(key[self])] # None
                                           THEN /\ pc' = [pc EXCEPT ![self] = "GPause"]
                                                /\ req' = req
                                           ELSE /\ req' = [req EXCEPT ![GKey(key[self])] = TRUE]
                                                /\ pc' = [pc EXCEPT ![self] = "GCount"]
               /\ UNCHANGED << started, res, mk, sw, done, tally, dec, nval, 
                               choice, failed, reply, ambig, crashes, fired, 
                               firedN, badRoute, badFire, rdecl, cur, dd, jn, 
                               ord, tm, mord, seen, refused, treq, tdec, 
                               ttally, ncnt, badTaint, badResume, revoked, 
                               badPre, badOrder, stack, key, bodyVal, ckey, 
                               val, name, it, out, term, rk, rv, rf >>

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
                                          /\ ncnt' = [ncnt EXCEPT ![GKey(key[self])] = IF Separate THEN {a \in Approvers : dec[GKey(key[self])][a] = "yes"} ELSE {}]
                                     ELSE /\ TRUE
                                          /\ UNCHANGED << tally, ncnt >>
                               /\ IF reply'[self] # "ok"
                                     THEN /\ failed' = [failed EXCEPT ![self] = "error"]
                                          /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                          /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                                          /\ tneed' = [tneed EXCEPT ![self] = Head(stack[self]).tneed]
                                          /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                                          /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                     ELSE /\ pc' = [pc EXCEPT ![self] = "GPause"]
                                          /\ UNCHANGED << failed, stack, key, 
                                                          bodyVal, tneed >>
                          ELSE /\ failed' = [failed EXCEPT ![self] = "pause"]
                               /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                               /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                               /\ tneed' = [tneed EXCEPT ![self] = Head(stack[self]).tneed]
                               /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                               /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                               /\ UNCHANGED << tally, reply, ambig, ncnt >>
                /\ UNCHANGED << started, res, mk, sw, done, req, dec, nval, 
                                choice, crashes, fired, firedN, badRoute, 
                                badFire, rdecl, cur, dd, jn, ord, tm, mord, 
                                seen, grec, refused, treq, tdec, ttally, 
                                badTaint, badResume, revoked, pre, preRev, 
                                badPre, badOrder, ckey, val, name, it, out, 
                                term, rk, rv, rf >>

GPause(self) == /\ pc[self] = "GPause"
                /\ IF tally[GKey(key[self])] = "deny"
                      THEN /\ failed' = [failed EXCEPT ![self] = "denied"]
                           /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                           /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                           /\ tneed' = [tneed EXCEPT ![self] = Head(stack[self]).tneed]
                           /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                           /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                      ELSE /\ IF tneed[self]
                                 THEN /\ pc' = [pc EXCEPT ![self] = "TGate"]
                                 ELSE /\ IF Track
                                            THEN /\ pc' = [pc EXCEPT ![self] = "TPre"]
                                            ELSE /\ pc' = [pc EXCEPT ![self] = "NGet"]
                           /\ UNCHANGED << failed, stack, key, bodyVal, tneed >>
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, reply, ambig, crashes, fired, 
                                firedN, badRoute, badFire, rdecl, cur, dd, jn, 
                                ord, tm, mord, seen, grec, refused, treq, tdec, 
                                ttally, ncnt, badTaint, badResume, revoked, 
                                pre, preRev, badPre, badOrder, ckey, val, name, 
                                it, out, term, rk, rv, rf >>

NGet(self) == /\ pc[self] = "NGet"
              /\ IF res[key[self]].rec
                    THEN /\ nval' = [nval EXCEPT ![self] = res[key[self]].v]
                         /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                         /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                         /\ tneed' = [tneed EXCEPT ![self] = Head(stack[self]).tneed]
                         /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                         /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                    ELSE /\ IF key[self][1] \notin SideEffects
                               THEN /\ pc' = [pc EXCEPT ![self] = "NBody"]
                               ELSE /\ pc' = [pc EXCEPT ![self] = "NClaim"]
                         /\ UNCHANGED << nval, stack, key, bodyVal, tneed >>
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              choice, failed, reply, ambig, crashes, fired, 
                              firedN, badRoute, badFire, rdecl, cur, dd, jn, 
                              ord, tm, mord, seen, grec, refused, treq, tdec, 
                              ttally, ncnt, badTaint, badResume, revoked, pre, 
                              preRev, badPre, badOrder, ckey, val, name, it, 
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
                                /\ badTaint' = (badTaint \/ TaintBreach(key[self]))
                                /\ badPre' = (badPre \/ (Track /\ (key[self][1] \in Gated \/ tneed[self]) /\ (~pre[self] \/ preRev[self])))
                                /\ badOrder' = (badOrder \/ (Track /\ rdecl.pol # "none" /\ key[self][1] \notin rdecl.ex /\ TrueReads(key[self]) /\ ~grec[key[self]]))
                           ELSE /\ TRUE
                                /\ UNCHANGED << mk, badTaint, badPre, badOrder >>
                     /\ IF reply'[self] # "ok"
                           THEN /\ failed' = [failed EXCEPT ![self] = "error"]
                                /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                                /\ tneed' = [tneed EXCEPT ![self] = Head(stack[self]).tneed]
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
                                           /\ tneed' = [tneed EXCEPT ![self] = Head(stack[self]).tneed]
                                           /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                                           /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                      ELSE /\ pc' = [pc EXCEPT ![self] = "NBody"]
                                           /\ UNCHANGED << nval, failed, stack, 
                                                           key, bodyVal, tneed >>
                /\ UNCHANGED << started, res, sw, done, tally, req, dec, 
                                choice, crashes, fired, firedN, badRoute, 
                                badFire, rdecl, cur, dd, jn, ord, tm, mord, 
                                seen, grec, refused, treq, tdec, ttally, ncnt, 
                                badResume, revoked, pre, preRev, ckey, val, 
                                name, it, out, term, rk, rv, rf >>

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
               /\ IF cur[self] # rdecl
                     THEN /\ badResume' = TRUE
                     ELSE /\ TRUE
                          /\ UNCHANGED badResume
               /\ IF key[self][1] \in MarkNodes /\ ~tm[key[self]]
                     THEN /\ \/ /\ TRUE
                                /\ UNCHANGED <<jn, tm, mord>>
                             \/ /\ tm' = [tm EXCEPT ![key[self]] = TRUE]
                                /\ mord' = [mord EXCEPT ![key[self]] = IF Track THEN jn ELSE 0]
                                /\ jn' = IF Track THEN jn + 1 ELSE jn
                     ELSE /\ TRUE
                          /\ UNCHANGED << jn, tm, mord >>
               /\ IF key[self][1] = "H"
                     THEN /\ pc' = [pc EXCEPT ![self] = "NNested"]
                     ELSE /\ pc' = [pc EXCEPT ![self] = "NRecord"]
               /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                               nval, choice, failed, reply, ambig, crashes, 
                               firedN, badRoute, rdecl, cur, dd, ord, seen, 
                               grec, refused, treq, tdec, ttally, ncnt, 
                               badTaint, revoked, pre, preRev, badPre, 
                               badOrder, stack, key, tneed, ckey, val, name, 
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
                                 badRoute, badFire, rdecl, cur, dd, jn, ord, 
                                 tm, mord, seen, grec, refused, treq, tdec, 
                                 ttally, ncnt, badTaint, badResume, revoked, 
                                 pre, preRev, badPre, badOrder, stack, key, 
                                 bodyVal, tneed, ckey, val, name, it, out, 
                                 term, rk, rv, rf >>

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
                            /\ ord' = [ord EXCEPT ![key[self]] = IF Track THEN jn ELSE 0]
                            /\ jn' = IF Track THEN jn + 1 ELSE jn
                            /\ seen' = [seen EXCEPT ![self] = IF Bug = "ReadSetOnResumeEmpty" THEN seen[self] \cup {key[self]} ELSE {}]
                       ELSE /\ TRUE
                            /\ UNCHANGED << res, jn, ord, seen >>
                 /\ IF reply'[self] # "ok"
                       THEN /\ failed' = [failed EXCEPT ![self] = "error"]
                            /\ nval' = nval
                       ELSE /\ nval' = [nval EXCEPT ![self] = IF res'[key[self]].rec THEN res'[key[self]].v ELSE bodyVal[self]]
                            /\ UNCHANGED failed
                 /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                 /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                 /\ tneed' = [tneed EXCEPT ![self] = Head(stack[self]).tneed]
                 /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                 /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                 /\ UNCHANGED << started, mk, sw, done, tally, req, dec, 
                                 choice, crashes, fired, firedN, badRoute, 
                                 badFire, rdecl, cur, dd, tm, mord, grec, 
                                 refused, treq, tdec, ttally, ncnt, badTaint, 
                                 badResume, revoked, pre, preRev, badPre, 
                                 badOrder, ckey, val, name, it, out, term, rk, 
                                 rv, rf >>

TRefuse(self) == /\ pc[self] = "TRefuse"
                 /\ refused' = [refused EXCEPT ![key[self]] = TRUE]
                 /\ IF Bug = "RefuseAsSkip"
                       THEN /\ res' = [res EXCEPT ![key[self]] = [rec |-> TRUE, v |-> 0, by |-> "drv", ty |-> "F"]]
                            /\ ord' = [ord EXCEPT ![key[self]] = IF Track THEN jn ELSE 0]
                            /\ jn' = IF Track THEN jn + 1 ELSE jn
                            /\ nval' = [nval EXCEPT ![self] = 0]
                            /\ UNCHANGED failed
                       ELSE /\ failed' = [failed EXCEPT ![self] = "refused"]
                            /\ UNCHANGED << res, nval, jn, ord >>
                 /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                 /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                 /\ tneed' = [tneed EXCEPT ![self] = Head(stack[self]).tneed]
                 /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                 /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                 /\ UNCHANGED << started, mk, sw, done, tally, req, dec, 
                                 choice, reply, ambig, crashes, fired, firedN, 
                                 badRoute, badFire, rdecl, cur, dd, tm, mord, 
                                 seen, grec, treq, tdec, ttally, ncnt, 
                                 badTaint, badResume, revoked, pre, preRev, 
                                 badPre, badOrder, ckey, val, name, it, out, 
                                 term, rk, rv, rf >>

TGate(self) == /\ pc[self] = "TGate"
               /\ IF ttally[TKey(key[self])] # None
                     THEN /\ pc' = [pc EXCEPT ![self] = "TPause"]
                          /\ treq' = treq
                     ELSE /\ treq' = [treq EXCEPT ![TKey(key[self])] = TRUE]
                          /\ pc' = [pc EXCEPT ![self] = "TCount"]
               /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                               nval, choice, failed, reply, ambig, crashes, 
                               fired, firedN, badRoute, badFire, rdecl, cur, 
                               dd, jn, ord, tm, mord, seen, grec, refused, 
                               tdec, ttally, ncnt, badTaint, badResume, 
                               revoked, pre, preRev, badPre, badOrder, stack, 
                               key, bodyVal, tneed, ckey, val, name, it, out, 
                               term, rk, rv, rf >>

TCount(self) == /\ pc[self] = "TCount"
                /\ LET y == TYes(key[self]) IN
                     LET n == TNo(key[self]) IN
                       IF y >= TNeed \/ Cardinality(TElig(key[self])) - n < TNeed
                          THEN /\ \/ /\ reply' = [reply EXCEPT ![self] = "ok"]
                                     /\ ambig' = ambig
                                  \/ /\ ambig < MaxAmbig
                                     /\ ambig' = ambig + 1
                                     /\ reply' = [reply EXCEPT ![self] = "err_nc"]
                                  \/ /\ ambig < MaxAmbig
                                     /\ ambig' = ambig + 1
                                     /\ reply' = [reply EXCEPT ![self] = "err_c"]
                               /\ IF reply'[self] # "err_nc" /\ ttally[TKey(key[self])] = None
                                     THEN /\ ttally' = [ttally EXCEPT ![TKey(key[self])] = IF y >= TNeed THEN "pass" ELSE "deny"]
                                     ELSE /\ TRUE
                                          /\ UNCHANGED ttally
                               /\ IF reply'[self] # "ok"
                                     THEN /\ failed' = [failed EXCEPT ![self] = "error"]
                                          /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                          /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                                          /\ tneed' = [tneed EXCEPT ![self] = Head(stack[self]).tneed]
                                          /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                                          /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                     ELSE /\ pc' = [pc EXCEPT ![self] = "TPause"]
                                          /\ UNCHANGED << failed, stack, key, 
                                                          bodyVal, tneed >>
                          ELSE /\ failed' = [failed EXCEPT ![self] = "pause"]
                               /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                               /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                               /\ tneed' = [tneed EXCEPT ![self] = Head(stack[self]).tneed]
                               /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                               /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                               /\ UNCHANGED << reply, ambig, ttally >>
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, crashes, fired, firedN, badRoute, 
                                badFire, rdecl, cur, dd, jn, ord, tm, mord, 
                                seen, grec, refused, treq, tdec, ncnt, 
                                badTaint, badResume, revoked, pre, preRev, 
                                badPre, badOrder, ckey, val, name, it, out, 
                                term, rk, rv, rf >>

TPause(self) == /\ pc[self] = "TPause"
                /\ IF ttally[TKey(key[self])] = "deny"
                      THEN /\ failed' = [failed EXCEPT ![self] = "denied"]
                           /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                           /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                           /\ tneed' = [tneed EXCEPT ![self] = Head(stack[self]).tneed]
                           /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                           /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "TPre"]
                           /\ UNCHANGED << failed, stack, key, bodyVal, tneed >>
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, reply, ambig, crashes, fired, 
                                firedN, badRoute, badFire, rdecl, cur, dd, jn, 
                                ord, tm, mord, seen, grec, refused, treq, tdec, 
                                ttally, ncnt, badTaint, badResume, revoked, 
                                pre, preRev, badPre, badOrder, ckey, val, name, 
                                it, out, term, rk, rv, rf >>

TPre(self) == /\ pc[self] = "TPre"
              /\ IF Bug # "NoPreExec" /\ revoked /\ Bug # "PreExecIgnoresRevocation"
                    THEN /\ pre' = [pre EXCEPT ![self] = TRUE]
                         /\ preRev' = [preRev EXCEPT ![self] = TRUE]
                         /\ failed' = [failed EXCEPT ![self] = "error"]
                         /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                         /\ bodyVal' = [bodyVal EXCEPT ![self] = Head(stack[self]).bodyVal]
                         /\ tneed' = [tneed EXCEPT ![self] = Head(stack[self]).tneed]
                         /\ key' = [key EXCEPT ![self] = Head(stack[self]).key]
                         /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                    ELSE /\ IF Bug # "NoPreExec"
                               THEN /\ pre' = [pre EXCEPT ![self] = TRUE]
                                    /\ preRev' = [preRev EXCEPT ![self] = revoked]
                                    /\ pc' = [pc EXCEPT ![self] = "NGet"]
                               ELSE /\ pc' = [pc EXCEPT ![self] = "NGet"]
                                    /\ UNCHANGED << pre, preRev >>
                         /\ UNCHANGED << failed, stack, key, bodyVal, tneed >>
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, reply, ambig, crashes, fired, 
                              firedN, badRoute, badFire, rdecl, cur, dd, jn, 
                              ord, tm, mord, seen, grec, refused, treq, tdec, 
                              ttally, ncnt, badTaint, badResume, revoked, 
                              badPre, badOrder, ckey, val, name, it, out, term, 
                              rk, rv, rf >>

Node(self) == GRead(self) \/ GCount(self) \/ GPause(self) \/ NGet(self)
                 \/ NClaim(self) \/ NBody(self) \/ NNested(self)
                 \/ NRecord(self) \/ TRefuse(self) \/ TGate(self)
                 \/ TCount(self) \/ TPause(self) \/ TPre(self)

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
                             rdecl, cur, dd, jn, ord, tm, mord, seen, grec, 
                             refused, treq, tdec, ttally, ncnt, badTaint, 
                             badResume, revoked, pre, preRev, badPre, badOrder, 
                             key, bodyVal, tneed, it, out, term, rk, rv, rf >>

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
                                firedN, badFire, rdecl, cur, dd, jn, ord, tm, 
                                mord, seen, grec, refused, treq, tdec, ttally, 
                                ncnt, badTaint, badResume, revoked, pre, 
                                preRev, badPre, badOrder, key, bodyVal, tneed, 
                                it, out, term, rk, rv, rf >>

Choose(self) == CDo(self) \/ CRoute(self)

Begin(self) == /\ pc[self] = "Begin"
               /\ failed' = [failed EXCEPT ![self] = None]
               /\ it' = [it EXCEPT ![self] = 0]
               /\ out' = [out EXCEPT ![self] = 0]
               /\ term' = [term EXCEPT ![self] = None]
               /\ nval' = [nval EXCEPT ![self] = 0]
               /\ choice' = [choice EXCEPT ![self] = None]
               /\ reply' = [reply EXCEPT ![self] = ""]
               /\ seen' = [seen EXCEPT ![self] = {}]
               /\ \E c \in Decls:
                    cur' = [cur EXCEPT ![self] = c]
               /\ IF done.rec
                     THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                     ELSE /\ pc' = [pc EXCEPT ![self] = "BeginStart"]
               /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                               ambig, crashes, fired, firedN, badRoute, 
                               badFire, rdecl, dd, jn, ord, tm, mord, grec, 
                               refused, treq, tdec, ttally, ncnt, badTaint, 
                               badResume, revoked, pre, preRev, badPre, 
                               badOrder, stack, key, bodyVal, tneed, ckey, val, 
                               name, rk, rv, rf >>

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
                          THEN /\ IF ~started
                                     THEN /\ rdecl' = cur[self]
                                     ELSE /\ TRUE
                                          /\ rdecl' = rdecl
                               /\ started' = TRUE
                               /\ dd' = [dd EXCEPT ![self] = IF Bug = "ResumeNoCompare" \/ ~started' THEN cur[self] ELSE rdecl']
                          ELSE /\ TRUE
                               /\ UNCHANGED << started, rdecl, dd >>
                    /\ IF reply'[self] # "ok"
                          THEN /\ failed' = [failed EXCEPT ![self] = "error"]
                               /\ pc' = [pc EXCEPT ![self] = "Finish"]
                          ELSE /\ IF Bug # "ResumeNoCompare" /\ started' /\ cur[self] # rdecl'
                                     THEN /\ failed' = [failed EXCEPT ![self] = "config"]
                                          /\ pc' = [pc EXCEPT ![self] = "Finish"]
                                     ELSE /\ pc' = [pc EXCEPT ![self] = "RunE"]
                                          /\ UNCHANGED failed
                    /\ UNCHANGED << res, mk, sw, done, tally, req, dec, nval, 
                                    choice, crashes, fired, firedN, badRoute, 
                                    badFire, cur, jn, ord, tm, mord, seen, 
                                    grec, refused, treq, tdec, ttally, ncnt, 
                                    badTaint, badResume, revoked, pre, preRev, 
                                    badPre, badOrder, stack, key, bodyVal, 
                                    tneed, ckey, val, name, it, out, term, rk, 
                                    rv, rf >>

RunE(self) == /\ pc[self] = "RunE"
              /\ /\ key' = [key EXCEPT ![self] = <<"E", 0>>]
                 /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Node",
                                                          pc        |->  "AfterE",
                                                          bodyVal   |->  bodyVal[self],
                                                          tneed     |->  tneed[self],
                                                          key       |->  key[self] ] >>
                                                      \o stack[self]]
              /\ bodyVal' = [bodyVal EXCEPT ![self] = 0]
              /\ tneed' = [tneed EXCEPT ![self] = FALSE]
              /\ pc' = [pc EXCEPT ![self] = "GRead"]
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, failed, reply, ambig, crashes, 
                              fired, firedN, badRoute, badFire, rdecl, cur, dd, 
                              jn, ord, tm, mord, seen, grec, refused, treq, 
                              tdec, ttally, ncnt, badTaint, badResume, revoked, 
                              pre, preRev, badPre, badOrder, ckey, val, name, 
                              it, out, term, rk, rv, rf >>

AfterE(self) == /\ pc[self] = "AfterE"
                /\ IF failed[self] # None
                      THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "ChooseE"]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, failed, reply, ambig, crashes, 
                                fired, firedN, badRoute, badFire, rdecl, cur, 
                                dd, jn, ord, tm, mord, seen, grec, refused, 
                                treq, tdec, ttally, ncnt, badTaint, badResume, 
                                revoked, pre, preRev, badPre, badOrder, stack, 
                                key, bodyVal, tneed, ckey, val, name, it, out, 
                                term, rk, rv, rf >>

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
                                 fired, firedN, badRoute, badFire, rdecl, cur, 
                                 dd, jn, ord, tm, mord, seen, grec, refused, 
                                 treq, tdec, ttally, ncnt, badTaint, badResume, 
                                 revoked, pre, preRev, badPre, badOrder, key, 
                                 bodyVal, tneed, it, out, term, rk, rv, rf >>

AfterChooseE(self) == /\ pc[self] = "AfterChooseE"
                      /\ IF failed[self] # None
                            THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                            ELSE /\ IF choice[self] = "Q"
                                       THEN /\ pc' = [pc EXCEPT ![self] = "RunQ"]
                                       ELSE /\ pc' = [pc EXCEPT ![self] = "LoopH"]
                      /\ UNCHANGED << started, res, mk, sw, done, tally, req, 
                                      dec, nval, choice, failed, reply, ambig, 
                                      crashes, fired, firedN, badRoute, 
                                      badFire, rdecl, cur, dd, jn, ord, tm, 
                                      mord, seen, grec, refused, treq, tdec, 
                                      ttally, ncnt, badTaint, badResume, 
                                      revoked, pre, preRev, badPre, badOrder, 
                                      stack, key, bodyVal, tneed, ckey, val, 
                                      name, it, out, term, rk, rv, rf >>

LoopH(self) == /\ pc[self] = "LoopH"
               /\ /\ key' = [key EXCEPT ![self] = <<"H", it[self]>>]
                  /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Node",
                                                           pc        |->  "AfterH",
                                                           bodyVal   |->  bodyVal[self],
                                                           tneed     |->  tneed[self],
                                                           key       |->  key[self] ] >>
                                                       \o stack[self]]
               /\ bodyVal' = [bodyVal EXCEPT ![self] = 0]
               /\ tneed' = [tneed EXCEPT ![self] = FALSE]
               /\ pc' = [pc EXCEPT ![self] = "GRead"]
               /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                               nval, choice, failed, reply, ambig, crashes, 
                               fired, firedN, badRoute, badFire, rdecl, cur, 
                               dd, jn, ord, tm, mord, seen, grec, refused, 
                               treq, tdec, ttally, ncnt, badTaint, badResume, 
                               revoked, pre, preRev, badPre, badOrder, ckey, 
                               val, name, it, out, term, rk, rv, rf >>

AfterH(self) == /\ pc[self] = "AfterH"
                /\ IF failed[self] # None
                      THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "RunS"]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, failed, reply, ambig, crashes, 
                                fired, firedN, badRoute, badFire, rdecl, cur, 
                                dd, jn, ord, tm, mord, seen, grec, refused, 
                                treq, tdec, ttally, ncnt, badTaint, badResume, 
                                revoked, pre, preRev, badPre, badOrder, stack, 
                                key, bodyVal, tneed, ckey, val, name, it, out, 
                                term, rk, rv, rf >>

RunS(self) == /\ pc[self] = "RunS"
              /\ /\ key' = [key EXCEPT ![self] = <<"S", it[self]>>]
                 /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Node",
                                                          pc        |->  "AfterS",
                                                          bodyVal   |->  bodyVal[self],
                                                          tneed     |->  tneed[self],
                                                          key       |->  key[self] ] >>
                                                      \o stack[self]]
              /\ bodyVal' = [bodyVal EXCEPT ![self] = 0]
              /\ tneed' = [tneed EXCEPT ![self] = FALSE]
              /\ pc' = [pc EXCEPT ![self] = "GRead"]
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, failed, reply, ambig, crashes, 
                              fired, firedN, badRoute, badFire, rdecl, cur, dd, 
                              jn, ord, tm, mord, seen, grec, refused, treq, 
                              tdec, ttally, ncnt, badTaint, badResume, revoked, 
                              pre, preRev, badPre, badOrder, ckey, val, name, 
                              it, out, term, rk, rv, rf >>

AfterS(self) == /\ pc[self] = "AfterS"
                /\ IF failed[self] # None
                      THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "ChooseS"]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, failed, reply, ambig, crashes, 
                                fired, firedN, badRoute, badFire, rdecl, cur, 
                                dd, jn, ord, tm, mord, seen, grec, refused, 
                                treq, tdec, ttally, ncnt, badTaint, badResume, 
                                revoked, pre, preRev, badPre, badOrder, stack, 
                                key, bodyVal, tneed, ckey, val, name, it, out, 
                                term, rk, rv, rf >>

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
                                 fired, firedN, badRoute, badFire, rdecl, cur, 
                                 dd, jn, ord, tm, mord, seen, grec, refused, 
                                 treq, tdec, ttally, ncnt, badTaint, badResume, 
                                 revoked, pre, preRev, badPre, badOrder, key, 
                                 bodyVal, tneed, it, out, term, rk, rv, rf >>

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
                                      fired, firedN, badRoute, badFire, rdecl, 
                                      cur, dd, jn, ord, tm, mord, seen, grec, 
                                      refused, treq, tdec, ttally, ncnt, 
                                      badTaint, badResume, revoked, pre, 
                                      preRev, badPre, badOrder, stack, key, 
                                      bodyVal, tneed, ckey, val, name, out, 
                                      term, rk, rv, rf >>

RunT(self) == /\ pc[self] = "RunT"
              /\ /\ key' = [key EXCEPT ![self] = <<"T", 0>>]
                 /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Node",
                                                          pc        |->  "AfterT",
                                                          bodyVal   |->  bodyVal[self],
                                                          tneed     |->  tneed[self],
                                                          key       |->  key[self] ] >>
                                                      \o stack[self]]
              /\ bodyVal' = [bodyVal EXCEPT ![self] = 0]
              /\ tneed' = [tneed EXCEPT ![self] = FALSE]
              /\ pc' = [pc EXCEPT ![self] = "GRead"]
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, failed, reply, ambig, crashes, 
                              fired, firedN, badRoute, badFire, rdecl, cur, dd, 
                              jn, ord, tm, mord, seen, grec, refused, treq, 
                              tdec, ttally, ncnt, badTaint, badResume, revoked, 
                              pre, preRev, badPre, badOrder, ckey, val, name, 
                              it, out, term, rk, rv, rf >>

AfterT(self) == /\ pc[self] = "AfterT"
                /\ IF failed[self] # None
                      THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "SetT"]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, failed, reply, ambig, crashes, 
                                fired, firedN, badRoute, badFire, rdecl, cur, 
                                dd, jn, ord, tm, mord, seen, grec, refused, 
                                treq, tdec, ttally, ncnt, badTaint, badResume, 
                                revoked, pre, preRev, badPre, badOrder, stack, 
                                key, bodyVal, tneed, ckey, val, name, it, out, 
                                term, rk, rv, rf >>

SetT(self) == /\ pc[self] = "SetT"
              /\ term' = [term EXCEPT ![self] = "T"]
              /\ out' = [out EXCEPT ![self] = nval[self]]
              /\ pc' = [pc EXCEPT ![self] = "Complete"]
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, failed, reply, ambig, crashes, 
                              fired, firedN, badRoute, badFire, rdecl, cur, dd, 
                              jn, ord, tm, mord, seen, grec, refused, treq, 
                              tdec, ttally, ncnt, badTaint, badResume, revoked, 
                              pre, preRev, badPre, badOrder, stack, key, 
                              bodyVal, tneed, ckey, val, name, it, rk, rv, rf >>

RunQ(self) == /\ pc[self] = "RunQ"
              /\ /\ key' = [key EXCEPT ![self] = <<"Q", 0>>]
                 /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Node",
                                                          pc        |->  "AfterQ",
                                                          bodyVal   |->  bodyVal[self],
                                                          tneed     |->  tneed[self],
                                                          key       |->  key[self] ] >>
                                                      \o stack[self]]
              /\ bodyVal' = [bodyVal EXCEPT ![self] = 0]
              /\ tneed' = [tneed EXCEPT ![self] = FALSE]
              /\ pc' = [pc EXCEPT ![self] = "GRead"]
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, failed, reply, ambig, crashes, 
                              fired, firedN, badRoute, badFire, rdecl, cur, dd, 
                              jn, ord, tm, mord, seen, grec, refused, treq, 
                              tdec, ttally, ncnt, badTaint, badResume, revoked, 
                              pre, preRev, badPre, badOrder, ckey, val, name, 
                              it, out, term, rk, rv, rf >>

AfterQ(self) == /\ pc[self] = "AfterQ"
                /\ IF failed[self] # None
                      THEN /\ pc' = [pc EXCEPT ![self] = "Finish"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "SetQ"]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, failed, reply, ambig, crashes, 
                                fired, firedN, badRoute, badFire, rdecl, cur, 
                                dd, jn, ord, tm, mord, seen, grec, refused, 
                                treq, tdec, ttally, ncnt, badTaint, badResume, 
                                revoked, pre, preRev, badPre, badOrder, stack, 
                                key, bodyVal, tneed, ckey, val, name, it, out, 
                                term, rk, rv, rf >>

SetQ(self) == /\ pc[self] = "SetQ"
              /\ term' = [term EXCEPT ![self] = "Q"]
              /\ out' = [out EXCEPT ![self] = nval[self]]
              /\ pc' = [pc EXCEPT ![self] = "Complete"]
              /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                              nval, choice, failed, reply, ambig, crashes, 
                              fired, firedN, badRoute, badFire, rdecl, cur, dd, 
                              jn, ord, tm, mord, seen, grec, refused, treq, 
                              tdec, ttally, ncnt, badTaint, badResume, revoked, 
                              pre, preRev, badPre, badOrder, stack, key, 
                              bodyVal, tneed, ckey, val, name, it, rk, rv, rf >>

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
                                  badFire, rdecl, cur, dd, jn, ord, tm, mord, 
                                  seen, grec, refused, treq, tdec, ttally, 
                                  ncnt, badTaint, badResume, revoked, pre, 
                                  preRev, badPre, badOrder, stack, key, 
                                  bodyVal, tneed, ckey, val, name, it, out, 
                                  term, rk, rv, rf >>

Finish(self) == /\ pc[self] = "Finish"
                /\ IF failed[self] \notin {None, "runaway", "denied"}
                      THEN /\ pc' = [pc EXCEPT ![self] = "Begin"]
                      ELSE /\ pc' = [pc EXCEPT ![self] = "Done"]
                /\ UNCHANGED << started, res, mk, sw, done, tally, req, dec, 
                                nval, choice, failed, reply, ambig, crashes, 
                                fired, firedN, badRoute, badFire, rdecl, cur, 
                                dd, jn, ord, tm, mord, seen, grec, refused, 
                                treq, tdec, ttally, ncnt, badTaint, badResume, 
                                revoked, pre, preRev, badPre, badOrder, stack, 
                                key, bodyVal, tneed, ckey, val, name, it, out, 
                                term, rk, rv, rf >>

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
                               fired, firedN, badRoute, badFire, rdecl, cur, 
                               dd, jn, ord, tm, mord, seen, grec, refused, 
                               treq, tdec, ttally, ncnt, badTaint, badResume, 
                               revoked, pre, preRev, badPre, badOrder, stack, 
                               key, bodyVal, tneed, ckey, val, name, it, out, 
                               term >>

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
                           /\ ord' = [ord EXCEPT ![rk[self]] = IF Track THEN jn ELSE 0]
                           /\ jn' = IF Track THEN jn + 1 ELSE jn
                      ELSE /\ TRUE
                           /\ UNCHANGED << res, jn, ord >>
                /\ pc' = [pc EXCEPT ![self] = "Done"]
                /\ UNCHANGED << started, mk, sw, done, tally, req, dec, nval, 
                                choice, failed, crashes, fired, firedN, 
                                badRoute, badFire, rdecl, cur, dd, tm, mord, 
                                seen, grec, refused, treq, tdec, ttally, ncnt, 
                                badTaint, badResume, revoked, pre, preRev, 
                                badPre, badOrder, stack, key, bodyVal, tneed, 
                                ckey, val, name, it, out, term, rk, rv, rf >>

resolver(self) == RPick(self) \/ RWrite(self)

ADecide(self) == /\ pc[self] = "ADecide"
                 /\ \/ /\ \E k \in {k \in GateKeys : (dec[k][self] = None /\ (req[k] \/ Early)) \/ (dec[k][self] \in {"eyes", "eno"} /\ req[k])}:
                            \E v \in IF req[k] THEN {"yes", "no"} ELSE {"eyes", "eno"}:
                              dec' = [dec EXCEPT ![k][self] = v]
                       /\ tdec' = tdec
                    \/ /\ \E k \in {k \in TKeys : tdec[k][self] = None /\ treq[k]}:
                            \E v \in {"yes", "no"}:
                              tdec' = [tdec EXCEPT ![k][self] = v]
                       /\ dec' = dec
                 /\ IF \E k \in GateKeys : dec'[k][self] \in {None, "eyes", "eno"}
                       THEN /\ pc' = [pc EXCEPT ![self] = "ADecide"]
                       ELSE /\ IF \E k \in TKeys : tdec'[k][self] = None
                                  THEN /\ pc' = [pc EXCEPT ![self] = "ADecide"]
                                  ELSE /\ pc' = [pc EXCEPT ![self] = "Done"]
                 /\ UNCHANGED << started, res, mk, sw, done, tally, req, nval, 
                                 choice, failed, reply, ambig, crashes, fired, 
                                 firedN, badRoute, badFire, rdecl, cur, dd, jn, 
                                 ord, tm, mord, seen, grec, refused, treq, 
                                 ttally, ncnt, badTaint, badResume, revoked, 
                                 pre, preRev, badPre, badOrder, stack, key, 
                                 bodyVal, tneed, ckey, val, name, it, out, 
                                 term, rk, rv, rf >>

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
  /\ cur' = [cur EXCEPT ![d] = NoDecl]
  /\ dd' = [dd EXCEPT ![d] = NoDecl]
  /\ seen' = [seen EXCEPT ![d] = {}]
  /\ tneed' = [tneed EXCEPT ![d] = FALSE]
  /\ key' = [key EXCEPT ![d] = defaultInitValue]
  /\ ckey' = [ckey EXCEPT ![d] = defaultInitValue]
  /\ val' = [val EXCEPT ![d] = defaultInitValue]
  /\ name' = [name EXCEPT ![d] = defaultInitValue]
  /\ UNCHANGED <<started, res, mk, sw, done, tally, req, dec, ambig, fired, firedN, badRoute, badFire, rk, rv, rf,
                 rdecl, jn, ord, tm, mord, grec, refused, treq, tdec, ttally, ncnt, badTaint, badResume,
                 revoked, pre, preRev, badPre, badOrder>>

\* A certificate of an approval is revoked (once), whenever (only in a model that tracks taint).
Revoke ==
  /\ Track /\ ~revoked
  /\ revoked' = TRUE
  /\ UNCHANGED <<started, res, mk, sw, done, tally, req, dec, nval, choice, failed, reply, ambig, crashes,
                 fired, firedN, badRoute, badFire, rdecl, cur, dd, jn, ord, tm, mord, seen, grec, refused,
                 treq, tdec, ttally, ncnt, badTaint, badResume, pre, preRev, badPre, badOrder, pc, stack,
                 key, bodyVal, tneed, ckey, val, name, it, out, term, rk, rv, rf>>

FullNext == Next \/ \E d \in Drivers : Crash(d) \/ Revoke

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

\* Taint (docs/design/flow-taint.md, A5b). No side-effecting node attempt is claimed after a source
\* read (a non-empty read set) unless its taint gate passed or it is exempt, under a policy; across
\* crashes, error replies, drivers and resumes.
TaintGateHeld == ~badTaint

\* No node body runs under declarations other than the recorded ones: a drive resuming under
\* changed declarations (a weaker policy, a dropped declaration) is refused before the first node.
ResumeExact == ~badResume

\* A recorded instance tally says what the instance's own decisions say (decisions of the node gate
\* do not count for it): a pass has TNeed approvals, a deny leaves too few approvers.
TaintTallySound == \A k \in TKeys :
  LET ex == IF Separate /\ k \in GateKeys THEN ncnt[k] ELSE {}
      no == Cardinality({a \in Approvers \ ex : tdec[k][a] = "no"})
  IN /\ ttally[k] = "pass" => Cardinality({a \in Approvers : tdec[k][a] = "yes"}) >= TNeed
     /\ ttally[k] = "deny" => Cardinality(Approvers \ ex) - no < TNeed

\* SeparateApprovers: no approver counted by the node's own gate also counted by a passing instance.
SeparateHeld == Separate =>
  \A k \in TKeys \cap GateKeys : ttally[k] = "pass" =>
    Cardinality({a \in Approvers \ ncnt[k] : tdec[k][a] = "yes"}) >= TNeed

\* A refused node has a gate-reason record and no node record, no attempt, no approval request.
RefusedClean == \A k \in NodeKeys : refused[k] =>
  /\ grec[k] /\ ~res[k].rec /\ mk[k] = None
  /\ (k \in GateKeys => ~req[k])
  /\ (k \in TKeys => ~treq[k])

\* PreExec: an approved attempt is claimed only after a PreExec that saw no revocation.
PreExecHeld == ~badPre

\* The gate-reason record (and so a refusal) is journaled before any attempt of a tainted node.
GateReasonFirst == ~badOrder

\* Vacuity, expected to be violated: the flow completes after an effect fired.
EffectNotReachable == ~(done.rec /\ \E k \in NodeKeys : fired[k] > 0)

\* Vacuity of the taint checks, each expected to be violated by a configuration that claims it:
\* a tainted attempt claimed after its instance passed (in iteration 1 of the loop), a terminal
\* denial of the instance, a refused node, a drive refused for changed declarations, a taint mark.
RevokedStopNotReachable == ~(revoked /\ \E d \in Drivers : failed[d] = "error")
TaintPassNotReachable == ~(\E k \in TKeys : ttally[k] = "pass" /\ mk[k] # None)
TaintPassLoopNotReachable == ~(\E k \in TKeys : k[2] = 1 /\ ttally[k] = "pass" /\ mk[k] # None)
TaintDenyNotReachable == ~(\E k \in TKeys : ttally[k] = "deny")
RefusalNotReachable == ~(\E k \in NodeKeys : refused[k])
ConfigRefusalNotReachable == ~(\E d \in Drivers : failed[d] = "config")
MarkNotReachable == ~(\E j \in NodeKeys : tm[j] /\ \E k \in TKeys : ttally[k] = "pass" /\ mk[k] # None)

\* Liveness: a run whose drives do not crash or halt completes (recovery skips it then).
Completes == <>(done.rec \/ \E d \in Drivers : failed[d] \in {"halt", "runaway", "denied", "refused"})
=============================================================================
