package term

import (
	"bytes"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
)

// env is a fake environment for enabled.
func env(kv ...string) func(string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(k string) string { return m[k] }
}

// TestEnabled: auto colors a terminal only, never with NO_COLOR set (to any value) or TERM=dumb,
// and on Windows only where the terminal is known to read ANSI escapes; always and never override
// all of it.
func TestEnabled(t *testing.T) {
	for _, c := range []struct {
		name   string
		mode   Mode
		tty    bool
		getenv func(string) string
		goos   string
		want   bool
	}{
		{"auto, a terminal", Auto, true, env("TERM", "xterm-256color"), "linux", true},
		{"auto, a terminal, no TERM", Auto, true, env(), "darwin", true},
		{"auto, not a terminal", Auto, false, env("TERM", "xterm-256color"), "linux", false},
		{"auto, NO_COLOR=1", Auto, true, env("TERM", "xterm", "NO_COLOR", "1"), "linux", false},
		{"auto, NO_COLOR=0 (any value)", Auto, true, env("TERM", "xterm", "NO_COLOR", "0"), "linux", false},
		{"auto, NO_COLOR empty", Auto, true, env("TERM", "xterm", "NO_COLOR", ""), "linux", true},
		{"auto, TERM=dumb", Auto, true, env("TERM", "dumb"), "linux", false},
		{"auto, TERM=dumb on Windows", Auto, true, env("TERM", "dumb", "WT_SESSION", "x"), "windows", false},
		{"always, not a terminal", Always, false, env(), "linux", true},
		{"always, NO_COLOR", Always, false, env("NO_COLOR", "1"), "linux", true},
		{"always, TERM=dumb", Always, true, env("TERM", "dumb"), "linux", true},
		{"always, a bare Windows console", Always, true, env(), "windows", true},
		{"never, a terminal", Never, true, env("TERM", "xterm-256color"), "linux", false},
		{"never, Windows Terminal", Never, true, env("WT_SESSION", "x"), "windows", false},
		{"Windows, a bare console", Auto, true, env(), "windows", false},
		{"Windows, an unknown TERM", Auto, true, env("TERM", "vt52"), "windows", false},
		{"Windows Terminal", Auto, true, env("WT_SESSION", "{guid}"), "windows", true},
		{"Windows Terminal, not a terminal", Auto, false, env("WT_SESSION", "{guid}"), "windows", false},
		{"Windows Terminal, NO_COLOR", Auto, true, env("WT_SESSION", "{guid}", "NO_COLOR", "1"), "windows", false},
		{"Windows, ConEmu", Auto, true, env("ConEmuANSI", "ON"), "windows", true},
		{"Windows, ConEmu off", Auto, true, env("ConEmuANSI", "OFF"), "windows", false},
		{"Windows, ANSICON", Auto, true, env("ANSICON", "120x50"), "windows", true},
		{"Windows, VS Code", Auto, true, env("TERM_PROGRAM", "vscode"), "windows", true},
		{"Windows, another TERM_PROGRAM", Auto, true, env("TERM_PROGRAM", "other"), "windows", false},
		{"Windows, TERM=xterm", Auto, true, env("TERM", "xterm"), "windows", true},
		{"Windows, TERM=cygwin-color", Auto, true, env("TERM", "cygwin-color"), "windows", true},
		{"Windows, TERM=ansi", Auto, true, env("TERM", "ansi"), "windows", true},
	} {
		if got := enabled(c.mode, c.tty, c.getenv, c.goos); got != c.want {
			t.Errorf("%s: enabled = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestModeFlag: -color reads auto, always and never, defaults to auto, and refuses anything else.
func TestModeFlag(t *testing.T) {
	for in, want := range map[string]Mode{"auto": Auto, "always": Always, "never": Never} {
		fs := flag.NewFlagSet("x", flag.ContinueOnError)
		m := Flag(fs)
		if err := fs.Parse([]string{"-color=" + in}); err != nil || *m != want || m.String() != in {
			t.Errorf("-color=%s: %v, mode %v (%s)", in, err, *m, m)
		}
	}
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	m := Flag(fs)
	if err := fs.Parse(nil); err != nil || *m != Auto {
		t.Fatalf("default: %v, mode %v", err, *m)
	}
	for _, bad := range []string{"yes", "true", "", "ALWAYS"} {
		if err := fs.Parse([]string{"-color=" + bad}); err == nil {
			t.Errorf("-color=%q was accepted", bad)
		}
	}
}

// TestFor: a buffer and a pipe are not terminals, so auto leaves their output plain; always colors
// even them, and never colors nothing.
func TestFor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	for _, out := range []io.Writer{&bytes.Buffer{}, w} {
		if For(out, Auto).On() {
			t.Errorf("auto colors %T, which is not a terminal", out)
		}
		if !For(out, Always).On() {
			t.Errorf("always does not color %T", out)
		}
		if For(out, Never).On() {
			t.Errorf("never colors %T", out)
		}
	}
	// A character device is a terminal to isTerminal; /dev/null is one on Unix.
	if f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0); err == nil {
		defer f.Close()
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 && !isTerminal(f) {
			t.Errorf("%s is a character device, and isTerminal says it is not a terminal", os.DevNull)
		}
	}
}

// TestStyle: an off style returns its text unchanged and adds no mark; an on style wraps each role
// in its escape and resets after it.
func TestStyle(t *testing.T) {
	off := New(false)
	for _, f := range []func(string) string{off.OK, off.Fail, off.Warn, off.Path, off.Dim, off.Heading, off.Bold, off.Blue, off.Cyan, off.BoldOK, off.BoldFail} {
		if got := f("text"); got != "text" {
			t.Errorf("off style changed the text: %q", got)
		}
	}
	if off.Pass()+off.Cross()+off.Bang() != "" {
		t.Error("an off style adds a mark")
	}
	on := New(true)
	for got, want := range map[string]string{
		on.OK("ok"):       "\x1b[32mok\x1b[0m",
		on.Fail("no"):     "\x1b[31mno\x1b[0m",
		on.Warn("w"):      "\x1b[33mw\x1b[0m",
		on.Path("p"):      "\x1b[1;36mp\x1b[0m",
		on.Dim("d"):       "\x1b[2md\x1b[0m",
		on.Heading("h"):   "\x1b[1mh\x1b[0m",
		on.BoldFail("bf"): "\x1b[1;31mbf\x1b[0m",
		on.Pass():         "\x1b[32m✓\x1b[0m ",
		on.Cross():        "\x1b[31m✗\x1b[0m ",
		on.Bang():         "\x1b[33m!\x1b[0m ",
		on.OK(""):         "",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if !strings.Contains(on.Pass(), "✓") || !strings.Contains(on.Cross(), "✗") {
		t.Error("a colored mark lost its symbol")
	}
}
