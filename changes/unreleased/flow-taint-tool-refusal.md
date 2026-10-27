Section: Changed

A plan flow tool that declares `Untrusted` or `TaintExempt` is now refused with `ErrConfig` when the flow is built (`Build`, `Load`), not at `RegisterTool` or `Builder.Tool`; flows still do not enforce taint.
