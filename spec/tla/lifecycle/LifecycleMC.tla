----------------------------- MODULE LifecycleMC -----------------------------
(* Named worker sets and holder names, which the configurations select with <-. *)
EXTENDS Lifecycle

W1 == {"w1"}
W2 == {"w1", "w2"}
\* Every worker under a name of its own, and the primary under another.
NameOwn == [w \in {"w1", "w2"} |-> w]
\* #58 finding 4: a worker's primary and its recoverer, or a restarted worker and its stalled
\* predecessor, under one WithLeaseHolder name.
NameShared == [w \in {"w1", "w2"} |-> "w1"]
=============================================================================
