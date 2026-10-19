Section: Fixed

`plan.InspectConfig` with a Registry no longer reports "Model step has no bound model" as an error for a config with a Model block (since v0.14.0, which added it). A check never runs the flow, so it does not need the model that `Load` binds. The programs affected are those that check a config against their own Registry: `planview.Handler`, `planview.Check`, `planview.DiffHandler`, `planview.CheckDiff` and direct `InspectConfig` callers; they showed such a config as broken, with no digest. `bide check` and `bide view -config` pass no Registry and were not affected.
