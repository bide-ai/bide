//go:build unix

package approvecmd

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// A secret file must be the current user's: /etc/hosts is a regular file that root owns, so a
// non-root user is refused it as a key file and by OpenPrivate, whatever its mode. (As root there is
// no such file to try, and the test is skipped.)
func TestPrivateFileMustBeOwnedByYou(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	if _, err := os.Stat("/etc/hosts"); err != nil {
		t.Skip("no /etc/hosts")
	}
	f, err := OpenPrivate("/etc/hosts", "PIN file")
	if f != nil {
		f.Close()
	}
	if pe, ok := errors.AsType[*PrivateFileError](err); !ok || !strings.Contains(pe.Msg, "owned by another user") {
		t.Fatalf("OpenPrivate = %v", err)
	}
	if _, err := LoadSigner("/etc/hosts"); err == nil || !strings.Contains(err.Error(), "owned by another user") || !strings.Contains(err.Error(), "key file") {
		t.Fatalf("LoadSigner = %v", err)
	}
	// Your own file at 0600 opens; a missing one is the open's error, not a refusal.
	p := t.TempDir() + "/f"
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := OpenPrivate(p, "PIN file")
	if err != nil {
		t.Fatal(err)
	}
	g.Close()
	if _, err := OpenPrivate(p+"-missing", "PIN file"); err == nil {
		t.Fatal("a missing file opened")
	} else if _, ok := errors.AsType[*PrivateFileError](err); ok {
		t.Fatalf("a missing file is not a refusal: %v", err)
	}
}
