//go:build !unix

package safefile

import "os"

// Open opens path for reading. Windows has no named pipe that can sit at a file path and
// block an open, so opening first and deciding on the opened file's Stat is enough.
func Open(path string) (*os.File, error) { return os.Open(path) }

// OpenIn is Open for a name inside root, which cannot leave it.
func OpenIn(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
