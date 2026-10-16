//go:build unix

package flowedit

import (
	"os"
	"syscall"
)

// linkCount is the number of names the file has (1 when it cannot be told).
func linkCount(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}

// carryOwner gives tmp the owner and group of the file it replaces, as far as the user may: when it
// may not (not root, not in the group) the new file keeps the user's own, which is the same file's
// owner in the usual case, and the failure is not one of the save.
func carryOwner(tmp *os.File, fi os.FileInfo) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = tmp.Chown(int(st.Uid), int(st.Gid))
	}
}

// ownedByUs reports the file is owned by the user running the editor.
func ownedByUs(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
