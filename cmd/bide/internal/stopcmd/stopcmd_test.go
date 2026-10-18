package stopcmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// memStores serves one in-memory journal for any -db path that exists.
func memStores() (Stores, agent.Store) {
	st := agent.NewMemStore()
	ev := audit.NewMemEventStore()
	open := func(string, bool) (Opened, error) { return Opened{Store: st, Events: ev, Close: nopCloser{}}, nil }
	return Stores{SQLite: open}, st
}

func tempDB(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "run.db")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func run(t *testing.T, stores Stores, tty bool, in string, args ...string) (int, string, string) {
	t.Helper()
	old := isTerminal
	isTerminal = func(io.Reader) bool { return tty }
	defer func() { isTerminal = old }()
	var out, errb bytes.Buffer
	code := Main(args, strings.NewReader(in), &out, &errb, stores)
	return code, out.String(), errb.String()
}

func active(t *testing.T, st agent.Store) []agent.StopEntry {
	t.Helper()
	j, _ := agent.NewJournal(st)
	ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic))
	if err != nil {
		t.Fatal(err)
	}
	s, err := ctl.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return s.Active()
}

// On a terminal a stop is asked about once, and a declined or empty answer writes nothing.
func TestTerminalPromptSet(t *testing.T) {
	stores, st := memStores()
	db := tempDB(t)
	for _, in := range []string{"", "n\n", "no\n", "\n"} {
		code, out, errOut := run(t, stores, true, in, "-db", db, "-all", "-reason", "r")
		if code != 1 || !strings.Contains(out, "Type y to set 1 stop") || !strings.Contains(errOut, "not confirmed") {
			t.Fatalf("answer %q: exit %d\n%s\n%s", in, code, out, errOut)
		}
		if len(active(t, st)) != 0 {
			t.Fatalf("answer %q wrote a stop", in)
		}
	}
	if code, _, errOut := run(t, stores, true, "y\n", "-db", db, "-all", "-reason", "r"); code != 0 || len(active(t, st)) != 1 {
		t.Fatalf("y: exit %d\n%s", code, errOut)
	}
}

// A stop without a terminal is written without a prompt (an incident script must not hang or refuse);
// -yes on a terminal skips the prompt.
func TestNoTerminalSetsWithoutPrompt(t *testing.T) {
	stores, st := memStores()
	db := tempDB(t)
	code, out, errOut := run(t, stores, false, "", "-db", db, "-tool", "refund", "-reason", "r")
	if code != 0 || strings.Contains(out, "Type y") || len(active(t, st)) != 1 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if code, out, _ := run(t, stores, true, "", "-db", db, "-tool", "payout", "-reason", "r", "-yes"); code != 0 || strings.Contains(out, "Type y") || len(active(t, st)) != 2 {
		t.Fatalf("-yes on a terminal: exit %d\n%s", code, out)
	}
}

// A lift always asks on a terminal, and refuses without one unless -yes.
func TestLiftAsksOnATerminal(t *testing.T) {
	stores, st := memStores()
	db := tempDB(t)
	if code, _, errOut := run(t, stores, false, "", "-db", db, "-all", "-reason", "r"); code != 0 {
		t.Fatalf("set: %d %s", code, errOut)
	}
	id := active(t, st)[0].ID
	if code, out, _ := run(t, stores, true, "n\n", "-db", db, "-lift", id, "-reason", "r"); code != 1 || !strings.Contains(out, "Type y to lift stop") || len(active(t, st)) != 1 {
		t.Fatalf("declined lift: exit %d\n%s", code, out)
	}
	if code, _, _ := run(t, stores, false, "y\n", "-db", db, "-lift", id, "-reason", "r"); code != 2 || len(active(t, st)) != 1 {
		t.Fatalf("lift without a terminal or -yes: exit %d", code)
	}
	if code, _, errOut := run(t, stores, true, "y\n", "-db", db, "-lift", id, "-reason", "r"); code != 0 || len(active(t, st)) != 0 {
		t.Fatalf("confirmed lift: exit %d\n%s", code, errOut)
	}
}

// /dev/null is a character device, but a cron job's standard input must not be waited on.
func TestDevNullIsNotATerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Fatal("the null device was taken for a terminal")
	}
	if isTerminal(strings.NewReader("")) {
		t.Fatal("a reader that is not a file was taken for a terminal")
	}
}

// A key reference given without -key can hold a secret: a stray argument is never echoed back.
func TestStrayArgumentIsNotEchoed(t *testing.T) {
	stores, _ := memStores()
	src := FileKeys()
	src.NoEchoArgs = true
	var out, errb bytes.Buffer
	code := MainWith([]string{"-db", tempDB(t), "-all", "-reason", "r", "pkcs11:pin-value=hunter2"}, strings.NewReader(""), &out, &errb, stores, src)
	if code != 2 || strings.Contains(errb.String()+out.String(), "hunter2") {
		t.Fatalf("exit %d\n%s%s", code, out.String(), errb.String())
	}
}
