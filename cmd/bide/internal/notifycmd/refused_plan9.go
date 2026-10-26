package notifycmd

// isRefused reports a refused connection. Plan 9 has no syscall.ECONNREFUSED, so it goes by the
// wording (refusedByText).
func isRefused(err error) bool { return refusedByText(err) }
