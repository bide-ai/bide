//go:build unix

package relay

import "os"

// syncDir flushes a directory's entries to disk, so a rename into it survives a power loss: the
// file's own fsync makes its contents durable, not the name that points at them.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
