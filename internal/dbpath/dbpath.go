// Package dbpath names a SQLite database file the one way every handle on it will agree on.
//
// SQLite coordinates the connections to a WAL-mode database through files whose names it derives
// from the path it was given (the -wal and -shm files, and on Windows the lock identity). Two
// handles that reach one file by different paths (a symlink, another spelling, a relative path, a
// different case on a case-insensitive file system, a hard link) can derive different names, each
// then believes it owns the WAL, and the database is corrupted: https://sqlite.org/howtocorrupt.html
// ("Multiple links to the same file", "Symlinks"). Canonical gives the path with symlinks resolved
// and made absolute, which removes every alias but a hard link; a hard link has no canonical name,
// so Canonical refuses a file with more than one.
package dbpath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrHardLinked is wrapped by the error Canonical returns for a file with more than one hard link.
var ErrHardLinked = errors.New("dbpath: the database file has more than one hard link")

// IsMemory reports whether path names an in-memory or temporary database, which has no file.
func IsMemory(path string) bool {
	return path == "" || path == ":memory:" || strings.Contains(path, "mode=memory")
}

// Canonical returns path with symlinks resolved and made absolute, keeping any "?query" and
// "file:" prefix; an in-memory path is returned as is. A file that does not exist is created
// empty first when create is true (SQLite takes an empty file for a new database, and the file
// needs to exist to be resolved), and left as the cleaned absolute path otherwise. It fails with
// an error wrapping ErrHardLinked for a file with more than one hard link.
func Canonical(path string, create bool) (string, error) {
	if IsMemory(path) {
		return path, nil
	}
	file, query, hasQuery := strings.Cut(path, "?")
	prefix := ""
	if strings.HasPrefix(file, "file:") && !strings.HasPrefix(file, "file://") {
		prefix, file = "file:", strings.TrimPrefix(file, "file:")
	}
	if strings.HasPrefix(file, "file:") {
		return path, nil // a URI with an authority: not a plain path, left to SQLite
	}
	abs, err := filepath.Abs(file)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) && create {
		// A file that cannot be created (its directory is missing, say) is left for SQLite to report,
		// which says what to check.
		if f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0o644); err == nil {
			f.Close()
		}
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	} else if dir, derr := filepath.EvalSymlinks(filepath.Dir(abs)); derr == nil {
		abs = filepath.Join(dir, filepath.Base(abs))
	}
	if err := CheckLinks(abs); err != nil {
		return "", err
	}
	out := prefix + abs
	if hasQuery {
		out += "?" + query
	}
	return out, nil
}

// CheckLinks returns an error wrapping ErrHardLinked if the file at path has more than one hard
// link. A file whose links cannot be counted (it does not exist, or the platform cannot say) passes.
func CheckLinks(path string) error {
	if n, err := links(path); err == nil && n > 1 {
		return fmt.Errorf("%w (%d links to %s): SQLite cannot tell the links apart, so two handles on them would each own the WAL and corrupt the database; open the one file, not a hard link to it, and remove the extra links (see https://sqlite.org/howtocorrupt.html)", ErrHardLinked, n, path)
	}
	return nil
}
