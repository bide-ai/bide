// Package safefiletest has the helpers the tests of readers built on safefile share: a named pipe
// to point a reader at, and a deadline that turns a reader waiting for a writer into a failure.
package safefiletest

import (
	"testing"
	"time"
)

// Deadline runs f and fails the test if it has not returned in 5 s, which is how a read of a named
// pipe shows: it waits for a writer that never comes. what names the reader in the failure.
func Deadline(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked on a named pipe", what)
	}
}
