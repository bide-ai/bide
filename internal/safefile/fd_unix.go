//go:build unix

package safefile

import (
	"regexp"
	"runtime"
)

var (
	devFD  = regexp.MustCompile(`^/dev/fd/[0-9]+$`)
	procFD = regexp.MustCompile(`^/proc/self/fd/[0-9]+$`)
)

// isDescriptorPath reports whether path, as written, names an open descriptor of this process:
// /dev/fd/N, or /proc/self/fd/N on Linux. Descriptor 0 is standard input and is isStdinPath.
func isDescriptorPath(path string) bool {
	if isStdinPath(path) {
		return false
	}
	return devFD.MatchString(path) || (runtime.GOOS == "linux" && procFD.MatchString(path))
}

// isStdinPath reports whether path, as written, is standard input: /dev/stdin, or descriptor 0.
func isStdinPath(path string) bool {
	return path == "/dev/stdin" || path == "/dev/fd/0" || (runtime.GOOS == "linux" && path == "/proc/self/fd/0")
}
