//go:build !unix

package themeaccess

import "os"

// openNoWait opens path for reading. Windows has no named pipe that can sit at a file path and
// block an open, so opening first and deciding on the opened file's Stat is enough.
func openNoWait(path string) (*os.File, error) { return os.Open(path) }
