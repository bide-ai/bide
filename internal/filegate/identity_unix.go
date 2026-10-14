//go:build unix

package filegate

import (
	"fmt"
	"os"
	"syscall"
)

// identity is the device and inode of the file at path.
func identity(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d:%d", uint64(st.Dev), uint64(st.Ino)), true
}
