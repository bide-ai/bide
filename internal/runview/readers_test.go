package runview

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/safefile"
	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// A directory named as one of those files is refused, on every platform, with the reason.
func TestReaders_RefuseADirectory(t *testing.T) {
	dir := t.TempDir()
	var v map[string]any
	var refused *safefile.RefusedError
	if err := readStrict("-sth", dir, &v); !errors.As(err, &refused) || !strings.Contains(err.Error(), "is not a regular file") {
		t.Errorf("readStrict(dir) = %v, want a refusal", err)
	}
	if _, err := readKey(dir); !errors.As(err, &refused) || !strings.Contains(err.Error(), "is not a regular file") {
		t.Errorf("readKey(dir) = %v, want a refusal", err)
	}
}

// The size limits stay: readStrict keeps maxInput's wording, and a public key file past 1 MiB is
// refused.
func TestReaders_SizeLimits(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(p, []byte(strings.Repeat(" ", maxKeyFile+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(p); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("readKey of an oversize file = %v", err)
	}
}

// "-" takes the key and the signed documents from standard input, one flag only.
func TestReaders_Stdin(t *testing.T) {
	safefiletest.Stdin(t, safefiletest.KeyText(t))
	if _, err := readKey("-"); err != nil {
		t.Fatalf("readKey(-) = %v", err)
	}
	var v map[string]any
	if err := readStrict("-sth", "-", &v); err == nil || !strings.Contains(err.Error(), "both read standard input") {
		t.Fatalf("readStrict(-sth, -) after readKey(-) = %v, want the stdin conflict", err)
	}
	safefiletest.Stdin(t, `{"a":1}`)
	if err := readStrict("-sth", "-", &v); err != nil || v["a"] == nil {
		t.Fatalf("readStrict(-sth, -) = %v, %v", v, err)
	}
	// The journals are positional files, and a file named "-" is a file there.
	if err := readStrict("", "-", &v); err == nil {
		t.Fatal("a positional journal named - read standard input")
	}
}

// A refusal names the flag, and the "-" to use instead of a pipe, and the limit it hit.
func TestReaders_RefusalsNameTheFlag(t *testing.T) {
	var v map[string]any
	err := readStrict("-sth", t.TempDir(), &v)
	if err == nil || !strings.Contains(err.Error(), "pass - to read -sth from standard input") {
		t.Errorf("readStrict(-sth, dir) = %v", err)
	}
	p := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(p, []byte(strings.Repeat(" ", maxKeyFile+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(p); err == nil || !strings.Contains(err.Error(), "larger than 1048576 bytes (the limit for -pubkey)") {
		t.Errorf("readKey(oversize) = %v", err)
	}
}

// "-pubkey -" with nothing on standard input, or too much, says which; a terminal is not waited on.
func TestReadKey_StdinReasons(t *testing.T) {
	safefiletest.Stdin(t, "")
	if _, err := readKey("-"); err == nil || !strings.Contains(err.Error(), "standard input is empty") {
		t.Errorf("empty stdin = %v", err)
	}
	safefiletest.Stdin(t, strings.Repeat("a", maxKeyFile+1))
	if _, err := readKey("-"); err == nil || !strings.Contains(err.Error(), "larger than 1048576 bytes (the limit for -pubkey)") {
		t.Errorf("oversize stdin = %v", err)
	}
	safefiletest.Stdin(t, "not a key")
	if _, err := readKey("-"); err == nil || strings.Contains(err.Error(), "neither a key nor a readable file") {
		t.Errorf("a bad key = %v, want the parser's reason", err)
	}
}
