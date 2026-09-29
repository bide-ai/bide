package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A command line the CLI does not fully read is a usage error, exit 2, never a verdict. Flag
// parsing stops at the first argument that is not a flag, so without this check a stray argument
// silently drops every flag after it: here the -digest the auditor asked to be checked, while the
// command still printed OK and exited 0. A help request verifies nothing, so it exits 2 as well, as
// does an unknown flag, and prove takes one of -tool and -index, as documented, not both.
func TestCLI_UnreadArgumentsAreUsageErrors(t *testing.T) {
	dir := t.TempDir()
	bin := buildCLI(t, dir)
	policy := filepath.Join(dir, "policy.machine")
	if err := os.WriteFile(policy, []byte("machine m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrong := strings.Repeat("00", 32)
	if code, out := exitCode(t, bin, "verify-governance", "-policy", policy, "-digest", wrong); code != 1 {
		t.Fatalf("a digest mismatch exited %d, want 1:\n%s", code, out)
	}
	for name, args := range map[string][]string{
		"a stray argument before -digest": {"verify-governance", "-policy", policy, "stray", "-digest", wrong},
		"a trailing argument":             {"verify-governance", "-policy", policy, "-digest", wrong, "stray"},
		"-h on a verify verb":             {"verify", "-h"},
		"-help on a verify verb":          {"verify-evidence", "-help"},
		"-h after every required flag":    {"verify-governance", "-policy", policy, "-h"},
		"an unknown flag":                 {"verify-governance", "-policy", policy, "-digets", wrong},
		"both -tool and -index":           {"prove", "-journal", policy, "-sth", policy, "-tool", "t1", "-index", "0"},
	} {
		if code, out := exitCode(t, bin, args...); code != 2 {
			t.Errorf("%s: exited %d, want 2 (usage):\n%s", name, code, out)
		}
	}
}
