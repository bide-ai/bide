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

// OpenIn opens a name inside a root and a swapped-in directory shows in the opened file's Stat.
func TestOpenIn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	f, err := OpenIn(root, "f")
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 3)
	if n, err := f.Read(b); err != nil || string(b[:n]) != "abc" {
		t.Errorf("read through OpenIn = %q, %v", b[:n], err)
	}
	f.Close()
	if _, err := OpenIn(root, "../x"); err == nil {
		t.Error("OpenIn left the root")
	}
}

func TestReadOrStdin(t *testing.T) {
	defer func() { Stdin = os.Stdin; ResetStdin() }()
	ResetStdin()
	Stdin = strings.NewReader("key-bytes")
	b, err := ReadOrStdin("-pubkey", "-", 64)
	if err != nil || string(b) != "key-bytes" {
		t.Fatalf("stdin = %q, %v", b, err)
	}
	// The same flag read again gets the same bytes; another flag is told stdin is taken.
	if b, err := ReadOrStdin("-pubkey", "-", 64); err != nil || string(b) != "key-bytes" {
		t.Errorf("second read by the same flag = %q, %v", b, err)
	}
	if _, err := ReadOrStdin("-stop-policy", "-", 64); err == nil || !strings.Contains(err.Error(), "both read standard input") {
		t.Errorf("a second flag = %v, want a refusal naming both", err)
	}
	// A path that is not "-" is Read.
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadOrStdin("-pubkey", p, 64); err != nil || string(b) != "x" {
		t.Errorf("a file = %q, %v", b, err)
	}
	// Over the limit, and empty.
	ResetStdin()
	Stdin = strings.NewReader("123456")
	_, err = ReadOrStdin("-k", "-", 5)
	refusal(t, err, TooLarge)
	ResetStdin()
	Stdin = strings.NewReader("")
	if _, err := ReadOrStdin("-k", "-", 5); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty stdin = %v", err)
	}
	// The limit is not passed: exactly max is read.
	ResetStdin()
	Stdin = strings.NewReader("12345")
	if b, err := ReadOrStdin("-k", "-", 5); err != nil || string(b) != "12345" {
		t.Errorf("exactly the limit = %q, %v", b, err)
	}
}

func TestExplain(t *testing.T) {
	if Explain("-k", nil, true) != nil {
		t.Error("nil became an error")
	}
	plain := errors.New("x")
	if Explain("-k", plain, true) != plain {
		t.Error("a plain error was wrapped")
	}
	big := Explain("-pubkey", &RefusedError{Path: "k", Kind: TooLarge, Max: 7}, true)
	if got := big.Error(); got != "k: larger than 7 bytes (the limit for -pubkey)" {
		t.Errorf("too large = %q", got)
	}
	pipe := Explain("-pubkey", &RefusedError{Path: "p", Kind: NotRegular}, true)
	if got := pipe.Error(); !strings.Contains(got, "p is not a regular file") || !strings.Contains(got, "pass - to read -pubkey from standard input") {
		t.Errorf("not regular = %q", got)
	}
	if got := Explain("-cert", &RefusedError{Path: "p", Kind: NotRegular}, false).Error(); strings.Contains(got, "pass -") {
		t.Errorf("a flag without stdin suggests it: %q", got)
	}
	var re *RefusedError
	if !errors.As(pipe, &re) || re.Kind != NotRegular {
		t.Error("the refusal is not reachable through the wrapper")
	}
}

func TestLoadX509KeyPair(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadX509KeyPair(dir, dir); err == nil {
		t.Error("a directory was read as a certificate")
	} else {
		refusal(t, err, NotRegular)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadX509KeyPair(empty, empty); err == nil || strings.Contains(err.Error(), "regular file") {
		t.Errorf("an empty pair = %v, want tls's complaint about the PEM", err)
	}
}
