Section: Added

`agent.EvaluateGate` says where an approval gate stands on a call (waiting, decided, single, ungated, or misconfigured, with the tally, the classification of each decision and why a gate asks again), reading the journal, the live policy and the approver verifiers exactly as the agent's own gate does: the gate's counting is now one function (`countGate`) used by both, and `agent.GateReask` is the rule for a recorded tally the gate supersedes. `bide view` uses `GateReask`. No behavior of the gate changes.
