//go:build !unix

package runview

import "os"

func openNoWait(path string) (*os.File, error) { return os.Open(path) }
