package runview

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// syncBuf is a writer Main's server goroutine and the test can both use.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// cliInputs are the files bide view reads for the gated run g1, written to a temp dir.
type cliInputs struct {
	dir                                    string
	journal, sth, evidence, pubkey, apkeys string
	j                                      *agent.Journal
}

func writeFileJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newCLIInputs(t *testing.T) cliInputs {
	t.Helper()
	j, sth := gatedRun(t, map[string]bool{"ops": true, "finance": true})
	in := cliInputs{dir: t.TempDir(), j: j}
	exp, err := audit.ExportJournal(context.Background(), j, "g1")
	if err != nil {
		t.Fatal(err)
	}
	in.journal = filepath.Join(in.dir, "g1.json")
	writeFileJSON(t, in.journal, exp)
	in.sth = filepath.Join(in.dir, "sth.json")
	writeFileJSON(t, in.sth, sth)
	pkg, err := audit.Evidence(context.Background(), j, "g1", audit.Ed25519Signer{Priv: key("log")}, time.Now().UnixNano(), audit.WithToolCall("c1"))
	if err != nil {
		t.Fatal(err)
	}
	in.evidence = filepath.Join(in.dir, "evidence.json")
	writeFileJSON(t, in.evidence, pkg)
	in.pubkey = audit.FormatPublicKey(audit.AlgEd25519, key("log").Public().(ed25519.PublicKey))
	keys := map[string]string{}
	for _, id := range []string{"ops", "finance", "risk"} {
		keys[id] = audit.FormatPublicKey(audit.AlgEd25519, key(id).Public().(ed25519.PublicKey))
	}
	in.apkeys = filepath.Join(in.dir, "approver-keys.json")
	writeFileJSON(t, in.apkeys, keys)
	return in
}

// TestOptionsReadsInputs: options turns the flags' files into the Source and the provers bide
// view serves; each prover's proof reaches the gate the page shows.
func TestOptionsReadsInputs(t *testing.T) {
	in := newCLIInputs(t)
	o, closer, err := options(flags{journals: multi{in.journal}, sth: in.sth, evidence: in.evidence, pubkey: in.pubkey, approverKeys: in.apkeys, need: 2, approvers: "ops,finance,risk"}, Stores{})
	if err != nil || closer != nil {
		t.Fatalf("options = %v, closer %v", err, closer)
	}
	ids, _ := o.Source.Runs(context.Background())
	if len(ids) != 1 || ids[0] != "g1" {
		t.Fatalf("runs = %q, want [g1]", ids)
	}
	if len(o.Provers) != 2 {
		t.Fatalf("provers = %d, want the -sth and -evidence ones", len(o.Provers))
	}
	v := viewOf(t, Handler(o), "g1")
	if len(v.Gates) != 1 || !v.Gates[0].SignaturesOK || !strings.HasSuffix(v.Gates[0].Signatures, "(against the expected policy)") {
		t.Fatalf("gate = %+v, want signatures verified against the expected policy", v.Gates)
	}
}

