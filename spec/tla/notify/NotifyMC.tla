------------------------------ MODULE NotifyMC ------------------------------
(* Model values and symmetry for the notification configurations. *)
EXTENDS Notify, TLC

CONSTANTS p1, p2, p3

ProcSym == Permutations(Procs)
\* A budget of -1: no budget (a .cfg cannot write a negative number).
NoBudget == -1
=============================================================================
