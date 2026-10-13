Section: Fixed

`RecoverLoop` no longer starts a drive or a recovery pass after its context is cancelled. A select picks at random among ready cases, so a free drive slot or a pass tick could win over the done context, and the loop could lease a run and call the `Resumer` with a cancelled context during shutdown (still before `RecoverLoop` returned); it now checks the context again after each wait (`TestRecoverLoop_NothingStartsAfterCancel`).
