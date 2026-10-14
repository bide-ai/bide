//go:build !windows

package demo

import (
	"os"
	"syscall"
	"testing"
)

// interruptSelf sends this process SIGINT, the way Ctrl-C stops a serving command.
func interruptSelf(t *testing.T) { t.Helper(); _ = syscall.Kill(os.Getpid(), syscall.SIGINT) }

// skipNoSelfInterrupt is a no-op where a process can signal itself.
func skipNoSelfInterrupt(t *testing.T) { t.Helper() }
