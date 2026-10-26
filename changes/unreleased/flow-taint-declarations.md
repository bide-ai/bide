Section: Added

`plan.Untrusted`, `plan.Trusted` and `plan.TaintExempt` (node options), `Builder.WithTaintPolicy` and `plan.WithLoadedTaintPolicy`, and the `untrusted` key of a flow config node declare taint for a plan flow. The engine does not enforce flow taint yet, so `Build` and `Load` refuse a flow that declares any of it, and a tool that declares `Untrusted` or `TaintExempt`, with `ErrConfig` ("flow taint is not enforced yet"); a tool is now refused at `Build`, not at `RegisterTool` or `Builder.Tool`. Experimental.
