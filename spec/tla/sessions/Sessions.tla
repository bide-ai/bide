------------------------------- MODULE Sessions -------------------------------
(***************************************************************************)
(* Model 12: agent sessions (agent/session.go). A Session is a durable     *)
(* multi-turn conversation. Each Send or SendOnce is one turn, and each     *)
(* turn is its own run ("<id>>@turn/<n>" for Send, "<id>>@event/<key>" for *)
(* SendOnce) seeded with the transcript so far; the session's own journal,  *)
(* "<id>>@session", holds which message started each Send turn (start/N),   *)
(* each turn's starting point (from/<turn run>) and each completed turn     *)
(* (turn/N), so the shared history is the turn/N records in index order.    *)
(*                                                                          *)
(* Callers send messages through handles; handles live in processes, and    *)
(* several callers may share one handle (it is safe for concurrent use: its *)
(* mutex is held while the session journal is read or written, never while *)
(* a turn's run is in progress). A turn's run is abstracted to what the     *)
(* session depends on: its run:start input (first writer), its model calls *)
(* (each a first-writer record of the transcript it was seeded with, under  *)
(* the per-run token budget), and its end (run:complete, or P14's           *)
(* run:cancelled). The claim protocol inside a turn is model 1's, the spend *)
(* records model 8's, the end markers model 10's.                           *)
(***************************************************************************)
EXTENDS Integers, Sequences, FiniteSets, TLC

CONSTANTS
  Sess,        \* session ids (strings)
  Handles,     \* session handles (model values)
  ProcOf,      \* [Handles -> process]: a crash restarts every handle of its process
  HSess,       \* [Handles -> Sess]: the session each handle has open
  Callers,     \* callers (model values): each sends one message, and again until it is answered
  CH,          \* [Callers -> Handles \cup {"none"}]: the handle a caller sends on ("none" for a root run)
  COp,         \* [Callers -> {"send", "once", "root"}]: Send, SendOnce, or Agent.Run of a root run
  CMsg,        \* [Callers -> message]: the message (callers with one message and op are one delivery)
  CKey,        \* [Callers -> key]: SendOnce's key ("none" otherwise)
  CRoot,       \* [Callers -> run id]: a root run's id ("none" otherwise)
  Keys,        \* the SendOnce keys
  NCalls,      \* model calls a turn's run makes before it completes
  Budget,      \* WithTokenBudget per run, in model calls (0: none)
  MaxErr,      \* budget of error replies on the session journal's writes (A3: committed or not)
  MaxCrash,    \* budget of process crashes
  MaxPause,    \* budget of drives that pause (an approval, an interrupt) before a model call
  MaxCancel,   \* budget of P14 Cancel calls on a turn's run
  MaxTry,      \* sends per caller (0: a caller sends again until it is answered, for liveness)
  MaxStarts,   \* bound on Send turns per session
  MaxClaims,   \* bound on the claims one caller draws
  CancelRule,  \* "none": P14 as designed; "close": the proposed rule (finding S3); "rollback": it,
               \* and a saga turn's rollback request driven by the session (#138 review, finding 6)
  SagaTurns,   \* TRUE: the turns' runs are sagas, so Cancel writes a rollback request, not run:cancelled
  TurnLease,   \* FALSE: a turn's run is driven with no lease; TRUE: the proposed rule (finding S4)
  Fix,         \* the proposed fixes in force: a subset of {"S1", "S2"} (findings S1, S2)
  Bug          \* "none" or a historical rule, see regress/

None == "none"
Ops == {"send", "once", "root"}
ASSUME Bug \in {"none", "SlashIds", "IndexTurns", "NoSkip", "StepWrapped", "NoFrom", "DoneAnyInput"}
ASSUME CancelRule \in {"none", "close", "rollback"} /\ Fix \subseteq {"S1", "S2"} /\ TurnLease \in BOOLEAN
ASSUME SagaTurns \in BOOLEAN
ASSUME \A c \in Callers : COp[c] \in Ops /\ (COp[c] = "root") = (CH[c] = None)
ASSUME NCalls >= 1

SessCallers == {c \in Callers : COp[c] # "root"}
SOf(c) == IF COp[c] = "root" THEN None ELSE HSess[CH[c]]

\* Run ids. Since #86 a session's runs are "<id>>@turn/<n>" and "<id>>@event/<key>", and a root
\* run id may not contain '>' (checkRunID), so none of them meet. Before #86 (Bug = "SlashIds")
\* they were "<id>/t<n>" and "<id>/e/<key>", which a root run id or another session's id could
\* spell. (The key is encoded since #86: encodeID is one-to-one, so the model uses keys as they
\* are, with no reserved characters.)
TurnId(s, n)  == IF Bug = "SlashIds" THEN s \o "/t" \o ToString(n) ELSE s \o ">@turn/" \o ToString(n)
EventId(s, k) == IF Bug = "SlashIds" THEN s \o "/e/" \o k ELSE s \o ">@event/" \o k
RootIds == {CRoot[c] : c \in {d \in Callers : COp[d] = "root"}}
RunIds == {TurnId(s, n) : s \in Sess, n \in 0..MaxStarts} \cup {EventId(s, k) : s \in Sess, k \in Keys}
          \cup RootIds

\* A run's journal, abstracted: run:start's input (and a ghost tag: the SendOnce key whose turn
\* started it), the model records (each the number of transcript turns its call was seeded
\* with), run:complete's answer, P14's run:cancelled (cx) and a saga's rollback request (rq), and
\* the spend: billed model calls and the journaled spend (records and late spend).
NoAns == [msg |-> None, seen |-> -2]
NoTag == <<None, None>>
NoTurn == [in |-> None, ans |-> NoAns, key |-> None, run |-> None, claim |-> <<None, 0>>]
NoRun == [inp |-> None, tag |-> NoTag, llm |-> <<>>, done |-> NoAns, cx |-> FALSE, rq |-> FALSE,
          billed |-> 0, jsp |-> 0]
Tag(c) == IF COp[c] = "once" THEN <<SOf(c), CKey[c]>> ELSE NoTag

Min(S) == CHOOSE x \in S : \A y \in S : x <= y
Max(S) == CHOOSE x \in S : \A y \in S : x >= y

(* --algorithm sessions
variables
  runs   = [r \in RunIds |-> NoRun],
  \* The session journals: start/N, turn/N, from/<run> (-1: none), and, for Bug = "StepWrapped",
  \* the caller's Step record of a key's reply.
  starts = [s \in Sess |-> <<>>],
  turns  = [s \in Sess |-> <<>>],
  from   = [s \in Sess |-> [r \in RunIds |-> -1]],
  stepv  = [s \in Sess |-> [k \in Keys |-> NoAns]],
  \* Handle state: turns and starts loaded, the open Send turn (an index into starts, 0: none),
  \* whether the handle is loaded (a crash drops it), and its mutex.
  ht  = [h \in Handles |-> 0],
  hs  = [h \in Handles |-> 0],
  ho  = [h \in Handles |-> 0],
  hl  = [h \in Handles |-> TRUE],
  hmu = [h \in Handles |-> None],
  \* S4's rule (TurnLease): the caller whose drive holds a turn run's lease.
  lease = [r \in RunIds |-> None],
  \* Fault budgets.
  errs = 0, crashes = 0, pauses = 0, cancels = 0,
  \* Claims drawn per caller (newClaim), and ghosts: each caller's outcome, reply and the run
  \* that answered it, and whether a refusal was ever not justified by the journal.
  nonce   = [c \in Callers |-> 0],
  outcome = [c \in Callers |-> None],
  ret     = [c \in Callers |-> NoAns],
  okRun   = [c \in Callers |-> None],
  badRef  = FALSE;

define
  \* The first Send turn started and not recorded (reload's s.open), 0 if none. A closing record
  \* (CancelRule = "close") records the turn too.
  Finished(s) == {turns[s][j].run : j \in DOMAIN turns[s]}
  FirstOpen(s) ==
    LET op == {i \in DOMAIN starts[s] : starts[s][i].run \notin Finished(s)}
    IN IF op = {} THEN 0 ELSE Min(op)
  \* The runs recorded among the first k turns, and the last record of a key among them (reload
  \* fills s.keyed in index order, so the last one wins).
  Seen(s, k) == {turns[s][j].run : j \in 1..k}
  KeyedIn(s, k, key) ==
    LET js == {j \in 1..k : turns[s][j].key = key}
    IN IF js = {} THEN NoTurn ELSE turns[s][Max(js)]
  \* A refusal ("a turn for m' is still open") is justified when the journal holds an open Send
  \* turn of another message that can still be finished.
  Justified(s, m) ==
    LET o == FirstOpen(s)
    IN o # 0 /\ starts[s][o].in # m /\ ~runs[starts[s][o].run].cx
       /\ ~(runs[starts[s][o].run].rq /\ runs[starts[s][o].run].done = NoAns)
  \* The open turn's run can be closed: cancelled, or (CancelRule = "rollback") a saga with a
  \* rollback request whose answer is not recorded, which the session rolls back itself.
  Closable(r) == runs[r].cx \/ (CancelRule = "rollback" /\ runs[r].rq /\ runs[r].done = NoAns)
  \* A delivery is acknowledged once one caller of its message and op was answered; the others
  \* stop sending it.
  Acked(c) == \E d \in Callers : CMsg[d] = CMsg[c] /\ COp[d] = COp[c] /\ outcome[d] = "ok"
  TurnRec(c, cl, closing, cin, a, r) ==
    IF closing THEN [in |-> cin, ans |-> [msg |-> cin, seen |-> -1], key |-> None, run |-> r, claim |-> cl]
    ELSE [in |-> CMsg[c], ans |-> a, key |-> IF COp[c] = "once" THEN CKey[c] ELSE None,
          run |-> r, claim |-> cl]
end define;

\* reload: rebuild a handle's state from its session's journal.
macro Reload(h) begin
  ht[h] := Len(turns[HSess[h]]);
  hs[h] := Len(starts[HSess[h]]);
  ho[h] := FirstOpen(HSess[h]);
end macro;

fair process caller \in Callers
variables rid = None, seed = 0, cnt = 0, i = 0, n = 0, ans = NoAns, att = 0, fresh = FALSE,
          closing = FALSE, cin = None, why = None, tries = 0;
begin
Idle:
  if COp[self] = "root" then
    rid := CRoot[self];
    goto DLoad;
  elsif ~hl[CH[self]] then
    goto Open;
  elsif COp[self] = "send" /\ Bug = "IndexTurns" then
    \* #22's rule: a Send turn ran under the handle's next turn index, with no start record.
    rid := TurnId(SOf(self), ht[CH[self]]);
    goto Seed;
  elsif COp[self] = "send" then
    att := 0;
    goto SCheck;
  elsif Bug = "StepWrapped" then
    goto SWGet;
  else
    goto KLook;
  end if;

\* Agent.Session after a restart: a fresh handle, loaded from the journal.
Open:
  await hmu[CH[self]] = None;
  Reload(CH[self]);
  hl[CH[self]] := TRUE;
  goto Idle;

\* Send: startTurn, under the handle's mutex (taken here, or still held after a reload).
SCheck:
  await hmu[CH[self]] \in {None, self};
  \* rd: the journal is read in this step: after a lost start (reload, then the check), or, under
  \* S1's fix, when the handle's open turn is another message's (read again before refusing).
  with h = CH[self], s = SOf(self),
       rd = fresh \/ ("S1" \in Fix /\ ho[CH[self]] # 0 /\ starts[SOf(self)][ho[CH[self]]].in # CMsg[self]),
       o = IF fresh \/ ("S1" \in Fix /\ ho[CH[self]] # 0 /\ starts[SOf(self)][ho[CH[self]]].in # CMsg[self])
           THEN FirstOpen(SOf(self)) ELSE ho[CH[self]],
       t = IF fresh \/ ("S1" \in Fix /\ ho[CH[self]] # 0 /\ starts[SOf(self)][ho[CH[self]]].in # CMsg[self])
           THEN Len(turns[SOf(self)]) ELSE ht[CH[self]] do
    if rd then Reload(h) end if;
    fresh := FALSE;
    if o # 0 /\ starts[s][o].in = CMsg[self] then
      \* The open turn is this message's: resume it.
      rid := starts[s][o].run;
      hmu[h] := None;
      goto Seed;
    elsif o # 0 /\ CancelRule # "none" /\ Closable(starts[s][o].run) /\ ~runs[starts[s][o].run].cx
          /\ TurnLease /\ lease[starts[s][o].run] \notin {None, self} then
      \* The saga turn's rollback is another holder's drive (ErrTurnContended): send again later.
      why := "contended";
      goto Fail;
    elsif o # 0 /\ CancelRule # "none" /\ Closable(starts[s][o].run) then
      \* S3's rule: the open turn's run was cancelled; record the turn closed, then start ours.
      \* Under "rollback", a saga turn's request is acted on first: the session drives the turn's
      \* run under its lease, which rolls it back and writes run:cancelled (closeIfCancelled).
      runs[starts[s][o].run].cx := TRUE;
      hmu[h] := self;
      closing := TRUE;
      cin := starts[s][o].in;
      rid := starts[s][o].run;
      n := t;
      nonce[self] := nonce[self] + 1;
      goto ADo;
    elsif o # 0 then
      badRef := badRef \/ ~Justified(s, CMsg[self]);
      why := "open";
      goto Fail;
    else
      hmu[h] := self;
      nonce[self] := nonce[self] + 1;
      goto SDo;
    end if;
  end with;
SDo:
  with h = CH[self], s = SOf(self), k = hs[CH[self]], cl = <<self, nonce[self]>> do
    if k = Len(starts[s]) then
      either
        starts[s] := Append(starts[s], [in |-> CMsg[self], run |-> TurnId(s, k), claim |-> cl]);
        hs[h] := k + 1;
        ho[h] := k + 1;
        rid := TurnId(s, k);
        hmu[h] := None;
        goto Seed;
      or
        await errs < MaxErr;
        errs := errs + 1;
        why := "storage";
        goto Fail;
      or
        await errs < MaxErr;
        errs := errs + 1;
        starts[s] := Append(starts[s], [in |-> CMsg[self], run |-> TurnId(s, k), claim |-> cl]);
        why := "storage";
        goto Fail;
      end either;
    elsif att > 0 then
      why := "writer";
      goto Fail;
    else
      \* Another handle started turn k first: this handle is stale. Catch up and try again.
      att := 1;
      fresh := TRUE;
      goto SCheck;
    end if;
  end with;

\* SendOnce: keyedTurn (reload if the key is unseen), under the handle's mutex.
KLook:
  await hmu[CH[self]] = None;
  with h = CH[self], s = SOf(self),
       tr = IF KeyedIn(SOf(self), ht[CH[self]], CKey[self]) = NoTurn
            THEN KeyedIn(SOf(self), Len(turns[SOf(self)]), CKey[self])
            ELSE KeyedIn(SOf(self), ht[CH[self]], CKey[self]) do
    if KeyedIn(s, ht[h], CKey[self]) = NoTurn then Reload(h) end if;
    if tr = NoTurn then
      rid := EventId(s, CKey[self]);
      goto Seed;
    elsif tr.in # CMsg[self] then
      why := "reuse";
      goto Fail;
    else
      ret[self] := tr.ans;
      okRun[self] := tr.run;
      outcome[self] := "ok";
      goto Done;
    end if;
  end with;

\* #20's rule (Bug = "StepWrapped"): the messaging guide wrapped Send in a Step keyed by the
\* event id; the Step's record is written after the session recorded the turn.
SWGet:
  if stepv[SOf(self)][CKey[self]] # NoAns then
    ret[self] := stepv[SOf(self)][CKey[self]];
    outcome[self] := "ok";
    goto Done;
  else
    att := 0;
    goto SCheck;
  end if;
SWPut:
  if stepv[SOf(self)][CKey[self]] = NoAns then
    stepv[SOf(self)][CKey[self]] := ans;
  end if;
  outcome[self] := "ok";
  okRun[self] := rid;
  goto Done;

\* turnSeed: from/<run> holds the turn's starting point, journaled before its first model call.
Seed:
  if COp[self] = "root" then
    seed := 0;
    goto DLoad;
  elsif Bug = "NoFrom" then
    \* Before #56: every attempt was seeded with the transcript the handle holds when it runs.
    seed := ht[CH[self]];
    goto DLoad;
  end if;
FDo:
  await hmu[CH[self]] = None;
  with h = CH[self], s = SOf(self), f = from[SOf(self)][rid] do
    if f = -1 then
      either
        from[s][rid] := ht[h];
        seed := ht[h];
        goto DLoad;
      or
        await errs < MaxErr;
        errs := errs + 1;
        why := "storage";
        goto Fail;
      or
        await errs < MaxErr;
        errs := errs + 1;
        from[s][rid] := ht[h];
        why := "storage";
        goto Fail;
      end either;
    elsif f > ht[h] then
      \* Another handle started the turn having seen more of the journal.
      seed := f;
      hmu[h] := self;
      goto FReload;
    else
      seed := f;
      goto DLoad;
    end if;
  end with;
FReload:
  Reload(CH[self]);
  hmu[CH[self]] := None;

\* The turn's run (Agent.run): its Load, run:start, the model calls under the budget, and the end.
DLoad:
  with r = runs[rid] do
    if r.done # NoAns /\ Bug = "none" /\ r.inp # None /\ r.inp # CMsg[self] then
      \* #137 (R137-2): a finished run answers the input its run:start recorded, under its lease or
      \* another holder's; a drive with another input is ErrConfig. Every historical rule (Bug)
      \* predates it: a finished run then returned its answer whatever input it was given.
      why := "input";
      goto Fail;
    elsif r.done # NoAns /\ COp[self] = "root" then
      ans := r.done;
      ret[self] := r.done;
      okRun[self] := rid;
      outcome[self] := "ok";
      goto Done;
    elsif r.done # NoAns then
      \* A finished run returns its recorded answer (completedAnswer), whatever input it is given.
      ans := r.done;
      n := -1;
      goto ADo;
    elsif TurnLease /\ lease[rid] \notin {None, self} then
      \* S4's rule: another drive holds the turn run's lease (ErrTurnContended); send again
      \* later. The lease is asked for before the drive reads anything else of the run (a
      \* finished run, above, needs none): a worker without it does not judge the input.
      why := "contended";
      goto Fail;
    elsif r.cx \/ r.rq then
      \* A cancelled run, or a saga's request: the drive rolls it back and writes run:cancelled.
      runs[rid].cx := TRUE;
      why := "cancelled";
      goto Fail;
    elsif r.inp # None /\ r.inp # CMsg[self] then
      \* #70: an unfinished run resumed with another input is ErrConfig.
      why := "input";
      goto Fail;
    else
      either
        \* The drive pauses (an approval or an interrupt): Send returns it, transcript unadvanced.
        await pauses < MaxPause;
        pauses := pauses + 1;
        why := "paused";
        goto Fail;
      or
        runs[rid] := [r EXCEPT !.inp = IF r.inp = None THEN CMsg[self] ELSE @,
                               !.tag = IF r.inp = None THEN Tag(self) ELSE @];
        if TurnLease then lease[rid] := self end if;
        cnt := r.jsp;
        i := Len(r.llm);
      end either;
    end if;
  end with;
DCall:
  if runs[rid].cx \/ runs[rid].rq then
    \* P14 (D1): run:cancelled (a saga's request: the drive rolls it back, then writes
    \* run:cancelled) is read at every turn boundary.
    runs[rid].cx := TRUE;
    why := "cancelled";
    goto Fail;
  elsif i >= NCalls then
    goto DDone;
  elsif Budget > 0 /\ cnt >= Budget then
    why := "budget";
    goto Fail;
  elsif Len(runs[rid].llm) = i then
    \* The model call, and its @llm/<i> record (first writer wins).
    runs[rid] := [runs[rid] EXCEPT !.llm = Append(@, seed), !.billed = @ + 1, !.jsp = @ + 1];
    cnt := cnt + 1;
    i := i + 1;
    goto DCall;
  else
    \* Another driver recorded this call first: take its record and count its spend. This
    \* call's own spend goes to the meter, journaled as late spend at the drive's end, and the
    \* drive's count does not see it (loop.go, recorded).
    runs[rid] := [runs[rid] EXCEPT !.billed = @ + 1, !.jsp = @ + 1];
    cnt := cnt + 1;
    i := i + 1;
    goto DCall;
  end if;
DDone:
  if runs[rid].cx then
    why := "cancelled";
    goto Fail;
  else
    with d = IF runs[rid].done = NoAns THEN [msg |-> runs[rid].inp, seen |-> runs[rid].llm[1]]
             ELSE runs[rid].done do
      runs[rid].done := d;
      ans := d;
      if TurnLease then lease[rid] := None end if;
      if COp[self] = "root" then
        ret[self] := d;
        okRun[self] := rid;
        outcome[self] := "ok";
        goto Done;
      else
        n := -1;
        goto ADo;
      end if;
    end with;
  end if;

\* appendTurn: the next free turn/N at or after the handle's count, under the mutex (n = -1:
\* the append starts, with a fresh claim).
ADo:
  await hmu[CH[self]] \in {None, self};
  with s = SOf(self), h = CH[self], m = IF n = -1 THEN ht[CH[self]] ELSE n,
       cl = <<self, IF n = -1 THEN nonce[self] + 1 ELSE nonce[self]>>,
       rec = TurnRec(self, <<self, IF n = -1 THEN nonce[self] + 1 ELSE nonce[self]>>, closing, cin, ans, rid) do
    hmu[h] := self;
    if n = -1 then nonce[self] := nonce[self] + 1 end if;
    if n = -1 /\ "S2" \in Fix /\ rid \in Seen(s, ht[h]) then
      \* S2's fix: a run whose turn the handle has loaded is recorded already.
      goto AReload;
    elsif m = Len(turns[s]) then
      either
        turns[s] := Append(turns[s], rec);
        goto AReload;
      or
        await errs < MaxErr;
        errs := errs + 1;
        why := "storage";
        goto Fail;
      or
        await errs < MaxErr;
        errs := errs + 1;
        turns[s] := Append(turns[s], rec);
        why := "storage";
        goto Fail;
      end either;
    elsif turns[s][m + 1].claim = cl \/ turns[s][m + 1].run = rid \/ Bug = "NoSkip" then
      \* Recorded (by this write, or this run's turn by another handle). #22's rule
      \* (Bug = "NoSkip") ended the append at any taken slot.
      goto AReload;
    else
      n := m + 1;
      goto ADo;
    end if;
  end with;
AReload:
  Reload(CH[self]);
  if closing then
    closing := FALSE;
    att := 0;
    fresh := TRUE;
    goto SCheck;
  else
    hmu[CH[self]] := None;
    if COp[self] = "once" /\ Bug = "StepWrapped" then
      goto SWPut;
    else
      ret[self] := ans;
      okRun[self] := rid;
      outcome[self] := "ok";
      goto Done;
    end if;
  end if;

\* The call returns an error. A storage error, a pause, a refusal of an open turn or a lost
\* start is sent again (the caller resolves it, or the message is redelivered) until the message
\* is answered; a budget stop, a cancellation and a mismatched input are final.
Fail:
  if COp[self] # "root" then
    if hmu[CH[self]] = self then hmu[CH[self]] := None; end if;
  end if;
  if rid \in RunIds then
    if lease[rid] = self then lease[rid] := None; end if;
  end if;
  if why \in {"budget", "cancelled", "input", "reuse"} \/ Acked(self)
     \/ (MaxTry > 0 /\ tries + 1 >= MaxTry) then
    if outcome[self] = None then outcome[self] := "fail"; end if;
    goto Done;
  else
    tries := IF MaxTry > 0 THEN tries + 1 ELSE 0;
    why := None;
    rid := None;
    closing := FALSE;
    goto Idle;
  end if;
end process;
end algorithm; *)
\* BEGIN TRANSLATION
VARIABLES runs, starts, turns, from, stepv, ht, hs, ho, hl, hmu, lease, errs, 
          crashes, pauses, cancels, nonce, outcome, ret, okRun, badRef, pc

(* define statement *)
Finished(s) == {turns[s][j].run : j \in DOMAIN turns[s]}
FirstOpen(s) ==
  LET op == {i \in DOMAIN starts[s] : starts[s][i].run \notin Finished(s)}
  IN IF op = {} THEN 0 ELSE Min(op)


Seen(s, k) == {turns[s][j].run : j \in 1..k}
KeyedIn(s, k, key) ==
  LET js == {j \in 1..k : turns[s][j].key = key}
  IN IF js = {} THEN NoTurn ELSE turns[s][Max(js)]


Justified(s, m) ==
  LET o == FirstOpen(s)
  IN o # 0 /\ starts[s][o].in # m /\ ~runs[starts[s][o].run].cx
     /\ ~(runs[starts[s][o].run].rq /\ runs[starts[s][o].run].done = NoAns)


Closable(r) == runs[r].cx \/ (CancelRule = "rollback" /\ runs[r].rq /\ runs[r].done = NoAns)


Acked(c) == \E d \in Callers : CMsg[d] = CMsg[c] /\ COp[d] = COp[c] /\ outcome[d] = "ok"
TurnRec(c, cl, closing, cin, a, r) ==
  IF closing THEN [in |-> cin, ans |-> [msg |-> cin, seen |-> -1], key |-> None, run |-> r, claim |-> cl]
  ELSE [in |-> CMsg[c], ans |-> a, key |-> IF COp[c] = "once" THEN CKey[c] ELSE None,
        run |-> r, claim |-> cl]

VARIABLES rid, seed, cnt, i, n, ans, att, fresh, closing, cin, why, tries

vars == << runs, starts, turns, from, stepv, ht, hs, ho, hl, hmu, lease, errs, 
           crashes, pauses, cancels, nonce, outcome, ret, okRun, badRef, pc, 
           rid, seed, cnt, i, n, ans, att, fresh, closing, cin, why, tries >>

ProcSet == (Callers)

Init == (* Global variables *)
        /\ runs = [r \in RunIds |-> NoRun]
        /\ starts = [s \in Sess |-> <<>>]
        /\ turns = [s \in Sess |-> <<>>]
        /\ from = [s \in Sess |-> [r \in RunIds |-> -1]]
        /\ stepv = [s \in Sess |-> [k \in Keys |-> NoAns]]
        /\ ht = [h \in Handles |-> 0]
        /\ hs = [h \in Handles |-> 0]
        /\ ho = [h \in Handles |-> 0]
        /\ hl = [h \in Handles |-> TRUE]
        /\ hmu = [h \in Handles |-> None]
        /\ lease = [r \in RunIds |-> None]
        /\ errs = 0
        /\ crashes = 0
        /\ pauses = 0
        /\ cancels = 0
        /\ nonce = [c \in Callers |-> 0]
        /\ outcome = [c \in Callers |-> None]
        /\ ret = [c \in Callers |-> NoAns]
        /\ okRun = [c \in Callers |-> None]
        /\ badRef = FALSE
        (* Process caller *)
        /\ rid = [self \in Callers |-> None]
        /\ seed = [self \in Callers |-> 0]
        /\ cnt = [self \in Callers |-> 0]
        /\ i = [self \in Callers |-> 0]
        /\ n = [self \in Callers |-> 0]
        /\ ans = [self \in Callers |-> NoAns]
        /\ att = [self \in Callers |-> 0]
        /\ fresh = [self \in Callers |-> FALSE]
        /\ closing = [self \in Callers |-> FALSE]
        /\ cin = [self \in Callers |-> None]
        /\ why = [self \in Callers |-> None]
        /\ tries = [self \in Callers |-> 0]
        /\ pc = [self \in ProcSet |-> "Idle"]

Idle(self) == /\ pc[self] = "Idle"
              /\ IF COp[self] = "root"
                    THEN /\ rid' = [rid EXCEPT ![self] = CRoot[self]]
                         /\ pc' = [pc EXCEPT ![self] = "DLoad"]
                         /\ att' = att
                    ELSE /\ IF ~hl[CH[self]]
                               THEN /\ pc' = [pc EXCEPT ![self] = "Open"]
                                    /\ UNCHANGED << rid, att >>
                               ELSE /\ IF COp[self] = "send" /\ Bug = "IndexTurns"
                                          THEN /\ rid' = [rid EXCEPT ![self] = TurnId(SOf(self), ht[CH[self]])]
                                               /\ pc' = [pc EXCEPT ![self] = "Seed"]
                                               /\ att' = att
                                          ELSE /\ IF COp[self] = "send"
                                                     THEN /\ att' = [att EXCEPT ![self] = 0]
                                                          /\ pc' = [pc EXCEPT ![self] = "SCheck"]
                                                     ELSE /\ IF Bug = "StepWrapped"
                                                                THEN /\ pc' = [pc EXCEPT ![self] = "SWGet"]
                                                                ELSE /\ pc' = [pc EXCEPT ![self] = "KLook"]
                                                          /\ att' = att
                                               /\ rid' = rid
              /\ UNCHANGED << runs, starts, turns, from, stepv, ht, hs, ho, hl, 
                              hmu, lease, errs, crashes, pauses, cancels, 
                              nonce, outcome, ret, okRun, badRef, seed, cnt, i, 
                              n, ans, fresh, closing, cin, why, tries >>

Open(self) == /\ pc[self] = "Open"
              /\ hmu[CH[self]] = None
              /\ ht' = [ht EXCEPT ![(CH[self])] = Len(turns[HSess[(CH[self])]])]
              /\ hs' = [hs EXCEPT ![(CH[self])] = Len(starts[HSess[(CH[self])]])]
              /\ ho' = [ho EXCEPT ![(CH[self])] = FirstOpen(HSess[(CH[self])])]
              /\ hl' = [hl EXCEPT ![CH[self]] = TRUE]
              /\ pc' = [pc EXCEPT ![self] = "Idle"]
              /\ UNCHANGED << runs, starts, turns, from, stepv, hmu, lease, 
                              errs, crashes, pauses, cancels, nonce, outcome, 
                              ret, okRun, badRef, rid, seed, cnt, i, n, ans, 
                              att, fresh, closing, cin, why, tries >>

SCheck(self) == /\ pc[self] = "SCheck"
                /\ hmu[CH[self]] \in {None, self}
                /\ LET h == CH[self] IN
                     LET s == SOf(self) IN
                       LET rd == fresh[self] \/ ("S1" \in Fix /\ ho[CH[self]] # 0 /\ starts[SOf(self)][ho[CH[self]]].in # CMsg[self]) IN
                         LET o == IF fresh[self] \/ ("S1" \in Fix /\ ho[CH[self]] # 0 /\ starts[SOf(self)][ho[CH[self]]].in # CMsg[self])
                                  THEN FirstOpen(SOf(self)) ELSE ho[CH[self]] IN
                           LET t == IF fresh[self] \/ ("S1" \in Fix /\ ho[CH[self]] # 0 /\ starts[SOf(self)][ho[CH[self]]].in # CMsg[self])
                                    THEN Len(turns[SOf(self)]) ELSE ht[CH[self]] IN
                             /\ IF rd
                                   THEN /\ ht' = [ht EXCEPT ![h] = Len(turns[HSess[h]])]
                                        /\ hs' = [hs EXCEPT ![h] = Len(starts[HSess[h]])]
                                        /\ ho' = [ho EXCEPT ![h] = FirstOpen(HSess[h])]
                                   ELSE /\ TRUE
                                        /\ UNCHANGED << ht, hs, ho >>
                             /\ fresh' = [fresh EXCEPT ![self] = FALSE]
                             /\ IF o # 0 /\ starts[s][o].in = CMsg[self]
                                   THEN /\ rid' = [rid EXCEPT ![self] = starts[s][o].run]
                                        /\ hmu' = [hmu EXCEPT ![h] = None]
                                        /\ pc' = [pc EXCEPT ![self] = "Seed"]
                                        /\ UNCHANGED << runs, nonce, badRef, n, 
                                                        closing, cin, why >>
                                   ELSE /\ IF o # 0 /\ CancelRule # "none" /\ Closable(starts[s][o].run) /\ ~runs[starts[s][o].run].cx
                                              /\ TurnLease /\ lease[starts[s][o].run] \notin {None, self}
                                              THEN /\ why' = [why EXCEPT ![self] = "contended"]
                                                   /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                                   /\ UNCHANGED << runs, hmu, 
                                                                   nonce, 
                                                                   badRef, rid, 
                                                                   n, closing, 
                                                                   cin >>
                                              ELSE /\ IF o # 0 /\ CancelRule # "none" /\ Closable(starts[s][o].run)
                                                         THEN /\ runs' = [runs EXCEPT ![starts[s][o].run].cx = TRUE]
                                                              /\ hmu' = [hmu EXCEPT ![h] = self]
                                                              /\ closing' = [closing EXCEPT ![self] = TRUE]
                                                              /\ cin' = [cin EXCEPT ![self] = starts[s][o].in]
                                                              /\ rid' = [rid EXCEPT ![self] = starts[s][o].run]
                                                              /\ n' = [n EXCEPT ![self] = t]
                                                              /\ nonce' = [nonce EXCEPT ![self] = nonce[self] + 1]
                                                              /\ pc' = [pc EXCEPT ![self] = "ADo"]
                                                              /\ UNCHANGED << badRef, 
                                                                              why >>
                                                         ELSE /\ IF o # 0
                                                                    THEN /\ badRef' = (badRef \/ ~Justified(s, CMsg[self]))
                                                                         /\ why' = [why EXCEPT ![self] = "open"]
                                                                         /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                                                         /\ UNCHANGED << hmu, 
                                                                                         nonce >>
                                                                    ELSE /\ hmu' = [hmu EXCEPT ![h] = self]
                                                                         /\ nonce' = [nonce EXCEPT ![self] = nonce[self] + 1]
                                                                         /\ pc' = [pc EXCEPT ![self] = "SDo"]
                                                                         /\ UNCHANGED << badRef, 
                                                                                         why >>
                                                              /\ UNCHANGED << runs, 
                                                                              rid, 
                                                                              n, 
                                                                              closing, 
                                                                              cin >>
                /\ UNCHANGED << starts, turns, from, stepv, hl, lease, errs, 
                                crashes, pauses, cancels, outcome, ret, okRun, 
                                seed, cnt, i, ans, att, tries >>

SDo(self) == /\ pc[self] = "SDo"
             /\ LET h == CH[self] IN
                  LET s == SOf(self) IN
                    LET k == hs[CH[self]] IN
                      LET cl == <<self, nonce[self]>> IN
                        IF k = Len(starts[s])
                           THEN /\ \/ /\ starts' = [starts EXCEPT ![s] = Append(starts[s], [in |-> CMsg[self], run |-> TurnId(s, k), claim |-> cl])]
                                      /\ hs' = [hs EXCEPT ![h] = k + 1]
                                      /\ ho' = [ho EXCEPT ![h] = k + 1]
                                      /\ rid' = [rid EXCEPT ![self] = TurnId(s, k)]
                                      /\ hmu' = [hmu EXCEPT ![h] = None]
                                      /\ pc' = [pc EXCEPT ![self] = "Seed"]
                                      /\ UNCHANGED <<errs, why>>
                                   \/ /\ errs < MaxErr
                                      /\ errs' = errs + 1
                                      /\ why' = [why EXCEPT ![self] = "storage"]
                                      /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                      /\ UNCHANGED <<starts, hs, ho, hmu, rid>>
                                   \/ /\ errs < MaxErr
                                      /\ errs' = errs + 1
                                      /\ starts' = [starts EXCEPT ![s] = Append(starts[s], [in |-> CMsg[self], run |-> TurnId(s, k), claim |-> cl])]
                                      /\ why' = [why EXCEPT ![self] = "storage"]
                                      /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                      /\ UNCHANGED <<hs, ho, hmu, rid>>
                                /\ UNCHANGED << att, fresh >>
                           ELSE /\ IF att[self] > 0
                                      THEN /\ why' = [why EXCEPT ![self] = "writer"]
                                           /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                           /\ UNCHANGED << att, fresh >>
                                      ELSE /\ att' = [att EXCEPT ![self] = 1]
                                           /\ fresh' = [fresh EXCEPT ![self] = TRUE]
                                           /\ pc' = [pc EXCEPT ![self] = "SCheck"]
                                           /\ why' = why
                                /\ UNCHANGED << starts, hs, ho, hmu, errs, rid >>
             /\ UNCHANGED << runs, turns, from, stepv, ht, hl, lease, crashes, 
                             pauses, cancels, nonce, outcome, ret, okRun, 
                             badRef, seed, cnt, i, n, ans, closing, cin, tries >>

KLook(self) == /\ pc[self] = "KLook"
               /\ hmu[CH[self]] = None
               /\ LET h == CH[self] IN
                    LET s == SOf(self) IN
                      LET tr == IF KeyedIn(SOf(self), ht[CH[self]], CKey[self]) = NoTurn
                                THEN KeyedIn(SOf(self), Len(turns[SOf(self)]), CKey[self])
                                ELSE KeyedIn(SOf(self), ht[CH[self]], CKey[self]) IN
                        /\ IF KeyedIn(s, ht[h], CKey[self]) = NoTurn
                              THEN /\ ht' = [ht EXCEPT ![h] = Len(turns[HSess[h]])]
                                   /\ hs' = [hs EXCEPT ![h] = Len(starts[HSess[h]])]
                                   /\ ho' = [ho EXCEPT ![h] = FirstOpen(HSess[h])]
                              ELSE /\ TRUE
                                   /\ UNCHANGED << ht, hs, ho >>
                        /\ IF tr = NoTurn
                              THEN /\ rid' = [rid EXCEPT ![self] = EventId(s, CKey[self])]
                                   /\ pc' = [pc EXCEPT ![self] = "Seed"]
                                   /\ UNCHANGED << outcome, ret, okRun, why >>
                              ELSE /\ IF tr.in # CMsg[self]
                                         THEN /\ why' = [why EXCEPT ![self] = "reuse"]
                                              /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                              /\ UNCHANGED << outcome, ret, 
                                                              okRun >>
                                         ELSE /\ ret' = [ret EXCEPT ![self] = tr.ans]
                                              /\ okRun' = [okRun EXCEPT ![self] = tr.run]
                                              /\ outcome' = [outcome EXCEPT ![self] = "ok"]
                                              /\ pc' = [pc EXCEPT ![self] = "Done"]
                                              /\ why' = why
                                   /\ rid' = rid
               /\ UNCHANGED << runs, starts, turns, from, stepv, hl, hmu, 
                               lease, errs, crashes, pauses, cancels, nonce, 
                               badRef, seed, cnt, i, n, ans, att, fresh, 
                               closing, cin, tries >>

SWGet(self) == /\ pc[self] = "SWGet"
               /\ IF stepv[SOf(self)][CKey[self]] # NoAns
                     THEN /\ ret' = [ret EXCEPT ![self] = stepv[SOf(self)][CKey[self]]]
                          /\ outcome' = [outcome EXCEPT ![self] = "ok"]
                          /\ pc' = [pc EXCEPT ![self] = "Done"]
                          /\ att' = att
                     ELSE /\ att' = [att EXCEPT ![self] = 0]
                          /\ pc' = [pc EXCEPT ![self] = "SCheck"]
                          /\ UNCHANGED << outcome, ret >>
               /\ UNCHANGED << runs, starts, turns, from, stepv, ht, hs, ho, 
                               hl, hmu, lease, errs, crashes, pauses, cancels, 
                               nonce, okRun, badRef, rid, seed, cnt, i, n, ans, 
                               fresh, closing, cin, why, tries >>

SWPut(self) == /\ pc[self] = "SWPut"
               /\ IF stepv[SOf(self)][CKey[self]] = NoAns
                     THEN /\ stepv' = [stepv EXCEPT ![SOf(self)][CKey[self]] = ans[self]]
                     ELSE /\ TRUE
                          /\ stepv' = stepv
               /\ outcome' = [outcome EXCEPT ![self] = "ok"]
               /\ okRun' = [okRun EXCEPT ![self] = rid[self]]
               /\ pc' = [pc EXCEPT ![self] = "Done"]
               /\ UNCHANGED << runs, starts, turns, from, ht, hs, ho, hl, hmu, 
                               lease, errs, crashes, pauses, cancels, nonce, 
                               ret, badRef, rid, seed, cnt, i, n, ans, att, 
                               fresh, closing, cin, why, tries >>

Seed(self) == /\ pc[self] = "Seed"
              /\ IF COp[self] = "root"
                    THEN /\ seed' = [seed EXCEPT ![self] = 0]
                         /\ pc' = [pc EXCEPT ![self] = "DLoad"]
                    ELSE /\ IF Bug = "NoFrom"
                               THEN /\ seed' = [seed EXCEPT ![self] = ht[CH[self]]]
                                    /\ pc' = [pc EXCEPT ![self] = "DLoad"]
                               ELSE /\ pc' = [pc EXCEPT ![self] = "FDo"]
                                    /\ seed' = seed
              /\ UNCHANGED << runs, starts, turns, from, stepv, ht, hs, ho, hl, 
                              hmu, lease, errs, crashes, pauses, cancels, 
                              nonce, outcome, ret, okRun, badRef, rid, cnt, i, 
                              n, ans, att, fresh, closing, cin, why, tries >>

FDo(self) == /\ pc[self] = "FDo"
             /\ hmu[CH[self]] = None
             /\ LET h == CH[self] IN
                  LET s == SOf(self) IN
                    LET f == from[SOf(self)][rid[self]] IN
                      IF f = -1
                         THEN /\ \/ /\ from' = [from EXCEPT ![s][rid[self]] = ht[h]]
                                    /\ seed' = [seed EXCEPT ![self] = ht[h]]
                                    /\ pc' = [pc EXCEPT ![self] = "DLoad"]
                                    /\ UNCHANGED <<errs, why>>
                                 \/ /\ errs < MaxErr
                                    /\ errs' = errs + 1
                                    /\ why' = [why EXCEPT ![self] = "storage"]
                                    /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                    /\ UNCHANGED <<from, seed>>
                                 \/ /\ errs < MaxErr
                                    /\ errs' = errs + 1
                                    /\ from' = [from EXCEPT ![s][rid[self]] = ht[h]]
                                    /\ why' = [why EXCEPT ![self] = "storage"]
                                    /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                    /\ seed' = seed
                              /\ hmu' = hmu
                         ELSE /\ IF f > ht[h]
                                    THEN /\ seed' = [seed EXCEPT ![self] = f]
                                         /\ hmu' = [hmu EXCEPT ![h] = self]
                                         /\ pc' = [pc EXCEPT ![self] = "FReload"]
                                    ELSE /\ seed' = [seed EXCEPT ![self] = f]
                                         /\ pc' = [pc EXCEPT ![self] = "DLoad"]
                                         /\ hmu' = hmu
                              /\ UNCHANGED << from, errs, why >>
             /\ UNCHANGED << runs, starts, turns, stepv, ht, hs, ho, hl, lease, 
                             crashes, pauses, cancels, nonce, outcome, ret, 
                             okRun, badRef, rid, cnt, i, n, ans, att, fresh, 
                             closing, cin, tries >>

FReload(self) == /\ pc[self] = "FReload"
                 /\ ht' = [ht EXCEPT ![(CH[self])] = Len(turns[HSess[(CH[self])]])]
                 /\ hs' = [hs EXCEPT ![(CH[self])] = Len(starts[HSess[(CH[self])]])]
                 /\ ho' = [ho EXCEPT ![(CH[self])] = FirstOpen(HSess[(CH[self])])]
                 /\ hmu' = [hmu EXCEPT ![CH[self]] = None]
                 /\ pc' = [pc EXCEPT ![self] = "DLoad"]
                 /\ UNCHANGED << runs, starts, turns, from, stepv, hl, lease, 
                                 errs, crashes, pauses, cancels, nonce, 
                                 outcome, ret, okRun, badRef, rid, seed, cnt, 
                                 i, n, ans, att, fresh, closing, cin, why, 
                                 tries >>

DLoad(self) == /\ pc[self] = "DLoad"
               /\ LET r == runs[rid[self]] IN
                    IF r.done # NoAns /\ Bug = "none" /\ r.inp # None /\ r.inp # CMsg[self]
                       THEN /\ why' = [why EXCEPT ![self] = "input"]
                            /\ pc' = [pc EXCEPT ![self] = "Fail"]
                            /\ UNCHANGED << runs, lease, pauses, outcome, ret, 
                                            okRun, cnt, i, n, ans >>
                       ELSE /\ IF r.done # NoAns /\ COp[self] = "root"
                                  THEN /\ ans' = [ans EXCEPT ![self] = r.done]
                                       /\ ret' = [ret EXCEPT ![self] = r.done]
                                       /\ okRun' = [okRun EXCEPT ![self] = rid[self]]
                                       /\ outcome' = [outcome EXCEPT ![self] = "ok"]
                                       /\ pc' = [pc EXCEPT ![self] = "Done"]
                                       /\ UNCHANGED << runs, lease, pauses, 
                                                       cnt, i, n, why >>
                                  ELSE /\ IF r.done # NoAns
                                             THEN /\ ans' = [ans EXCEPT ![self] = r.done]
                                                  /\ n' = [n EXCEPT ![self] = -1]
                                                  /\ pc' = [pc EXCEPT ![self] = "ADo"]
                                                  /\ UNCHANGED << runs, lease, 
                                                                  pauses, cnt, 
                                                                  i, why >>
                                             ELSE /\ IF TurnLease /\ lease[rid[self]] \notin {None, self}
                                                        THEN /\ why' = [why EXCEPT ![self] = "contended"]
                                                             /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                                             /\ UNCHANGED << runs, 
                                                                             lease, 
                                                                             pauses, 
                                                                             cnt, 
                                                                             i >>
                                                        ELSE /\ IF r.cx \/ r.rq
                                                                   THEN /\ runs' = [runs EXCEPT ![rid[self]].cx = TRUE]
                                                                        /\ why' = [why EXCEPT ![self] = "cancelled"]
                                                                        /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                                                        /\ UNCHANGED << lease, 
                                                                                        pauses, 
                                                                                        cnt, 
                                                                                        i >>
                                                                   ELSE /\ IF r.inp # None /\ r.inp # CMsg[self]
                                                                              THEN /\ why' = [why EXCEPT ![self] = "input"]
                                                                                   /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                                                                   /\ UNCHANGED << runs, 
                                                                                                   lease, 
                                                                                                   pauses, 
                                                                                                   cnt, 
                                                                                                   i >>
                                                                              ELSE /\ \/ /\ pauses < MaxPause
                                                                                         /\ pauses' = pauses + 1
                                                                                         /\ why' = [why EXCEPT ![self] = "paused"]
                                                                                         /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                                                                         /\ UNCHANGED <<runs, lease, cnt, i>>
                                                                                      \/ /\ runs' = [runs EXCEPT ![rid[self]] = [r EXCEPT !.inp = IF r.inp = None THEN CMsg[self] ELSE @,
                                                                                                                                          !.tag = IF r.inp = None THEN Tag(self) ELSE @]]
                                                                                         /\ IF TurnLease
                                                                                               THEN /\ lease' = [lease EXCEPT ![rid[self]] = self]
                                                                                               ELSE /\ TRUE
                                                                                                    /\ lease' = lease
                                                                                         /\ cnt' = [cnt EXCEPT ![self] = r.jsp]
                                                                                         /\ i' = [i EXCEPT ![self] = Len(r.llm)]
                                                                                         /\ pc' = [pc EXCEPT ![self] = "DCall"]
                                                                                         /\ UNCHANGED <<pauses, why>>
                                                  /\ UNCHANGED << n, ans >>
                                       /\ UNCHANGED << outcome, ret, okRun >>
               /\ UNCHANGED << starts, turns, from, stepv, ht, hs, ho, hl, hmu, 
                               errs, crashes, cancels, nonce, badRef, rid, 
                               seed, att, fresh, closing, cin, tries >>

DCall(self) == /\ pc[self] = "DCall"
               /\ IF runs[rid[self]].cx \/ runs[rid[self]].rq
                     THEN /\ runs' = [runs EXCEPT ![rid[self]].cx = TRUE]
                          /\ why' = [why EXCEPT ![self] = "cancelled"]
                          /\ pc' = [pc EXCEPT ![self] = "Fail"]
                          /\ UNCHANGED << cnt, i >>
                     ELSE /\ IF i[self] >= NCalls
                                THEN /\ pc' = [pc EXCEPT ![self] = "DDone"]
                                     /\ UNCHANGED << runs, cnt, i, why >>
                                ELSE /\ IF Budget > 0 /\ cnt[self] >= Budget
                                           THEN /\ why' = [why EXCEPT ![self] = "budget"]
                                                /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                                /\ UNCHANGED << runs, cnt, i >>
                                           ELSE /\ IF Len(runs[rid[self]].llm) = i[self]
                                                      THEN /\ runs' = [runs EXCEPT ![rid[self]] = [runs[rid[self]] EXCEPT !.llm = Append(@, seed[self]), !.billed = @ + 1, !.jsp = @ + 1]]
                                                           /\ cnt' = [cnt EXCEPT ![self] = cnt[self] + 1]
                                                           /\ i' = [i EXCEPT ![self] = i[self] + 1]
                                                           /\ pc' = [pc EXCEPT ![self] = "DCall"]
                                                      ELSE /\ runs' = [runs EXCEPT ![rid[self]] = [runs[rid[self]] EXCEPT !.billed = @ + 1, !.jsp = @ + 1]]
                                                           /\ cnt' = [cnt EXCEPT ![self] = cnt[self] + 1]
                                                           /\ i' = [i EXCEPT ![self] = i[self] + 1]
                                                           /\ pc' = [pc EXCEPT ![self] = "DCall"]
                                                /\ why' = why
               /\ UNCHANGED << starts, turns, from, stepv, ht, hs, ho, hl, hmu, 
                               lease, errs, crashes, pauses, cancels, nonce, 
                               outcome, ret, okRun, badRef, rid, seed, n, ans, 
                               att, fresh, closing, cin, tries >>

DDone(self) == /\ pc[self] = "DDone"
               /\ IF runs[rid[self]].cx
                     THEN /\ why' = [why EXCEPT ![self] = "cancelled"]
                          /\ pc' = [pc EXCEPT ![self] = "Fail"]
                          /\ UNCHANGED << runs, lease, outcome, ret, okRun, n, 
                                          ans >>
                     ELSE /\ LET d == IF runs[rid[self]].done = NoAns THEN [msg |-> runs[rid[self]].inp, seen |-> runs[rid[self]].llm[1]]
                                      ELSE runs[rid[self]].done IN
                               /\ runs' = [runs EXCEPT ![rid[self]].done = d]
                               /\ ans' = [ans EXCEPT ![self] = d]
                               /\ IF TurnLease
                                     THEN /\ lease' = [lease EXCEPT ![rid[self]] = None]
                                     ELSE /\ TRUE
                                          /\ lease' = lease
                               /\ IF COp[self] = "root"
                                     THEN /\ ret' = [ret EXCEPT ![self] = d]
                                          /\ okRun' = [okRun EXCEPT ![self] = rid[self]]
                                          /\ outcome' = [outcome EXCEPT ![self] = "ok"]
                                          /\ pc' = [pc EXCEPT ![self] = "Done"]
                                          /\ n' = n
                                     ELSE /\ n' = [n EXCEPT ![self] = -1]
                                          /\ pc' = [pc EXCEPT ![self] = "ADo"]
                                          /\ UNCHANGED << outcome, ret, okRun >>
                          /\ why' = why
               /\ UNCHANGED << starts, turns, from, stepv, ht, hs, ho, hl, hmu, 
                               errs, crashes, pauses, cancels, nonce, badRef, 
                               rid, seed, cnt, i, att, fresh, closing, cin, 
                               tries >>

ADo(self) == /\ pc[self] = "ADo"
             /\ hmu[CH[self]] \in {None, self}
             /\ LET s == SOf(self) IN
                  LET h == CH[self] IN
                    LET m == IF n[self] = -1 THEN ht[CH[self]] ELSE n[self] IN
                      LET cl == <<self, IF n[self] = -1 THEN nonce[self] + 1 ELSE nonce[self]>> IN
                        LET rec == TurnRec(self, <<self, IF n[self] = -1 THEN nonce[self] + 1 ELSE nonce[self]>>, closing[self], cin[self], ans[self], rid[self]) IN
                          /\ hmu' = [hmu EXCEPT ![h] = self]
                          /\ IF n[self] = -1
                                THEN /\ nonce' = [nonce EXCEPT ![self] = nonce[self] + 1]
                                ELSE /\ TRUE
                                     /\ nonce' = nonce
                          /\ IF n[self] = -1 /\ "S2" \in Fix /\ rid[self] \in Seen(s, ht[h])
                                THEN /\ pc' = [pc EXCEPT ![self] = "AReload"]
                                     /\ UNCHANGED << turns, errs, n, why >>
                                ELSE /\ IF m = Len(turns[s])
                                           THEN /\ \/ /\ turns' = [turns EXCEPT ![s] = Append(turns[s], rec)]
                                                      /\ pc' = [pc EXCEPT ![self] = "AReload"]
                                                      /\ UNCHANGED <<errs, why>>
                                                   \/ /\ errs < MaxErr
                                                      /\ errs' = errs + 1
                                                      /\ why' = [why EXCEPT ![self] = "storage"]
                                                      /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                                      /\ turns' = turns
                                                   \/ /\ errs < MaxErr
                                                      /\ errs' = errs + 1
                                                      /\ turns' = [turns EXCEPT ![s] = Append(turns[s], rec)]
                                                      /\ why' = [why EXCEPT ![self] = "storage"]
                                                      /\ pc' = [pc EXCEPT ![self] = "Fail"]
                                                /\ n' = n
                                           ELSE /\ IF turns[s][m + 1].claim = cl \/ turns[s][m + 1].run = rid[self] \/ Bug = "NoSkip"
                                                      THEN /\ pc' = [pc EXCEPT ![self] = "AReload"]
                                                           /\ n' = n
                                                      ELSE /\ n' = [n EXCEPT ![self] = m + 1]
                                                           /\ pc' = [pc EXCEPT ![self] = "ADo"]
                                                /\ UNCHANGED << turns, errs, 
                                                                why >>
             /\ UNCHANGED << runs, starts, from, stepv, ht, hs, ho, hl, lease, 
                             crashes, pauses, cancels, outcome, ret, okRun, 
                             badRef, rid, seed, cnt, i, ans, att, fresh, 
                             closing, cin, tries >>

AReload(self) == /\ pc[self] = "AReload"
                 /\ ht' = [ht EXCEPT ![(CH[self])] = Len(turns[HSess[(CH[self])]])]
                 /\ hs' = [hs EXCEPT ![(CH[self])] = Len(starts[HSess[(CH[self])]])]
                 /\ ho' = [ho EXCEPT ![(CH[self])] = FirstOpen(HSess[(CH[self])])]
                 /\ IF closing[self]
                       THEN /\ closing' = [closing EXCEPT ![self] = FALSE]
                            /\ att' = [att EXCEPT ![self] = 0]
                            /\ fresh' = [fresh EXCEPT ![self] = TRUE]
                            /\ pc' = [pc EXCEPT ![self] = "SCheck"]
                            /\ UNCHANGED << hmu, outcome, ret, okRun >>
                       ELSE /\ hmu' = [hmu EXCEPT ![CH[self]] = None]
                            /\ IF COp[self] = "once" /\ Bug = "StepWrapped"
                                  THEN /\ pc' = [pc EXCEPT ![self] = "SWPut"]
                                       /\ UNCHANGED << outcome, ret, okRun >>
                                  ELSE /\ ret' = [ret EXCEPT ![self] = ans[self]]
                                       /\ okRun' = [okRun EXCEPT ![self] = rid[self]]
                                       /\ outcome' = [outcome EXCEPT ![self] = "ok"]
                                       /\ pc' = [pc EXCEPT ![self] = "Done"]
                            /\ UNCHANGED << att, fresh, closing >>
                 /\ UNCHANGED << runs, starts, turns, from, stepv, hl, lease, 
                                 errs, crashes, pauses, cancels, nonce, badRef, 
                                 rid, seed, cnt, i, n, ans, cin, why, tries >>

Fail(self) == /\ pc[self] = "Fail"
              /\ IF COp[self] # "root"
                    THEN /\ IF hmu[CH[self]] = self
                               THEN /\ hmu' = [hmu EXCEPT ![CH[self]] = None]
                               ELSE /\ TRUE
                                    /\ hmu' = hmu
                    ELSE /\ TRUE
                         /\ hmu' = hmu
              /\ IF rid[self] \in RunIds
                    THEN /\ IF lease[rid[self]] = self
                               THEN /\ lease' = [lease EXCEPT ![rid[self]] = None]
                               ELSE /\ TRUE
                                    /\ lease' = lease
                    ELSE /\ TRUE
                         /\ lease' = lease
              /\ IF why[self] \in {"budget", "cancelled", "input", "reuse"} \/ Acked(self)
                    \/ (MaxTry > 0 /\ tries[self] + 1 >= MaxTry)
                    THEN /\ IF outcome[self] = None
                               THEN /\ outcome' = [outcome EXCEPT ![self] = "fail"]
                               ELSE /\ TRUE
                                    /\ UNCHANGED outcome
                         /\ pc' = [pc EXCEPT ![self] = "Done"]
                         /\ UNCHANGED << rid, closing, why, tries >>
                    ELSE /\ tries' = [tries EXCEPT ![self] = IF MaxTry > 0 THEN tries[self] + 1 ELSE 0]
                         /\ why' = [why EXCEPT ![self] = None]
                         /\ rid' = [rid EXCEPT ![self] = None]
                         /\ closing' = [closing EXCEPT ![self] = FALSE]
                         /\ pc' = [pc EXCEPT ![self] = "Idle"]
                         /\ UNCHANGED outcome
              /\ UNCHANGED << runs, starts, turns, from, stepv, ht, hs, ho, hl, 
                              errs, crashes, pauses, cancels, nonce, ret, 
                              okRun, badRef, seed, cnt, i, n, ans, att, fresh, 
                              cin >>

caller(self) == Idle(self) \/ Open(self) \/ SCheck(self) \/ SDo(self)
                   \/ KLook(self) \/ SWGet(self) \/ SWPut(self)
                   \/ Seed(self) \/ FDo(self) \/ FReload(self)
                   \/ DLoad(self) \/ DCall(self) \/ DDone(self)
                   \/ ADo(self) \/ AReload(self) \/ Fail(self)

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == (\E self \in Callers: caller(self))
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ \A self \in Callers : WF_vars(caller(self))

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION
-----------------------------------------------------------------------------

\* A process crash: every caller sending through its handles is cut off and sends again (the
\* message is redelivered), its handles are reopened from the journal, and a drive in flight is
\* gone (its billed calls with no record are a documented spend limit, model 8).
Crash(p) ==
  /\ crashes < MaxCrash
  /\ \E c \in SessCallers : ProcOf[CH[c]] = p /\ pc[c] # "Done"
  /\ crashes' = crashes + 1
  /\ LET hit(c) == c \in SessCallers /\ ProcOf[CH[c]] = p /\ pc[c] # "Done"
     IN /\ pc' = [c \in Callers |-> IF hit(c) THEN "Idle" ELSE pc[c]]
        /\ rid' = [c \in Callers |-> IF hit(c) THEN None ELSE rid[c]]
        /\ closing' = [c \in Callers |-> IF hit(c) THEN FALSE ELSE closing[c]]
        /\ why' = [c \in Callers |-> IF hit(c) THEN None ELSE why[c]]
        /\ fresh' = [c \in Callers |-> IF hit(c) THEN FALSE ELSE fresh[c]]
  /\ hl' = [h \in Handles |-> IF ProcOf[h] = p THEN FALSE ELSE hl[h]]
  /\ hmu' = [h \in Handles |-> IF ProcOf[h] = p THEN None ELSE hmu[h]]
  \* A dead holder's lease lapses (its TTL is not modelled: model 10 times it).
  /\ lease' = [r \in RunIds |-> IF lease[r] \in SessCallers /\ ProcOf[CH[lease[r]]] = p THEN None ELSE lease[r]]
  /\ UNCHANGED <<runs, starts, turns, from, stepv, ht, hs, ho, errs, pauses, cancels, nonce,
                 outcome, ret, okRun, badRef, seed, cnt, i, n, ans, att, cin, tries>>

\* P14's Cancel (D1) of a turn's run that has started and not ended: run:cancelled, or on a saga
\* the rollback request.
Cancel(r) ==
  /\ cancels < MaxCancel
  /\ runs[r].inp # None /\ runs[r].done = NoAns /\ ~runs[r].cx /\ ~runs[r].rq
  /\ runs' = IF SagaTurns THEN [runs EXCEPT ![r].rq = TRUE] ELSE [runs EXCEPT ![r].cx = TRUE]
  /\ cancels' = cancels + 1
  /\ UNCHANGED <<starts, turns, from, stepv, ht, hs, ho, hl, hmu, lease, errs, crashes, pauses, nonce,
                 outcome, ret, okRun, badRef, pc, rid, seed, cnt, i, n, ans, att, closing, cin,
                 why, tries, fresh>>

FullNext == Next \/ (\E p \in {ProcOf[h] : h \in Handles} : Crash(p)) \/ (\E r \in RunIds : Cancel(r))
FullSpec == Init /\ [][FullNext]_vars /\ \A self \in Callers : WF_vars(caller(self))

(***************************************************************************)
(* Properties                                                               *)
(***************************************************************************)

\* Each turn at most once: a run's turn is recorded in at most one slot of the history.
TurnOnce ==
  \A s \in Sess : \A j1, j2 \in DOMAIN turns[s] : j1 # j2 => turns[s][j1].run # turns[s][j2].run

\* SendOnce at most once: one key is one turn record, and at most one run did a key's work
\* (made a model call; its tools are under model 1's claims).
KeyOnce ==
  \A s \in Sess : \A k \in Keys :
    /\ Cardinality({j \in DOMAIN turns[s] : turns[s][j].key = k}) <= 1
    /\ Cardinality({r \in RunIds : runs[r].tag = <<s, k>> /\ runs[r].llm # <<>>}) <= 1

\* No cross-talk: every reply answers the caller's own message, and so does every turn record,
\* in its own session.
NoCrossTalk ==
  /\ \A c \in Callers : ret[c] # NoAns => ret[c].msg = CMsg[c]
  /\ \A s \in Sess : \A j \in DOMAIN turns[s] : turns[s][j].ans.msg = turns[s][j].in

\* No lost turn: a caller answered by a session turn finds that turn in the history.
NoLostTurn ==
  \A c \in SessCallers : (outcome[c] = "ok" /\ okRun[c] # None) =>
    \E j \in DOMAIN turns[SOf(c)] : turns[SOf(c)][j].run = okRun[c]

\* A turn's model calls all see one transcript, the one its from/ record names, and a turn
\* never saw a turn recorded after it (every reader reads the history in one order).
SeedFaithful ==
  \A s \in Sess : \A r \in RunIds :
    /\ \A a, b \in DOMAIN runs[r].llm : runs[r].llm[a] = runs[r].llm[b]
    /\ (from[s][r] >= 0 /\ runs[r].llm # <<>>) => runs[r].llm[1] = from[s][r]
CausalOrder ==
  \A s \in Sess : \A j \in DOMAIN turns[s] : turns[s][j].ans.seen < j

\* One message, one turn: no two turn records answer one message (the configurations give each
\* delivered message its own text). Send has no key, so this holds only for SendOnce; for Send it
\* is a limit (limits/send-redelivered).
MsgOnce ==
  \A s \in Sess : \A j1, j2 \in DOMAIN turns[s] :
    (j1 # j2 /\ turns[s][j1].ans.seen >= 0 /\ turns[s][j2].ans.seen >= 0) => turns[s][j1].in # turns[s][j2].in

\* The history is append-only: a recorded turn or start never changes or moves.
AppendOnly ==
  [][\A s \in Sess :
       /\ Len(turns'[s]) >= Len(turns[s]) /\ SubSeq(turns'[s], 1, Len(turns[s])) = turns[s]
       /\ Len(starts'[s]) >= Len(starts[s]) /\ SubSeq(starts'[s], 1, Len(starts[s])) = starts[s]]_vars

\* The per-run budget (WithTokenBudget, per Session turn): never more billed calls than Budget,
\* and the journal counts no call twice.
BudgetHeld == Budget > 0 => \A r \in RunIds : runs[r].billed <= Budget
SpendOnce == \A r \in RunIds : runs[r].jsp <= runs[r].billed

\* A refusal of a new message ("a turn for ... is still open") is justified by the journal.
NoFalseRefusal == ~badRef

\* Liveness: every caller is answered, or stops for a final reason, and every Send turn started
\* is eventually recorded.
Answered == \A c \in Callers : <>(pc[c] = "Done")
TurnsSettle == <>[](\A s \in Sess : FirstOpen(s) = 0)

\* Vacuity, expected violated: some session caller is answered.
EffectNotReachable == \A c \in SessCallers : outcome[c] # "ok"

BoundNotHit ==
  /\ \A s \in Sess : Len(starts[s]) <= MaxStarts
  /\ \A c \in Callers : nonce[c] <= MaxClaims
=============================================================================
