Section: Changed

`Step`, `Parallel` and `ResolveHalt` now refuse a step name that starts with `taint-mark:`, which the engine reserves for the taint marks of a plan flow (`agent.IsReservedStepName`).
