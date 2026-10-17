package audit

import "time"

// SetFinalityNeedsLC switches the status rule between the user's decision (false) and the design
// note's earlier literal reading (true) for a test, and returns the function that restores it.
func SetFinalityNeedsLC(v bool) func() {
	old := finalityNeedsLC
	finalityNeedsLC = v
	return func() { finalityNeedsLC = old }
}

// SetAttemptTimeout shortens the per-attempt deadline of every feed, so a test of a slow server does not
// wait the real two minutes.
func SetAttemptTimeout(r *CRLRefresher, d time.Duration) {
	for _, f := range r.feeds() {
		f.attemptTimeout = d
	}
}

// DropAllCRLs empties the snapshot without touching its eviction bookkeeping, to stage a snapshot that
// lacks a CRL the refresher's mark already covers.
func DropAllCRLs(s *StatusSnapshot) {
	s.mu.Lock()
	hs := make([][32]byte, 0, len(s.views))
	for _, v := range s.views {
		hs = append(hs, v.hash)
	}
	s.mu.Unlock()
	for _, h := range hs {
		s.remove(h)
	}
}
