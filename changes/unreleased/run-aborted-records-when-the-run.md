Section: Changed

`run:aborted` records when a saga's finished rollback ended the run: its value is now an object with the reason (`reason`, as before) and `aborted_at` (UTC, RFC 3339 with milliseconds, by the drive's clock), where it was the reason as a JSON string. The time is informational, like `started_at`: no drive, resume or recovery pass reads it, `agent.Status` reads the reason from either form, and a `run:aborted` an earlier version wrote loads and behaves the same. The notifier's catch-up sweep uses it to bound which aborted runs it raises.
