----------------------------- MODULE Delegation -----------------------------
(***************************************************************************)
(* Model 11: delegation, sub-run authority and saga trees.                 *)
(*                                                                          *)
(* What model 9 (toolcall) treats as a black box: a call that runs another *)
(* agent's run. AttenuatingSubAgent mints a child grant from the grant      *)
(* bound to the context, journals it as a leaf of the sub-run, reuses it on *)
(* resume, checks expiry when it mints and reuses and (CallGuard) before    *)
(* every call of the sub-run, checks the subject, journals the ungranted    *)
(* marker, and refuses a resume under other authority as Unrecorded. A     *)
(* tool's programmatic sub-run (RunInfo.SubRunFor) is linked in a saga     *)
(* tree's journal before it records anything, must share the tree's store, *)
(* and is refused once its call has returned. A saga's rollback walks the  *)
(* whole tree: into sub-agents through BindRollback, which rebinds the     *)
(* journaled grant and identity, and into linked sub-runs with the agent   *)
(* WithSubRuns declares, latest first and depth first.                     *)
(*                                                                          *)
(* One driver runs the tree: a run's calls run one after another (an       *)
(* errgroup with a limit of one; sibling concurrency is model 9's), and a  *)
(* sub-run is a nested drive, as the parent's call waits for it. Crashes   *)
(* (at every step), ambiguous writes (A3), transient read errors, the      *)
(* clock (grant expiry), a drive bound to the wrong authority and the      *)
(* operator who binds the right one are the environment.                   *)
(***************************************************************************)
EXTENDS Integers, Sequences, FiniteSets, TLC

CONSTANTS
  Tree,        \* Tree[n]: the calls of run n (one turn), records built in DelegationMC; run 1 is the root
  RootSaga,    \* the root is driven by RunSaga
  Orig,        \* the authority the root's first drive binds: "P" (a signed grant) or "none"
  PExp,        \* P's NotAfter, in ticks (Inf: never within the bound)
  MaxTime,     \* ticks of the clock
  Legacy,      \* delegation sub-runs that start with a write and no journaled authority
  Foreign,     \* delegation sub-runs that start with a grant for another subject and a write under it
  MErr,        \* runs whose turn ends with a model error instead of an answer
  MaxAmbig,    \* write errors (A3: committed or not) on the wrapper's leaves, links, compensations
  MaxReadErr,  \* transient read errors of a sub-run's journal (journaledAuthority)
  MaxCrash,    \* process crashes
  MaxWrong,    \* drives bound to the wrong authority (another grant, or none)
  MaxLost,     \* calls whose outcome is lost (ErrToolOutcomeUnknown)
  Bug,         \* "none" or a historical rule, see regress/
  Fix          \* "none" or a proposed fix of a finding, see findings/

Inf == 99
Root == 1
Nodes == 1..Len(Tree)
Idx(n) == 1..Len(Tree[n])
C(n, k) == Tree[n][k]
K(n, k) == Tree[n][k].k
Ch(n, k) == Tree[n][k].ch
Min(x, y) == IF x < y THEN x ELSE y

ASSUME Bug \in {"none", "HiddenHalt", "StorageRecorded", "MintOnRecords", "ForeignBind", "NoBind",
                "ExpiredUnrecorded", "B1", "B2", "B3", "B4", "NoCallGuard", "WrongAuthRecorded",
                "NoReturnedCheck", "MintForeign"}
ASSUME Fix \in {"none", "ChainBind", "GuardRerun", "RecurseFailed"}
ASSUME Orig \in {"P", "none"}
ASSUME \A x \in Nodes : \A y \in Idx(x) :
         K(x, y) \in {"eff", "fail", "deleg", "sub"} /\ (K(x, y) = "deleg" => Ch(x, y) # 0)

\* The call that starts run n, and the static tree above it.
Calls == UNION {{<<x, y>> : y \in Idx(x)} : x \in Nodes}
PC(n) == CHOOSE p \in Calls : Ch(p[1], p[2]) = n
RECURSIVE DelOf(_), AboveOf(_)
\* The delegation sub-run n is in: itself, or the nearest one above it (0: none).
DelOf(n) == IF n = Root THEN 0 ELSE IF K(PC(n)[1], PC(n)[2]) = "deleg" THEN n ELSE DelOf(PC(n)[1])
\* The calls above run n, up to the root.
AboveOf(n) == IF n = Root THEN {} ELSE {PC(n)} \cup AboveOf(PC(n)[1])
DelF == [n \in Nodes |-> DelOf(n)]
AboveF == [n \in Nodes |-> AboveOf(n)]
\* A programmatic sub-run started on another store: refused since #127 B2 (it never runs).
OnOther(n) == Bug = "B2" /\ n # Root /\ K(PC(n)[1], PC(n)[2]) = "sub" /\ C(PC(n)[1], PC(n)[2]).store = "other"
\* The calls that already ran in a Legacy or Foreign sub-run: its first, a side effect.
PreW(n, k) == n \in Legacy \cup Foreign /\ k = 1

(***************************************************************************)
(* Grants. An id names the chain (a minted grant's id extends its parent's); *)
(* sub is the subject (0 a root principal's grant, c the delegation of      *)
(* sub-run c, -1 another subject); par is the parent's id; e the NotAfter.  *)
(* Every grant is signed by the one signer bound beside it.                 *)
(***************************************************************************)
G(id, sub, par, e) == [id |-> id, sub |-> sub, par |-> par, e |-> e]
NoneG == G("none", 0, "", Inf)
PG == G("P", 0, "", PExp)
P2G == G("P2", 0, "", Inf)        \* a live grant the operator binds once P expired
WG == G("W", 0, "", Inf)          \* another principal's grant: the wrong authority
FG == G("FG", -1, "P", Inf)       \* a grant P's subject issued to another delegation
RootGrants == {PG, P2G, WG}
RootFor(id, dflt) == IF \E r \in RootGrants : r.id = id THEN CHOOSE r \in RootGrants : r.id = id ELSE dflt
OrigG == IF Orig = "P" THEN PG ELSE NoneG
\* AttenuatingSubAgent's mint: the AttenuateFunc's subject and NotAfter (0: the parent's).
MintOf(x, n, k) ==
  G(x.id \o ">" \o ToString(Ch(n, k)),
    IF C(n, k).nsub = "other" THEN -1 ELSE Ch(n, k),
    x.id, Min(x.e, C(n, k).ne))
IsChild(g) == g.sub # 0
Expired(g, t) == t > g.e

\* The root's verdicts after a drive. A re-drive follows the retryable ones.
Retry == {"init", "unrec", "storage", "rbstop", "crash"}
Final == {"complete", "aborted", "halt", "failed"}

(* --algorithm delegation
variables
  \* The tree's one store: per run, whether its turn is journaled, each call's attempt marker
  \* (side effects) and result ("ok", "err", "sf" a StepSagaFail, "sfu" one with OutcomeUnknown),
  \* each call's link to the programmatic sub-run it started, each compensation's record, the
  \* authority leaf a delegation journaled in its sub-run (a grant, or the ungranted marker),
  \* and the end markers.
  opened = [x \in Nodes |-> x \in Legacy \cup Foreign],
  mk  = [x \in Nodes |-> [y \in Idx(x) |-> PreW(x, y)]],
  res = [x \in Nodes |-> [y \in Idx(x) |-> IF PreW(x, y) THEN "ok" ELSE "none"]],
  lk  = [x \in Nodes |-> [y \in Idx(x) |-> FALSE]],
  cp  = [x \in Nodes |-> [y \in Idx(x) |-> FALSE]],
  aj  = [x \in Nodes |-> [g |-> IF x \in Foreign THEN FG ELSE NoneG, ung |-> FALSE]],
  cm  = [x \in Nodes |-> FALSE],
  ab  = [x \in Nodes |-> FALSE],
  \* The environment: the clock, the authority the next drive binds and the one the last
  \* refusal asked for, the root's verdict, and the fault budgets.
  now = 0, bound = OrigG, want = OrigG, verdict = "init",
  ambig = 0, readErr = 0, crashes = 0, wrongs = 0, lost = 0,
  \* What the procedures return: a run's error (a set of tags: "U" Unrecorded, "H" a halt,
  \* "L" a lost outcome, "S" ErrStorage, "A" SagaAborted, "F" any other error; {} success),
  \* and a rollback's end ("ok", "stop", "halt").
  rv = [x \in Nodes |-> {}],
  rb = [x \in Nodes |-> "ok"],
  \* Ghosts. haltIn[n]: a call of n's drive returned a halt or a lost outcome. fx and fa: each
  \* write's effect ("none", "in", "undone") and the authority it fired under. granted[c]: the
  \* authorities delegation c granted. walk: what the root's rollback walk is reporting
  \* (Uncompensated, UnknownOutcome); listed: what its last finished walk reported.
  haltIn = [x \in Nodes |-> FALSE],
  fx = [x \in Nodes |-> [y \in Idx(x) |-> IF PreW(x, y) THEN "in" ELSE "none"]],
  fa = [x \in Nodes |-> [y \in Idx(x) |-> IF PreW(x, y) THEN (IF x \in Foreign THEN "FG" ELSE "legacy") ELSE "none"]],
  granted = [x \in Nodes |-> {}],
  walk = {},
  listed = {},
  anyFire = FALSE, badAct = FALSE, admitLate = FALSE, fireLate = FALSE, badComp = FALSE,
  startAfterHalt = FALSE, falseFail = FALSE, forged = FALSE;

define
  Recs(c) == opened[c] \/ aj[c].g # NoneG \/ aj[c].ung
  JAuth(c) == IF aj[c].ung THEN "none" ELSE aj[c].g.id
  \* An act in a delegation's sub-run (or below it) under authority the delegation did not grant.
  BadAuth(m, id) == DelF[m] # 0 /\ id \notin granted[DelF[m]]
  \* The proposed fix of finding D1: the journaled grant's chain verifies under the bound signer.
  ChainOK(g) == g.par \in {r.id : r \in RootGrants} \/ \E c \in Nodes : aj[c].g.id = g.par
  Lst(m, q, l) == IF l THEN {<<m, q>>} ELSE {}
  Links(m) == {q \in Idx(m) : lk[m][q]}
  SubWalk(m, q, ls0) == K(m, q) = "sub" /\ Ch(m, q) # 0 /\ q \in ls0 /\ ~(Bug = "B1" /\ C(m, q).long)
  VerdictOf(x) ==
    IF x = {} THEN "complete"
    ELSE IF "A" \in x THEN (IF ab[Root] THEN "aborted" ELSE IF rb[Root] = "halt" THEN "halt" ELSE "rbstop")
    ELSE IF x \cap {"H", "L"} # {} THEN "halt"
    ELSE IF "U" \in x THEN "unrec"
    ELSE IF "S" \in x THEN "storage"
    ELSE "failed"
end define;

\* Agent.run (and runSaga) of run n under bound authority a (delegated: dg), as a saga or not
\* (sg), in a saga's tree or not (st).
procedure Drive(n, a, dg, sg, st)
variables i = 1, held = {}, hl = FALSE, e = {}, ca = NoneG, cdg = FALSE, cz = "perm";
begin
DOpen:
  opened[n] := TRUE;
  haltIn[n] := FALSE;
  if cm[n] then
    rv[n] := {}; return;
  elsif sg /\ \E y \in Idx(n) : res[n][y] \in {"sf", "sfu"} then
    goto DRb;
  elsif \E y \in Idx(n) : K(n, y) = "eff" /\ mk[n][y] /\ res[n][y] = "none" then
    rv[n] := {"H"}; return;
  end if;
DNext:
  if i > Len(Tree[n]) then
    goto DEnd;
  elsif res[n][i] # "none" \/ hl then
    i := i + 1; goto DNext;
  end if;
DGuard:
  startAfterHalt := startAfterHalt \/ (sg /\ haltIn[n]);
  admitLate := admitLate \/ (IsChild(a) /\ Expired(a, now) /\ ~(dg /\ Bug # "NoCallGuard"));
  cz := "perm";
  if dg /\ Expired(a, now) /\ Bug # "NoCallGuard" then
    e := {"F"}; goto DClass;
  elsif K(n, i) = "eff" then
    e := {}; goto EMark;
  elsif K(n, i) = "fail" then
    e := {"F"}; goto DClass;
  elsif K(n, i) = "deleg" then
    e := {}; goto DgRead;
  else
    e := {}; goto SStart;
  end if;
EMark:
  mk[n][i] := TRUE;
EFire:
  fx[n][i] := "in"; fa[n][i] := a.id; anyFire := TRUE;
  badAct := badAct \/ BadAuth(n, a.id);
  fireLate := fireLate \/ (IsChild(a) /\ Expired(a, now));
ERes:
  either
    e := {};
  or
    await lost < MaxLost; lost := lost + 1; e := {"L"};
  end either;
  goto DClass;
DgRead:
  either
    await readErr < MaxReadErr;
    readErr := readErr + 1;
    if Bug = "StorageRecorded" then e := {"F"}; cz := "trans"; else e := {"U"}; end if;
    goto DClass;
  or
    if a = NoneG then
      if aj[Ch(n, i)].g # NoneG then
        e := {"U"}; want := RootFor(aj[Ch(n, i)].g.par, want); goto DClass;
      else
        goto DgUng;
      end if;
    elsif aj[Ch(n, i)].ung then
      e := {"U"}; want := NoneG; goto DClass;
    elsif aj[Ch(n, i)].g = NoneG /\ Recs(Ch(n, i)) /\ Bug # "MintOnRecords" then
      e := {"U"}; goto DClass;
    elsif aj[Ch(n, i)].g # NoneG then
      if aj[Ch(n, i)].g.sub # Ch(n, i) then
        e := {"F"}; goto DClass;
      elsif aj[Ch(n, i)].g.par # a.id then
        if Bug = "WrongAuthRecorded" then
          e := {"F"}; cz := "trans";
        else
          e := {"U"}; want := RootFor(aj[Ch(n, i)].g.par, want);
        end if;
        goto DClass;
      elsif Expired(aj[Ch(n, i)].g, now) then
        if Bug = "ExpiredUnrecorded" then e := {"U"}; else e := {"F"}; end if;
        goto DClass;
      else
        ca := aj[Ch(n, i)].g; cdg := TRUE; goto DgRun;
      end if;
    else
      goto DgMint;
    end if;
  end either;
DgUng:
  either
    aj[Ch(n, i)].ung := TRUE; granted[Ch(n, i)] := granted[Ch(n, i)] \cup {"none"};
    ca := a; cdg := dg; goto DgRun;
  or
    await ambig < MaxAmbig; ambig := ambig + 1;
    aj[Ch(n, i)].ung := TRUE; granted[Ch(n, i)] := granted[Ch(n, i)] \cup {"none"};
    if Bug = "StorageRecorded" then e := {"F"}; cz := "trans"; else e := {"U"}; end if;
    goto DClass;
  or
    await ambig < MaxAmbig; ambig := ambig + 1;
    if Bug = "StorageRecorded" then e := {"F"}; cz := "trans"; else e := {"U"}; end if;
    goto DClass;
  end either;
DgMint:
  if Expired(a, now) then
    e := {"U"}; want := RootFor("P2", want); goto DClass;
  elsif C(n, i).nsub = "other" /\ Bug # "MintForeign" then
    e := {"F"}; goto DClass;
  elsif Expired(MintOf(a, n, i), now) then
    e := {"F"}; goto DClass;
  end if;
DgRec:
  granted[Ch(n, i)] := IF MintOf(a, n, i).sub = Ch(n, i) THEN granted[Ch(n, i)] \cup {MintOf(a, n, i).id}
                       ELSE granted[Ch(n, i)];
  either
    aj[Ch(n, i)].g := MintOf(a, n, i); ca := MintOf(a, n, i); cdg := TRUE; goto DgRun;
  or
    await ambig < MaxAmbig; ambig := ambig + 1;
    aj[Ch(n, i)].g := MintOf(a, n, i);
    if Bug = "StorageRecorded" then e := {"F"}; cz := "trans"; else e := {"U"}; end if;
    goto DClass;
  or
    await ambig < MaxAmbig; ambig := ambig + 1;
    if Bug = "StorageRecorded" then e := {"F"}; cz := "trans"; else e := {"U"}; end if;
    goto DClass;
  end either;
DgRun:
  call Drive(Ch(n, i), ca, cdg, sg, sg \/ st);
DgRet:
  e := rv[Ch(n, i)]; goto DClass;
SStart:
  \* A run ID from another call's scope is refused (derivedRunID): the forged attempt never runs.
  forged := forged \/ (C(n, i).forge /\ <<n, i>> = <<n, i + 1>>);
  if Ch(n, i) = 0 \/ C(n, i).late then
    goto SWrite;
  elsif ~(IF Bug = "B3" THEN sg ELSE st) then
    goto SRun;
  elsif C(n, i).store = "other" /\ Bug # "B2" then
    e := {"F"}; goto DClass;
  end if;
SLink:
  either
    lk[n][i] := TRUE; goto SRun;
  or
    await ambig < MaxAmbig; ambig := ambig + 1; lk[n][i] := TRUE; e := {"F"}; goto DClass;
  or
    await ambig < MaxAmbig; ambig := ambig + 1; e := {"F"}; goto DClass;
  end either;
SRun:
  call Drive(Ch(n, i), a, dg, C(n, i).saga, C(n, i).saga \/ st);
SRet:
  e := rv[Ch(n, i)];
SWrite:
  if C(n, i).w /\ e = {} then
    fx[n][i] := "in"; fa[n][i] := a.id; anyFire := TRUE;
    badAct := badAct \/ BadAuth(n, a.id);
    fireLate := fireLate \/ (IsChild(a) /\ Expired(a, now));
  end if;
DClass:
  if e = {} then
    res[n][i] := "ok";
    if K(n, i) = "sub" /\ C(n, i).late then goto SLate; else i := i + 1; goto DNext; end if;
  elsif "U" \in e then
    held := held \cup e;
    haltIn[n] := haltIn[n] \/ (sg /\ e \cap {"H", "L"} # {});
    hl := hl \/ (sg /\ e \cap {"H", "L"} # {} /\ Bug # "HiddenHalt");
    i := i + 1; goto DNext;
  elsif "H" \in e \/ ("L" \in e /\ K(n, i) \in {"eff", "deleg"}) then
    held := held \cup e; haltIn[n] := haltIn[n] \/ sg; hl := hl \/ sg;
    i := i + 1; goto DNext;
  elsif "S" \in e /\ K(n, i) = "deleg" then
    rv[n] := e; return;
  elsif sg then
    res[n][i] := IF "L" \in e THEN "sfu" ELSE "sf";
    falseFail := falseFail \/ (K(n, i) = "deleg" /\ cz = "trans");
    goto DRb;
  else
    res[n][i] := "err";
    falseFail := falseFail \/ (K(n, i) = "deleg" /\ cz = "trans");
    i := i + 1; goto DNext;
  end if;
SLate:
  \* A goroutine of the call starts the sub-run after the call returned.
  if Bug = "NoReturnedCheck" then
    forged := TRUE;
    if st then lk[n][i] := TRUE; end if;
    call Drive(Ch(n, i), a, dg, C(n, i).saga, C(n, i).saga \/ st);
  else
    i := i + 1; goto DNext;
  end if;
SLateRet:
  i := i + 1; goto DNext;
DRb:
  if n = Root then walk := {}; end if;
  call Rollback(n, a, dg, FALSE, n = Root);
DRbEnd:
  if rb[n] = "ok" then
    ab[n] := TRUE;
    if n = Root then listed := walk; end if;
  end if;
  rv[n] := {"A"}; return;
DEnd:
  if held # {} then
    rv[n] := held; return;
  elsif n \in MErr then
    rv[n] := {"F"}; return;
  else
    cm[n] := TRUE; rv[n] := {}; return;
  end if;
end procedure;

\* rollbackRun of run rn under bound authority ra (delegated: rdg), with the run's agent or
\* with no tools (tl, an undeclared or unusable programmatic sub-run); lst: in the root's
\* rollback, whose lists the property reads.
procedure Rollback(rn, ra, rdg, tl, lst)
variables j = 0, ph = 1, ls = {}, ba = NoneG, bdg = FALSE, re = {};
begin
RbOpen:
  if OnOther(rn) \/ ~opened[rn] then
    rb[rn] := "ok"; return;
  else
    ls := Links(rn); j := Len(Tree[rn]); ph := 1;
  end if;
RbLoop:
  if j = 0 /\ ph = 1 then
    ph := 2; j := Len(Tree[rn]); goto RbLoop;
  elsif j = 0 then
    rb[rn] := "ok"; return;
  elsif ph = 1 then
    \* The failed calls first: their sub-runs, then a failed sub-agent's run.
    if res[rn][j] \in {"sf", "sfu"} /\ K(rn, j) = "sub" then
      goto RbSub;
    elsif res[rn][j] \in {"sf", "sfu"} /\ K(rn, j) = "deleg" /\ ~tl then
      goto RbBind;
    else
      j := j - 1; goto RbLoop;
    end if;
  elsif res[rn][j] \in {"sf", "sfu"} then
    if res[rn][j] = "sfu" then walk := walk \cup Lst(rn, j, lst); end if;
    j := j - 1; goto RbLoop;
  elsif res[rn][j] = "err" /\ ~(Fix = "RecurseFailed" /\ K(rn, j) = "deleg" /\ ~tl) then
    goto RbSub;
  elsif tl then
    if K(rn, j) = "sub" /\ ~C(rn, j).w /\ res[rn][j] = "ok" then
      goto RbSub;
    elsif res[rn][j] = "none" /\ mk[rn][j] then
      walk := walk \cup Lst(rn, j, lst); rb[rn] := "halt"; return;
    else
      walk := walk \cup Lst(rn, j, lst); goto RbSub;
    end if;
  elsif K(rn, j) = "deleg" then
    goto RbBind;
  elsif K(rn, j) = "eff" /\ res[rn][j] = "ok" then
    goto RbComp;
  elsif K(rn, j) = "eff" /\ mk[rn][j] then
    walk := walk \cup Lst(rn, j, lst); rb[rn] := "halt"; return;
  elsif K(rn, j) = "sub" /\ C(rn, j).w /\ res[rn][j] = "ok" then
    goto RbComp;
  elsif K(rn, j) = "sub" /\ C(rn, j).w then
    goto RbRe;
  else
    goto RbSub;
  end if;
RbSub:
  \* walkSubRuns / rollbackSubRun: the call's linked sub-run, with its declared agent.
  if SubWalk(rn, j, ls) then
    if ~tl /\ C(rn, j).decl = "other" then walk := walk \cup Lst(rn, j, lst); end if;
    call Rollback(Ch(rn, j), ra, rdg, tl \/ C(rn, j).decl # "same", lst);
  else
    j := j - 1; goto RbLoop;
  end if;
RbSubRet:
  if rb[Ch(rn, j)] # "ok" then rb[rn] := rb[Ch(rn, j)]; return; else j := j - 1; goto RbLoop; end if;
RbBind:
  \* bindRollback: AttenuatingSubAgent.BindRollback reads the sub-run's journaled authority.
  if Bug = "NoBind" then
    ba := ra; bdg := rdg; goto RbRec;
  else
    either
      await readErr < MaxReadErr; readErr := readErr + 1; rb[rn] := "stop"; return;
    or
      if aj[Ch(rn, j)].g = NoneG /\ (aj[Ch(rn, j)].ung \/ ~Recs(Ch(rn, j))) then
        ba := NoneG; bdg := FALSE; goto RbRec;
      elsif aj[Ch(rn, j)].g = NoneG then
        rb[rn] := "stop"; return;
      elsif aj[Ch(rn, j)].g.sub # Ch(rn, j) /\ Bug # "ForeignBind" then
        rb[rn] := "stop"; return;
      elsif ra = NoneG \/ ~(aj[Ch(rn, j)].g.par = ra.id \/ (Fix = "ChainBind" /\ ChainOK(aj[Ch(rn, j)].g))) then
        want := RootFor(aj[Ch(rn, j)].g.par, want); rb[rn] := "stop"; return;
      else
        ba := aj[Ch(rn, j)].g; bdg := Fix = "GuardRerun"; goto RbRec;
      end if;
    end either;
  end if;
RbRec:
  call Rollback(Ch(rn, j), ba, bdg, FALSE, lst);
RbBindRet:
  if rb[Ch(rn, j)] # "ok" then rb[rn] := rb[Ch(rn, j)]; return; else j := j - 1; goto RbLoop; end if;
RbComp:
  \* The memoized sagaCompensateStep: Compensate, then its record.
  if cp[rn][j] then
    goto RbSub;
  else
    fx[rn][j] := "undone";
    badAct := badAct \/ BadAuth(rn, ra.id);
    badComp := badComp \/ (DelF[rn] # 0 /\ (ra.id # fa[rn][j] \/ ra.id # JAuth(DelF[rn])));
    either
      cp[rn][j] := TRUE; goto RbSub;
    or
      await ambig < MaxAmbig; ambig := ambig + 1; cp[rn][j] := TRUE; rb[rn] := "stop"; return;
    or
      await ambig < MaxAmbig; ambig := ambig + 1; rb[rn] := "stop"; return;
    end either;
  end if;
RbRe:
  \* The re-run of a retry-safe compensable call with no result, through the base handler.
  if rdg /\ Expired(ra, now) /\ Bug # "NoCallGuard" then
    if Fix = "GuardRerun" then
      walk := walk \cup Lst(rn, j, lst); ls := Links(rn); goto RbSub;
    else
      rb[rn] := "stop"; return;
    end if;
  else
    admitLate := admitLate \/ (IsChild(ra) /\ Expired(ra, now));
    re := {};
    if Ch(rn, j) = 0 then goto RbReW; else lk[rn][j] := TRUE; goto RbReRun; end if;
  end if;
RbReRun:
  call Drive(Ch(rn, j), ra, rdg, C(rn, j).saga, TRUE);
RbReRet:
  re := rv[Ch(rn, j)];
RbReW:
  if re # {} /\ "L" \notin re then
    rb[rn] := "stop"; return;
  else
    fx[rn][j] := "in"; fa[rn][j] := ra.id; anyFire := TRUE;
    badAct := badAct \/ BadAuth(rn, ra.id);
    fireLate := fireLate \/ (IsChild(ra) /\ Expired(ra, now));
    either
      await re = {}; res[rn][j] := "ok"; ls := Links(rn); goto RbComp;
    or
      await re # {} \/ lost < MaxLost;
      lost := IF re = {} THEN lost + 1 ELSE lost;
      walk := walk \cup Lst(rn, j, lst);
      if Bug # "B4" then ls := Links(rn); end if;
      goto RbSub;
    end either;
  end if;
end procedure;

\* The root's drives: RunSaga (or Run), again after every retryable verdict.
fair process drv = "d"
begin
Idle:
  await verdict \in Retry;
  call Drive(Root, bound, FALSE, RootSaga, RootSaga);
Back:
  verdict := VerdictOf(rv[Root]);
  goto Idle;
end process;
end algorithm; *)
\* BEGIN TRANSLATION
CONSTANT defaultInitValue
VARIABLES opened, mk, res, lk, cp, aj, cm, ab, now, bound, want, verdict, 
          ambig, readErr, crashes, wrongs, lost, rv, rb, haltIn, fx, fa, 
          granted, walk, listed, anyFire, badAct, admitLate, fireLate, 
          badComp, startAfterHalt, falseFail, forged, pc, stack

(* define statement *)
Recs(c) == opened[c] \/ aj[c].g # NoneG \/ aj[c].ung
JAuth(c) == IF aj[c].ung THEN "none" ELSE aj[c].g.id

BadAuth(m, id) == DelF[m] # 0 /\ id \notin granted[DelF[m]]

ChainOK(g) == g.par \in {r.id : r \in RootGrants} \/ \E c \in Nodes : aj[c].g.id = g.par
Lst(m, q, l) == IF l THEN {<<m, q>>} ELSE {}
Links(m) == {q \in Idx(m) : lk[m][q]}
SubWalk(m, q, ls0) == K(m, q) = "sub" /\ Ch(m, q) # 0 /\ q \in ls0 /\ ~(Bug = "B1" /\ C(m, q).long)
VerdictOf(x) ==
  IF x = {} THEN "complete"
  ELSE IF "A" \in x THEN (IF ab[Root] THEN "aborted" ELSE IF rb[Root] = "halt" THEN "halt" ELSE "rbstop")
  ELSE IF x \cap {"H", "L"} # {} THEN "halt"
  ELSE IF "U" \in x THEN "unrec"
  ELSE IF "S" \in x THEN "storage"
  ELSE "failed"

VARIABLES n, a, dg, sg, st, i, held, hl, e, ca, cdg, cz, rn, ra, rdg, tl, lst, 
          j, ph, ls, ba, bdg, re

vars == << opened, mk, res, lk, cp, aj, cm, ab, now, bound, want, verdict, 
           ambig, readErr, crashes, wrongs, lost, rv, rb, haltIn, fx, fa, 
           granted, walk, listed, anyFire, badAct, admitLate, fireLate, 
           badComp, startAfterHalt, falseFail, forged, pc, stack, n, a, dg, 
           sg, st, i, held, hl, e, ca, cdg, cz, rn, ra, rdg, tl, lst, j, ph, 
           ls, ba, bdg, re >>

ProcSet == {"d"}

Init == (* Global variables *)
        /\ opened = [x \in Nodes |-> x \in Legacy \cup Foreign]
        /\ mk = [x \in Nodes |-> [y \in Idx(x) |-> PreW(x, y)]]
        /\ res = [x \in Nodes |-> [y \in Idx(x) |-> IF PreW(x, y) THEN "ok" ELSE "none"]]
        /\ lk = [x \in Nodes |-> [y \in Idx(x) |-> FALSE]]
        /\ cp = [x \in Nodes |-> [y \in Idx(x) |-> FALSE]]
        /\ aj = [x \in Nodes |-> [g |-> IF x \in Foreign THEN FG ELSE NoneG, ung |-> FALSE]]
        /\ cm = [x \in Nodes |-> FALSE]
        /\ ab = [x \in Nodes |-> FALSE]
        /\ now = 0
        /\ bound = OrigG
        /\ want = OrigG
        /\ verdict = "init"
        /\ ambig = 0
        /\ readErr = 0
        /\ crashes = 0
        /\ wrongs = 0
        /\ lost = 0
        /\ rv = [x \in Nodes |-> {}]
        /\ rb = [x \in Nodes |-> "ok"]
        /\ haltIn = [x \in Nodes |-> FALSE]
        /\ fx = [x \in Nodes |-> [y \in Idx(x) |-> IF PreW(x, y) THEN "in" ELSE "none"]]
        /\ fa = [x \in Nodes |-> [y \in Idx(x) |-> IF PreW(x, y) THEN (IF x \in Foreign THEN "FG" ELSE "legacy") ELSE "none"]]
        /\ granted = [x \in Nodes |-> {}]
        /\ walk = {}
        /\ listed = {}
        /\ anyFire = FALSE
        /\ badAct = FALSE
        /\ admitLate = FALSE
        /\ fireLate = FALSE
        /\ badComp = FALSE
        /\ startAfterHalt = FALSE
        /\ falseFail = FALSE
        /\ forged = FALSE
        (* Procedure Drive *)
        /\ n = [ self \in ProcSet |-> defaultInitValue]
        /\ a = [ self \in ProcSet |-> defaultInitValue]
        /\ dg = [ self \in ProcSet |-> defaultInitValue]
        /\ sg = [ self \in ProcSet |-> defaultInitValue]
        /\ st = [ self \in ProcSet |-> defaultInitValue]
        /\ i = [ self \in ProcSet |-> 1]
        /\ held = [ self \in ProcSet |-> {}]
        /\ hl = [ self \in ProcSet |-> FALSE]
        /\ e = [ self \in ProcSet |-> {}]
        /\ ca = [ self \in ProcSet |-> NoneG]
        /\ cdg = [ self \in ProcSet |-> FALSE]
        /\ cz = [ self \in ProcSet |-> "perm"]
        (* Procedure Rollback *)
        /\ rn = [ self \in ProcSet |-> defaultInitValue]
        /\ ra = [ self \in ProcSet |-> defaultInitValue]
        /\ rdg = [ self \in ProcSet |-> defaultInitValue]
        /\ tl = [ self \in ProcSet |-> defaultInitValue]
        /\ lst = [ self \in ProcSet |-> defaultInitValue]
        /\ j = [ self \in ProcSet |-> 0]
        /\ ph = [ self \in ProcSet |-> 1]
        /\ ls = [ self \in ProcSet |-> {}]
        /\ ba = [ self \in ProcSet |-> NoneG]
        /\ bdg = [ self \in ProcSet |-> FALSE]
        /\ re = [ self \in ProcSet |-> {}]
        /\ stack = [self \in ProcSet |-> << >>]
        /\ pc = [self \in ProcSet |-> "Idle"]

DOpen(self) == /\ pc[self] = "DOpen"
               /\ opened' = [opened EXCEPT ![n[self]] = TRUE]
               /\ haltIn' = [haltIn EXCEPT ![n[self]] = FALSE]
               /\ IF cm[n[self]]
                     THEN /\ rv' = [rv EXCEPT ![n[self]] = {}]
                          /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                          /\ i' = [i EXCEPT ![self] = Head(stack[self]).i]
                          /\ held' = [held EXCEPT ![self] = Head(stack[self]).held]
                          /\ hl' = [hl EXCEPT ![self] = Head(stack[self]).hl]
                          /\ e' = [e EXCEPT ![self] = Head(stack[self]).e]
                          /\ ca' = [ca EXCEPT ![self] = Head(stack[self]).ca]
                          /\ cdg' = [cdg EXCEPT ![self] = Head(stack[self]).cdg]
                          /\ cz' = [cz EXCEPT ![self] = Head(stack[self]).cz]
                          /\ n' = [n EXCEPT ![self] = Head(stack[self]).n]
                          /\ a' = [a EXCEPT ![self] = Head(stack[self]).a]
                          /\ dg' = [dg EXCEPT ![self] = Head(stack[self]).dg]
                          /\ sg' = [sg EXCEPT ![self] = Head(stack[self]).sg]
                          /\ st' = [st EXCEPT ![self] = Head(stack[self]).st]
                          /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                     ELSE /\ IF sg[self] /\ \E y \in Idx(n[self]) : res[n[self]][y] \in {"sf", "sfu"}
                                THEN /\ pc' = [pc EXCEPT ![self] = "DRb"]
                                     /\ UNCHANGED << rv, stack, n, a, dg, sg, 
                                                     st, i, held, hl, e, ca, 
                                                     cdg, cz >>
                                ELSE /\ IF \E y \in Idx(n[self]) : K(n[self], y) = "eff" /\ mk[n[self]][y] /\ res[n[self]][y] = "none"
                                           THEN /\ rv' = [rv EXCEPT ![n[self]] = {"H"}]
                                                /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                                /\ i' = [i EXCEPT ![self] = Head(stack[self]).i]
                                                /\ held' = [held EXCEPT ![self] = Head(stack[self]).held]
                                                /\ hl' = [hl EXCEPT ![self] = Head(stack[self]).hl]
                                                /\ e' = [e EXCEPT ![self] = Head(stack[self]).e]
                                                /\ ca' = [ca EXCEPT ![self] = Head(stack[self]).ca]
                                                /\ cdg' = [cdg EXCEPT ![self] = Head(stack[self]).cdg]
                                                /\ cz' = [cz EXCEPT ![self] = Head(stack[self]).cz]
                                                /\ n' = [n EXCEPT ![self] = Head(stack[self]).n]
                                                /\ a' = [a EXCEPT ![self] = Head(stack[self]).a]
                                                /\ dg' = [dg EXCEPT ![self] = Head(stack[self]).dg]
                                                /\ sg' = [sg EXCEPT ![self] = Head(stack[self]).sg]
                                                /\ st' = [st EXCEPT ![self] = Head(stack[self]).st]
                                                /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                           ELSE /\ pc' = [pc EXCEPT ![self] = "DNext"]
                                                /\ UNCHANGED << rv, stack, n, 
                                                                a, dg, sg, st, 
                                                                i, held, hl, e, 
                                                                ca, cdg, cz >>
               /\ UNCHANGED << mk, res, lk, cp, aj, cm, ab, now, bound, want, 
                               verdict, ambig, readErr, crashes, wrongs, lost, 
                               rb, fx, fa, granted, walk, listed, anyFire, 
                               badAct, admitLate, fireLate, badComp, 
                               startAfterHalt, falseFail, forged, rn, ra, rdg, 
                               tl, lst, j, ph, ls, ba, bdg, re >>

DNext(self) == /\ pc[self] = "DNext"
               /\ IF i[self] > Len(Tree[n[self]])
                     THEN /\ pc' = [pc EXCEPT ![self] = "DEnd"]
                          /\ i' = i
                     ELSE /\ IF res[n[self]][i[self]] # "none" \/ hl[self]
                                THEN /\ i' = [i EXCEPT ![self] = i[self] + 1]
                                     /\ pc' = [pc EXCEPT ![self] = "DNext"]
                                ELSE /\ pc' = [pc EXCEPT ![self] = "DGuard"]
                                     /\ i' = i
               /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, bound, 
                               want, verdict, ambig, readErr, crashes, wrongs, 
                               lost, rv, rb, haltIn, fx, fa, granted, walk, 
                               listed, anyFire, badAct, admitLate, fireLate, 
                               badComp, startAfterHalt, falseFail, forged, 
                               stack, n, a, dg, sg, st, held, hl, e, ca, cdg, 
                               cz, rn, ra, rdg, tl, lst, j, ph, ls, ba, bdg, 
                               re >>

DGuard(self) == /\ pc[self] = "DGuard"
                /\ startAfterHalt' = (startAfterHalt \/ (sg[self] /\ haltIn[n[self]]))
                /\ admitLate' = (admitLate \/ (IsChild(a[self]) /\ Expired(a[self], now) /\ ~(dg[self] /\ Bug # "NoCallGuard")))
                /\ cz' = [cz EXCEPT ![self] = "perm"]
                /\ IF dg[self] /\ Expired(a[self], now) /\ Bug # "NoCallGuard"
                      THEN /\ e' = [e EXCEPT ![self] = {"F"}]
                           /\ pc' = [pc EXCEPT ![self] = "DClass"]
                      ELSE /\ IF K(n[self], i[self]) = "eff"
                                 THEN /\ e' = [e EXCEPT ![self] = {}]
                                      /\ pc' = [pc EXCEPT ![self] = "EMark"]
                                 ELSE /\ IF K(n[self], i[self]) = "fail"
                                            THEN /\ e' = [e EXCEPT ![self] = {"F"}]
                                                 /\ pc' = [pc EXCEPT ![self] = "DClass"]
                                            ELSE /\ IF K(n[self], i[self]) = "deleg"
                                                       THEN /\ e' = [e EXCEPT ![self] = {}]
                                                            /\ pc' = [pc EXCEPT ![self] = "DgRead"]
                                                       ELSE /\ e' = [e EXCEPT ![self] = {}]
                                                            /\ pc' = [pc EXCEPT ![self] = "SStart"]
                /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                bound, want, verdict, ambig, readErr, crashes, 
                                wrongs, lost, rv, rb, haltIn, fx, fa, granted, 
                                walk, listed, anyFire, badAct, fireLate, 
                                badComp, falseFail, forged, stack, n, a, dg, 
                                sg, st, i, held, hl, ca, cdg, rn, ra, rdg, tl, 
                                lst, j, ph, ls, ba, bdg, re >>

EMark(self) == /\ pc[self] = "EMark"
               /\ mk' = [mk EXCEPT ![n[self]][i[self]] = TRUE]
               /\ pc' = [pc EXCEPT ![self] = "EFire"]
               /\ UNCHANGED << opened, res, lk, cp, aj, cm, ab, now, bound, 
                               want, verdict, ambig, readErr, crashes, wrongs, 
                               lost, rv, rb, haltIn, fx, fa, granted, walk, 
                               listed, anyFire, badAct, admitLate, fireLate, 
                               badComp, startAfterHalt, falseFail, forged, 
                               stack, n, a, dg, sg, st, i, held, hl, e, ca, 
                               cdg, cz, rn, ra, rdg, tl, lst, j, ph, ls, ba, 
                               bdg, re >>

EFire(self) == /\ pc[self] = "EFire"
               /\ fx' = [fx EXCEPT ![n[self]][i[self]] = "in"]
               /\ fa' = [fa EXCEPT ![n[self]][i[self]] = a[self].id]
               /\ anyFire' = TRUE
               /\ badAct' = (badAct \/ BadAuth(n[self], a[self].id))
               /\ fireLate' = (fireLate \/ (IsChild(a[self]) /\ Expired(a[self], now)))
               /\ pc' = [pc EXCEPT ![self] = "ERes"]
               /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, bound, 
                               want, verdict, ambig, readErr, crashes, wrongs, 
                               lost, rv, rb, haltIn, granted, walk, listed, 
                               admitLate, badComp, startAfterHalt, falseFail, 
                               forged, stack, n, a, dg, sg, st, i, held, hl, e, 
                               ca, cdg, cz, rn, ra, rdg, tl, lst, j, ph, ls, 
                               ba, bdg, re >>

ERes(self) == /\ pc[self] = "ERes"
              /\ \/ /\ e' = [e EXCEPT ![self] = {}]
                    /\ lost' = lost
                 \/ /\ lost < MaxLost
                    /\ lost' = lost + 1
                    /\ e' = [e EXCEPT ![self] = {"L"}]
              /\ pc' = [pc EXCEPT ![self] = "DClass"]
              /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, bound, 
                              want, verdict, ambig, readErr, crashes, wrongs, 
                              rv, rb, haltIn, fx, fa, granted, walk, listed, 
                              anyFire, badAct, admitLate, fireLate, badComp, 
                              startAfterHalt, falseFail, forged, stack, n, a, 
                              dg, sg, st, i, held, hl, ca, cdg, cz, rn, ra, 
                              rdg, tl, lst, j, ph, ls, ba, bdg, re >>

DgRead(self) == /\ pc[self] = "DgRead"
                /\ \/ /\ readErr < MaxReadErr
                      /\ readErr' = readErr + 1
                      /\ IF Bug = "StorageRecorded"
                            THEN /\ e' = [e EXCEPT ![self] = {"F"}]
                                 /\ cz' = [cz EXCEPT ![self] = "trans"]
                            ELSE /\ e' = [e EXCEPT ![self] = {"U"}]
                                 /\ cz' = cz
                      /\ pc' = [pc EXCEPT ![self] = "DClass"]
                      /\ UNCHANGED <<want, ca, cdg>>
                   \/ /\ IF a[self] = NoneG
                            THEN /\ IF aj[Ch(n[self], i[self])].g # NoneG
                                       THEN /\ e' = [e EXCEPT ![self] = {"U"}]
                                            /\ want' = RootFor(aj[Ch(n[self], i[self])].g.par, want)
                                            /\ pc' = [pc EXCEPT ![self] = "DClass"]
                                       ELSE /\ pc' = [pc EXCEPT ![self] = "DgUng"]
                                            /\ UNCHANGED << want, e >>
                                 /\ UNCHANGED << ca, cdg, cz >>
                            ELSE /\ IF aj[Ch(n[self], i[self])].ung
                                       THEN /\ e' = [e EXCEPT ![self] = {"U"}]
                                            /\ want' = NoneG
                                            /\ pc' = [pc EXCEPT ![self] = "DClass"]
                                            /\ UNCHANGED << ca, cdg, cz >>
                                       ELSE /\ IF aj[Ch(n[self], i[self])].g = NoneG /\ Recs(Ch(n[self], i[self])) /\ Bug # "MintOnRecords"
                                                  THEN /\ e' = [e EXCEPT ![self] = {"U"}]
                                                       /\ pc' = [pc EXCEPT ![self] = "DClass"]
                                                       /\ UNCHANGED << want, 
                                                                       ca, cdg, 
                                                                       cz >>
                                                  ELSE /\ IF aj[Ch(n[self], i[self])].g # NoneG
                                                             THEN /\ IF aj[Ch(n[self], i[self])].g.sub # Ch(n[self], i[self])
                                                                        THEN /\ e' = [e EXCEPT ![self] = {"F"}]
                                                                             /\ pc' = [pc EXCEPT ![self] = "DClass"]
                                                                             /\ UNCHANGED << want, 
                                                                                             ca, 
                                                                                             cdg, 
                                                                                             cz >>
                                                                        ELSE /\ IF aj[Ch(n[self], i[self])].g.par # a[self].id
                                                                                   THEN /\ IF Bug = "WrongAuthRecorded"
                                                                                              THEN /\ e' = [e EXCEPT ![self] = {"F"}]
                                                                                                   /\ cz' = [cz EXCEPT ![self] = "trans"]
                                                                                                   /\ want' = want
                                                                                              ELSE /\ e' = [e EXCEPT ![self] = {"U"}]
                                                                                                   /\ want' = RootFor(aj[Ch(n[self], i[self])].g.par, want)
                                                                                                   /\ cz' = cz
                                                                                        /\ pc' = [pc EXCEPT ![self] = "DClass"]
                                                                                        /\ UNCHANGED << ca, 
                                                                                                        cdg >>
                                                                                   ELSE /\ IF Expired(aj[Ch(n[self], i[self])].g, now)
                                                                                              THEN /\ IF Bug = "ExpiredUnrecorded"
                                                                                                         THEN /\ e' = [e EXCEPT ![self] = {"U"}]
                                                                                                         ELSE /\ e' = [e EXCEPT ![self] = {"F"}]
                                                                                                   /\ pc' = [pc EXCEPT ![self] = "DClass"]
                                                                                                   /\ UNCHANGED << ca, 
                                                                                                                   cdg >>
                                                                                              ELSE /\ ca' = [ca EXCEPT ![self] = aj[Ch(n[self], i[self])].g]
                                                                                                   /\ cdg' = [cdg EXCEPT ![self] = TRUE]
                                                                                                   /\ pc' = [pc EXCEPT ![self] = "DgRun"]
                                                                                                   /\ e' = e
                                                                                        /\ UNCHANGED << want, 
                                                                                                        cz >>
                                                             ELSE /\ pc' = [pc EXCEPT ![self] = "DgMint"]
                                                                  /\ UNCHANGED << want, 
                                                                                  e, 
                                                                                  ca, 
                                                                                  cdg, 
                                                                                  cz >>
                      /\ UNCHANGED readErr
                /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                bound, verdict, ambig, crashes, wrongs, lost, 
                                rv, rb, haltIn, fx, fa, granted, walk, listed, 
                                anyFire, badAct, admitLate, fireLate, badComp, 
                                startAfterHalt, falseFail, forged, stack, n, a, 
                                dg, sg, st, i, held, hl, rn, ra, rdg, tl, lst, 
                                j, ph, ls, ba, bdg, re >>

DgUng(self) == /\ pc[self] = "DgUng"
               /\ \/ /\ aj' = [aj EXCEPT ![Ch(n[self], i[self])].ung = TRUE]
                     /\ granted' = [granted EXCEPT ![Ch(n[self], i[self])] = granted[Ch(n[self], i[self])] \cup {"none"}]
                     /\ ca' = [ca EXCEPT ![self] = a[self]]
                     /\ cdg' = [cdg EXCEPT ![self] = dg[self]]
                     /\ pc' = [pc EXCEPT ![self] = "DgRun"]
                     /\ UNCHANGED <<ambig, e, cz>>
                  \/ /\ ambig < MaxAmbig
                     /\ ambig' = ambig + 1
                     /\ aj' = [aj EXCEPT ![Ch(n[self], i[self])].ung = TRUE]
                     /\ granted' = [granted EXCEPT ![Ch(n[self], i[self])] = granted[Ch(n[self], i[self])] \cup {"none"}]
                     /\ IF Bug = "StorageRecorded"
                           THEN /\ e' = [e EXCEPT ![self] = {"F"}]
                                /\ cz' = [cz EXCEPT ![self] = "trans"]
                           ELSE /\ e' = [e EXCEPT ![self] = {"U"}]
                                /\ cz' = cz
                     /\ pc' = [pc EXCEPT ![self] = "DClass"]
                     /\ UNCHANGED <<ca, cdg>>
                  \/ /\ ambig < MaxAmbig
                     /\ ambig' = ambig + 1
                     /\ IF Bug = "StorageRecorded"
                           THEN /\ e' = [e EXCEPT ![self] = {"F"}]
                                /\ cz' = [cz EXCEPT ![self] = "trans"]
                           ELSE /\ e' = [e EXCEPT ![self] = {"U"}]
                                /\ cz' = cz
                     /\ pc' = [pc EXCEPT ![self] = "DClass"]
                     /\ UNCHANGED <<aj, granted, ca, cdg>>
               /\ UNCHANGED << opened, mk, res, lk, cp, cm, ab, now, bound, 
                               want, verdict, readErr, crashes, wrongs, lost, 
                               rv, rb, haltIn, fx, fa, walk, listed, anyFire, 
                               badAct, admitLate, fireLate, badComp, 
                               startAfterHalt, falseFail, forged, stack, n, a, 
                               dg, sg, st, i, held, hl, rn, ra, rdg, tl, lst, 
                               j, ph, ls, ba, bdg, re >>

DgMint(self) == /\ pc[self] = "DgMint"
                /\ IF Expired(a[self], now)
                      THEN /\ e' = [e EXCEPT ![self] = {"U"}]
                           /\ want' = RootFor("P2", want)
                           /\ pc' = [pc EXCEPT ![self] = "DClass"]
                      ELSE /\ IF C(n[self], i[self]).nsub = "other" /\ Bug # "MintForeign"
                                 THEN /\ e' = [e EXCEPT ![self] = {"F"}]
                                      /\ pc' = [pc EXCEPT ![self] = "DClass"]
                                 ELSE /\ IF Expired(MintOf(a[self], n[self], i[self]), now)
                                            THEN /\ e' = [e EXCEPT ![self] = {"F"}]
                                                 /\ pc' = [pc EXCEPT ![self] = "DClass"]
                                            ELSE /\ pc' = [pc EXCEPT ![self] = "DgRec"]
                                                 /\ e' = e
                           /\ want' = want
                /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                bound, verdict, ambig, readErr, crashes, 
                                wrongs, lost, rv, rb, haltIn, fx, fa, granted, 
                                walk, listed, anyFire, badAct, admitLate, 
                                fireLate, badComp, startAfterHalt, falseFail, 
                                forged, stack, n, a, dg, sg, st, i, held, hl, 
                                ca, cdg, cz, rn, ra, rdg, tl, lst, j, ph, ls, 
                                ba, bdg, re >>

DgRec(self) == /\ pc[self] = "DgRec"
               /\ granted' = [granted EXCEPT ![Ch(n[self], i[self])] = IF MintOf(a[self], n[self], i[self]).sub = Ch(n[self], i[self]) THEN granted[Ch(n[self], i[self])] \cup {MintOf(a[self], n[self], i[self]).id}
                                                                       ELSE granted[Ch(n[self], i[self])]]
               /\ \/ /\ aj' = [aj EXCEPT ![Ch(n[self], i[self])].g = MintOf(a[self], n[self], i[self])]
                     /\ ca' = [ca EXCEPT ![self] = MintOf(a[self], n[self], i[self])]
                     /\ cdg' = [cdg EXCEPT ![self] = TRUE]
                     /\ pc' = [pc EXCEPT ![self] = "DgRun"]
                     /\ UNCHANGED <<ambig, e, cz>>
                  \/ /\ ambig < MaxAmbig
                     /\ ambig' = ambig + 1
                     /\ aj' = [aj EXCEPT ![Ch(n[self], i[self])].g = MintOf(a[self], n[self], i[self])]
                     /\ IF Bug = "StorageRecorded"
                           THEN /\ e' = [e EXCEPT ![self] = {"F"}]
                                /\ cz' = [cz EXCEPT ![self] = "trans"]
                           ELSE /\ e' = [e EXCEPT ![self] = {"U"}]
                                /\ cz' = cz
                     /\ pc' = [pc EXCEPT ![self] = "DClass"]
                     /\ UNCHANGED <<ca, cdg>>
                  \/ /\ ambig < MaxAmbig
                     /\ ambig' = ambig + 1
                     /\ IF Bug = "StorageRecorded"
                           THEN /\ e' = [e EXCEPT ![self] = {"F"}]
                                /\ cz' = [cz EXCEPT ![self] = "trans"]
                           ELSE /\ e' = [e EXCEPT ![self] = {"U"}]
                                /\ cz' = cz
                     /\ pc' = [pc EXCEPT ![self] = "DClass"]
                     /\ UNCHANGED <<aj, ca, cdg>>
               /\ UNCHANGED << opened, mk, res, lk, cp, cm, ab, now, bound, 
                               want, verdict, readErr, crashes, wrongs, lost, 
                               rv, rb, haltIn, fx, fa, walk, listed, anyFire, 
                               badAct, admitLate, fireLate, badComp, 
                               startAfterHalt, falseFail, forged, stack, n, a, 
                               dg, sg, st, i, held, hl, rn, ra, rdg, tl, lst, 
                               j, ph, ls, ba, bdg, re >>

DgRun(self) == /\ pc[self] = "DgRun"
               /\ /\ a' = [a EXCEPT ![self] = ca[self]]
                  /\ dg' = [dg EXCEPT ![self] = cdg[self]]
                  /\ n' = [n EXCEPT ![self] = Ch(n[self], i[self])]
                  /\ sg' = [sg EXCEPT ![self] = sg[self]]
                  /\ st' = [st EXCEPT ![self] = sg[self] \/ st[self]]
                  /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Drive",
                                                           pc        |->  "DgRet",
                                                           i         |->  i[self],
                                                           held      |->  held[self],
                                                           hl        |->  hl[self],
                                                           e         |->  e[self],
                                                           ca        |->  ca[self],
                                                           cdg       |->  cdg[self],
                                                           cz        |->  cz[self],
                                                           n         |->  n[self],
                                                           a         |->  a[self],
                                                           dg        |->  dg[self],
                                                           sg        |->  sg[self],
                                                           st        |->  st[self] ] >>
                                                       \o stack[self]]
               /\ i' = [i EXCEPT ![self] = 1]
               /\ held' = [held EXCEPT ![self] = {}]
               /\ hl' = [hl EXCEPT ![self] = FALSE]
               /\ e' = [e EXCEPT ![self] = {}]
               /\ ca' = [ca EXCEPT ![self] = NoneG]
               /\ cdg' = [cdg EXCEPT ![self] = FALSE]
               /\ cz' = [cz EXCEPT ![self] = "perm"]
               /\ pc' = [pc EXCEPT ![self] = "DOpen"]
               /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, bound, 
                               want, verdict, ambig, readErr, crashes, wrongs, 
                               lost, rv, rb, haltIn, fx, fa, granted, walk, 
                               listed, anyFire, badAct, admitLate, fireLate, 
                               badComp, startAfterHalt, falseFail, forged, rn, 
                               ra, rdg, tl, lst, j, ph, ls, ba, bdg, re >>

DgRet(self) == /\ pc[self] = "DgRet"
               /\ e' = [e EXCEPT ![self] = rv[Ch(n[self], i[self])]]
               /\ pc' = [pc EXCEPT ![self] = "DClass"]
               /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, bound, 
                               want, verdict, ambig, readErr, crashes, wrongs, 
                               lost, rv, rb, haltIn, fx, fa, granted, walk, 
                               listed, anyFire, badAct, admitLate, fireLate, 
                               badComp, startAfterHalt, falseFail, forged, 
                               stack, n, a, dg, sg, st, i, held, hl, ca, cdg, 
                               cz, rn, ra, rdg, tl, lst, j, ph, ls, ba, bdg, 
                               re >>

SStart(self) == /\ pc[self] = "SStart"
                /\ forged' = (forged \/ (C(n[self], i[self]).forge /\ <<n[self], i[self]>> = <<n[self], i[self] + 1>>))
                /\ IF Ch(n[self], i[self]) = 0 \/ C(n[self], i[self]).late
                      THEN /\ pc' = [pc EXCEPT ![self] = "SWrite"]
                           /\ e' = e
                      ELSE /\ IF ~(IF Bug = "B3" THEN sg[self] ELSE st[self])
                                 THEN /\ pc' = [pc EXCEPT ![self] = "SRun"]
                                      /\ e' = e
                                 ELSE /\ IF C(n[self], i[self]).store = "other" /\ Bug # "B2"
                                            THEN /\ e' = [e EXCEPT ![self] = {"F"}]
                                                 /\ pc' = [pc EXCEPT ![self] = "DClass"]
                                            ELSE /\ pc' = [pc EXCEPT ![self] = "SLink"]
                                                 /\ e' = e
                /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                bound, want, verdict, ambig, readErr, crashes, 
                                wrongs, lost, rv, rb, haltIn, fx, fa, granted, 
                                walk, listed, anyFire, badAct, admitLate, 
                                fireLate, badComp, startAfterHalt, falseFail, 
                                stack, n, a, dg, sg, st, i, held, hl, ca, cdg, 
                                cz, rn, ra, rdg, tl, lst, j, ph, ls, ba, bdg, 
                                re >>

SLink(self) == /\ pc[self] = "SLink"
               /\ \/ /\ lk' = [lk EXCEPT ![n[self]][i[self]] = TRUE]
                     /\ pc' = [pc EXCEPT ![self] = "SRun"]
                     /\ UNCHANGED <<ambig, e>>
                  \/ /\ ambig < MaxAmbig
                     /\ ambig' = ambig + 1
                     /\ lk' = [lk EXCEPT ![n[self]][i[self]] = TRUE]
                     /\ e' = [e EXCEPT ![self] = {"F"}]
                     /\ pc' = [pc EXCEPT ![self] = "DClass"]
                  \/ /\ ambig < MaxAmbig
                     /\ ambig' = ambig + 1
                     /\ e' = [e EXCEPT ![self] = {"F"}]
                     /\ pc' = [pc EXCEPT ![self] = "DClass"]
                     /\ lk' = lk
               /\ UNCHANGED << opened, mk, res, cp, aj, cm, ab, now, bound, 
                               want, verdict, readErr, crashes, wrongs, lost, 
                               rv, rb, haltIn, fx, fa, granted, walk, listed, 
                               anyFire, badAct, admitLate, fireLate, badComp, 
                               startAfterHalt, falseFail, forged, stack, n, a, 
                               dg, sg, st, i, held, hl, ca, cdg, cz, rn, ra, 
                               rdg, tl, lst, j, ph, ls, ba, bdg, re >>

SRun(self) == /\ pc[self] = "SRun"
              /\ /\ a' = [a EXCEPT ![self] = a[self]]
                 /\ dg' = [dg EXCEPT ![self] = dg[self]]
                 /\ n' = [n EXCEPT ![self] = Ch(n[self], i[self])]
                 /\ sg' = [sg EXCEPT ![self] = C(n[self], i[self]).saga]
                 /\ st' = [st EXCEPT ![self] = C(n[self], i[self]).saga \/ st[self]]
                 /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Drive",
                                                          pc        |->  "SRet",
                                                          i         |->  i[self],
                                                          held      |->  held[self],
                                                          hl        |->  hl[self],
                                                          e         |->  e[self],
                                                          ca        |->  ca[self],
                                                          cdg       |->  cdg[self],
                                                          cz        |->  cz[self],
                                                          n         |->  n[self],
                                                          a         |->  a[self],
                                                          dg        |->  dg[self],
                                                          sg        |->  sg[self],
                                                          st        |->  st[self] ] >>
                                                      \o stack[self]]
              /\ i' = [i EXCEPT ![self] = 1]
              /\ held' = [held EXCEPT ![self] = {}]
              /\ hl' = [hl EXCEPT ![self] = FALSE]
              /\ e' = [e EXCEPT ![self] = {}]
              /\ ca' = [ca EXCEPT ![self] = NoneG]
              /\ cdg' = [cdg EXCEPT ![self] = FALSE]
              /\ cz' = [cz EXCEPT ![self] = "perm"]
              /\ pc' = [pc EXCEPT ![self] = "DOpen"]
              /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, bound, 
                              want, verdict, ambig, readErr, crashes, wrongs, 
                              lost, rv, rb, haltIn, fx, fa, granted, walk, 
                              listed, anyFire, badAct, admitLate, fireLate, 
                              badComp, startAfterHalt, falseFail, forged, rn, 
                              ra, rdg, tl, lst, j, ph, ls, ba, bdg, re >>

SRet(self) == /\ pc[self] = "SRet"
              /\ e' = [e EXCEPT ![self] = rv[Ch(n[self], i[self])]]
              /\ pc' = [pc EXCEPT ![self] = "SWrite"]
              /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, bound, 
                              want, verdict, ambig, readErr, crashes, wrongs, 
                              lost, rv, rb, haltIn, fx, fa, granted, walk, 
                              listed, anyFire, badAct, admitLate, fireLate, 
                              badComp, startAfterHalt, falseFail, forged, 
                              stack, n, a, dg, sg, st, i, held, hl, ca, cdg, 
                              cz, rn, ra, rdg, tl, lst, j, ph, ls, ba, bdg, re >>

SWrite(self) == /\ pc[self] = "SWrite"
                /\ IF C(n[self], i[self]).w /\ e[self] = {}
                      THEN /\ fx' = [fx EXCEPT ![n[self]][i[self]] = "in"]
                           /\ fa' = [fa EXCEPT ![n[self]][i[self]] = a[self].id]
                           /\ anyFire' = TRUE
                           /\ badAct' = (badAct \/ BadAuth(n[self], a[self].id))
                           /\ fireLate' = (fireLate \/ (IsChild(a[self]) /\ Expired(a[self], now)))
                      ELSE /\ TRUE
                           /\ UNCHANGED << fx, fa, anyFire, badAct, fireLate >>
                /\ pc' = [pc EXCEPT ![self] = "DClass"]
                /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                bound, want, verdict, ambig, readErr, crashes, 
                                wrongs, lost, rv, rb, haltIn, granted, walk, 
                                listed, admitLate, badComp, startAfterHalt, 
                                falseFail, forged, stack, n, a, dg, sg, st, i, 
                                held, hl, e, ca, cdg, cz, rn, ra, rdg, tl, lst, 
                                j, ph, ls, ba, bdg, re >>

DClass(self) == /\ pc[self] = "DClass"
                /\ IF e[self] = {}
                      THEN /\ res' = [res EXCEPT ![n[self]][i[self]] = "ok"]
                           /\ IF K(n[self], i[self]) = "sub" /\ C(n[self], i[self]).late
                                 THEN /\ pc' = [pc EXCEPT ![self] = "SLate"]
                                      /\ i' = i
                                 ELSE /\ i' = [i EXCEPT ![self] = i[self] + 1]
                                      /\ pc' = [pc EXCEPT ![self] = "DNext"]
                           /\ UNCHANGED << rv, haltIn, falseFail, stack, n, a, 
                                           dg, sg, st, held, hl, e, ca, cdg, 
                                           cz >>
                      ELSE /\ IF "U" \in e[self]
                                 THEN /\ held' = [held EXCEPT ![self] = held[self] \cup e[self]]
                                      /\ haltIn' = [haltIn EXCEPT ![n[self]] = haltIn[n[self]] \/ (sg[self] /\ e[self] \cap {"H", "L"} # {})]
                                      /\ hl' = [hl EXCEPT ![self] = hl[self] \/ (sg[self] /\ e[self] \cap {"H", "L"} # {} /\ Bug # "HiddenHalt")]
                                      /\ i' = [i EXCEPT ![self] = i[self] + 1]
                                      /\ pc' = [pc EXCEPT ![self] = "DNext"]
                                      /\ UNCHANGED << res, rv, falseFail, 
                                                      stack, n, a, dg, sg, st, 
                                                      e, ca, cdg, cz >>
                                 ELSE /\ IF "H" \in e[self] \/ ("L" \in e[self] /\ K(n[self], i[self]) \in {"eff", "deleg"})
                                            THEN /\ held' = [held EXCEPT ![self] = held[self] \cup e[self]]
                                                 /\ haltIn' = [haltIn EXCEPT ![n[self]] = haltIn[n[self]] \/ sg[self]]
                                                 /\ hl' = [hl EXCEPT ![self] = hl[self] \/ sg[self]]
                                                 /\ i' = [i EXCEPT ![self] = i[self] + 1]
                                                 /\ pc' = [pc EXCEPT ![self] = "DNext"]
                                                 /\ UNCHANGED << res, rv, 
                                                                 falseFail, 
                                                                 stack, n, a, 
                                                                 dg, sg, st, e, 
                                                                 ca, cdg, cz >>
                                            ELSE /\ IF "S" \in e[self] /\ K(n[self], i[self]) = "deleg"
                                                       THEN /\ rv' = [rv EXCEPT ![n[self]] = e[self]]
                                                            /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                                            /\ i' = [i EXCEPT ![self] = Head(stack[self]).i]
                                                            /\ held' = [held EXCEPT ![self] = Head(stack[self]).held]
                                                            /\ hl' = [hl EXCEPT ![self] = Head(stack[self]).hl]
                                                            /\ e' = [e EXCEPT ![self] = Head(stack[self]).e]
                                                            /\ ca' = [ca EXCEPT ![self] = Head(stack[self]).ca]
                                                            /\ cdg' = [cdg EXCEPT ![self] = Head(stack[self]).cdg]
                                                            /\ cz' = [cz EXCEPT ![self] = Head(stack[self]).cz]
                                                            /\ n' = [n EXCEPT ![self] = Head(stack[self]).n]
                                                            /\ a' = [a EXCEPT ![self] = Head(stack[self]).a]
                                                            /\ dg' = [dg EXCEPT ![self] = Head(stack[self]).dg]
                                                            /\ sg' = [sg EXCEPT ![self] = Head(stack[self]).sg]
                                                            /\ st' = [st EXCEPT ![self] = Head(stack[self]).st]
                                                            /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                                            /\ UNCHANGED << res, 
                                                                            falseFail >>
                                                       ELSE /\ IF sg[self]
                                                                  THEN /\ res' = [res EXCEPT ![n[self]][i[self]] = IF "L" \in e[self] THEN "sfu" ELSE "sf"]
                                                                       /\ falseFail' = (falseFail \/ (K(n[self], i[self]) = "deleg" /\ cz[self] = "trans"))
                                                                       /\ pc' = [pc EXCEPT ![self] = "DRb"]
                                                                       /\ i' = i
                                                                  ELSE /\ res' = [res EXCEPT ![n[self]][i[self]] = "err"]
                                                                       /\ falseFail' = (falseFail \/ (K(n[self], i[self]) = "deleg" /\ cz[self] = "trans"))
                                                                       /\ i' = [i EXCEPT ![self] = i[self] + 1]
                                                                       /\ pc' = [pc EXCEPT ![self] = "DNext"]
                                                            /\ UNCHANGED << rv, 
                                                                            stack, 
                                                                            n, 
                                                                            a, 
                                                                            dg, 
                                                                            sg, 
                                                                            st, 
                                                                            held, 
                                                                            hl, 
                                                                            e, 
                                                                            ca, 
                                                                            cdg, 
                                                                            cz >>
                                                 /\ UNCHANGED haltIn
                /\ UNCHANGED << opened, mk, lk, cp, aj, cm, ab, now, bound, 
                                want, verdict, ambig, readErr, crashes, wrongs, 
                                lost, rb, fx, fa, granted, walk, listed, 
                                anyFire, badAct, admitLate, fireLate, badComp, 
                                startAfterHalt, forged, rn, ra, rdg, tl, lst, 
                                j, ph, ls, ba, bdg, re >>

SLate(self) == /\ pc[self] = "SLate"
               /\ IF Bug = "NoReturnedCheck"
                     THEN /\ forged' = TRUE
                          /\ IF st[self]
                                THEN /\ lk' = [lk EXCEPT ![n[self]][i[self]] = TRUE]
                                ELSE /\ TRUE
                                     /\ lk' = lk
                          /\ /\ a' = [a EXCEPT ![self] = a[self]]
                             /\ dg' = [dg EXCEPT ![self] = dg[self]]
                             /\ n' = [n EXCEPT ![self] = Ch(n[self], i[self])]
                             /\ sg' = [sg EXCEPT ![self] = C(n[self], i[self]).saga]
                             /\ st' = [st EXCEPT ![self] = C(n[self], i[self]).saga \/ st[self]]
                             /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Drive",
                                                                      pc        |->  "SLateRet",
                                                                      i         |->  i[self],
                                                                      held      |->  held[self],
                                                                      hl        |->  hl[self],
                                                                      e         |->  e[self],
                                                                      ca        |->  ca[self],
                                                                      cdg       |->  cdg[self],
                                                                      cz        |->  cz[self],
                                                                      n         |->  n[self],
                                                                      a         |->  a[self],
                                                                      dg        |->  dg[self],
                                                                      sg        |->  sg[self],
                                                                      st        |->  st[self] ] >>
                                                                  \o stack[self]]
                          /\ i' = [i EXCEPT ![self] = 1]
                          /\ held' = [held EXCEPT ![self] = {}]
                          /\ hl' = [hl EXCEPT ![self] = FALSE]
                          /\ e' = [e EXCEPT ![self] = {}]
                          /\ ca' = [ca EXCEPT ![self] = NoneG]
                          /\ cdg' = [cdg EXCEPT ![self] = FALSE]
                          /\ cz' = [cz EXCEPT ![self] = "perm"]
                          /\ pc' = [pc EXCEPT ![self] = "DOpen"]
                     ELSE /\ i' = [i EXCEPT ![self] = i[self] + 1]
                          /\ pc' = [pc EXCEPT ![self] = "DNext"]
                          /\ UNCHANGED << lk, forged, stack, n, a, dg, sg, st, 
                                          held, hl, e, ca, cdg, cz >>
               /\ UNCHANGED << opened, mk, res, cp, aj, cm, ab, now, bound, 
                               want, verdict, ambig, readErr, crashes, wrongs, 
                               lost, rv, rb, haltIn, fx, fa, granted, walk, 
                               listed, anyFire, badAct, admitLate, fireLate, 
                               badComp, startAfterHalt, falseFail, rn, ra, rdg, 
                               tl, lst, j, ph, ls, ba, bdg, re >>

SLateRet(self) == /\ pc[self] = "SLateRet"
                  /\ i' = [i EXCEPT ![self] = i[self] + 1]
                  /\ pc' = [pc EXCEPT ![self] = "DNext"]
                  /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                  bound, want, verdict, ambig, readErr, 
                                  crashes, wrongs, lost, rv, rb, haltIn, fx, 
                                  fa, granted, walk, listed, anyFire, badAct, 
                                  admitLate, fireLate, badComp, startAfterHalt, 
                                  falseFail, forged, stack, n, a, dg, sg, st, 
                                  held, hl, e, ca, cdg, cz, rn, ra, rdg, tl, 
                                  lst, j, ph, ls, ba, bdg, re >>

DRb(self) == /\ pc[self] = "DRb"
             /\ IF n[self] = Root
                   THEN /\ walk' = {}
                   ELSE /\ TRUE
                        /\ walk' = walk
             /\ /\ lst' = [lst EXCEPT ![self] = n[self] = Root]
                /\ ra' = [ra EXCEPT ![self] = a[self]]
                /\ rdg' = [rdg EXCEPT ![self] = dg[self]]
                /\ rn' = [rn EXCEPT ![self] = n[self]]
                /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Rollback",
                                                         pc        |->  "DRbEnd",
                                                         j         |->  j[self],
                                                         ph        |->  ph[self],
                                                         ls        |->  ls[self],
                                                         ba        |->  ba[self],
                                                         bdg       |->  bdg[self],
                                                         re        |->  re[self],
                                                         rn        |->  rn[self],
                                                         ra        |->  ra[self],
                                                         rdg       |->  rdg[self],
                                                         tl        |->  tl[self],
                                                         lst       |->  lst[self] ] >>
                                                     \o stack[self]]
                /\ tl' = [tl EXCEPT ![self] = FALSE]
             /\ j' = [j EXCEPT ![self] = 0]
             /\ ph' = [ph EXCEPT ![self] = 1]
             /\ ls' = [ls EXCEPT ![self] = {}]
             /\ ba' = [ba EXCEPT ![self] = NoneG]
             /\ bdg' = [bdg EXCEPT ![self] = FALSE]
             /\ re' = [re EXCEPT ![self] = {}]
             /\ pc' = [pc EXCEPT ![self] = "RbOpen"]
             /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, bound, 
                             want, verdict, ambig, readErr, crashes, wrongs, 
                             lost, rv, rb, haltIn, fx, fa, granted, listed, 
                             anyFire, badAct, admitLate, fireLate, badComp, 
                             startAfterHalt, falseFail, forged, n, a, dg, sg, 
                             st, i, held, hl, e, ca, cdg, cz >>

DRbEnd(self) == /\ pc[self] = "DRbEnd"
                /\ IF rb[n[self]] = "ok"
                      THEN /\ ab' = [ab EXCEPT ![n[self]] = TRUE]
                           /\ IF n[self] = Root
                                 THEN /\ listed' = walk
                                 ELSE /\ TRUE
                                      /\ UNCHANGED listed
                      ELSE /\ TRUE
                           /\ UNCHANGED << ab, listed >>
                /\ rv' = [rv EXCEPT ![n[self]] = {"A"}]
                /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                /\ i' = [i EXCEPT ![self] = Head(stack[self]).i]
                /\ held' = [held EXCEPT ![self] = Head(stack[self]).held]
                /\ hl' = [hl EXCEPT ![self] = Head(stack[self]).hl]
                /\ e' = [e EXCEPT ![self] = Head(stack[self]).e]
                /\ ca' = [ca EXCEPT ![self] = Head(stack[self]).ca]
                /\ cdg' = [cdg EXCEPT ![self] = Head(stack[self]).cdg]
                /\ cz' = [cz EXCEPT ![self] = Head(stack[self]).cz]
                /\ n' = [n EXCEPT ![self] = Head(stack[self]).n]
                /\ a' = [a EXCEPT ![self] = Head(stack[self]).a]
                /\ dg' = [dg EXCEPT ![self] = Head(stack[self]).dg]
                /\ sg' = [sg EXCEPT ![self] = Head(stack[self]).sg]
                /\ st' = [st EXCEPT ![self] = Head(stack[self]).st]
                /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, now, bound, 
                                want, verdict, ambig, readErr, crashes, wrongs, 
                                lost, rb, haltIn, fx, fa, granted, walk, 
                                anyFire, badAct, admitLate, fireLate, badComp, 
                                startAfterHalt, falseFail, forged, rn, ra, rdg, 
                                tl, lst, j, ph, ls, ba, bdg, re >>

DEnd(self) == /\ pc[self] = "DEnd"
              /\ IF held[self] # {}
                    THEN /\ rv' = [rv EXCEPT ![n[self]] = held[self]]
                         /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                         /\ i' = [i EXCEPT ![self] = Head(stack[self]).i]
                         /\ held' = [held EXCEPT ![self] = Head(stack[self]).held]
                         /\ hl' = [hl EXCEPT ![self] = Head(stack[self]).hl]
                         /\ e' = [e EXCEPT ![self] = Head(stack[self]).e]
                         /\ ca' = [ca EXCEPT ![self] = Head(stack[self]).ca]
                         /\ cdg' = [cdg EXCEPT ![self] = Head(stack[self]).cdg]
                         /\ cz' = [cz EXCEPT ![self] = Head(stack[self]).cz]
                         /\ n' = [n EXCEPT ![self] = Head(stack[self]).n]
                         /\ a' = [a EXCEPT ![self] = Head(stack[self]).a]
                         /\ dg' = [dg EXCEPT ![self] = Head(stack[self]).dg]
                         /\ sg' = [sg EXCEPT ![self] = Head(stack[self]).sg]
                         /\ st' = [st EXCEPT ![self] = Head(stack[self]).st]
                         /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                         /\ cm' = cm
                    ELSE /\ IF n[self] \in MErr
                               THEN /\ rv' = [rv EXCEPT ![n[self]] = {"F"}]
                                    /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                    /\ i' = [i EXCEPT ![self] = Head(stack[self]).i]
                                    /\ held' = [held EXCEPT ![self] = Head(stack[self]).held]
                                    /\ hl' = [hl EXCEPT ![self] = Head(stack[self]).hl]
                                    /\ e' = [e EXCEPT ![self] = Head(stack[self]).e]
                                    /\ ca' = [ca EXCEPT ![self] = Head(stack[self]).ca]
                                    /\ cdg' = [cdg EXCEPT ![self] = Head(stack[self]).cdg]
                                    /\ cz' = [cz EXCEPT ![self] = Head(stack[self]).cz]
                                    /\ n' = [n EXCEPT ![self] = Head(stack[self]).n]
                                    /\ a' = [a EXCEPT ![self] = Head(stack[self]).a]
                                    /\ dg' = [dg EXCEPT ![self] = Head(stack[self]).dg]
                                    /\ sg' = [sg EXCEPT ![self] = Head(stack[self]).sg]
                                    /\ st' = [st EXCEPT ![self] = Head(stack[self]).st]
                                    /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                    /\ cm' = cm
                               ELSE /\ cm' = [cm EXCEPT ![n[self]] = TRUE]
                                    /\ rv' = [rv EXCEPT ![n[self]] = {}]
                                    /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                    /\ i' = [i EXCEPT ![self] = Head(stack[self]).i]
                                    /\ held' = [held EXCEPT ![self] = Head(stack[self]).held]
                                    /\ hl' = [hl EXCEPT ![self] = Head(stack[self]).hl]
                                    /\ e' = [e EXCEPT ![self] = Head(stack[self]).e]
                                    /\ ca' = [ca EXCEPT ![self] = Head(stack[self]).ca]
                                    /\ cdg' = [cdg EXCEPT ![self] = Head(stack[self]).cdg]
                                    /\ cz' = [cz EXCEPT ![self] = Head(stack[self]).cz]
                                    /\ n' = [n EXCEPT ![self] = Head(stack[self]).n]
                                    /\ a' = [a EXCEPT ![self] = Head(stack[self]).a]
                                    /\ dg' = [dg EXCEPT ![self] = Head(stack[self]).dg]
                                    /\ sg' = [sg EXCEPT ![self] = Head(stack[self]).sg]
                                    /\ st' = [st EXCEPT ![self] = Head(stack[self]).st]
                                    /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
              /\ UNCHANGED << opened, mk, res, lk, cp, aj, ab, now, bound, 
                              want, verdict, ambig, readErr, crashes, wrongs, 
                              lost, rb, haltIn, fx, fa, granted, walk, listed, 
                              anyFire, badAct, admitLate, fireLate, badComp, 
                              startAfterHalt, falseFail, forged, rn, ra, rdg, 
                              tl, lst, j, ph, ls, ba, bdg, re >>

Drive(self) == DOpen(self) \/ DNext(self) \/ DGuard(self) \/ EMark(self)
                  \/ EFire(self) \/ ERes(self) \/ DgRead(self)
                  \/ DgUng(self) \/ DgMint(self) \/ DgRec(self)
                  \/ DgRun(self) \/ DgRet(self) \/ SStart(self)
                  \/ SLink(self) \/ SRun(self) \/ SRet(self)
                  \/ SWrite(self) \/ DClass(self) \/ SLate(self)
                  \/ SLateRet(self) \/ DRb(self) \/ DRbEnd(self)
                  \/ DEnd(self)

RbOpen(self) == /\ pc[self] = "RbOpen"
                /\ IF OnOther(rn[self]) \/ ~opened[rn[self]]
                      THEN /\ rb' = [rb EXCEPT ![rn[self]] = "ok"]
                           /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                           /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                           /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                           /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                           /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                           /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                           /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                           /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                           /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                           /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                           /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                           /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                           /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                      ELSE /\ ls' = [ls EXCEPT ![self] = Links(rn[self])]
                           /\ j' = [j EXCEPT ![self] = Len(Tree[rn[self]])]
                           /\ ph' = [ph EXCEPT ![self] = 1]
                           /\ pc' = [pc EXCEPT ![self] = "RbLoop"]
                           /\ UNCHANGED << rb, stack, rn, ra, rdg, tl, lst, ba, 
                                           bdg, re >>
                /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                bound, want, verdict, ambig, readErr, crashes, 
                                wrongs, lost, rv, haltIn, fx, fa, granted, 
                                walk, listed, anyFire, badAct, admitLate, 
                                fireLate, badComp, startAfterHalt, falseFail, 
                                forged, n, a, dg, sg, st, i, held, hl, e, ca, 
                                cdg, cz >>

RbLoop(self) == /\ pc[self] = "RbLoop"
                /\ IF j[self] = 0 /\ ph[self] = 1
                      THEN /\ ph' = [ph EXCEPT ![self] = 2]
                           /\ j' = [j EXCEPT ![self] = Len(Tree[rn[self]])]
                           /\ pc' = [pc EXCEPT ![self] = "RbLoop"]
                           /\ UNCHANGED << rb, walk, stack, rn, ra, rdg, tl, 
                                           lst, ls, ba, bdg, re >>
                      ELSE /\ IF j[self] = 0
                                 THEN /\ rb' = [rb EXCEPT ![rn[self]] = "ok"]
                                      /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                      /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                                      /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                                      /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                                      /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                                      /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                                      /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                                      /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                                      /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                                      /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                                      /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                                      /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                                      /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                      /\ walk' = walk
                                 ELSE /\ IF ph[self] = 1
                                            THEN /\ IF res[rn[self]][j[self]] \in {"sf", "sfu"} /\ K(rn[self], j[self]) = "sub"
                                                       THEN /\ pc' = [pc EXCEPT ![self] = "RbSub"]
                                                            /\ j' = j
                                                       ELSE /\ IF res[rn[self]][j[self]] \in {"sf", "sfu"} /\ K(rn[self], j[self]) = "deleg" /\ ~tl[self]
                                                                  THEN /\ pc' = [pc EXCEPT ![self] = "RbBind"]
                                                                       /\ j' = j
                                                                  ELSE /\ j' = [j EXCEPT ![self] = j[self] - 1]
                                                                       /\ pc' = [pc EXCEPT ![self] = "RbLoop"]
                                                 /\ UNCHANGED << rb, walk, 
                                                                 stack, rn, ra, 
                                                                 rdg, tl, lst, 
                                                                 ph, ls, ba, 
                                                                 bdg, re >>
                                            ELSE /\ IF res[rn[self]][j[self]] \in {"sf", "sfu"}
                                                       THEN /\ IF res[rn[self]][j[self]] = "sfu"
                                                                  THEN /\ walk' = (walk \cup Lst(rn[self], j[self], lst[self]))
                                                                  ELSE /\ TRUE
                                                                       /\ walk' = walk
                                                            /\ j' = [j EXCEPT ![self] = j[self] - 1]
                                                            /\ pc' = [pc EXCEPT ![self] = "RbLoop"]
                                                            /\ UNCHANGED << rb, 
                                                                            stack, 
                                                                            rn, 
                                                                            ra, 
                                                                            rdg, 
                                                                            tl, 
                                                                            lst, 
                                                                            ph, 
                                                                            ls, 
                                                                            ba, 
                                                                            bdg, 
                                                                            re >>
                                                       ELSE /\ IF res[rn[self]][j[self]] = "err" /\ ~(Fix = "RecurseFailed" /\ K(rn[self], j[self]) = "deleg" /\ ~tl[self])
                                                                  THEN /\ pc' = [pc EXCEPT ![self] = "RbSub"]
                                                                       /\ UNCHANGED << rb, 
                                                                                       walk, 
                                                                                       stack, 
                                                                                       rn, 
                                                                                       ra, 
                                                                                       rdg, 
                                                                                       tl, 
                                                                                       lst, 
                                                                                       j, 
                                                                                       ph, 
                                                                                       ls, 
                                                                                       ba, 
                                                                                       bdg, 
                                                                                       re >>
                                                                  ELSE /\ IF tl[self]
                                                                             THEN /\ IF K(rn[self], j[self]) = "sub" /\ ~C(rn[self], j[self]).w /\ res[rn[self]][j[self]] = "ok"
                                                                                        THEN /\ pc' = [pc EXCEPT ![self] = "RbSub"]
                                                                                             /\ UNCHANGED << rb, 
                                                                                                             walk, 
                                                                                                             stack, 
                                                                                                             rn, 
                                                                                                             ra, 
                                                                                                             rdg, 
                                                                                                             tl, 
                                                                                                             lst, 
                                                                                                             j, 
                                                                                                             ph, 
                                                                                                             ls, 
                                                                                                             ba, 
                                                                                                             bdg, 
                                                                                                             re >>
                                                                                        ELSE /\ IF res[rn[self]][j[self]] = "none" /\ mk[rn[self]][j[self]]
                                                                                                   THEN /\ walk' = (walk \cup Lst(rn[self], j[self], lst[self]))
                                                                                                        /\ rb' = [rb EXCEPT ![rn[self]] = "halt"]
                                                                                                        /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                                                                                        /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                                                                                                        /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                                                                                                        /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                                                                                                        /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                                                                                                        /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                                                                                                        /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                                                                                                        /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                                                                                                        /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                                                                                                        /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                                                                                                        /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                                                                                                        /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                                                                                                        /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                                                                                   ELSE /\ walk' = (walk \cup Lst(rn[self], j[self], lst[self]))
                                                                                                        /\ pc' = [pc EXCEPT ![self] = "RbSub"]
                                                                                                        /\ UNCHANGED << rb, 
                                                                                                                        stack, 
                                                                                                                        rn, 
                                                                                                                        ra, 
                                                                                                                        rdg, 
                                                                                                                        tl, 
                                                                                                                        lst, 
                                                                                                                        j, 
                                                                                                                        ph, 
                                                                                                                        ls, 
                                                                                                                        ba, 
                                                                                                                        bdg, 
                                                                                                                        re >>
                                                                             ELSE /\ IF K(rn[self], j[self]) = "deleg"
                                                                                        THEN /\ pc' = [pc EXCEPT ![self] = "RbBind"]
                                                                                             /\ UNCHANGED << rb, 
                                                                                                             walk, 
                                                                                                             stack, 
                                                                                                             rn, 
                                                                                                             ra, 
                                                                                                             rdg, 
                                                                                                             tl, 
                                                                                                             lst, 
                                                                                                             j, 
                                                                                                             ph, 
                                                                                                             ls, 
                                                                                                             ba, 
                                                                                                             bdg, 
                                                                                                             re >>
                                                                                        ELSE /\ IF K(rn[self], j[self]) = "eff" /\ res[rn[self]][j[self]] = "ok"
                                                                                                   THEN /\ pc' = [pc EXCEPT ![self] = "RbComp"]
                                                                                                        /\ UNCHANGED << rb, 
                                                                                                                        walk, 
                                                                                                                        stack, 
                                                                                                                        rn, 
                                                                                                                        ra, 
                                                                                                                        rdg, 
                                                                                                                        tl, 
                                                                                                                        lst, 
                                                                                                                        j, 
                                                                                                                        ph, 
                                                                                                                        ls, 
                                                                                                                        ba, 
                                                                                                                        bdg, 
                                                                                                                        re >>
                                                                                                   ELSE /\ IF K(rn[self], j[self]) = "eff" /\ mk[rn[self]][j[self]]
                                                                                                              THEN /\ walk' = (walk \cup Lst(rn[self], j[self], lst[self]))
                                                                                                                   /\ rb' = [rb EXCEPT ![rn[self]] = "halt"]
                                                                                                                   /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                                                                                                   /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                                                                                                                   /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                                                                                                                   /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                                                                                                                   /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                                                                                                                   /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                                                                                                                   /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                                                                                                                   /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                                                                                                                   /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                                                                                                                   /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                                                                                                                   /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                                                                                                                   /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                                                                                                                   /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                                                                                              ELSE /\ IF K(rn[self], j[self]) = "sub" /\ C(rn[self], j[self]).w /\ res[rn[self]][j[self]] = "ok"
                                                                                                                         THEN /\ pc' = [pc EXCEPT ![self] = "RbComp"]
                                                                                                                         ELSE /\ IF K(rn[self], j[self]) = "sub" /\ C(rn[self], j[self]).w
                                                                                                                                    THEN /\ pc' = [pc EXCEPT ![self] = "RbRe"]
                                                                                                                                    ELSE /\ pc' = [pc EXCEPT ![self] = "RbSub"]
                                                                                                                   /\ UNCHANGED << rb, 
                                                                                                                                   walk, 
                                                                                                                                   stack, 
                                                                                                                                   rn, 
                                                                                                                                   ra, 
                                                                                                                                   rdg, 
                                                                                                                                   tl, 
                                                                                                                                   lst, 
                                                                                                                                   j, 
                                                                                                                                   ph, 
                                                                                                                                   ls, 
                                                                                                                                   ba, 
                                                                                                                                   bdg, 
                                                                                                                                   re >>
                /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                bound, want, verdict, ambig, readErr, crashes, 
                                wrongs, lost, rv, haltIn, fx, fa, granted, 
                                listed, anyFire, badAct, admitLate, fireLate, 
                                badComp, startAfterHalt, falseFail, forged, n, 
                                a, dg, sg, st, i, held, hl, e, ca, cdg, cz >>

RbSub(self) == /\ pc[self] = "RbSub"
               /\ IF SubWalk(rn[self], j[self], ls[self])
                     THEN /\ IF ~tl[self] /\ C(rn[self], j[self]).decl = "other"
                                THEN /\ walk' = (walk \cup Lst(rn[self], j[self], lst[self]))
                                ELSE /\ TRUE
                                     /\ walk' = walk
                          /\ /\ lst' = [lst EXCEPT ![self] = lst[self]]
                             /\ ra' = [ra EXCEPT ![self] = ra[self]]
                             /\ rdg' = [rdg EXCEPT ![self] = rdg[self]]
                             /\ rn' = [rn EXCEPT ![self] = Ch(rn[self], j[self])]
                             /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Rollback",
                                                                      pc        |->  "RbSubRet",
                                                                      j         |->  j[self],
                                                                      ph        |->  ph[self],
                                                                      ls        |->  ls[self],
                                                                      ba        |->  ba[self],
                                                                      bdg       |->  bdg[self],
                                                                      re        |->  re[self],
                                                                      rn        |->  rn[self],
                                                                      ra        |->  ra[self],
                                                                      rdg       |->  rdg[self],
                                                                      tl        |->  tl[self],
                                                                      lst       |->  lst[self] ] >>
                                                                  \o stack[self]]
                             /\ tl' = [tl EXCEPT ![self] = tl[self] \/ C(rn[self], j[self]).decl # "same"]
                          /\ j' = [j EXCEPT ![self] = 0]
                          /\ ph' = [ph EXCEPT ![self] = 1]
                          /\ ls' = [ls EXCEPT ![self] = {}]
                          /\ ba' = [ba EXCEPT ![self] = NoneG]
                          /\ bdg' = [bdg EXCEPT ![self] = FALSE]
                          /\ re' = [re EXCEPT ![self] = {}]
                          /\ pc' = [pc EXCEPT ![self] = "RbOpen"]
                     ELSE /\ j' = [j EXCEPT ![self] = j[self] - 1]
                          /\ pc' = [pc EXCEPT ![self] = "RbLoop"]
                          /\ UNCHANGED << walk, stack, rn, ra, rdg, tl, lst, 
                                          ph, ls, ba, bdg, re >>
               /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, bound, 
                               want, verdict, ambig, readErr, crashes, wrongs, 
                               lost, rv, rb, haltIn, fx, fa, granted, listed, 
                               anyFire, badAct, admitLate, fireLate, badComp, 
                               startAfterHalt, falseFail, forged, n, a, dg, sg, 
                               st, i, held, hl, e, ca, cdg, cz >>

RbSubRet(self) == /\ pc[self] = "RbSubRet"
                  /\ IF rb[Ch(rn[self], j[self])] # "ok"
                        THEN /\ rb' = [rb EXCEPT ![rn[self]] = rb[Ch(rn[self], j[self])]]
                             /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                             /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                             /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                             /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                             /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                             /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                             /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                             /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                             /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                             /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                             /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                             /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                             /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                        ELSE /\ j' = [j EXCEPT ![self] = j[self] - 1]
                             /\ pc' = [pc EXCEPT ![self] = "RbLoop"]
                             /\ UNCHANGED << rb, stack, rn, ra, rdg, tl, lst, 
                                             ph, ls, ba, bdg, re >>
                  /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                  bound, want, verdict, ambig, readErr, 
                                  crashes, wrongs, lost, rv, haltIn, fx, fa, 
                                  granted, walk, listed, anyFire, badAct, 
                                  admitLate, fireLate, badComp, startAfterHalt, 
                                  falseFail, forged, n, a, dg, sg, st, i, held, 
                                  hl, e, ca, cdg, cz >>

RbBind(self) == /\ pc[self] = "RbBind"
                /\ IF Bug = "NoBind"
                      THEN /\ ba' = [ba EXCEPT ![self] = ra[self]]
                           /\ bdg' = [bdg EXCEPT ![self] = rdg[self]]
                           /\ pc' = [pc EXCEPT ![self] = "RbRec"]
                           /\ UNCHANGED << want, readErr, rb, stack, rn, ra, 
                                           rdg, tl, lst, j, ph, ls, re >>
                      ELSE /\ \/ /\ readErr < MaxReadErr
                                 /\ readErr' = readErr + 1
                                 /\ rb' = [rb EXCEPT ![rn[self]] = "stop"]
                                 /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                 /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                                 /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                                 /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                                 /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                                 /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                                 /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                                 /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                                 /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                                 /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                                 /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                                 /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                                 /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                 /\ want' = want
                              \/ /\ IF aj[Ch(rn[self], j[self])].g = NoneG /\ (aj[Ch(rn[self], j[self])].ung \/ ~Recs(Ch(rn[self], j[self])))
                                       THEN /\ ba' = [ba EXCEPT ![self] = NoneG]
                                            /\ bdg' = [bdg EXCEPT ![self] = FALSE]
                                            /\ pc' = [pc EXCEPT ![self] = "RbRec"]
                                            /\ UNCHANGED << want, rb, stack, 
                                                            rn, ra, rdg, tl, 
                                                            lst, j, ph, ls, re >>
                                       ELSE /\ IF aj[Ch(rn[self], j[self])].g = NoneG
                                                  THEN /\ rb' = [rb EXCEPT ![rn[self]] = "stop"]
                                                       /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                                       /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                                                       /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                                                       /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                                                       /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                                                       /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                                                       /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                                                       /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                                                       /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                                                       /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                                                       /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                                                       /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                                                       /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                                       /\ want' = want
                                                  ELSE /\ IF aj[Ch(rn[self], j[self])].g.sub # Ch(rn[self], j[self]) /\ Bug # "ForeignBind"
                                                             THEN /\ rb' = [rb EXCEPT ![rn[self]] = "stop"]
                                                                  /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                                                  /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                                                                  /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                                                                  /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                                                                  /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                                                                  /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                                                                  /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                                                                  /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                                                                  /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                                                                  /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                                                                  /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                                                                  /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                                                                  /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                                                  /\ want' = want
                                                             ELSE /\ IF ra[self] = NoneG \/ ~(aj[Ch(rn[self], j[self])].g.par = ra[self].id \/ (Fix = "ChainBind" /\ ChainOK(aj[Ch(rn[self], j[self])].g)))
                                                                        THEN /\ want' = RootFor(aj[Ch(rn[self], j[self])].g.par, want)
                                                                             /\ rb' = [rb EXCEPT ![rn[self]] = "stop"]
                                                                             /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                                                             /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                                                                             /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                                                                             /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                                                                             /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                                                                             /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                                                                             /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                                                                             /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                                                                             /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                                                                             /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                                                                             /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                                                                             /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                                                                             /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                                                        ELSE /\ ba' = [ba EXCEPT ![self] = aj[Ch(rn[self], j[self])].g]
                                                                             /\ bdg' = [bdg EXCEPT ![self] = Fix = "GuardRerun"]
                                                                             /\ pc' = [pc EXCEPT ![self] = "RbRec"]
                                                                             /\ UNCHANGED << want, 
                                                                                             rb, 
                                                                                             stack, 
                                                                                             rn, 
                                                                                             ra, 
                                                                                             rdg, 
                                                                                             tl, 
                                                                                             lst, 
                                                                                             j, 
                                                                                             ph, 
                                                                                             ls, 
                                                                                             re >>
                                 /\ UNCHANGED readErr
                /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                bound, verdict, ambig, crashes, wrongs, lost, 
                                rv, haltIn, fx, fa, granted, walk, listed, 
                                anyFire, badAct, admitLate, fireLate, badComp, 
                                startAfterHalt, falseFail, forged, n, a, dg, 
                                sg, st, i, held, hl, e, ca, cdg, cz >>

RbRec(self) == /\ pc[self] = "RbRec"
               /\ /\ lst' = [lst EXCEPT ![self] = lst[self]]
                  /\ ra' = [ra EXCEPT ![self] = ba[self]]
                  /\ rdg' = [rdg EXCEPT ![self] = bdg[self]]
                  /\ rn' = [rn EXCEPT ![self] = Ch(rn[self], j[self])]
                  /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Rollback",
                                                           pc        |->  "RbBindRet",
                                                           j         |->  j[self],
                                                           ph        |->  ph[self],
                                                           ls        |->  ls[self],
                                                           ba        |->  ba[self],
                                                           bdg       |->  bdg[self],
                                                           re        |->  re[self],
                                                           rn        |->  rn[self],
                                                           ra        |->  ra[self],
                                                           rdg       |->  rdg[self],
                                                           tl        |->  tl[self],
                                                           lst       |->  lst[self] ] >>
                                                       \o stack[self]]
                  /\ tl' = [tl EXCEPT ![self] = FALSE]
               /\ j' = [j EXCEPT ![self] = 0]
               /\ ph' = [ph EXCEPT ![self] = 1]
               /\ ls' = [ls EXCEPT ![self] = {}]
               /\ ba' = [ba EXCEPT ![self] = NoneG]
               /\ bdg' = [bdg EXCEPT ![self] = FALSE]
               /\ re' = [re EXCEPT ![self] = {}]
               /\ pc' = [pc EXCEPT ![self] = "RbOpen"]
               /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, bound, 
                               want, verdict, ambig, readErr, crashes, wrongs, 
                               lost, rv, rb, haltIn, fx, fa, granted, walk, 
                               listed, anyFire, badAct, admitLate, fireLate, 
                               badComp, startAfterHalt, falseFail, forged, n, 
                               a, dg, sg, st, i, held, hl, e, ca, cdg, cz >>

RbBindRet(self) == /\ pc[self] = "RbBindRet"
                   /\ IF rb[Ch(rn[self], j[self])] # "ok"
                         THEN /\ rb' = [rb EXCEPT ![rn[self]] = rb[Ch(rn[self], j[self])]]
                              /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                              /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                              /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                              /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                              /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                              /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                              /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                              /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                              /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                              /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                              /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                              /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                              /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                         ELSE /\ j' = [j EXCEPT ![self] = j[self] - 1]
                              /\ pc' = [pc EXCEPT ![self] = "RbLoop"]
                              /\ UNCHANGED << rb, stack, rn, ra, rdg, tl, lst, 
                                              ph, ls, ba, bdg, re >>
                   /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                   bound, want, verdict, ambig, readErr, 
                                   crashes, wrongs, lost, rv, haltIn, fx, fa, 
                                   granted, walk, listed, anyFire, badAct, 
                                   admitLate, fireLate, badComp, 
                                   startAfterHalt, falseFail, forged, n, a, dg, 
                                   sg, st, i, held, hl, e, ca, cdg, cz >>

RbComp(self) == /\ pc[self] = "RbComp"
                /\ IF cp[rn[self]][j[self]]
                      THEN /\ pc' = [pc EXCEPT ![self] = "RbSub"]
                           /\ UNCHANGED << cp, ambig, rb, fx, badAct, badComp, 
                                           stack, rn, ra, rdg, tl, lst, j, ph, 
                                           ls, ba, bdg, re >>
                      ELSE /\ fx' = [fx EXCEPT ![rn[self]][j[self]] = "undone"]
                           /\ badAct' = (badAct \/ BadAuth(rn[self], ra[self].id))
                           /\ badComp' = (badComp \/ (DelF[rn[self]] # 0 /\ (ra[self].id # fa[rn[self]][j[self]] \/ ra[self].id # JAuth(DelF[rn[self]]))))
                           /\ \/ /\ cp' = [cp EXCEPT ![rn[self]][j[self]] = TRUE]
                                 /\ pc' = [pc EXCEPT ![self] = "RbSub"]
                                 /\ UNCHANGED <<ambig, rb, stack, rn, ra, rdg, tl, lst, j, ph, ls, ba, bdg, re>>
                              \/ /\ ambig < MaxAmbig
                                 /\ ambig' = ambig + 1
                                 /\ cp' = [cp EXCEPT ![rn[self]][j[self]] = TRUE]
                                 /\ rb' = [rb EXCEPT ![rn[self]] = "stop"]
                                 /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                 /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                                 /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                                 /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                                 /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                                 /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                                 /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                                 /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                                 /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                                 /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                                 /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                                 /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                                 /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                              \/ /\ ambig < MaxAmbig
                                 /\ ambig' = ambig + 1
                                 /\ rb' = [rb EXCEPT ![rn[self]] = "stop"]
                                 /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                 /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                                 /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                                 /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                                 /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                                 /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                                 /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                                 /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                                 /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                                 /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                                 /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                                 /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                                 /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                 /\ cp' = cp
                /\ UNCHANGED << opened, mk, res, lk, aj, cm, ab, now, bound, 
                                want, verdict, readErr, crashes, wrongs, lost, 
                                rv, haltIn, fa, granted, walk, listed, anyFire, 
                                admitLate, fireLate, startAfterHalt, falseFail, 
                                forged, n, a, dg, sg, st, i, held, hl, e, ca, 
                                cdg, cz >>

RbRe(self) == /\ pc[self] = "RbRe"
              /\ IF rdg[self] /\ Expired(ra[self], now) /\ Bug # "NoCallGuard"
                    THEN /\ IF Fix = "GuardRerun"
                               THEN /\ walk' = (walk \cup Lst(rn[self], j[self], lst[self]))
                                    /\ ls' = [ls EXCEPT ![self] = Links(rn[self])]
                                    /\ pc' = [pc EXCEPT ![self] = "RbSub"]
                                    /\ UNCHANGED << rb, stack, rn, ra, rdg, tl, 
                                                    lst, j, ph, ba, bdg, re >>
                               ELSE /\ rb' = [rb EXCEPT ![rn[self]] = "stop"]
                                    /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                    /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                                    /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                                    /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                                    /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                                    /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                                    /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                                    /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                                    /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                                    /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                                    /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                                    /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                                    /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                    /\ walk' = walk
                         /\ UNCHANGED << lk, admitLate >>
                    ELSE /\ admitLate' = (admitLate \/ (IsChild(ra[self]) /\ Expired(ra[self], now)))
                         /\ re' = [re EXCEPT ![self] = {}]
                         /\ IF Ch(rn[self], j[self]) = 0
                               THEN /\ pc' = [pc EXCEPT ![self] = "RbReW"]
                                    /\ lk' = lk
                               ELSE /\ lk' = [lk EXCEPT ![rn[self]][j[self]] = TRUE]
                                    /\ pc' = [pc EXCEPT ![self] = "RbReRun"]
                         /\ UNCHANGED << rb, walk, stack, rn, ra, rdg, tl, lst, 
                                         j, ph, ls, ba, bdg >>
              /\ UNCHANGED << opened, mk, res, cp, aj, cm, ab, now, bound, 
                              want, verdict, ambig, readErr, crashes, wrongs, 
                              lost, rv, haltIn, fx, fa, granted, listed, 
                              anyFire, badAct, fireLate, badComp, 
                              startAfterHalt, falseFail, forged, n, a, dg, sg, 
                              st, i, held, hl, e, ca, cdg, cz >>

RbReRun(self) == /\ pc[self] = "RbReRun"
                 /\ /\ a' = [a EXCEPT ![self] = ra[self]]
                    /\ dg' = [dg EXCEPT ![self] = rdg[self]]
                    /\ n' = [n EXCEPT ![self] = Ch(rn[self], j[self])]
                    /\ sg' = [sg EXCEPT ![self] = C(rn[self], j[self]).saga]
                    /\ st' = [st EXCEPT ![self] = TRUE]
                    /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Drive",
                                                             pc        |->  "RbReRet",
                                                             i         |->  i[self],
                                                             held      |->  held[self],
                                                             hl        |->  hl[self],
                                                             e         |->  e[self],
                                                             ca        |->  ca[self],
                                                             cdg       |->  cdg[self],
                                                             cz        |->  cz[self],
                                                             n         |->  n[self],
                                                             a         |->  a[self],
                                                             dg        |->  dg[self],
                                                             sg        |->  sg[self],
                                                             st        |->  st[self] ] >>
                                                         \o stack[self]]
                 /\ i' = [i EXCEPT ![self] = 1]
                 /\ held' = [held EXCEPT ![self] = {}]
                 /\ hl' = [hl EXCEPT ![self] = FALSE]
                 /\ e' = [e EXCEPT ![self] = {}]
                 /\ ca' = [ca EXCEPT ![self] = NoneG]
                 /\ cdg' = [cdg EXCEPT ![self] = FALSE]
                 /\ cz' = [cz EXCEPT ![self] = "perm"]
                 /\ pc' = [pc EXCEPT ![self] = "DOpen"]
                 /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                 bound, want, verdict, ambig, readErr, crashes, 
                                 wrongs, lost, rv, rb, haltIn, fx, fa, granted, 
                                 walk, listed, anyFire, badAct, admitLate, 
                                 fireLate, badComp, startAfterHalt, falseFail, 
                                 forged, rn, ra, rdg, tl, lst, j, ph, ls, ba, 
                                 bdg, re >>

