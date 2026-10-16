---------------------------- MODULE IdentityMC ----------------------------
(* Model values and identifier ownership for the identity configurations. *)
EXTENDS Identity, TLC

CONSTANTS p1, p2, i1, i2

\* Each identifier belongs to its own person (the bank's evidenced injectivity control).
OwnerInj == (i1 :> p1) @@ (i2 :> p2)
\* An alias: both identifiers are one person's, so one person can fill two seats.
OwnerAlias == (i1 :> p1) @@ (i2 :> p1)
=============================================================================
