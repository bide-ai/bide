//go:build !unix

package runview

import "os"

// openConfig opens path for reading. Windows has no named pipe that can sit at a file path and
// block an open, so opening first and deciding on the opened file's Stat is enough.
func openConfig(path string) (*os.File, error) { return os.Open(path) }
