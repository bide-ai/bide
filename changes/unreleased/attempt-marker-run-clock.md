Section: Added

Every attempt marker the engine, `ClaimAttempt` or `ResolveHalt` (for the next attempt) claims now carries `recorded_at`, the instant of the claim on the clock the run was driven with (`WithClock`, else the wall clock), readable as `Record.RecordedAt()`. The marker's `AttemptedAt` stays the wall clock, which the age of a halt is measured on, so a run clock that lags or leads does not move it. A caller cannot set it. `bide audit caps` dates a call's cap window by it. Markers written before keep their form.
