------------------------------ MODULE ClaimsMC ------------------------------
(* Named assignments for the configurations: each .cfg picks its placement of drivers,  *)
(* calls and processes from these with <-, and its symmetry set.                         *)
EXTENDS Claims

CONSTANTS d1, d2, d3, p1, p2, c1, c2, a1, a2, a3, h1, h2, h3, hx, k1, k2, k3

\* Placements of two drivers: one process, or one process each.
SameProc  == [d \in Drivers |-> p1]
CrossProc == (d1 :> p1) @@ (d2 :> p2)
\* Three drivers: two in p1, one in p2.
ThreeProc == (d1 :> p1) @@ (d2 :> p1) @@ (d3 :> p2)

\* One call, driven by every driver.
OneCall   == [d \in Drivers |-> c1]
\* Two calls of one intent: d1 drives c1, d2 drives c2, which the caller issues when c1 fails.
TwoCalls  == (d1 :> c1) @@ (d2 :> c2)

AllStep   == [c \in Calls |-> "step"]
AllTool   == [c \in Calls |-> "tool"]
AllFlow   == [c \in Calls |-> "flow"]
NoNext    == [c \in Calls |-> None]
C1ThenC2  == (c1 :> c2) @@ (c2 :> None)

\* Symmetry: drivers of one process on one call are interchangeable (DriverSym), and so are the
\* two drivers of p1 in ThreeProc (PairSym).
DriverSym == Permutations(Drivers)
PairSym   == Permutations({d1, d2})

\* The approval gate (model 1b).
NoGate    == [c \in Calls |-> "none"]
GateOne   == [c \in Calls |-> "one"]
GateM     == [c \in Calls |-> "m"]
\* m: 2 of {a1, a2, a3}; tight: 3 of them; loose: 1 of them; fold: 2 of {a1, a2, a3} where a3 is
\* a1 spelled differently (FoldSame).
Pols      == ("m" :> [need |-> 2, apprs |-> {a1, a2, a3}])
             @@ ("tight" :> [need |-> 3, apprs |-> {a1, a2, a3}])
             @@ ("loose" :> [need |-> 1, apprs |-> {a1, a2, a3}])
NoKeys    == [x \in {} |-> x]
Keys      == (a1 :> k1) @@ (a2 :> k2) @@ (a3 :> k3)
\* a2's verifier resolves to a1's key (two approvers sharing one key).
KeysShared == (a1 :> k1) @@ (a2 :> k1) @@ (a3 :> k3)
Holders   == (k1 :> h1) @@ (k2 :> h2) @@ (k3 :> h3)
\* a3 is a1 spelled differently, and a1's person holds a3's key.
HoldersFold == (k1 :> h1) @@ (k2 :> h2) @@ (k3 :> h1)
FoldA1A3  == {<<a1, a3>>}
=============================================================================
