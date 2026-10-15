Section: Changed

The run viewer guide's expired-gate screenshot now shows the approvers and the timeline of a gate that expired while a second approval, stamped before the deadline, was written after the expiry, which the page labels "not counted: written after the gate expired"; `check -diff -check` has a light screenshot beside the dark one, embedded in the run viewer and plan flows guides with `<picture>`. `assets/demo/cli.sh` takes `THEME=light` and `ONLY=<shot>` and writes optimized PNGs, and `assets/demo/setup/deadline.go` writes the late-written decision.
