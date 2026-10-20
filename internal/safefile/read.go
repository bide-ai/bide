package safefile

import (
	"fmt"
	"io"
	"math"
	"os"
)

// Kind says why a file was refused.
type Kind int

const (
	// NotRegular: the path names a directory, a named pipe, a device or anything else that is not a
	// regular file.
	NotRegular Kind = iota
	// Changed: the path named a regular file when it was checked and names something else once opened.
	Changed
	// TooLarge: the file holds more than the limit.
	TooLarge
)

// RefusedError is a file Read would not read, as opposed to one it could not (a missing file, a
// permission error): those come back as the operating system's error.
type RefusedError struct {
	Path string
	Kind Kind
	Max  int
}

// Error says which file was refused and why, in one line.
func (e *RefusedError) Error() string {
	switch e.Kind {
	case Changed:
		return fmt.Sprintf("%s is not a regular file (it changed while it was opened)", e.Path)
	case TooLarge:
		return fmt.Sprintf("%s: larger than %d bytes", e.Path, e.Max)
	}
	return fmt.Sprintf("%s is not a regular file", e.Path)
}

// Read returns the bytes of the regular file at path, at most max of them. The path is checked with
// Stat first, which refuses a directory, a named pipe or a device without opening it; afterStat, when
// not nil, runs between that check and the open (a test swaps the path there). The file is then
// opened without waiting for a writer and the decision is made again on the opened file's own Stat,
// never by comparing it with the first one: os.SameFile cannot say, on Windows, that the path was
// swapped, because a FileInfo from os.Stat reads its file ID from the path when SameFile first asks,
// which is after any swap. At most one byte past max is read, so a huge file is refused without being
// read.
//
// On unix a path in the exact form of an open descriptor (/dev/fd/N, /proc/self/fd/N on Linux), which
// is what a process substitution (<(...)) expands to, is read as the stream it is, without the
// regular-file check: the user chose that descriptor, and a named pipe someone else placed at a path
// cannot take that form. /dev/stdin and descriptor 0 are standard input, owned as "-" is (see
// ReadOrStdin). The form is matched on the path as written, never after resolving links, so a
// symbolic link to /dev/fd/3 is a link to a pipe and is refused.
func Read(path string, max int, afterStat func()) ([]byte, error) {
	if isStdinPath(path) {
		return ReadOrStdin(path, "-", max)
	}
	if isDescriptorPath(path) {
		return readStream(path, max)
	}
	f, err := OpenRegular(path, afterStat)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readLimited(f, path, max)
}

// OpenRegular opens the regular file at path for reading and returns it: Stat first, which refuses
// a directory, a named pipe or a device without opening it; afterStat, when not nil, runs between
// that check and the open; the open does not wait for a writer, and the decision is made again on
// the opened file's own Stat. The caller closes the file and reads it as far as it needs to (a log
// read record by record has no total size to bound).
func OpenRegular(path string, afterStat func()) (*os.File, error) {
	before, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, &RefusedError{Path: path, Kind: NotRegular}
	}
	if afterStat != nil {
		afterStat()
	}
	f, err := Open(path)
	if err != nil {
		return nil, err
	}
	if after, err := f.Stat(); err != nil || !after.Mode().IsRegular() {
		f.Close()
		return nil, &RefusedError{Path: path, Kind: Changed}
	}
	return f, nil
}

// readStream opens the descriptor path and reads at most max bytes of it.
func readStream(path string, max int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readLimited(f, path, max)
}

// readLimited reads at most max bytes of f, refusing a longer one after reading one byte past max.
func readLimited(f *os.File, path string, max int) ([]byte, error) {
	// One byte past max proves the file is over it; a max at the top of the range cannot be passed.
	limit := int64(max)
	if limit < math.MaxInt64 {
		limit++
	}
	b, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, err
	}
	if len(b) > max {
		return nil, &RefusedError{Path: path, Kind: TooLarge, Max: max}
	}
	return b, nil
}

// ReadFile is os.ReadFile for a regular file with no size limit: for files bide manages itself,
// found by listing a directory, where only a swap for a named pipe between the listing and the open
// is to be refused. Anything but a regular file is a RefusedError.
func ReadFile(path string) ([]byte, error) {
	f, err := OpenRegular(path, nil)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
