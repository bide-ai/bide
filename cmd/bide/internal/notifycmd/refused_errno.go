//go:build !plan9

package notifycmd

import (
	"errors"
	"syscall"
)

// isRefused reports a refused connection by its error number, not by how the operating system
// words it ("connection refused" on Unix, "actively refused it" on Windows). Windows reports
// WSAECONNREFUSED (10061), which is not syscall.ECONNREFUSED there, so both are checked; 10061 is
// no error number on Unix, where nothing matches it.
func isRefused(err error) bool {
	return err != nil && (errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.Errno(10061)))
}
