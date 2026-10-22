-------------------------------- MODULE Caps --------------------------------
(***************************************************************************)
(* Model 17: the cap ledger (docs/design/untrusted-content-and-amount-      *)
(* caps.md, section 6.5). A cap ledger is a reserved journal run of dense,  *)
(* insert-once slots slot:0, slot:1, ..., one ledger per window. A slot is  *)
(* a reserve or a release and carries cum_after, the window's total after   *)
(* it. A driver that reserves reads the ledger, takes the last slot's       *)
(* cum_after, and inserts slot n+1; the store has one winner per name (A1), *)
(* so two drivers that read the same last slot race for the same name and   *)
(* exactly one wins; the loser reads again. A reader sees a prefix of the   *)
(* ledger (A2: no slot is visible before a lower one), so a read is a       *)
(* prefix and its last slot is well defined.                                *)
(*                                                                          *)
(* An attempt reserves under a cap-intent written first in the run's own    *)
(* journal, insert-once, which fixes the attempt's window: a restart or a   *)
(* later step reads the window from the intent and never derives it again   *)
(* from its clock. The clock bound is checked only when the intent is first *)
(* written: the driver's window must not be earlier than the newest window  *)
(* in the index run, nor more than Bound later; the window is inserted into *)
(* the index (insert-once) before the intent is written.                    *)
(*                                                                          *)
(* A tool that is not retry-safe (Marked) has an exclusive attempt claim    *)
(* before the reserve: the driver that won it is the only one that reserves *)
(* and begins, and a restart finds the marker and halts. A retry-safe tool  *)
(* (an Idempotent write; not Marked) has no claim: its attempt is the       *)
(* tool-use ID, and any number of drivers and restarts reserve it, find the *)
(* live slot and begin.                                                     *)
(*                                                                          *)
(* A release is written only when the driver can prove the tool did not     *)
(* begin, before the attempt is voided; a void that came without a release  *)
(* (the loop's other paths) leaves a slot counted until the reconcile pass  *)
(* releases it. A release is applied once: it looks for a live slot of the  *)
(* attempt and releases that one. A tool that began is never released.      *)
(*                                                                          *)
(* Not modelled: amount parsing, key digests and encodings (tested in Go),  *)
(* the cap-reserve audit record (recovery does not depend on it), the       *)
(* store's own implementation (A1 and A2 are assumed, as the storetest      *)
(* suite checks them), and waivers' provenance (WaiverOf is a given).       *)
(***************************************************************************)
EXTENDS Integers, FiniteSets

CONSTANTS
  Drivers,         \* drivers, each in its own process (model values)
  Attempts,        \* attempts (model values)
  AttOf,           \* the attempt each driver works on; two drivers may share one
  Amt,             \* each attempt's amount (minor units)
  Limit,           \* the rule's limit
  WaiverOf,        \* attempts that carry a waiver for the rule
  Marked,          \* attempts of tools that are not retry-safe (an exclusive attempt claim); the rest are Idempotent
  ReleaseUnmarked, \* TRUE: a driver may release the slot of a retry-safe attempt it cancelled; FALSE: only marked attempts are released
  MaxWin,          \* windows are 0..MaxWin
  Clock0,          \* each driver's clock (a window) at the start
  MaxTick,         \* budget of clock ticks (a clock moves to the next window)
  Bound,           \* the clock bound: windows a first intent may be later than the newest recorded one
  MaxSlots,        \* slot names are 0..MaxSlots-1 (a bound on the ledger)
  MaxCrash,        \* budget of crashes and re-runs (a retry, a resume, a middleware's second next)
  MaxCancel,       \* budget of releases by the reserving driver (a call that did not begin)
  MaxStray,        \* budget of voids written without a release
  MaxRecon,        \* budget of reconcile passes over one voided attempt
  Bug              \* "none" or a reverted rule, see regress/

None == "none"
Unset == -1000
Windows == 0..MaxWin
Names == 0..(MaxSlots - 1)
Null == [kind |-> "none"]
Max(S) == CHOOSE x \in S : \A y \in S : y <= x

VARIABLES
  slots,    \* slots[w][n]: the slot named slot:n of window w's ledger, or Null
  index,    \* the windows inserted into the index run (win:<w>)
  intent,   \* intent[a]: the cap-intent of attempt a (its window, and the index floor it was checked against), or Null
  claim,    \* claim[a]: the driver that won a marked attempt's claim
  began,    \* began[a]: the tool of attempt a began
  voided,   \* voided[a]: attempt a is recorded as not started
  stray,    \* stray[a]: a void written without a release (ghost: not the release-then-void path)
  L,        \* L[d]: driver d's local state (a record, see Fresh)
  now,      \* now[d]: driver d's clock, a window
  B,        \* the spent budgets
  seen      \* ghost: effects the run has reached, for the vacuity checks

vars == <<slots, index, intent, claim, began, voided, stray, L, now, B, seen>>

Fresh(d) == [pc |-> "start", op |-> "reserve", lw |-> -1, lfl |-> -1, lview |-> 0, lcum |-> 0,
             cum0 |-> Unset, lref |-> -1, lta |-> AttOf[d], res |-> FALSE]

\* The ledger of window w.
Filled(w) == {n \in Names : slots[w][n].kind /= "none"}
M(w) == IF Filled(w) = {} THEN 0 ELSE Max(Filled(w)) + 1
Below(w, v) == {n \in Filled(w) : n < v}
CumAt(w, v) == IF Below(w, v) = {} THEN 0 ELSE slots[w][Max(Below(w, v))].cum
ReleasedIn(w, n) == \E j \in Filled(w) : slots[w][j].kind = "release" /\ slots[w][j].ref = n
AnyIn(w, a) == {n \in Filled(w) : slots[w][n].kind = "reserve" /\ slots[w][n].att = a}
LiveIn(w, a) == {n \in AnyIn(w, a) : ~ReleasedIn(w, n)}
LiveAll(a) == {x \in Windows \X Names : x[2] \in LiveIn(x[1], a)}

HasIntent(a) == intent[a].kind /= "none"

\* What a reserve's lookup sees of the attempt's live slots: all of them (A2: a reader sees a prefix), or
\* (Bug "no-a2") only the last slot of the ledger, as a store whose reads skip lower slots would show.
Lookup(w, a) == IF Bug = "no-a2" THEN {n \in LiveIn(w, a) : n = M(w) - 1} ELSE LiveIn(w, a)

\* The window a driver reads and writes: the one its intent fixed, or (Bug "rederive") its clock again.
W(d) == IF Bug = "rederive" THEN now[d] ELSE L[d].lw

\* What happens when a release is in: a cancelled marked attempt is voided next (Bug "void-first" voided it first).
AfterRelease(l) == IF l.op = "cancel" /\ l.lta \in Marked /\ Bug /= "void-first" THEN "void" ELSE "done"

NewSlot(l) ==
  IF l.op = "reserve"
  THEN [kind |-> "reserve", att |-> l.lta, amt |-> Amt[l.lta], ref |-> -1, cum |-> l.lcum, over |-> l.lcum > Limit]
  ELSE [kind |-> "release", att |-> l.lta, amt |-> Amt[l.lta], ref |-> l.lref, cum |-> l.lcum, over |-> FALSE]

Go(d, f) == L' = [L EXCEPT ![d] = f]

Init ==
  /\ slots = [w \in Windows |-> [n \in Names |-> Null]]
  /\ index = {}
  /\ intent = [a \in Attempts |-> Null]
  /\ claim = [a \in Attempts |-> None]
  /\ began = [a \in Attempts |-> FALSE]
  /\ voided = [a \in Attempts |-> FALSE]
  /\ stray = [a \in Attempts |-> FALSE]
  /\ L = [d \in Drivers |-> Fresh(d)]
  /\ now = Clock0
  /\ B = [crash |-> 0, tick |-> 0, cancel |-> 0, stray |-> 0, recon |-> 0]
  /\ seen = {}

(* A driver reaches the reserve: a marked attempt takes its exclusive claim (a restart finds it and
   halts); the window comes from the intent if there is one, else from the clock. *)
Start(d) ==
  LET l == L[d]
      a == AttOf[d]
  IN /\ l.pc = "start"
     /\ claim' = IF a \in Marked /\ claim[a] = None THEN [claim EXCEPT ![a] = d] ELSE claim
     /\ IF a \in Marked /\ claim[a] /= None
        THEN /\ Go(d, [l EXCEPT !.pc = "halted"])
             /\ seen' = seen
        ELSE IF HasIntent(a)
        THEN /\ Go(d, [l EXCEPT !.pc = IF Bug = "check-on-resume" THEN "check" ELSE "read",
                                 !.lw = IF Bug = "rederive" THEN now[d] ELSE intent[a].win, !.res = TRUE])
             /\ seen' = seen \cup {"resumed"}
        ELSE /\ Go(d, [l EXCEPT !.pc = "check", !.lw = now[d]])
             /\ seen' = seen
     /\ UNCHANGED <<slots, index, intent, began, voided, stray, now, B>>

(* The clock bound, at the first intent only: not earlier than the newest window in the index, and
   not more than Bound later. *)
Check(d) ==
  LET l == L[d]
      fl == IF index = {} THEN -1 ELSE Max(index)
  IN /\ l.pc = "check"
     /\ IF Bug /= "no-clock-check" /\ fl >= 0 /\ (l.lw < fl \/ l.lw > fl + Bound)
        THEN /\ Go(d, [l EXCEPT !.pc = "refused"])
             /\ seen' = seen \cup {"clock"} \cup (IF l.res THEN {"resumeRefused"} ELSE {})
        ELSE /\ Go(d, [l EXCEPT !.pc = IF Bug = "index-after" THEN "intent" ELSE "idxins", !.lfl = fl])
             /\ seen' = seen
     /\ UNCHANGED <<slots, index, intent, claim, began, voided, stray, now, B>>

\* The window enters the index (insert-once, so racing drivers agree) before the intent is written.
IdxInsert(d) ==
  LET l == L[d] IN
  /\ l.pc = "idxins"
  /\ index' = index \cup {l.lw}
  /\ Go(d, [l EXCEPT !.pc = IF Bug = "index-after" THEN "read" ELSE "intent"])
  /\ UNCHANGED <<slots, intent, claim, began, voided, stray, now, B, seen>>

(* The cap-intent, insert-once in the run's own journal. A driver that loses the insert adopts the
   window the winner wrote. *)
Intent(d) ==
  LET l == L[d]
      a == AttOf[d]
      next == IF Bug = "index-after" THEN "idxins" ELSE "read"
  IN /\ l.pc = "intent"
     /\ IF HasIntent(a)
        THEN /\ UNCHANGED intent
             /\ Go(d, [l EXCEPT !.pc = next, !.lw = IF Bug = "rederive" THEN now[d] ELSE intent[a].win])
        ELSE /\ intent' = [intent EXCEPT ![a] = [kind |-> "intent", win |-> l.lw, floor |-> l.lfl]]
             /\ Go(d, [l EXCEPT !.pc = next])
     /\ UNCHANGED <<slots, index, claim, began, voided, stray, now, B, seen>>

(* Reserve, steps 1 and 2: read the ledger (a prefix, ending at the last slot, n); if a live slot
   carries the attempt it is the reservation; else cum_after' = cum_after + amount, refused when it
   exceeds the limit without a waiver. The driver remembers the length it read (the n it inserts
   after) and the total it computed. *)
ReserveRead(d, l, w, m) ==
  LET a == l.lta
      cumNow == CumAt(w, m)
      base == IF Bug = "stale-cum" /\ l.cum0 /= Unset THEN l.cum0 ELSE cumNow
      newcum == base + Amt[a]
  IN IF Lookup(w, a) /= {} /\ Bug /= "no-lookup"
     THEN /\ Go(d, [l EXCEPT !.pc = "reserved"])
          /\ seen' = seen \cup {"found"}
     ELSE IF newcum > Limit /\ a \notin WaiverOf
     THEN /\ Go(d, [l EXCEPT !.pc = "refused"])
          /\ seen' = seen \cup {"limited"}
     ELSE /\ Go(d, [l EXCEPT !.pc = "insert", !.lview = m, !.lcum = newcum,
                              !.cum0 = IF l.cum0 = Unset THEN cumNow ELSE l.cum0])
          /\ seen' = seen

(* A release looks for a live slot of the attempt (the dedupe scan; Bug "no-dedupe" takes any slot
   of it, released or not) and appends a release of it at the next name; a slot already released
   is done. *)
ReleaseRead(d, l, w, m) ==
  LET ts == IF Bug = "no-dedupe" THEN AnyIn(w, l.lta) ELSE LiveIn(w, l.lta)
      r == CHOOSE n \in ts : TRUE
  IN IF ts = {}
     THEN /\ Go(d, [l EXCEPT !.pc = AfterRelease(l)])
          /\ seen' = seen
     ELSE /\ Go(d, [l EXCEPT !.pc = "insert", !.lview = m, !.lref = r,
                              !.lcum = CumAt(w, m) - slots[w][r].amt])
          /\ seen' = seen

Read(d) ==
  LET l == L[d]
      w == W(d)
  IN /\ l.pc = "read"
     /\ IF l.op = "reserve" THEN ReserveRead(d, l, w, M(w)) ELSE ReleaseRead(d, l, w, M(w))
     /\ UNCHANGED <<slots, index, intent, claim, began, voided, stray, now, B>>

(* Reserve, step 3: Insert(slot:n+1). One writer wins the name (A1). A driver that wins has an exact
   cum_after: slot n was the last entry when its insert succeeded. A loser reads again.
   Bug "n2" inserts at n+2; Bug "no-a1" is a store whose insert overwrites a taken name. *)
Insert(d) ==
  LET l == L[d]
      w == W(d)
      k == l.lview + (IF Bug = "n2" THEN 1 ELSE 0)
  IN /\ l.pc = "insert"
     /\ k \in Names
     /\ IF slots[w][k].kind = "none" \/ Bug = "no-a1"
        THEN /\ slots' = [slots EXCEPT ![w][k] = NewSlot(l)]
             /\ Go(d, [l EXCEPT !.pc = IF l.op = "reserve" THEN "reserved" ELSE AfterRelease(l)])
             /\ seen' = IF l.op = "recon" THEN seen \cup {"reconciled"} ELSE seen
        ELSE /\ UNCHANGED slots
             /\ Go(d, [l EXCEPT !.pc = "read"])
             /\ seen' = seen \cup {"contended"}
     /\ UNCHANGED <<index, intent, claim, began, voided, stray, now, B>>

\* The tool begins (beginCall): a marked attempt only for the driver that holds the claim and was not voided.
Begin(d) ==
  LET l == L[d]
      a == AttOf[d]
  IN /\ l.pc = "reserved"
     /\ a \in Marked => (claim[a] = d /\ ~voided[a])
     /\ began' = [began EXCEPT ![a] = TRUE]
     /\ Go(d, [l EXCEPT !.pc = "begun"])
     /\ UNCHANGED <<slots, index, intent, claim, voided, stray, now, B, seen>>

(* The driver, reserved and not begun, finds the call cancelled or the store failed: it releases
   (proof it did not begin) and then voids the attempt (a retry-safe attempt has no void). Bug
   "void-first" voids before the release. *)
Cancel(d) ==
  LET l == L[d]
      a == AttOf[d]
  IN /\ l.pc = "reserved"
     /\ (a \in Marked \/ ReleaseUnmarked)
     /\ B.cancel < MaxCancel
     /\ B' = [B EXCEPT !.cancel = @ + 1]
     /\ voided' = IF Bug = "void-first" /\ a \in Marked THEN [voided EXCEPT ![a] = TRUE] ELSE voided
     /\ Go(d, [l EXCEPT !.pc = "read", !.op = "cancel"])
     /\ UNCHANGED <<slots, index, intent, claim, began, stray, now, seen>>

\* Another path of the loop voids the attempt without a release: the slot stays counted until the reconcile pass.
StrayVoid(d) ==
  LET l == L[d]
      a == AttOf[d]
  IN /\ l.pc = "reserved"
     /\ a \in Marked
     /\ B.stray < MaxStray
     /\ B' = [B EXCEPT !.stray = @ + 1]
     /\ voided' = [voided EXCEPT ![a] = TRUE]
     /\ stray' = [stray EXCEPT ![a] = TRUE]
     /\ Go(d, [l EXCEPT !.pc = "done"])
     /\ UNCHANGED <<slots, index, intent, claim, began, now, seen>>

Void(d) ==
  LET l == L[d] IN
  /\ l.pc = "void"
  /\ voided' = [voided EXCEPT ![AttOf[d]] = TRUE]
  /\ Go(d, [l EXCEPT !.pc = "done"])
  /\ UNCHANGED <<slots, index, intent, claim, began, stray, now, B, seen>>

Finish(d) ==
  LET l == L[d] IN
  /\ l.pc = "begun"
  /\ Go(d, [l EXCEPT !.pc = "done"])
  /\ UNCHANGED <<slots, index, intent, claim, began, voided, stray, now, B, seen>>

\* Bug "rollback-credit": a saga's rollback releases the slot of a call that began.
RollbackCredit(d) ==
  LET l == L[d] IN
  /\ Bug = "rollback-credit"
  /\ l.pc = "begun"
  /\ B.cancel < MaxCancel
  /\ B' = [B EXCEPT !.cancel = @ + 1]
  /\ Go(d, [l EXCEPT !.pc = "read", !.op = "rollback"])
  /\ UNCHANGED <<slots, index, intent, claim, began, voided, stray, now, seen>>

(* The reconcile pass (Recover, bide audit caps): a driver that has finished lists a voided attempt
   whose slot is still live and releases it, in the window the attempt's intent names. *)
Reconcile(d) ==
  LET l == L[d] IN
  /\ l.pc = "done"
  /\ B.recon < MaxRecon
  /\ \E a \in Marked :
       /\ voided[a]
       /\ HasIntent(a)
       /\ LiveIn(intent[a].win, a) /= {}
       /\ Go(d, [l EXCEPT !.pc = "read", !.op = "recon", !.lta = a, !.lw = intent[a].win])
  /\ B' = [B EXCEPT !.recon = @ + 1]
  /\ UNCHANGED <<slots, index, intent, claim, began, voided, stray, now, seen>>

\* A crash, or a re-run of the call (a retry, a resume, a middleware's second next): local state is lost.
Crash(d) ==
  /\ B.crash < MaxCrash
  /\ L[d].pc \notin {"start", "halted"}
  /\ Go(d, Fresh(d))
  /\ B' = [B EXCEPT !.crash = @ + 1]
  /\ UNCHANGED <<slots, index, intent, claim, began, voided, stray, now, seen>>

Tick(d) ==
  /\ B.tick < MaxTick
  /\ now[d] < MaxWin
  /\ now' = [now EXCEPT ![d] = @ + 1]
  /\ B' = [B EXCEPT !.tick = @ + 1]
  /\ UNCHANGED <<slots, index, intent, claim, began, voided, stray, L, seen>>

Next ==
  \E d \in Drivers :
    \/ Start(d) \/ Check(d) \/ IdxInsert(d) \/ Intent(d) \/ Read(d) \/ Insert(d)
    \/ Begin(d) \/ Cancel(d) \/ StrayVoid(d) \/ Void(d) \/ Finish(d) \/ RollbackCredit(d)
    \/ Reconcile(d) \/ Crash(d) \/ Tick(d)

Spec == Init /\ [][Next]_vars

----------------------------------------------------------------------------
\* Properties.

RECURSIVE SumNames(_, _)
SumNames(w, S) == IF S = {} THEN 0 ELSE LET n == CHOOSE x \in S : TRUE IN slots[w][n].amt + SumNames(w, S \ {n})

LiveNames(w) == {n \in Filled(w) : slots[w][n].kind = "reserve" /\ ~ReleasedIn(w, n)}
NonOver(w) == {n \in LiveNames(w) : ~slots[w][n].over}

TypeOK ==
  /\ \A w \in Windows, n \in Names : slots[w][n].kind \in {"none", "reserve", "release"}
  /\ index \subseteq Windows
  /\ \A d \in Drivers : now[d] \in Windows

\* The sum of the live reservations of a window never exceeds the limit but by a waived slot: the
\* slots without a waiver stay within the limit, and a total over it holds a live waived slot.
NoOvershoot ==
  \A w \in Windows :
    /\ SumNames(w, NonOver(w)) <= Limit
    /\ SumNames(w, LiveNames(w)) > Limit => LiveNames(w) \ NonOver(w) /= {}

\* No gap: the slot names of a ledger are 0..k-1, so no slot at n+2 exists before n+1.
Dense == \A w \in Windows : Filled(w) = 0..(Cardinality(Filled(w)) - 1)

\* Each slot's cum_after is the one before it, plus or minus its amount.
CumExact ==
  \A w \in Windows : \A n \in Filled(w) :
    slots[w][n].cum = (IF n = 0 THEN 0 ELSE slots[w][n - 1].cum)
                      + (IF slots[w][n].kind = "reserve" THEN slots[w][n].amt ELSE -slots[w][n].amt)

\* An attempt has at most one live slot, in all windows.
ExactlyOnce == \A a \in Attempts : Cardinality(LiveAll(a)) <= 1

\* A slot is released at most once.
ReleaseOnce ==
  \A w \in Windows : \A n \in Filled(w) :
    Cardinality({j \in Filled(w) : slots[w][j].kind = "release" /\ slots[w][j].ref = n}) <= 1

\* An attempt whose tool began is counted: it holds a live slot (no release, rollback or otherwise, took it back).
NeverUndercount == \A a \in Attempts : began[a] => LiveAll(a) /= {}

\* An attempt's window never changes after its intent: every slot of the attempt is in its intent's window,
\* and so is every read and write of a driver that has passed the intent.
IntentStable ==
  /\ \A w \in Windows : \A n \in Filled(w) : HasIntent(slots[w][n].att) => intent[slots[w][n].att].win = w
  /\ \A d \in Drivers : L[d].pc \in {"read", "insert"} => (HasIntent(L[d].lta) /\ W(d) = intent[L[d].lta].win)

\* The window of every intent is in the index: the index is written first.
IndexCoversIntents == \A a \in Attempts : HasIntent(a) => intent[a].win \in index

\* The clock bound held when the intent was written: not earlier than the newest recorded window, and not more than Bound later.
IntentChecked ==
  \A a \in Attempts : HasIntent(a) => (intent[a].floor < 0 \/ (intent[a].win >= intent[a].floor /\ intent[a].win <= intent[a].floor + Bound))

\* A cancelled marked attempt is voided only after its release (a stray void is the reconcile pass's).
ReleaseBeforeVoid == \A a \in Marked : (voided[a] /\ ~stray[a]) => LiveAll(a) = {}

\* A call that resumed with an intent is never refused by the clock check, which runs at the first intent only.
ResumedNeverClockRefused == "resumeRefused" \notin seen

\* The ledger's name bound is never hit: a driver is never blocked for want of a slot name, so a bound
\* that is too small is a configuration error, not a result.
SlotBoundNotHit == \A d \in Drivers : L[d].pc = "insert" => (L[d].lview + (IF Bug = "n2" THEN 1 ELSE 0)) \in Names

\* Vacuity and reachability: each is violated when the effect is reached.
EffectNotReachable == \A a \in Attempts : ~began[a]
ContendedNotReachable == "contended" \notin seen
FoundNotReachable == "found" \notin seen
LimitedNotReachable == "limited" \notin seen
ClockNotReachable == "clock" \notin seen
ResumedNotReachable == "resumed" \notin seen
ReconciledNotReachable == "reconciled" \notin seen
ReleasedNotReachable == \A w \in Windows : \A n \in Filled(w) : slots[w][n].kind /= "release"
WaivedNotReachable == \A w \in Windows : \A n \in Filled(w) : ~slots[w][n].over
VoidedNotReachable == \A a \in Attempts : ~voided[a]
TwoWindowsNotReachable == Cardinality(index) < 2
=============================================================================
