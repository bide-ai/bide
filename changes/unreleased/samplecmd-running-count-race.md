Section: Fixed

`TestStartFailureStopsTheCommand` (the `bide sample` test helper) could fail on a loaded runner with "1 bide sample command(s) still running after the failed start": its running count was decremented by a deferred call that runs after the exit status is sent, so the test could read the count in between. The count now drops before the status is sent. Test code only.
