//go:build unix

package dbpath

import (
	"os"
	"syscall"
)

// links is the number of hard links to the file at path.
func links(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, os.ErrInvalid
	}
	return uint64(st.Nlink), nil
}
