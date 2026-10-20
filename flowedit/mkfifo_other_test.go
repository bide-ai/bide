//go:build !unix

package flowedit

func mkfifo(string) error { return nil }
