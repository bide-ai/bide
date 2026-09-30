package agent

// SetExploreFlightHooks installs the two hooks the concurrent claim exploration
// (claim_explore_conc_test.go) schedules around, and returns a function that restores the hooks
// it replaced.
//
// join is called by a caller that joins another caller's step call in flight, before it waits,
// with a function that reports whether the joined call has returned (its flight is gone from the
// process's flights). decode is called at every record decode; the owner of a call in flight
// decodes the call's outcome right after the call returns, before it does anything else.
func SetExploreFlightHooks(join func(returned func() bool), decode func()) (restore func()) {
	jh := func(k flightKey) {
		flights.mu.Lock()
		f := flights.m[k]
		flights.mu.Unlock()
		returned := func() bool {
			if f == nil {
				return true
			}
			flights.mu.Lock()
			defer flights.mu.Unlock()
			return flights.m[k] != f
		}
		join(returned)
	}
	dh := func([]byte) { decode() }
	oldJ := flightJoinHook.Swap(&jh)
	oldD := decodeHook.Swap(&dh)
	return func() {
		flightJoinHook.Store(oldJ)
		decodeHook.Store(oldD)
	}
}
