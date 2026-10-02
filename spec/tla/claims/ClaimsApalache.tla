--------------------------- MODULE ClaimsApalache ---------------------------
(***************************************************************************)
(* Typed wrapper of Claims for the Apalache symbolic model checker. It      *)
(* declares every constant and variable of Claims with its type (Apalache's *)
(* Snowcat checker needs one) and instantiates Claims unchanged, so the     *)
(* translation stays the one TLC checks. Model values are strings here.     *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, Sequences

CONSTANTS
  \* @type: Set(Str);
  Drivers,
  \* @type: Set(Str);
  Procs,
  \* @type: Set(Str);
  Calls,
  \* @type: Str -> Str;
  ProcOf,
  \* @type: Str -> Str;
  CallOf,
  \* @type: Str -> Str;
  Kind,
  \* @type: Set(Str);
  PauseCalls,
  \* @type: Str -> Str;
  NextOf,
  \* @type: Set(Str);
  FirstCalls,
  \* @type: Int;
  MaxGen,
  \* @type: Int;
  MaxIds,
  \* @type: Int;
  MaxAmbig,
  \* @type: Int;
  MaxCrash,
  \* @type: Int;
  MaxCancel,
  \* @type: Int;
  MaxEvict,
  \* @type: Bool;
  LateCommit,
  \* @type: Bool;
  HasResolver,
  \* @type: Str;
  LiveCheck,
  \* @type: Set(Str);
  LeasedDrivers,
  \* @type: Bool;
  ResolveClaim,
  \* @type: Str;
  ResolverProc,
  \* @type: Bool;
  PlainRunIdleAtCheck,
  \* @type: Bool;
  ResolveVoidOnError,
  \* @type: Str -> Str;
  Policy0,
  \* @type: Str -> { need: Int, apprs: Set(Str) };
  Policies,
  \* @type: Set(Str);
  PolicyChoices,
  \* @type: Set(Str);
  ApproverIds,
  \* @type: Set(Str);
  Actors,
  \* @type: Str -> Set(Str);
  KeyOf,
  \* @type: Str -> Set(Str);
  KeyOf2,
  \* @type: Int;
  MaxResolverChange,
  \* @type: Str -> Str;
  Holder,
  \* @type: Set(<<Str, Str>>);
  FoldSame,
  \* @type: Set(Str);
  Subjects,
  \* @type: Int;
  MaxApprove1,
  \* @type: Int;
  MaxSubmit,
  \* @type: Int;
  MaxRedeploy,
  \* @type: Bool;
  KeyCheck,
  \* @type: Str;
  Bug

VARIABLES
  \* @type: Str -> (Int -> Int);
  marker,
  \* @type: Set(<<Str, Int, Int>>);
  nsSet,
  \* @type: Set(<<Str, Int, Int>>);
  heldSet,
  \* @type: Str -> Str;
  result,
  \* @type: Str -> Bool;
  toolFail,
  \* @type: Set(<<Str, Int, Int>>);
  lateMarker,
  \* @type: Set(<<Str, Int, Int>>);
  lateNS,
  \* @type: Set(<<Str, Str>>);
  lateResult,
  \* @type: Str -> (Str -> (Int -> Set(Int)));
  pending,
  \* @type: Str -> (Str -> Str);
  fl,
  \* @type: Str -> (Str -> Set(Str));
  waiters,
  \* @type: Str -> Str;
  jres,
  \* @type: Str -> Int;
  cid,
  \* @type: Str -> Int;
  oldId,
  \* @type: Str -> Set(Int);
  toRetry,
  \* @type: Str;
  lease,
  \* @type: Set(Int);
  rids,
  \* @type: Int;
  rcid,
  \* @type: Int;
  ambig,
  \* @type: Int;
  crashes,
  \* @type: Int;
  cancels,
  \* @type: Int;
  evictions,
  \* @type: Str -> Int;
  fired,
  \* @type: Str -> (Int -> Int);
  firedAt,
  \* @type: Set(Int);
  lost,
  \* @type: Set(Int);
  evicted,
  \* @type: Str -> Str;
  one,
  \* @type: Str -> Seq({ a: Str, h: Str, ok: Bool, k: Str, subj: Str });
  dlog,
  \* @type: Str -> { rec: Bool, passed: Bool, pol: Str, signers: Set(Str), excl: Int, denials: Int };
  tally,
  \* @type: Str -> (Str -> Str);
  policy,
  \* @type: Int;
  approves1,
  \* @type: Int;
  submits,
  \* @type: Int;
  redeploys,
  \* @type: Bool;
  badFire,
  \* @type: Bool;
  badPause,
  \* @type: Bool;
  badDeny,
  \* @type: Str -> Set(Str);
  keys,
  \* @type: Int;
  resChanges,
  \* @type: Str -> Str;
  pc,
  \* @type: Str -> Int;
  g,
  \* @type: Str -> Int;
  gg,
  \* @type: Str -> Str;
  reply,
  \* @type: Str -> Str;
  outcome,
  \* @type: Str -> Bool;
  won,
  \* @type: Str -> Bool;
  reused,
  \* @type: Str -> Str;
  snapOne,
  \* @type: Str -> { rec: Bool, passed: Bool, pol: Str, signers: Set(Str), excl: Int, denials: Int };
  snapTally,
  \* @type: Str -> Str;
  gp,
  \* @type: Str -> { rec: Bool, passed: Bool, pol: Str, signers: Set(Str), excl: Int, denials: Int };
  qt,
  \* @type: Str -> Bool;
  loadDenied,
  \* @type: Str -> Str;
  rc,
  \* @type: Str -> Int;
  rg,
  \* @type: Str -> Str;
  rreply,
  \* @type: Str -> Bool;
  rclaimed

INSTANCE Claims

\* Operators the configurations assign with <-.
\* @type: Str -> Set(Str);
ApNoKeys == [x \in {} |-> {}]
\* @type: Str -> Str;
ApNoHolder == [x \in {} |-> "none"]
\* @type: Set(Str);
ApNoStrs == {}
\* @type: Set(<<Str, Str>>);
ApNoPairs == {}
ApAllTool == [c \in Calls |-> "tool"]
ApCrossProc == [d \in Drivers |-> IF d = "d1" THEN "p1" ELSE "p2"]
ApAllStep == [c \in Calls |-> "step"]
ApSameProc == [d \in Drivers |-> "p1"]
ApC1ThenC2 == [c \in Calls |-> IF c = "c1" THEN "c2" ELSE "none"]
ApOneCall == [d \in Drivers |-> "c1"]
ApNoNext == [c \in Calls |-> "none"]
ApNoGate == [c \in Calls |-> "none"]
ApPols == [n \in {"m"} |-> [need |-> 2, apprs |-> {"a1", "a2", "a3"}]]

\* The constants a configuration (apalache/*.cfg) leaves to the solver: every placement of the
\* drivers D in the processes P and on the calls C, every kind of call (tool, step or flow) for the calls C, whether
\* a Step pauses, which drivers hold the lease, and whether errored writes may commit late (weak
\* A3). One check covers what TLC needs one configuration each for. D, P and C must be the
\* configuration's Drivers, Procs and Calls (Apalache's --cinit reads no constant the .cfg sets).
Placements(D, P, C) ==
  /\ ProcOf' \in [D -> P]
  /\ CallOf' \in [D -> C]
  /\ Kind' \in [C -> {"tool", "step", "flow"}]
  /\ PauseCalls' \in SUBSET {c \in C : Kind'[c] = "step"}
  /\ LeasedDrivers' \in SUBSET D
  /\ LateCommit' \in BOOLEAN

CInit2x2x1 == Placements({"d1", "d2"}, {"p1", "p2"}, {"c1"})
CInit3x2x1 == Placements({"d1", "d2", "d3"}, {"p1", "p2"}, {"c1"})
CInit2x2x2 == Placements({"d1", "d2"}, {"p1", "p2"}, {"c1", "c2"})
CInit3x2x2 == Placements({"d1", "d2", "d3"}, {"p1", "p2"}, {"c1", "c2"})

\* Halt resolution as the current protocol does it: the resolver claims the attempt after the live
\* one, leaves its attempt live when its result write errors, checks either the lease or the
\* minimum age, and runs in either process or in neither. Under the lease check, no plain run
\* holds the live attempt at the check (PlainRunIdleAtCheck): #90's accepted limit, which
\* limits/lease-plain-run-claims-first states.
CInitResolver2x2x2 ==
  /\ Placements({"d1", "d2"}, {"p1", "p2"}, {"c1", "c2"})
  /\ LiveCheck' \in {"lease", "minAge"}
  /\ ResolverProc' \in {"p1", "p2", "none"}

\* The core safety invariants of model 1, checked together.
CoreSafety == AtMostOnce /\ NotStartedExclusive /\ NoLiveOverride /\ AtMostOncePerIntent

=============================================================================
