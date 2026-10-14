//go:build !unix

package approvecmd

import "os"

func openNonblocking(path string) (*os.File, error) { return os.Open(path) }

func owner(os.FileInfo) (int, bool) { return 0, false }
