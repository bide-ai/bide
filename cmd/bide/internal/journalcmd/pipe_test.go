package journalcmd

import (
	"os"
	"path/filepath"
	"strings"
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

// The signed-heads log only grows, so it has no total size limit: a log past 256 MiB is read, one
// head per line, and it is each line that is limited.
func TestReadHeads_LargeLogAndOversizedRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 260 MiB log")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "heads.jsonl")
	f, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(`{"pad":"` + strings.Repeat("a", 60000) + "\"}\n") // ~60 KB, under the record limit
	n := 260 << 20 / len(line)
	for i := 0; i <= n; i++ {
		if _, err := f.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	heads, err := readHeads("-head-out", log)
	if err != nil || len(heads) != n+1 {
		t.Fatalf("a log of %d heads over 256 MiB: %d heads, %v", n+1, len(heads), err)
	}

	big := filepath.Join(dir, "big.jsonl")
	rec := `{"pad":"` + strings.Repeat("b", maxHeadRecord) + `"}` + "\n"
	if err := os.WriteFile(big, []byte("{}\n"+rec), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = readHeads("-head-out", big)
	if err == nil || !strings.Contains(err.Error(), "line 2 is longer than 65536 bytes (the limit for a head in -head-out)") {
		t.Fatalf("an oversized record = %v", err)
	}
	// A record of exactly the limit is read, with or without a final newline.
	exact := `{"pad":"` + strings.Repeat("c", maxHeadRecord-len(`{"pad":""}`)) + `"}`
	if len(exact) != maxHeadRecord {
		t.Fatalf("test record is %d bytes", len(exact))
	}
	ok := filepath.Join(dir, "ok.jsonl")
	if err := os.WriteFile(ok, []byte("{}\n\n"+exact), 0o600); err != nil {
		t.Fatal(err)
	}
	if heads, err := readHeads("-head-out", ok); err != nil || len(heads) != 2 {
		t.Fatalf("exact-limit record: %d heads, %v", len(heads), err)
	}
	// A line that is not a head names its line.
	bad := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(bad, []byte("{}\nnot json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readHeads("-head-out", bad); err == nil || !strings.Contains(err.Error(), "line 2 is not a signed head") {
		t.Fatalf("a bad line = %v", err)
	}
	if h, err := readHeads("-head-out", filepath.Join(dir, "none")); h != nil || err != nil {
		t.Fatalf("a missing log = %v, %v", h, err)
	}
}
