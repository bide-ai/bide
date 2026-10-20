//go:build !unix

package safefiletest

import "testing"

// FIFO skips the test: no named pipe can sit at a file path here, so there is nothing to wait on.
func FIFO(t *testing.T) string {
	t.Helper()
	t.Skip("no named pipe can sit at a file path on this system")
	return ""
}

// Descriptor skips the test: there is no /dev/fd here.
func Descriptor(t *testing.T, content string) string {
	t.Helper()
	t.Skip("no /dev/fd on this system")
	return ""
}
