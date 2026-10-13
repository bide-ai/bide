Section: Added

A run made of `Journal.Step` calls alone has no `run:start`; `bide view` listed it as "not started" even when it had halted and been resolved. It is now labeled "steps only (no run:start)" in the run list and on the run page, with a state read from the journal: halted (a live attempt marker with no result), resolved (`agent.ResolveHalt` recorded an outcome) or steps done. A journal holding only its header is still not started. The run list's kind for such a run is `steps` in `/runs.json` and `/run.json`.
