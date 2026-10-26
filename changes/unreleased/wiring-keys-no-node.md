Section: Fixed

A flow config that sets `safety` or `approval` on an `edge` or `switch` wiring element is now refused with `ErrConfig` naming `wiring[i]` (by `Load`, `Validate` and `InspectConfig`). The key has no node to apply to there and was accepted and dropped, so a config could look gated and not be.
