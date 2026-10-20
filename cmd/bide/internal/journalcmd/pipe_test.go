package journalcmd

import (
	"testing"

	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// The files the journal verbs name are read without waiting on a named pipe, and a directory in
// their place is refused.
func TestReaders_NamedPipeAndDirectory(t *testing.T) {
	p := safefiletest.FIFO(t)
	d := t.TempDir()
	var v map[string]any
	safefiletest.Deadline(t, "readers", func() {
		if err := readJSON("-sth", p, &v); err == nil {
			t.Error("readJSON read a named pipe")
		}
		if _, err := readPubKey(p); err == nil {
			t.Error("readPubKey read a named pipe")
		}
		if _, err := readHeads("-head-out", p); err == nil {
			t.Error("readHeads read a named pipe")
		}
	})
	if err := readJSON("-sth", d, &v); err == nil {
		t.Error("readJSON read a directory")
	}
	if _, err := readPubKey(d); err == nil {
		t.Error("readPubKey read a directory")
	}
	if _, err := readHeads("-head-out", d); err == nil {
		t.Error("readHeads read a directory")
	}
}

// "-" takes the key from standard input.
func TestReadPubKey_Stdin(t *testing.T) {
	safefiletest.Stdin(t, safefiletest.KeyText(t)+"\n")
	if _, err := readPubKey("-"); err != nil {
		t.Fatalf("readPubKey(-) = %v", err)
	}
}
