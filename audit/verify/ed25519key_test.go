package verify

import (
	"bufio"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// ed25519KeyVectors reads testdata/ed25519-keys.txt: key encodings (small-order, mixed-order,
// non-canonical, random, and good keys) with whether each is usable, as decided by an independent
// implementation (filippo.io/edwards25519, outside this repository). audit's tests read the same
// file, so the two copies of the check agree with it and with each other.
func ed25519KeyVectors(t *testing.T, path string) (keys [][]byte, want []bool, labels []string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fs := strings.Fields(line)
		k, err := hex.DecodeString(fs[0])
		if err != nil || len(fs) != 3 {
			t.Fatalf("bad vector line %q", line)
		}
		keys, want, labels = append(keys, k), append(want, fs[1] == "true"), append(labels, fs[2])
	}
	if len(keys) < 200 {
		t.Fatalf("only %d vectors", len(keys))
	}
	return keys, want, labels
}

func TestEd25519KeyVectors(t *testing.T) {
	keys, want, labels := ed25519KeyVectors(t, "testdata/ed25519-keys.txt")
	for i, k := range keys {
		if got := usableKey(k); got != want[i] {
			t.Errorf("%s (%x): usableKey = %v, want %v", labels[i], k, got, want[i])
		}
	}
}
