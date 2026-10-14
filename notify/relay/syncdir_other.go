//go:build !unix

package relay

// syncDir does nothing here. Windows cannot fsync a directory (opening one for that needs
// FILE_FLAG_BACKUP_SEMANTICS and Sync on it fails); NTFS journals the rename's metadata, and
// os.Rename replaces the file with MoveFileEx, which the file system makes durable on its own
// schedule. The file's contents are still synced before the rename.
func syncDir(string) error { return nil }
