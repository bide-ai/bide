------------------------------ MODULE NotifyMC ------------------------------
(* Model values and symmetry for the notification configurations. *)
EXTENDS Notify, TLC

CONSTANTS p1, p2, p3

ProcSym == Permutations(Procs)
\* A budget of -1: no budget (a .cfg cannot write a negative number).
NoBudget == -1

\* A view for the large configurations whose invariants and properties read none of the ghost
\* counters (stale, late, lateFail, failed) or the failure memory (fails, memFS, memD) and the
\* notice counter: states that differ only in those are one state, and deliveries are kept as
\* whether there was one (the vacuity checks), not how many. Used only where the configuration
\* names no invariant that reads them (DupBound, FailingBy, OverdueBounded, NoStaleDelivery,
\* ExactlyOnce, NoticeDupBound); everything that decides what a step may do stays in the view.
GhostView == <<trail, cur, lapsed, past, dpast, broken, opened, lease, inc, progressed, pc, ev, len, act,
               claim, nreq, ok, nclaim, [k \in Keys |-> deliv[k] > 0], crashes, lost, ambig, sfail, slow, boundHit>>
=============================================================================
