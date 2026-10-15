Section: Fixed

The `bide sample` tests (`TestServe`, `TestLive`) waited 20 seconds for the viewer to come up, which a loaded Windows runner did not always meet, and on that failure left `bide sample` running over the test directory, so its cleanup failed with a sharing violation on `sample.db`. They now wait up to two minutes for the printed address, and every path stops the command and waits for it to return before the directory is removed; a test covers the failed start.
