Section: Fixed

The store/sqlite write-fairness tests no longer fail on a heavily loaded machine: they start every writer queued on the file's gate and read the commit order from the file, instead of timing goroutines. Test change only; the internal write gate gains a `Waiting` count that the tests use, and its behavior is unchanged.
