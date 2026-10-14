Section: Fixed

`audit.VerifyApprovals` and `audit.ApprovalEvidence` honor a re-approval tally (`approval-tally:<id>:v3`) only over a tally that predates `agent.DecisionFormat`, as `agent.RecordedTally` does at runtime, so a re-tally injected beside a current-format denial no longer replaces it.
