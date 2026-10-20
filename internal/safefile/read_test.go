package safefile

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func refusal(t *testing.T, err error, kind Kind) {
	t.Helper()
	var re *RefusedError
	if !errors.As(err, &re) || re.Kind != kind {
		t.Fatalf("error = %v, want a refusal of kind %d", err, kind)
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if b, err := Read(write("at", "12345"), 5, nil); err != nil || string(b) != "12345" {
		t.Errorf("a file of exactly the limit = %q, %v", b, err)
	}
	_, err := Read(write("over", "123456"), 5, nil)
	refusal(t, err, TooLarge)
	if !strings.Contains(err.Error(), "larger than 5 bytes") {
		t.Errorf("size error = %q", err)
	}
	// A directory is refused on every platform, Windows included, before anything is opened.
	_, err = Read(dir, 5, nil)
	refusal(t, err, NotRegular)
	if !strings.Contains(err.Error(), "is not a regular file") {
		t.Errorf("directory error = %q", err)
	}
	// A missing file is the system's error, not a refusal.
	_, err = Read(filepath.Join(dir, "nope"), 5, nil)
	var re *RefusedError
	if err == nil || errors.As(err, &re) {
		t.Errorf("a missing file = %v, want the system's error", err)
	}
}

// A regular file replaced by a directory between the check and the open is refused on the opened
// file's own Stat. No os.SameFile, which says nothing on Windows.
func TestReadPathSwapped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Read(p, 5, func() {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
	})
	refusal(t, err, Changed)
	if !strings.Contains(err.Error(), "changed while it was opened") {
		t.Errorf("error = %q", err)
	}
}

// A limit at the top of the range reads the whole file; it does not wrap around to read nothing.
func TestReadMaxInt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := Read(p, math.MaxInt, nil); err != nil || string(b) != "abc" {
		t.Errorf("Read with max = MaxInt = %q, %v", b, err)
	}
}
