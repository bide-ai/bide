package agent

// SessionMuHeld reports whether s's mutex is held: the session tests check, from a compensator, that
// a cancelled saga turn's rollback runs without it. It is a test hook: TryLock fails while any
// goroutine holds the mutex, so it is meaningful only while no other caller uses s.
func SessionMuHeld(s *Session) bool {
	if s.mu.TryLock() {
		s.mu.Unlock()
		return false
	}
	return true
}
