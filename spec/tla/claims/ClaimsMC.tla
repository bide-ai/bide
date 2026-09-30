------------------------------ MODULE ClaimsMC ------------------------------
(* Named assignments for the configurations: each .cfg picks its placement of drivers,  *)
(* calls and processes from these with <-, and its symmetry set.                         *)
EXTENDS Claims

CONSTANTS d1, d2, d3, p1, p2, c1, c2

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
NoNext    == [c \in Calls |-> None]
C1ThenC2  == (c1 :> c2) @@ (c2 :> None)

\* Symmetry: drivers of one process on one call are interchangeable (DriverSym), and so are the
\* two drivers of p1 in ThreeProc (PairSym).
DriverSym == Permutations(Drivers)
PairSym   == Permutations({d1, d2})
=============================================================================
