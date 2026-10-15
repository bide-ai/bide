Section: Fixed

The approve page's DOM tests (approvals/page/test) no longer sleep a fixed 200 ms for the page to load the call: they wait until the page shows the call or says why not. On a slow Windows runner the fixed sleep ended before the page had rendered, which failed the check that every argument is on display once in CI; the page itself was not at fault and ships unchanged.
