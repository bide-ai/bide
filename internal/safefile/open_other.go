//go:build !unix

package safefile

import "os"

// Open opens path for reading. Windows has no named pipe that can sit at a file path and
// block an open, so opening first and deciding on the opened file's Stat is enough.
func Open(path string) (*os.File, error) { return os.Open(path) }
