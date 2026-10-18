------------------------------- MODULE StopMC -------------------------------
(* Model values and the policy tables for the stop configurations. Two stops: s1 covers call c1 and
   s2 covers c2. Policy 1 enrols setter a and lifters a, b (quorum 2); policy 2 offboards a and enrols
   setter b and lifters b, c. The forger holds no enrolled key. *)
EXTENDS Stop, TLC

CONSTANTS a, b, c, eve, c1, c2, s1, s2

TargetOfDef == (s1 :> "x1") @@ (s2 :> "x2")
SameTargetDef == (s1 :> "x") @@ (s2 :> "x")
CompOfDef == (s1 :> "allow") @@ (s2 :> "allow")
StricterCompDef == (s1 :> "allow") @@ (s2 :> "block")
SameCoverDef == (s1 :> {c1}) @@ (s2 :> {c1})
CoverOfDef == (s1 :> {c1}) @@ (s2 :> {c2})
SettersOfDef == (0 :> {eve}) @@ (1 :> {a}) @@ (2 :> {b})
LiftersOfDef == (0 :> {eve}) @@ (1 :> {a, b}) @@ (2 :> {b, c})
QuorumOfDef == (0 :> 1) @@ (1 :> 2) @@ (2 :> 2)
QuorumOfOneDef == (0 :> 1) @@ (1 :> 1) @@ (2 :> 1)
=============================================================================
