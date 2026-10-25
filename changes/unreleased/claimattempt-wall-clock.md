Section: Fixed

`ClaimAttempt` stamps a marker's `AttemptedAt` from the wall clock again. v0.23.1 stamped it from the clock bound to the context (`WithClock`), so under a run clock that lagged the wall clock a marker claimed this instant looked old: `ResolveHalt` with `WithMinHaltAge` resolved it at once, and the notifier's sweep reported it as a halt before `WithNotifyHaltAge`, defeating the grace period they enforce. The engine's own claims for a tool call and a `Journal.Step` were always stamped from the wall clock. Without `WithClock` nothing changes.