// TestOptionsStore: -db and -postgres go through the Stores the build gives; a build without one
// says so, a failed open names what was opened, and the closer is handed back.
func TestOptionsStore(t *testing.T) {
	in := newCLIInputs(t)
	var gotPath string
	closed := false
	stores := Stores{SQLite: func(p string) (agent.Store, io.Closer, error) {
		gotPath = p
		return in.j.Store(), closerFunc(func() error { closed = true; return nil }), nil
	}}
	o, closer, err := options(flags{db: "runs.db"}, stores)
	if err != nil || gotPath != "runs.db" {
		t.Fatalf("options(-db) = %v, path %q", err, gotPath)
	}
	if _, ok := o.Source.(JournalSource); !ok {
		t.Fatalf("source = %T, want JournalSource", o.Source)
	}
	if viewOf(t, Handler(o), "g1").State == "" {
		t.Fatal("the run read through the store has no state")
	}
	closer.Close()
	if !closed {
		t.Error("the store's closer was not returned")
	}

	if _, _, err := options(flags{db: "x"}, Stores{}); err == nil || !strings.Contains(err.Error(), "this build cannot read that store") {
		t.Errorf("no SQLite opener: %v", err)
	}
	if _, _, err := options(flags{postgres: "postgres://x"}, Stores{SQLite: stores.SQLite}); err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Errorf("-postgres without a Postgres opener must not fall back to SQLite: %v", err)
	}
	fail := Stores{Postgres: func(string) (agent.Store, io.Closer, error) { return nil, nil, errors.New("refused") }}
	if _, _, err := options(flags{postgres: "postgres://x"}, fail); err == nil || err.Error() != "open the Postgres journal: refused" {
		t.Errorf("failed open: %v", err)
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// TestOptionsTopology: -topology is read strictly; its digest is the one given, else computed
// from a topology that carries the digest's inputs.
func TestOptionsTopology(t *testing.T) {
	in := newCLIInputs(t)
	dir := t.TempDir()
	topo := filepath.Join(dir, "topo.json")
	if err := os.WriteFile(topo, []byte(`{"flow":"refund"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	o, _, err := options(flags{journals: multi{in.journal}, topo: topo, digest: "abc"}, Stores{})
	if err != nil {
		t.Fatal(err)
	}
	if fl := o.Flows["refund"]; fl == nil || fl.Digest != "abc" || fl.Source != "from -topology" {
		t.Fatalf("flow = %+v, want digest abc from -topology", fl)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"flow":"refund","nosuchfield":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := options(flags{journals: multi{in.journal}, topo: bad}, Stores{}); err == nil || !strings.Contains(err.Error(), "parse "+bad) {
		t.Errorf("unknown topology field: %v, want a parse error naming the file", err)
	}
}

// TestOptionsErrors: each unreadable input fails with an error naming it.
func TestOptionsErrors(t *testing.T) {
	in := newCLIInputs(t)
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.json")
	if err := os.WriteFile(junk, []byte(`{"bogus":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.json")
	badKeys := filepath.Join(dir, "badkeys.json")
	if err := os.WriteFile(badKeys, []byte(`{"ops":"not-a-key"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	base := flags{journals: multi{in.journal}, pubkey: in.pubkey}
	cases := []struct {
		name string
		f    flags
		want string
	}{
		{"missing journal", flags{journals: multi{missing}}, "no such file"},
		{"journal of another shape", flags{journals: multi{junk}}, "parse " + junk},
		{"missing sth", with(base, func(f *flags) { f.sth = missing }), "no such file"},
		{"missing evidence", with(base, func(f *flags) { f.evidence = missing }), "no such file"},
		{"missing approver keys", with(base, func(f *flags) { f.sth, f.approverKeys = in.sth, missing }), "no such file"},
		{"bad approver key", with(base, func(f *flags) { f.sth, f.approverKeys = in.sth, badKeys }), `approver "ops" key`},
		{"pubkey neither key nor file", flags{journals: multi{in.journal}, pubkey: missing}, "is neither a key nor a readable file"},
	}
	for _, c := range cases {
		if _, _, err := options(c.f, Stores{}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}
}

func with(f flags, mod func(*flags)) flags { mod(&f); return f }

// TestReadKey: a key is read inline, from a file holding it, and a weak key is refused rather than
// tried as a file.
func TestReadKey(t *testing.T) {
	text := audit.FormatPublicKey(audit.AlgEd25519, key("log").Public().(ed25519.PublicKey))
	if _, err := readKey(text); err != nil {
		t.Errorf("inline key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "log.pub")
	if err := os.WriteFile(path, []byte(text+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readKey(path); err != nil {
		t.Errorf("key in a file: %v", err)
	}
	weak := audit.FormatPublicKey(audit.AlgEd25519, make([]byte, ed25519.PublicKeySize))
	if _, err := readKey(weak); !errors.Is(err, audit.ErrWeakKey) {
		t.Errorf("all-zero key: %v, want ErrWeakKey", err)
	}
}

// TestReadStrictAndConfig: unreadable inputs are errors; a YAML config goes through the build's converter or is refused.
func TestReadStrictAndConfig(t *testing.T) {
	dir := t.TempDir()
	var v map[string]any
	if err := readStrict(dir, &v); err == nil {
		t.Error("readStrict of a directory succeeded")
	}
	if _, err := readConfig(dir, nil); err == nil {
		t.Error("readConfig of a directory succeeded")
	}
	if _, err := readConfig(filepath.Join(dir, "none.json"), nil); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("missing config: %v", err)
	}
	yml := filepath.Join(dir, "f.yml")
	if err := os.WriteFile(yml, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(yml, nil); err == nil || err.Error() != "this build reads JSON configs only" {
		t.Errorf("YAML without a converter: %v", err)
	}
	c, err := readConfig(yml, func(b []byte) ([]byte, error) { return []byte(`{"converted":true}`), nil })
	if err != nil || c.Name != yml || string(c.Data) != `{"converted":true}` {
		t.Errorf("YAML with a converter: %+v, %v", c, err)
	}
	if _, err := readConfig(yml, func([]byte) ([]byte, error) { return nil, errors.New("bad yaml") }); err == nil || err.Error() != "YAML: bad yaml" {
		t.Errorf("converter failure: %v", err)
	}
}

// TestMainConfigCheck: -config -check prints the problems and exits 1 on an error, -strict makes
// a warning fail too; a config that is not readable exits 1 naming the file; -config takes none
// of the run flags.
func TestMainConfigCheck(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(diffV1), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(args, &out, &errb, Stores{})
		return code, out.String(), errb.String()
	}
	if code, out, errs := run("-config", good, "-check"); code != 0 || errs != "" {
		t.Errorf("valid config: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	missing := filepath.Join(dir, "missing.json")
	if code, _, errs := run("-config", missing, "-check"); code != 1 || !strings.Contains(errs, missing+": ") {
		t.Errorf("missing config: exit %d, stderr %q", code, errs)
	}
	if code, _, errs := run("-config", good, "-db", "x"); code != 2 || !strings.Contains(errs, "-config shows a flow definition") {
		t.Errorf("-config with -db: exit %d, stderr %q", code, errs)
	}
	if code, _, errs := run("-check"); code != 2 || !strings.Contains(errs, "-check needs -config") {
		t.Errorf("-check alone: exit %d, stderr %q", code, errs)
	}
	if code, _, errs := run("-strict"); code != 2 || !strings.Contains(errs, "-strict needs -check") {
		t.Errorf("-strict alone: exit %d, stderr %q", code, errs)
	}
	if code, _, errs := run("-diff", good); code != 2 || !strings.Contains(errs, "-diff needs -config") {
		t.Errorf("-diff alone: exit %d, stderr %q", code, errs)
	}
	if code, _, errs := run("-nosuchflag"); code != 2 || !strings.Contains(errs, "flag provided but not defined") {
		t.Errorf("unknown flag: exit %d, stderr %q", code, errs)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run("-config", bad, "-check"); code != 1 || !strings.Contains(out, bad) {
		t.Errorf("unparsable config: exit %d, stdout %q, want exit 1 and the file named", code, out)
	}
	if code, _, errs := run("-config", bad, "-diff", good); code != 1 || !strings.Contains(errs, "bide view:") {
		t.Errorf("-diff with an unparsable new config: exit %d, stderr %q", code, errs)
	}
}

// TestMainRunFlagErrors: inputs that cannot be read exit 1 with the error on stderr.
func TestMainRunFlagErrors(t *testing.T) {
	var out, errb bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.json")
	if code := Main([]string{"-journal", missing}, &out, &errb, Stores{}); code != 1 || !strings.Contains(errb.String(), "bide view:") || !strings.Contains(errb.String(), "no such file") {
		t.Errorf("missing journal: exit %d, stderr %q", code, errb.String())
	}
	errb.Reset()
	in := newCLIInputs(t)
	if code := Main([]string{"-journal", in.journal, "-addr", "0.0.0.0:0"}, &out, &errb, Stores{}); code != 2 || !strings.Contains(errb.String(), "bide view:") {
		t.Errorf("non-loopback -addr: exit %d, stderr %q, want exit 2", code, errb.String())
	}
}

// serveMain runs Main until probe has seen the page, then interrupts it as Ctrl-C does. The
// interrupt is repeated until Main returns, since the server installs its handler after it prints
// its address.
func serveMain(t *testing.T, args []string, probe func(base string)) (int, string, string) {
	t.Helper()
	guard := make(chan os.Signal, 8) // keeps an early interrupt from killing the test binary
	signal.Notify(guard, os.Interrupt)
	defer signal.Stop(guard)
	var out, errb syncBuf
	done := make(chan int, 1)
	go func() { done <- Main(args, &out, &errb, Stores{}) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "serving") {
		if time.Now().After(deadline) {
			t.Fatalf("no serving line; stderr %q", errb.String())
		}
		select {
		case code := <-done:
			t.Fatalf("Main returned %d before serving; stderr %q", code, errb.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	line := out.String()
	start := strings.Index(line, "http://")
	base := strings.Fields(line[start:])[0]
	probe(base)
	for {
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
		select {
		case code := <-done:
			return code, out.String(), errb.String()
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline.Add(10 * time.Second)) {
			t.Fatal("Main did not stop on interrupt")
		}
	}
}

func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestMainServesRun: bide view -journal prints the address of the run's page, serves it
// read-only on the loopback, and exits 0 on interrupt.
func TestMainServesRun(t *testing.T) {
	in := newCLIInputs(t)
	code, out, errs := serveMain(t, []string{"-journal", in.journal, "-run", "g1 x", "-addr", "127.0.0.1:0", "-color", "never"}, func(base string) {
		if !strings.HasSuffix(base, "/run?id=g1+x") {
			t.Errorf("link %q lacks the escaped run id", base)
		}
		root := base[:strings.Index(base, "/run?")+1]
		if code, body := fetch(t, root); code != 200 || !strings.Contains(body, "g1") {
			t.Errorf("GET / = %d, want the run list naming g1:\n%s", code, body)
		}
		if code, body := fetch(t, root+"run?id=g1"); code != 200 || !strings.Contains(body, "refund") {
			t.Errorf("GET /run?id=g1 = %d:\n%s", code, body)
		}
	})
	if code != 0 || errs != "" {
		t.Errorf("exit %d, stderr %q, want 0 and none", code, errs)
	}
	if !strings.HasPrefix(out, "bide view: serving http://127.0.0.1:") || !strings.Contains(out, "(read-only; Ctrl-C to stop)") {
		t.Errorf("stdout = %q", out)
	}
}

// TestMainServesConfigAndDiff: -config serves the flow's graph page, -config -diff the change page.
func TestMainServesConfigAndDiff(t *testing.T) {
	dir := t.TempDir()
	v1, v2 := filepath.Join(dir, "v1.json"), filepath.Join(dir, "v2.json")
	for p, s := range map[string]string{v1: diffV1, v2: diffV2} {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, args := range map[string][]string{
		"config": {"-config", v2, "-addr", "127.0.0.1:0"},
		"diff":   {"-config", v2, "-diff", v1, "-addr", "127.0.0.1:0"},
	} {
		code, _, errs := serveMain(t, args, func(base string) {
			if code, body := fetch(t, base); code != 200 || !strings.Contains(body, "charge") {
				t.Errorf("%s: GET %s = %d, want the page naming node charge", name, base, code)
			}
		})
		if code != 0 || errs != "" {
			t.Errorf("%s: exit %d, stderr %q", name, code, errs)
		}
	}
}
