package crlrefresh

import "time"

// SetAttemptTimeout shortens the per-attempt deadline of every feed, so a test of a slow server does not
// wait the real two minutes.
func SetAttemptTimeout(r *Refresher, d time.Duration) {
	for _, f := range r.feeds() {
		f.attemptTimeout = d
	}
}
