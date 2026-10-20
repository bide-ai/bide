//go:build !unix

package safefile

func isDescriptorPath(string) bool { return false }
func isStdinPath(string) bool      { return false }
