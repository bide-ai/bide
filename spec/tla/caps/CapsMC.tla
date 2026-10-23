------------------------------- MODULE CapsMC -------------------------------
(* Model values and the tables for the cap ledger configurations. Drivers d1..d3 work on attempts
   a1..a3: DiffAtt gives each its own, SameAtt makes d1 and d2 two drivers of one attempt (a retry, a
   second driver, a middleware's second next), Mixed shares a1 between two of three. Amounts are in
   minor units; the limit of a configuration is chosen against them. *)
EXTENDS Caps, TLC

CONSTANTS d1, d2, d3, a1, a2, a3, l1, l2, l3

NoLoop == [x \in {} |-> a1]
LoopA1 == (l1 :> a1)
LoopA1A2 == (l1 :> a1) @@ (l2 :> a2)
LoopA1A2A3 == (l1 :> a1) @@ (l2 :> a2) @@ (l3 :> a3)

DiffAtt == (d1 :> a1) @@ (d2 :> a2) @@ (d3 :> a3)
SameAtt == (d1 :> a1) @@ (d2 :> a1) @@ (d3 :> a1)
MixedAtt == (d1 :> a1) @@ (d2 :> a1) @@ (d3 :> a2)
MixedAtt2 == (d1 :> a1) @@ (d2 :> a2) @@ (d3 :> a2)

AmtOne == (a1 :> 1) @@ (a2 :> 1) @@ (a3 :> 1)
AmtTwo == (a1 :> 2) @@ (a2 :> 2) @@ (a3 :> 2)
AmtMix == (a1 :> 1) @@ (a2 :> 2) @@ (a3 :> 2)

ClockSame == (d1 :> 0) @@ (d2 :> 0) @@ (d3 :> 0)
ClockSkew == (d1 :> 0) @@ (d2 :> 1) @@ (d3 :> 0)
ClockFar == (d1 :> 0) @@ (d2 :> 2) @@ (d3 :> 0)
=============================================================================
