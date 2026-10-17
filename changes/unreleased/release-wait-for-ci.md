Section: Changed

`scripts/release.sh --push` now waits for main's CI instead of failing at once when the release commit has no completed push run yet (right after an admin merge the run has often not started): it polls every 30 seconds (`BIDE_CI_PREFLIGHT_POLL_SECS`) for up to 45 minutes (`BIDE_CI_PREFLIGHT_WAIT_SECS`) with a progress line, stops at once on a failure, and still accepts a passed merge queue run of the same tree. A dry run asks once. The self-test covers waiting, failure, the time limit and the dry run.
