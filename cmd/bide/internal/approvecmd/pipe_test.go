package approvecmd

import (
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// The files bide approve names (-item, -approver-keys, -cert, -log-key) are read without waiting on a
// named pipe, and a directory in their place is refused.
func TestReaders_NamedPipeAndDirectory(t *testing.T) {
	p := safefiletest.FIFO(t)
	d := t.TempDir()
	safefiletest.Deadline(t, "readers", func() {
		if _, err := approverKeys(p); err == nil {
			t.Error("approverKeys read a named pipe")
		}
		if _, err := readChain(p); err == nil {
			t.Error("readChain read a named pipe")
		}
		if _, err := readLogKey(p); err == nil {
			t.Error("readLogKey read a named pipe")
		}
		f := &flags{item: p}
		if err := f.readItem(strings.NewReader("")); err == nil {
			t.Error("readItem read a named pipe")
		}
	})
	if _, err := approverKeys(d); err == nil {
		t.Error("approverKeys read a directory")
	}
	if _, err := readChain(d); err == nil {
		t.Error("readChain read a directory")
	}
	if _, err := readLogKey(d); err == nil {
		t.Error("readLogKey read a directory")
	}
	f := &flags{item: d}
	if err := f.readItem(strings.NewReader("")); err == nil {
		t.Error("readItem read a directory")
	}
}

// Only one flag can read standard input, and the confirmation would read it too, so -yes is needed.
func TestCheckStdin(t *testing.T) {
	if msg := (flags{approverKeys: "-", yes: true}).checkStdin(); msg != "" {
		t.Errorf("one - with -yes: %q", msg)
	}
	if msg := (flags{approverKeys: "-"}).checkStdin(); !strings.Contains(msg, "-yes") {
		t.Errorf("- without -yes: %q", msg)
	}
	if msg := (flags{approverKeys: "-", logKey: "-", yes: true}).checkStdin(); !strings.Contains(msg, "only one") {
		t.Errorf("two -: %q", msg)
	}
	if msg := (flags{item: "-", approverKeys: "-", yes: true}).checkStdin(); !strings.Contains(msg, "only one") {
		t.Errorf("-item and another: %q", msg)
	}
}

// "-" takes the log key and the approver keys from standard input; one stdin, one flag.
func TestReaders_Stdin(t *testing.T) {
	safefiletest.Stdin(t, safefiletest.KeyText(t))
	if _, err := readLogKey("-"); err != nil {
		t.Fatalf("readLogKey(-) = %v", err)
	}
	if _, err := approverKeys("-"); err == nil || !strings.Contains(err.Error(), "both read standard input") {
		t.Fatalf("approverKeys(-) after readLogKey(-) = %v, want the stdin conflict", err)
	}
}
