//go:build windows

package runview

import "testing"

// interruptSelf is unused on Windows: skipNoSelfInterrupt skips the tests that need it.
func interruptSelf(t *testing.T) { t.Helper() }

// skipNoSelfInterrupt skips a test that stops a serving command with SIGINT, which a Windows
// process cannot send itself.
func skipNoSelfInterrupt(t *testing.T) {
	t.Helper()
	t.Skip("Windows: a process cannot send itself SIGINT")
}
