//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package flowedit

// haveFIFO says whether this system can make a named pipe (syscall.Mkfifo); the tests that need one skip
// when it cannot.
const haveFIFO = false

func mkfifo(string) error { return nil }
