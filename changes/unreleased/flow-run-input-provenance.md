Section: Added

`Flow.Run` takes `agent.WithUntrustedInput` and `agent.WithTrustedInput` (a flow run refused them with `ErrConfig` before): the source or reason is recorded in `run:start`'s settings, a later drive that states another source is `ErrConfig`, and an untrusted source is the first mark of a run under a flow taint policy. Experimental.
