Section: Added

`Flow.Run` takes `agent.WithUntrustedInput` and `agent.WithTrustedInput` (a flow run refused them with `ErrConfig` before): the source or reason is recorded in `run:start`'s settings, and a later drive that states another source is `ErrConfig`. They have no other effect yet. `plan.ErrTaintRefused` exists, but no flow can return it yet: `Build` refuses a flow with a taint policy until flow taint is enforced. Experimental.
