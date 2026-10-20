//go:build !unix

package browsertest

import "time"

// stall waits for ever: there is no process group to test here.
func stall() { time.Sleep(time.Hour) }
