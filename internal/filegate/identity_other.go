//go:build !unix && !windows

package filegate

// identity has no file identity on this platform; the gate is keyed by path.
func identity(string) (string, bool) { return "", false }
