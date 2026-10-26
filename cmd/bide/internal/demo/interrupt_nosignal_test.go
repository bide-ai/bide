//go:build windows || plan9

package demo

import "testing"

// interruptSelf is unused on Windows and Plan 9: skipNoSelfInterrupt skips the tests that need it.
func interruptSelf(t *testing.T) { t.Helper() }

// skipNoSelfInterrupt skips a test that stops a serving command with SIGINT, which a Windows
// process cannot send itself, nor a Plan 9 one (it has no syscall.Kill).
func skipNoSelfInterrupt(t *testing.T) {
	t.Helper()
	t.Skip("Windows and Plan 9: a process cannot send itself SIGINT")
}
