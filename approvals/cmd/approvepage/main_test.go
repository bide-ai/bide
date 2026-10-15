package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The command writes every file and a SHA256SUMS that matches what it wrote, twice the same.
func TestWrite(t *testing.T) {
	dir := t.TempDir()
	if err := write(dir); err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(sums)), "\n")
	if len(lines) != 3 {
		t.Fatalf("SHA256SUMS: %q", sums)
	}
	for _, l := range lines {
		sum, name, ok := strings.Cut(l, "  ")
		if !ok {
			t.Fatalf("line %q", l)
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if h := sha256.Sum256(b); hex.EncodeToString(h[:]) != sum {
			t.Errorf("%s: the sum does not match the file", name)
		}
	}
	dir2 := t.TempDir()
	if err := write(dir2); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(dir2, "SHA256SUMS"))
	if string(again) != string(sums) {
		t.Fatal("two builds differ")
	}
}