RbReRet(self) == /\ pc[self] = "RbReRet"
                 /\ re' = [re EXCEPT ![self] = rv[Ch(rn[self], j[self])]]
                 /\ pc' = [pc EXCEPT ![self] = "RbReW"]
                 /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, 
                                 bound, want, verdict, ambig, readErr, crashes, 
                                 wrongs, lost, rv, rb, haltIn, fx, fa, granted, 
                                 walk, listed, anyFire, badAct, admitLate, 
                                 fireLate, badComp, startAfterHalt, falseFail, 
                                 forged, stack, n, a, dg, sg, st, i, held, hl, 
                                 e, ca, cdg, cz, rn, ra, rdg, tl, lst, j, ph, 
                                 ls, ba, bdg >>

RbReW(self) == /\ pc[self] = "RbReW"
               /\ IF re[self] # {} /\ "L" \notin re[self]
                     THEN /\ rb' = [rb EXCEPT ![rn[self]] = "stop"]
                          /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                          /\ j' = [j EXCEPT ![self] = Head(stack[self]).j]
                          /\ ph' = [ph EXCEPT ![self] = Head(stack[self]).ph]
                          /\ ls' = [ls EXCEPT ![self] = Head(stack[self]).ls]
                          /\ ba' = [ba EXCEPT ![self] = Head(stack[self]).ba]
                          /\ bdg' = [bdg EXCEPT ![self] = Head(stack[self]).bdg]
                          /\ re' = [re EXCEPT ![self] = Head(stack[self]).re]
                          /\ rn' = [rn EXCEPT ![self] = Head(stack[self]).rn]
                          /\ ra' = [ra EXCEPT ![self] = Head(stack[self]).ra]
                          /\ rdg' = [rdg EXCEPT ![self] = Head(stack[self]).rdg]
                          /\ tl' = [tl EXCEPT ![self] = Head(stack[self]).tl]
                          /\ lst' = [lst EXCEPT ![self] = Head(stack[self]).lst]
                          /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                          /\ UNCHANGED << res, lost, fx, fa, walk, anyFire, 
                                          badAct, fireLate >>
                     ELSE /\ fx' = [fx EXCEPT ![rn[self]][j[self]] = "in"]
                          /\ fa' = [fa EXCEPT ![rn[self]][j[self]] = ra[self].id]
                          /\ anyFire' = TRUE
                          /\ badAct' = (badAct \/ BadAuth(rn[self], ra[self].id))
                          /\ fireLate' = (fireLate \/ (IsChild(ra[self]) /\ Expired(ra[self], now)))
                          /\ \/ /\ re[self] = {}
                                /\ res' = [res EXCEPT ![rn[self]][j[self]] = "ok"]
                                /\ ls' = [ls EXCEPT ![self] = Links(rn[self])]
                                /\ pc' = [pc EXCEPT ![self] = "RbComp"]
                                /\ UNCHANGED <<lost, walk>>
                             \/ /\ re[self] # {} \/ lost < MaxLost
                                /\ lost' = (IF re[self] = {} THEN lost + 1 ELSE lost)
                                /\ walk' = (walk \cup Lst(rn[self], j[self], lst[self]))
                                /\ IF Bug # "B4"
                                      THEN /\ ls' = [ls EXCEPT ![self] = Links(rn[self])]
                                      ELSE /\ TRUE
                                           /\ ls' = ls
                                /\ pc' = [pc EXCEPT ![self] = "RbSub"]
                                /\ res' = res
                          /\ UNCHANGED << rb, stack, rn, ra, rdg, tl, lst, j, 
                                          ph, ba, bdg, re >>
               /\ UNCHANGED << opened, mk, lk, cp, aj, cm, ab, now, bound, 
                               want, verdict, ambig, readErr, crashes, wrongs, 
                               rv, haltIn, granted, listed, admitLate, badComp, 
                               startAfterHalt, falseFail, forged, n, a, dg, sg, 
                               st, i, held, hl, e, ca, cdg, cz >>

