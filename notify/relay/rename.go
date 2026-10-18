package relay

import "time"

// renameRetryBudget is how long a save keeps retrying a rename the operating system refused for a reason that
// passes (see retryableRename): a few hundred milliseconds in all, then the save fails and is retried later.
const renameRetryBudget = 300 * time.Millisecond

// renameWithRetry runs rename(from, to) and, while it fails with an error retryable accepts, runs it again after
// a doubling pause (5 ms to start), until the pauses add up to renameRetryBudget. It returns the last error.
// sleep stands in for time.Sleep so a test does not wait.
func renameWithRetry(rename func(from, to string) error, retryable func(error) bool, sleep func(time.Duration), from, to string) error {
	wait, waited := 5*time.Millisecond, time.Duration(0)
	for {
		err := rename(from, to)
		if err == nil || !retryable(err) || waited >= renameRetryBudget {
			return err
		}
		d := min(wait, renameRetryBudget-waited)
		sleep(d)
		waited += d
		wait *= 2
	}
}
