//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package flowedit

import "syscall"

// haveFIFO says whether this system can make a named pipe (syscall.Mkfifo); the tests that need one skip
// when it cannot.
const haveFIFO = true

func mkfifo(p string) error { return syscall.Mkfifo(p, 0o600) }
