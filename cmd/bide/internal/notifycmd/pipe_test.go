package notifycmd

import (
	"testing"

	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// The secret and CA files bide notify names are read without waiting on a named pipe, and a
// directory in their place is refused.
func TestFiles_NamedPipeAndDirectory(t *testing.T) {
	p := safefiletest.FIFO(t)
	d := t.TempDir()
	safefiletest.Deadline(t, "notify readers", func() {
		if _, err := readOne(func(string) string { return "" }, readFile, "", p, "-secret-file"); err == nil {
			t.Error("readOne read a named pipe")
		}
	})
	if _, err := readOne(func(string) string { return "" }, readFile, "", d, "-secret-file"); err == nil {
		t.Error("readOne read a directory")
	}
}

// "-" takes the secret from standard input, and a process substitution (a descriptor) is read as the
// stream it is.
func TestReadOne_StdinAndDescriptor(t *testing.T) {
	safefiletest.Stdin(t, " s3cret\n")
	got, err := readOne(func(string) string { return "" }, readFile, "", "-", "-secret-file")
	if err != nil || got != "s3cret" {
		t.Fatalf("readOne(-) = %q, %v", got, err)
	}
	fd := safefiletest.Descriptor(t, "from-a-pipe")
	got, err = readOne(func(string) string { return "" }, readFile, "", fd, "-secret-file")
	if err != nil || got != "from-a-pipe" {
		t.Fatalf("readOne(%s) = %q, %v", fd, got, err)
	}
}
