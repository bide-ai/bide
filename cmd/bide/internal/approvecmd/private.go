package approvecmd

import (
	"fmt"
	"os"
	"runtime"
)

// PrivateFileError is a refusal to read a file that holds a secret: it is not a regular file, group
// or others can read or write it, or another user owns it. Its message names the file.
type PrivateFileError struct{ Msg string }

// Error is the refusal, naming the file.
func (e *PrivateFileError) Error() string { return e.Msg }

// OpenPrivate opens the file at path, which holds a secret (an approver key, a PIN), for reading,
// as ssh opens a private key: it must be a regular file, on Unix owned by the current user and not
// accessible to group or others. The checks are made on the open file, so what is checked is what is
// read, and the open does not block on a named pipe (O_NONBLOCK, then the regular-file check on the
// descriptor), which closes the gap between looking at a path and opening it. what names the file
// in messages ("key file", "PIN file"). An error that is not a *PrivateFileError is the open's own.
func OpenPrivate(path, what string) (*os.File, error) {
	f, err := openNonblocking(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	refuse := func(format string, args ...any) (*os.File, error) {
		f.Close()
		return nil, &PrivateFileError{fmt.Sprintf(format, args...)}
	}
	if !fi.Mode().IsRegular() {
		return refuse("%s %s is not a regular file", what, path)
	}
	if runtime.GOOS != "windows" {
		if uid, ok := owner(fi); ok && uid != os.Geteuid() {
			return refuse("%s %s is owned by another user (uid %d, you are %d); a private file must be yours", what, path, uid, os.Geteuid())
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return refuse("%s %s is accessible by others (mode %04o); a private file must not be: run chmod 600 %s", what, path, fi.Mode().Perm(), path)
		}
	}
	return f, nil
}
