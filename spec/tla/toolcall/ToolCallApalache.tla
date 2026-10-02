------------------------- MODULE ToolCallApalache -------------------------
(***************************************************************************)
(* Typed wrapper of ToolCall (model 9) for the Apalache symbolic model     *)
(* checker: every constant and variable with its type, and ToolCall        *)
(* instantiated unchanged. See spec/tla/README.md, "Apalache".             *)
(***************************************************************************)
EXTENDS Integers, FiniteSets

CONSTANTS
  \* @type: Int;
  NCalls,
  \* @type: Int -> Str;
  Kinds,
  \* @type: Int -> Set(Str);
  MW,
  \* @type: Bool;
  Saga,
  \* @type: Bool;
  Rewrite,
  \* @type: Bool;
  Timeout,
  \* @type: Int;
  MaxExtra,
  \* @type: Int;
  MaxErr,
  \* @type: Int;
  MaxCrash,
  \* @type: Int;
  MaxCancel,
  \* @type: Int;
  MaxDeadline,
  \* @type: Int;
  MaxGuard,
  \* @type: Int;
  MaxUnk,
  \* @type: Int;
  MaxToolErr,
  \* @type: Int;
  MaxWrongAuth,
  \* @type: Int;
  MaxAtt,
  \* @type: Set(Str);
  Bugs

VARIABLES
  \* @type: Int -> (Int -> Str);
  marker,
  \* @type: Int -> Int;
  att,
  \* @type: Int -> { k: Str, nc: Bool, ran: Bool, a: Int };
  res,
  \* @type: Int -> Bool;
  argsRec,
  \* @type: Int -> Bool;
  ibeg,
  \* @type: Int -> Bool;
  ibs,
  \* @type: Int -> Bool;
  iear,
  \* @type: Bool;
  complete,
  \* @type: Int -> Int;
  pend,
  \* @type: Bool;
  cancelled,
  \* @type: Bool;
  gcancel,
  \* @type: Str;
  auth,
  \* @type: Int -> Bool;
  go,
  \* @type: Str;
  werr,
  \* @type: Set(Str);
  held,
  \* @type: Str;
  runRet,
  \* @type: Int -> Str;
  st,
  \* @type: Int -> Str;
  bg,
  \* @type: Int -> Bool;
  ranc,
  \* @type: Int -> Str;
  tout,
  \* @type: Int -> Bool;
  closedc,
  \* @type: Int -> Bool;
  dl,
  \* @type: Int -> Bool;
  ion,
  \* @type: Int -> Str;
  iarg,
  \* @type: Int -> Str;
  iret,
  \* @type: Int -> Int;
  iatt,
  \* @type: Int -> Bool;
  iold,
  \* @type: Int -> Str;
  ost,
  \* @type: Int -> Str;
  obg,
  \* @type: Int -> Bool;
  oran,
  \* @type: Int -> Int;
  fired,
  \* @type: Int -> (Int -> Bool);
  firedAt,
  \* @type: Int -> (Int -> Bool);
  began,
  \* @type: Int -> Bool;
  everReached,
  \* @type: Int -> Set(Int);
  lostAtt,
  \* @type: Bool;
  rolled,
  \* @type: Int -> Bool;
  rbm,
  \* @type: Int -> Str;
  rbOut,
  \* @type: Int -> Bool;
  compd,
  \* @type: Bool;
  rbHalt,
  \* @type: Set(Int);
  rbUnk,
  \* @type: Int -> Bool;
  fx,
  \* @type: Bool;
  boundHit,
  \* @type: Int;
  extras,
  \* @type: Int;
  errs,
  \* @type: Int;
  crashes,
  \* @type: Int;
  cancels,
  \* @type: Int;
  deadlines,
  \* @type: Int;
  guards,
  \* @type: Int;
  unks,
  \* @type: Int;
  toolErrs,
  \* @type: Int;
  wrongs,
  \* @type: Int -> Str;
  r,
  \* @type: Int -> Int;
  slot,
  \* @type: Int -> Str;
  last,
  \* @type: Int -> Str;
  cerr,
  \* @type: Int -> Str;
  rec,
  \* @type: Int -> Bool;
  called,
  \* @type: Int -> Bool;
  made,
  \* @type: Int -> Str;
  gret,
  \* @type: Int -> Str;
  r2,
  \* @type: Int;
  gc,
  \* @type: Str;
  r3,
  \* @type: Int;
  rc,
  \* @type: Int -> Str;
  pc

INSTANCE ToolCall

\* ToolCallMC's middleware behaviors, and constants for the configurations to assign with <-.
ApContract == {"next", "ret", "maperr", "deny", "cache", "renamed"}
ApAdversarial == ApContract \cup {"retry", "async", "bare", "ctxerr"}
\* @type: Set(Str);
ApNoBugs == {}

\* The constants a configuration leaves to the solver, for one call: its kind, whether the run is
\* a saga, and whether the tool has a timeout.
CInitOne ==
  /\ MW' = [c \in {1} |-> ApAdversarial]
  /\ Kinds' \in [{1} -> {"side", "idem", "deleg"}]
  /\ Saga' \in BOOLEAN
  /\ Timeout' \in BOOLEAN

\* The core safety invariants of model 9.
\* @type: Bool;
CoreSafety == NoDoubleFire /\ TruthfulRecord /\ NoLostSibling /\ SagaAccounted /\ ArgsAfterReach

=============================================================================
