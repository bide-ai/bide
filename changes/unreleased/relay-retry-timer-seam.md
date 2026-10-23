Section: Fixed

notify/relay: the tests of the state-save retry (a transient refusal is retried on a timer and not in line, a permanent failure backs off by doubling) no longer measure wall-clock time, which Windows timer resolution broke; the relay schedules its retries through an unexported clock seam that the tests replace and assert on. The relay's behaviour does not change.