Rollback(self) == RbOpen(self) \/ RbLoop(self) \/ RbSub(self)
                     \/ RbSubRet(self) \/ RbBind(self) \/ RbRec(self)
                     \/ RbBindRet(self) \/ RbComp(self) \/ RbRe(self)
                     \/ RbReRun(self) \/ RbReRet(self) \/ RbReW(self)

Idle == /\ pc["d"] = "Idle"
        /\ verdict \in Retry
        /\ /\ a' = [a EXCEPT !["d"] = bound]
           /\ dg' = [dg EXCEPT !["d"] = FALSE]
           /\ n' = [n EXCEPT !["d"] = Root]
           /\ sg' = [sg EXCEPT !["d"] = RootSaga]
           /\ st' = [st EXCEPT !["d"] = RootSaga]
           /\ stack' = [stack EXCEPT !["d"] = << [ procedure |->  "Drive",
                                                   pc        |->  "Back",
                                                   i         |->  i["d"],
                                                   held      |->  held["d"],
                                                   hl        |->  hl["d"],
                                                   e         |->  e["d"],
                                                   ca        |->  ca["d"],
                                                   cdg       |->  cdg["d"],
                                                   cz        |->  cz["d"],
                                                   n         |->  n["d"],
                                                   a         |->  a["d"],
                                                   dg        |->  dg["d"],
                                                   sg        |->  sg["d"],
                                                   st        |->  st["d"] ] >>
                                               \o stack["d"]]
        /\ i' = [i EXCEPT !["d"] = 1]
        /\ held' = [held EXCEPT !["d"] = {}]
        /\ hl' = [hl EXCEPT !["d"] = FALSE]
        /\ e' = [e EXCEPT !["d"] = {}]
        /\ ca' = [ca EXCEPT !["d"] = NoneG]
        /\ cdg' = [cdg EXCEPT !["d"] = FALSE]
        /\ cz' = [cz EXCEPT !["d"] = "perm"]
        /\ pc' = [pc EXCEPT !["d"] = "DOpen"]
        /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, bound, want, 
                        verdict, ambig, readErr, crashes, wrongs, lost, rv, rb, 
                        haltIn, fx, fa, granted, walk, listed, anyFire, badAct, 
                        admitLate, fireLate, badComp, startAfterHalt, 
                        falseFail, forged, rn, ra, rdg, tl, lst, j, ph, ls, ba, 
                        bdg, re >>

