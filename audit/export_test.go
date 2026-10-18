package audit

import "time"

// SetFinalityNeedsLC switches the status rule between the user's decision (false) and the design
// note's earlier literal reading (true) for a test, and returns the function that restores it.
func SetFinalityNeedsLC(v bool) func() {
	old := finalityNeedsLC
	finalityNeedsLC = v
	return func() { finalityNeedsLC = old }
}

// SetClock gives an AuditedStore a manual clock and timers for a test of WithMinInterval.
func (a *AuditedStore) SetClock(wall func() time.Time, after func(time.Duration, func()) func() bool) {
	a.wall, a.after = wall, after
}

// SetClock gives an AuditedEventStore a manual clock and timers for a test of WithMinInterval.
func (a *AuditedEventStore) SetClock(wall func() time.Time, after func(time.Duration, func()) func() bool) {
	a.wall, a.after = wall, after
}

// HeldBack is the trails whose growth WithMinInterval is holding back.
func (a *AuditedEventStore) HeldBack() []string { return a.heldBack() }

// RetryDelay is retryDelay.
func RetryDelay(interval time.Duration, n int) time.Duration { return retryDelay(interval, n) }
