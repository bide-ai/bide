package themecmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/bide-ai/bide/theme"
)

// custom builds a custom theme file: a base plus the given top-level overrides.
func custom(base string, over map[string]any) []byte {
	m := map[string]any{"bide_theme": 1, "name": "acme-test", "base": base}
	for k, v := range over {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

func surface(k, v string) map[string]any { return map[string]any{"surface": map[string]any{k: v}} }

func noControls(t *testing.T, what, s string) {
	t.Helper()
	for _, c := range s {
		if c < 0x20 && c != '\n' || c == 0x7f || c == 0xfffd {
			t.Errorf("%s holds the control character %q: %q", what, c, s)
			return
		}
	}
}

// hostile is text a file could carry into an error, with the exact escaped form each must come
// out as. The expectations are written out, not derived, so a wrong escaping rule fails them.
var hostile = []struct{ in, want string }{
	{"\x1b[31mRED", `\x1b[31mRED`},
	{"\x1b]0;title\x07", `\x1b]0;title\a`},
	{"\x1b[2J", `\x1b[2J`},
	{"x\n/p.json: theme \"dark\" ok (0 errors)", `x\n/p.json: theme "dark" ok (0 errors)`},
	{"a\rb", `a\rb`},
	{"tab\there", `tab\there`},
	{"\u202eevil", `\u202eevil`},
	{"\u2028\u2029", `\u2028\u2029`},
	{"\u0085", `\u0085`},
	{"\u009b31m", `\u009b31m`},
	{"bad\xffbyte", `bad\xffbyte`},
	{"nul\x00", `nul\x00`},
	{"del\x7f", `del\x7f`},
	{"caf\u00e9 \u65e5\u672c", "caf\u00e9 \u65e5\u672c"}, // printable non-ASCII is kept
}

// TestTypeErrorsPrintSafely: encoding/json puts a map key into its type errors raw, so a key with
// an escape sequence and a value of the wrong type must still print safely through the command.
func TestTypeErrorsPrintSafely(t *testing.T) {
	for _, data := range []string{
		"{\"bide_theme\":1,\"name\":\"zz\",\"base\":\"dark\",\"surface\":{\"\\u001b[31mRED\":5}}",
		"{\"bide_theme\":1,\"name\":\"zz\",\"base\":\"dark\",\"semantic\":{\"ok\":{\"\\u001b]0;x\\u0007\":5}}}",
		"{\"bide_theme\":1,\"name\":\"zz\",\"base\":\"dark\",\"radius\":{\"\\u001b[2J\":\"x\"}}",
		"{\"bide_theme\":1,\"name\":\"zz\",\"base\":\"dark\",\"syntax\":{\"a\\u009bb\":7}}",
	} {
		_, out, errs := run("check", writeTheme(t, []byte(data)))
		noControls(t, "stdout", out)
		noControls(t, "stderr", errs)
	}
}

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

	big := writeTheme(t, append(custom("dark", nil), []byte(strings.Repeat(" ", theme.MaxSize))...))
	if code, _, errs = run("check", big); code != 1 || !strings.Contains(errs, "larger than") {
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

// TestNoForgedLines: a key with a newline cannot start a line of output that looks like the
// command's own.
func TestNoForgedLines(t *testing.T) {
	data := "{\"bide_theme\":1,\"name\":\"zz\",\"base\":\"dark\",\"surface\":{\"x\\n/p.json: theme \\\"dark\\\" ok (0 errors)\":5}}"
	_, out, errs := run("check", writeTheme(t, []byte(data)))
	if out != "" || strings.Count(errs, "\n") != 1 {
		t.Errorf("a newline in a key made %d lines: out %q err %q", strings.Count(errs, "\n"), out, errs)
	}
}

// TestCommandEscapesErrorText: the command prints a path and an error from the file system or the
// decoder escaped, one line each, with a path it was given that holds the hostile text, and with a
// file whose content does.
func TestCommandEscapesErrorText(t *testing.T) {
	dir := t.TempDir()
	single := func(what, stream string) {
		t.Helper()
		if strings.Count(stream, "\n") > 1 {
			t.Errorf("%s made %d lines: %q", what, strings.Count(stream, "\n"), stream)
		}
		for _, c := range strings.TrimSuffix(stream, "\n") {
			if !unicode.IsPrint(c) {
				t.Errorf("%s: output holds the non-printing %q: %q", what, c, stream)
				break
			}
		}
	}
	for _, h := range hostile {
		// Joined as text, not with filepath.Join, which cleans a "/" in the name on Windows.
		code, out, errs := run("check", dir+string(filepath.Separator)+"no such "+h.in)
		if code != 1 {
			t.Errorf("a missing file named %q: exit %d", h.in, code)
		}
		single("a path named "+h.in, out)
		single("a path named "+h.in, errs)
		if !strings.Contains(errs, "no such "+h.want) {
			t.Errorf("the path %q is not shown as %q: %q", h.in, "no such "+h.want, errs)
		}
	}
	// A file whose content holds a hostile key: the unknown-key message quotes it exactly.
	p := writeTheme(t, []byte("{\"bide_theme\":1,\"name\":\"zz\",\"base\":\"dark\",\"surface\":{\"\\u001b[2J\\u202e\":\"#fff\"}}"))
	code, out, errs := run("check", p)
	if code != 1 || !strings.Contains(errs, `surface.\x1b[2J\u202e`) {
		t.Errorf("a hostile key in a file: exit %d, stderr %q", code, errs)
	}
	single("a hostile key", out)
	single("a hostile key", errs)
}
