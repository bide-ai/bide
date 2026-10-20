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
func Read(path string, max int, afterStat func()) ([]byte, error) {
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
	defer f.Close()
	if after, err := f.Stat(); err != nil || !after.Mode().IsRegular() {
		return nil, &RefusedError{Path: path, Kind: Changed}
	}
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