Back == /\ pc["d"] = "Back"
        /\ verdict' = VerdictOf(rv[Root])
        /\ pc' = [pc EXCEPT !["d"] = "Idle"]
        /\ UNCHANGED << opened, mk, res, lk, cp, aj, cm, ab, now, bound, want, 
                        ambig, readErr, crashes, wrongs, lost, rv, rb, haltIn, 
                        fx, fa, granted, walk, listed, anyFire, badAct, 
                        admitLate, fireLate, badComp, startAfterHalt, 
                        falseFail, forged, stack, n, a, dg, sg, st, i, held, 
                        hl, e, ca, cdg, cz, rn, ra, rdg, tl, lst, j, ph, ls, 
                        ba, bdg, re >>

drv == Idle \/ Back

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == drv
           \/ (\E self \in ProcSet: Drive(self) \/ Rollback(self))
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ WF_vars(drv) /\ WF_vars(Drive("d")) /\ WF_vars(Rollback("d"))

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION
-----------------------------------------------------------------------------

\* The clock: grants expire as it runs.
Tick ==
  /\ now < MaxTime
  /\ now' = now + 1
  /\ UNCHANGED <<opened, mk, res, lk, cp, aj, cm, ab, bound, want, verdict, ambig, readErr,
                 crashes, wrongs, lost, rv, rb, haltIn, fx, fa, granted, walk, listed, anyFire, badAct,
                 admitLate, fireLate, badComp, startAfterHalt, falseFail, forged, pc, stack, n, a,
                 dg, sg, st, i, held, hl, e, ca, cdg, cz, rn, ra, rdg, tl, lst, j, ph, ls, ba, bdg,
                 re>>

