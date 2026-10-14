//go:build !unix && !windows

package dbpath

import "os"

// links is not known on this platform.
func links(string) (uint64, error) { return 0, os.ErrInvalid }
