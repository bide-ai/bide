//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package runview

import "testing"

func makeFIFO(t *testing.T, dir string) (string, bool) { return "", false }