\* Between drives, the next is bound to the wrong authority (another grant, or none).
WrongAuth ==
  /\ wrongs < MaxWrong /\ pc["d"] = "Idle" /\ verdict \in Retry
  /\ \E b \in {WG, NoneG} \ {bound} : bound' = b
  /\ wrongs' = wrongs + 1
  /\ UNCHANGED <<opened, mk, res, lk, cp, aj, cm, ab, now, want, verdict, ambig, readErr,
                 crashes, lost, rv, rb, haltIn, fx, fa, granted, walk, listed, anyFire, badAct,
                 admitLate, fireLate, badComp, startAfterHalt, falseFail, forged, pc, stack, n, a,
                 dg, sg, st, i, held, hl, e, ca, cdg, cz, rn, ra, rdg, tl, lst, j, ph, ls, ba, bdg,
                 re>>

\* The operator binds the authority the last refusal asked for.
FixAuth ==
  /\ pc["d"] = "Idle" /\ verdict \in Retry /\ bound # want
  /\ bound' = want
  /\ UNCHANGED <<opened, mk, res, lk, cp, aj, cm, ab, now, want, verdict, ambig, readErr,
                 crashes, wrongs, lost, rv, rb, haltIn, fx, fa, granted, walk, listed, anyFire, badAct,
                 admitLate, fireLate, badComp, startAfterHalt, falseFail, forged, pc, stack, n, a,
                 dg, sg, st, i, held, hl, e, ca, cdg, cz, rn, ra, rdg, tl, lst, j, ph, ls, ba, bdg,
                 re>>

