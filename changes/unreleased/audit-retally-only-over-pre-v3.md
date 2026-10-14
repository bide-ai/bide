Section: Fixed

`audit.VerifyApprovals` and `audit.ApprovalEvidence` honor a re-approval tally (`approval-tally:<id>:v3`) only over a tally that predates `agent.DecisionFormat`, as `agent.RecordedTally` does at runtime, so a re-tally injected beside a current-format denial no longer replaces it. `audit.ApprovalEvidence` now includes the superseded tally beside a re-approval tally, and `VerifyApprovals` refuses a re-approval tally without it.
