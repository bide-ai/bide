//go:build !unix

package flowedit

import "os"

// linkCount is 1: the editor cannot count a file's names here, so a hard link to the config is not
// detected (see docs/KNOWN-LIMITATIONS.md).
func linkCount(os.FileInfo) uint64 { return 1 }

// carryOwner does nothing: owners are not carried over by a save here.
func carryOwner(*os.File, os.FileInfo) {}