\* The process dies at any step and a new one drives the root again: the journal stays, every
\* in-memory value (the call stack, the held errors, the halted flag) is gone.
Crash ==
  /\ crashes < MaxCrash /\ pc["d"] # "Idle"
  /\ crashes' = crashes + 1
  /\ verdict' = "crash"
  /\ pc' = [pc EXCEPT !["d"] = "Idle"]
  /\ stack' = [stack EXCEPT !["d"] = <<>>]
  /\ n' = [n EXCEPT !["d"] = defaultInitValue]
  /\ a' = [a EXCEPT !["d"] = defaultInitValue]
  /\ dg' = [dg EXCEPT !["d"] = defaultInitValue]
  /\ sg' = [sg EXCEPT !["d"] = defaultInitValue]
  /\ st' = [st EXCEPT !["d"] = defaultInitValue]
  /\ i' = [i EXCEPT !["d"] = 1]
  /\ held' = [held EXCEPT !["d"] = {}]
  /\ hl' = [hl EXCEPT !["d"] = FALSE]
  /\ e' = [e EXCEPT !["d"] = {}]
  /\ ca' = [ca EXCEPT !["d"] = NoneG]
  /\ cdg' = [cdg EXCEPT !["d"] = FALSE]
  /\ cz' = [cz EXCEPT !["d"] = "perm"]
  /\ rn' = [rn EXCEPT !["d"] = defaultInitValue]
  /\ ra' = [ra EXCEPT !["d"] = defaultInitValue]
  /\ rdg' = [rdg EXCEPT !["d"] = defaultInitValue]
  /\ tl' = [tl EXCEPT !["d"] = defaultInitValue]
  /\ lst' = [lst EXCEPT !["d"] = defaultInitValue]
  /\ j' = [j EXCEPT !["d"] = 0]
  /\ ph' = [ph EXCEPT !["d"] = 1]
  /\ ls' = [ls EXCEPT !["d"] = {}]
  /\ ba' = [ba EXCEPT !["d"] = NoneG]
  /\ bdg' = [bdg EXCEPT !["d"] = FALSE]
  /\ re' = [re EXCEPT !["d"] = {}]
  /\ UNCHANGED <<opened, mk, res, lk, cp, aj, cm, ab, now, bound, want, ambig, readErr, wrongs,
                 lost, rv, rb, haltIn, fx, fa, granted, walk, listed, anyFire, badAct, admitLate,
                 fireLate, badComp, startAfterHalt, falseFail, forged>>

