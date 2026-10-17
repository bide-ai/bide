//go:build unix

package crlrefresh

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// checkArchiveDirOwner refuses an archive directory that others can write to or that another user owns:
// whoever can write there can rewrite the refresher's rollback memory.
func checkArchiveDirOwner(fi fs.FileInfo) error {
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("mode %v lets group or others write", fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("owned by uid %d, not the process user", st.Uid)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func isChmodUnsupported(err error) bool { return errors.Is(err, errors.ErrUnsupported) }
