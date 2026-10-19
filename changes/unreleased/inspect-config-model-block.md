Section: Fixed

`plan.InspectConfig`, and so `planview.Handler`, `planview.Check` and `bide check` with a Registry, no longer report "Model step has no bound model" as an error for a config with a Model block. A check never runs the flow, so it does not need the model that `Load` binds; the config is now typed-clean and has its digest.
