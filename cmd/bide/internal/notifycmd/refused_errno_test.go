//go:build !plan9

package notifycmd

import "syscall"

// errConnRefused is the system's refused-connection error (the number, where there is one).
var errConnRefused error = syscall.ECONNREFUSED

// refusedByErrno says the refused-connection rule in force is the error number (isRefused), so the tests
// of what it does not accept (an Errno 10061 on Unix, a message with only the words) apply.
const refusedByErrno = true
