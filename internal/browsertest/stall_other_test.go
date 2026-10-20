//go:build !unix

package browsertest

import "time"

// stall waits for ever: there is no process group to test here.
func stall() { time.Sleep(time.Hour) }

// detached has nothing to test without process groups and sessions.
func detached() { time.Sleep(time.Hour) }

func terminable() { time.Sleep(time.Hour) }
