//go:build !unix

package audit

import (
	"errors"
	"io/fs"
)

// checkArchiveDirOwner does nothing off Unix: permissions there are ACLs, which the operator sets.
func checkArchiveDirOwner(fs.FileInfo) error { return nil }

// syncDir is a no-op: Windows cannot sync a directory handle, and NTFS journals the rename.
func syncDir(string) error { return nil }

func isChmodUnsupported(err error) bool { return errors.Is(err, errors.ErrUnsupported) }
