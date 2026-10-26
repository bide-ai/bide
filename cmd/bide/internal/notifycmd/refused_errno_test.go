//go:build !plan9

package notifycmd

import "syscall"

// errConnRefused is the system's refused-connection error (the number, where there is one).
var errConnRefused error = syscall.ECONNREFUSED
