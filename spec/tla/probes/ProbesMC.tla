------------------------------ MODULE ProbesMC ------------------------------
(* Model values and symmetry for the probe configurations. *)
EXTENDS Probes, TLC

CONSTANTS d1, d2, d3

DriverSym == Permutations(Drivers)
=============================================================================