FullNext == Next \/ Tick \/ WrongAuth \/ FixAuth \/ Crash
FullSpec == Init /\ [][FullNext]_vars
\* Liveness: the driver's steps are weakly fair, and the operator strongly (it can act only
\* between drives, which the driver leaves at once); the clock, crashes, faults and a wrong
\* binding are not fair.
DriverStep == drv \/ Drive("d") \/ Rollback("d")
LiveSpec == FullSpec /\ WF_vars(DriverStep) /\ SF_vars(FixAuth)

(***************************************************************************)
(* Properties                                                               *)
(***************************************************************************)

\* A child never acts (fires a write, or compensates one) with authority its delegation did not
\* grant, and no call of a delegation's sub-run reaches its tool once the delegation's grant has
\* expired.
AuthorityNarrows == ~badAct /\ ~admitLate

\* The literal form of expiry: no write fires once the grant expired, including one whose tool
\* was entered before (limits/fire-in-flight).
NoFireAfterExpiry == ~fireLate

\* Every write in place anywhere in the tree, once the root's rollback finished (run:aborted), is
\* reported: itself, or a call above it, is in Uncompensated or UnknownOutcome. None is
\* silently skipped.
RollbackSound ==
  ab[Root] => \A x \in Nodes : \A y \in Idx(x) :
                fx[x][y] = "in" => (<<x, y>> \in listed \/ AboveF[x] \cap listed # {})

\* A compensation in a delegation's sub-run runs under the authority the sub-run journaled,
\* which is the one the write fired under.
RollbackUnderGrant == ~badComp

\* In a saga, no call starts after a call of the same drive returned a halt or a lost outcome
\* (held, or joined with an Unrecorded refusal), from anywhere below it.
HaltPropagates == ~startAfterHalt

\* A sub-run runs only under an ID derived from its own call, while the call is open.
NoForgedSubRun == ~forged

\* A delegation is recorded as failed only for a permanent cause: never for a storage error or
\* a resume under the wrong authority, which are Unrecorded.
NoFalseFailure == ~falseFail

\* A run whose drives stop on Unrecorded refusals reaches an end once the operator binds the
\* authority they ask for: it completes, aborts, or halts for a human.
UnrecordedContinues == <>(verdict \in Final)

\* A failed saga's rollback reaches its end.
SagaFailed == \E y \in Idx(Root) : res[Root][y] \in {"sf", "sfu"}
RollbackEnds == [](SagaFailed => <>(verdict \in {"aborted", "halt"}))

\* Vacuity: some write fires.
EffectNotReachable == ~anyFire

TypeOK ==
  /\ verdict \in Retry \cup Final
  /\ now \in 0..MaxTime
=============================================================================
