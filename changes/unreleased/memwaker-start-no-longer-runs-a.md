Section: Fixed

`MemWaker.Start` no longer runs a `Fire` after its context is cancelled. A select picks at random among ready cases, so a ready tick could win over the done context, and the loop would resume due runs with a cancelled context during shutdown (still before the stopped channel closed); it now checks the context again after each tick (`TestMemWaker_StartNothingFiresAfterCancel`).
