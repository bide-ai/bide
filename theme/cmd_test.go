package theme

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTheme(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "theme.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestMainCheck(t *testing.T) {
	good := writeTheme(t, custom("dark", surface("accent", "#79b8ff")))
	code, out, errs := run("check", good)
	if code != 0 || !strings.Contains(out, `theme "acme-test" ok`) || errs != "" {
		t.Errorf("good theme: code %d, out %q, err %q", code, out, errs)
	}
	if strings.Contains(out, ": ok      C1") {
		t.Error("a passing measure printed without -v")
	}
	code, out, _ = run("check", "-v", good)
	if code != 0 || !strings.Contains(out, ": ok      C1 text on bg: contrast") {
		t.Errorf("-v: code %d, out %q", code, out)
	}

	bad := writeTheme(t, custom("light", surface("text", "#f6f6f2")))
	code, out, _ = run("check", bad)
	if code != 1 || !strings.Contains(out, "error   C1 text on bg: contrast 1.00:1, needs at least 4.5:1") || !strings.Contains(out, "refused") {
		t.Errorf("bad theme: code %d, out %q", code, out)
	}

	// A format error is a failure with the file named and the key quoted.
	hostile := writeTheme(t, custom("dark", surface("bg", "\x1b[31m")))
	code, _, errs = run("check", hostile)
	if code != 1 || !strings.Contains(errs, `\x1b[31m`) || strings.Contains(errs, "\x1b") {
		t.Errorf("hostile theme: code %d, err %q", code, errs)
	}

	big := writeTheme(t, append(custom("dark", nil), []byte(strings.Repeat(" ", MaxSize))...))
	if code, _, errs = run("check", big); code != 1 || !strings.Contains(errs, "byte limit") {
		t.Errorf("oversize file: code %d, err %q", code, errs)
	}
	if code, _, errs = run("check", filepath.Join(t.TempDir(), "missing.json")); code != 1 || errs == "" {
		t.Errorf("missing file: code %d, err %q", code, errs)
	}
	// One bad file among good ones still fails the run, and every file is reported.
	code, out, _ = run("check", good, bad)
	if code != 1 || strings.Count(out, "theme \"") != 2 {
		t.Errorf("two files: code %d, out %q", code, out)
	}
	for _, args := range [][]string{nil, {"check"}, {"frob"}, {"check", "-nope"}} {
		if code, _, _ := run(args...); code != 2 {
			t.Errorf("%v: code %d, want 2", args, code)
		}
	}
	if code, out, _ := run("--help"); code != 0 || !strings.Contains(out, "usage: bide theme check") {
		t.Errorf("--help: code %d, out %q", code, out)
	}
}
