package demo

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/internal/term"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestFileAnchor: the anchor appends one head per line and hands back the last; an empty or
// missing file has none, and a line that is not a head is an error rather than a zero head.
func TestFileAnchor(t *testing.T) {
	dir := t.TempDir()
	a := fileAnchor(filepath.Join(dir, "anchor.jsonl"))
	if _, err := a.latest(); err == nil {
		t.Error("latest of a missing anchor succeeded")
	}
	write(t, string(a), "\n")
	if _, err := a.latest(); err == nil || !strings.Contains(err.Error(), "no signed tree head") {
		t.Errorf("latest of an empty anchor: %v", err)
	}
	if err := os.Remove(string(a)); err != nil {
		t.Fatal(err)
	}
	for i, size := range []int{3, 5} {
		if err := a.Publish(context.Background(), "r", audit.SignedTreeHead{TreeHead: audit.TreeHead{Size: size}}); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	b, _ := os.ReadFile(string(a))
	if n := strings.Count(string(b), "\n"); n != 2 {
		t.Errorf("anchor has %d lines, want 2:\n%s", n, b)
	}
	sth, err := a.latest()
	if err != nil || sth.Size != 5 {
		t.Errorf("latest = size %d, %v; want the last head, size 5", sth.Size, err)
	}
	write(t, string(a), "not json\n")
	if _, err := a.latest(); err == nil {
		t.Error("latest of a garbage anchor succeeded")
	}
	bad := fileAnchor(filepath.Join(dir, "no-such-dir", "anchor.jsonl"))
	if err := bad.Publish(context.Background(), "r", audit.SignedTreeHead{}); err == nil {
		t.Error("Publish into a missing directory succeeded")
	}
}

// TestProvider: the fake provider numbers its charges, and charges again on every call, as a
// provider without an idempotency key does; a damaged ledger is an error, not zero charges.
func TestProvider(t *testing.T) {
	dir := t.TempDir()
	if cs, err := providerCharges(dir); err != nil || len(cs) != 0 {
		t.Fatalf("a fresh provider holds %v, %v", cs, err)
	}
	in := chargeArgs{Order: 42, Amount: 120}
	c1, err := providerCharge(dir, in)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := providerCharge(dir, in)
	if err != nil {
		t.Fatal(err)
	}
	if c1.ID != "ch_1" || c2.ID != "ch_2" || c1.Status != "succeeded" || c1.Order != 42 || c1.Amount != 120 {
		t.Errorf("charges = %+v, %+v", c1, c2)
	}
	if got := receipt(c1); got != "charged $120 for order 42 (ch_1)" {
		t.Errorf("receipt = %q", got)
	}
	cs, err := providerCharges(dir)
	if err != nil || len(cs) != 2 || cs[1] != c2 {
		t.Errorf("provider holds %+v, %v; want both charges", cs, err)
	}
	write(t, filepath.Join(dir, providerFile), "{broken\n")
	if _, err := providerCharges(dir); err == nil {
		t.Error("a damaged ledger read as empty")
	}
	if _, err := providerCharge(dir, in); err == nil {
		t.Error("charging over a damaged ledger succeeded")
	}
	// On Windows a path below a file reads as not found, which is an empty ledger.
	if _, err := providerCharges(filepath.Join(dir, providerFile)); err == nil && runtime.GOOS != "windows" {
		t.Error("a ledger path that is not a directory succeeded")
	}
}

// TestOpenJournalSeed: the journal opens only with the log's ed25519 seed, hex, in the demo's
// directory.
func TestOpenJournalSeed(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := openJournal(dir); err == nil {
		t.Error("openJournal without a log key succeeded")
	}
	key := filepath.Join(dir, logKeyFile)
	for _, bad := range []string{"zz", hex.EncodeToString([]byte("short"))} {
		write(t, key, bad)
		if _, _, err := openJournal(dir); err == nil || !strings.Contains(err.Error(), "not an ed25519 seed") {
			t.Errorf("seed %q: %v, want not an ed25519 seed", bad, err)
		}
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	write(t, key, hex.EncodeToString(priv.Seed())+"\n")
	j, closeJ, err := openJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	closeJ()
	if j == nil {
		t.Fatal("no journal")
	}
	if _, err := os.Stat(filepath.Join(dir, journalFile)); err != nil {
		t.Errorf("the journal file was not created: %v", err)
	}
}

// TestJSONHelpers: readJSON reads leniently, readStrict refuses fields an audit artifact does not
// have, and both fail on a missing file.
func TestJSONHelpers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := writeJSON(path, map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "{\n  \"a\": 1\n}\n" {
		t.Errorf("writeJSON wrote %q", b)
	}
	var m map[string]int
	if err := readJSON(path, &m); err != nil || m["a"] != 1 {
		t.Errorf("readJSON = %v, %v", m, err)
	}
	if err := readJSON(filepath.Join(dir, "none"), &m); err == nil {
		t.Error("readJSON of a missing file succeeded")
	}
	if err := readStrict(filepath.Join(dir, "none"), &m); err == nil {
		t.Error("readStrict of a missing file succeeded")
	}
	write(t, path, `{"flow":"x","bogus":1}`)
	var sth audit.SignedTreeHead
	if err := readStrict(path, &sth); err == nil {
		t.Error("readStrict accepted a field the artifact does not have")
	}
	if err := writeJSON(path, func() {}); err == nil {
		t.Error("writeJSON of an unmarshalable value succeeded")
	}
}

// TestProofText: a verified proof says what it covers; a failed one shows its summary, in red.
func TestProofText(t *testing.T) {
	p := newPrinter(io.Discard, term.Never)
	if got := proofText(p, &runview.Proof{OK: true, Covers: 9}); !strings.Contains(got, "verified (9 records)") {
		t.Errorf("verified: %q", got)
	}
	if got := proofText(p, &runview.Proof{Summary: "head is for another run"}); got != "head is for another run" {
		t.Errorf("failed: %q", got)
	}
	red := newPrinter(io.Discard, term.Always)
	if got := proofText(red, &runview.Proof{Summary: "bad"}); got != "\x1b[31mbad\x1b[0m" {
		t.Errorf("failed, colored: %q", got)
	}
	if got := approvalText(p, runview.ApprovalCheck{OK: true, Text: "2 of 3: signatures verified (ops, finance) (against the recorded policy)"}); got != "2 of 3: signatures verified" {
		t.Errorf("approvals verified: %q", got)
	}
	if got := approvalText(p, runview.ApprovalCheck{Text: "signatures NOT verified: finance"}); got != "signatures NOT verified: finance" {
		t.Errorf("approvals failed: %q", got)
	}
}

// TestRecorder: with BIDE_DEMO_RECORD unset the recorder does nothing; with it set it appends the
// event and the elapsed seconds to marks.txt, and snapshot copies the journal there.
func TestRecorder(t *testing.T) {
	t.Setenv(recordEnv, "")
	off := newRecorder()
	if off.on() || off.mark("x") != nil {
		t.Fatal("an unset recorder did something")
	}
	rec, dir := t.TempDir(), t.TempDir()
	t.Setenv(recordEnv, rec)
	r := newRecorder()
	if !r.on() {
		t.Fatal("a set recorder is off")
	}
	write(t, filepath.Join(dir, journalFile), "journal bytes")
	if err := r.snapshot(dir, "halted"); err != nil {
		t.Fatal(err)
	}
	if err := r.mark("verified"); err != nil {
		t.Fatal(err)
	}
	marks, _ := os.ReadFile(filepath.Join(rec, "marks.txt"))
	lines := strings.Split(strings.TrimSpace(string(marks)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "halted ") || !strings.HasPrefix(lines[1], "verified ") {
		t.Errorf("marks = %q", marks)
	}
	if b, _ := os.ReadFile(filepath.Join(rec, "halted.db")); string(b) != "journal bytes" {
		t.Errorf("halted.db = %q", b)
	}
	if err := r.snapshot(t.TempDir(), "x"); err == nil {
		t.Error("snapshot of a directory with no journal succeeded")
	}
	t.Setenv(recordEnv, filepath.Join(rec, "missing-dir"))
	if err := newRecorder().mark("x"); err == nil {
		t.Error("mark into a missing directory succeeded")
	}
}

// TestDemoRecordsMarks: a demo run with BIDE_DEMO_RECORD set leaves the halted journal and the
// halt and verification marks that assets/demo/record.sh composes the recording from.
func TestDemoRecordsMarks(t *testing.T) {
	rec := t.TempDir()
	t.Setenv(recordEnv, rec)
	var out bytes.Buffer
	res, err := demo(context.Background(), t.TempDir(), config{}, newPrinter(&out, term.Never), true)
	if err != nil {
		t.Fatalf("demo: %v\n%s", err, out.String())
	}
	res.close()
	marks, _ := os.ReadFile(filepath.Join(rec, "marks.txt"))
	if !strings.HasPrefix(string(marks), "halted ") || !strings.Contains(string(marks), "\nverified ") {
		t.Errorf("marks = %q, want halted then verified", marks)
	}
	if fi, err := os.Stat(filepath.Join(rec, "halted.db")); err != nil || fi.Size() == 0 {
		t.Errorf("halted.db: %v", err)
	}
}

// TestChildErrors: the child process exits 1, saying why on stderr, when the demo directory lacks
// what it reads, instead of running the agent.
func TestChildErrors(t *testing.T) {
	dir := t.TempDir()
	if code := child(dir); code != 1 {
		t.Errorf("child with no config = %d, want 1", code)
	}
	write(t, filepath.Join(dir, configFile), "{}")
	if code := child(dir); code != 1 {
		t.Errorf("child with no log key = %d, want 1", code)
	}
}

// TestMainArguments: -h exits 0 with the flags listed, a stray argument exits 2 naming it, and an
// unknown flag exits 2.
func TestMainArguments(t *testing.T) {
	var out bytes.Buffer
	if code := Main([]string{"-h"}, []string{"demo"}, &out, &out); code != 0 || !strings.Contains(out.String(), "-no-browser") || !strings.Contains(out.String(), "-keep") {
		t.Errorf("-h: exit %d\n%s", code, out.String())
	}
	out.Reset()
	if code := Main([]string{"extra"}, []string{"demo"}, &out, &out); code != 2 || !strings.Contains(out.String(), `unexpected argument "extra"`) {
		t.Errorf("stray argument: exit %d\n%s", code, out.String())
	}
	out.Reset()
	if code := Main([]string{"-nosuch"}, []string{"demo"}, &out, &out); code != 2 {
		t.Errorf("unknown flag: exit %d", code)
	}
}

// TestKeptLines: the kept-directory text names the directory and gives the bide view command that
// reopens the run, with every file it needs.
func TestKeptLines(t *testing.T) {
	d := filepath.Join("tmp", "d")
	got := keptLines(d, &result{logPub: "ed25519:ab"})
	for _, want := range []string{"kept    " + d, "bide view -db " + filepath.Join(d, journalFile), "-run " + runID, "-evidence " + filepath.Join(d, evidenceFile), "-pubkey ed25519:ab", "-approver-keys " + filepath.Join(d, keysFile)} {
		if !strings.Contains(got, want) {
			t.Errorf("keptLines lacks %q:\n%s", want, got)
		}
	}
}

// TestViewErrors: the verification step fails, naming the file, when what it checks is missing or
// damaged; it never reports a proof it could not check.
func TestViewErrors(t *testing.T) {
	dir := t.TempDir()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := view(context.Background(), dir, pub); err == nil {
		t.Error("view of an empty directory succeeded")
	}
	write(t, filepath.Join(dir, evidenceFile), `{"bogus":1}`)
	if _, err := view(context.Background(), dir, pub); err == nil {
		t.Error("view with an evidence file of another shape succeeded")
	}
}

// TestDemoServes: without -fast the command serves the finished run's page on the loopback address
// it prints, and exits 0 when interrupted.
func TestDemoServes(t *testing.T) {
	skipNoSelfInterrupt(t)
	if testing.Short() {
		t.Skip("the narration pauses between steps")
	}
	guard := make(chan os.Signal, 8)
	signal.Notify(guard, os.Interrupt)
	defer signal.Stop(guard)
	var out syncBuf
	done := make(chan int, 1)
	go func() { done <- Main([]string{"-no-browser"}, []string{"demo"}, &out, &out) }()
	deadline := time.Now().Add(2 * time.Minute)
	for !strings.Contains(out.String(), "(Ctrl-C to quit)") {
		select {
		case code := <-done:
			t.Fatalf("Main returned %d before serving:\n%s", code, out.String())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("never served:\n%s", out.String())
		}
	}
	text := out.String()
	i := strings.Index(text, "http://")
	link := strings.Fields(text[i:])[0]
	if !strings.HasSuffix(link, "/run?id="+runID) {
		t.Errorf("link = %q", link)
	}
	resp, err := http.Get(link)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "reconciled by ResolveHalt") {
		t.Errorf("GET %s = %d:\n%s", link, resp.StatusCode, body)
	}
	for {
		interruptSelf(t)
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("exit %d after interrupt\n%s", code, out.String())
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

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

// TestDemoFailsInUnusableDirectory: the demo reports a directory it cannot write to rather than
// narrating a story it cannot tell.
func TestDemoFailsInUnusableDirectory(t *testing.T) {
	var out bytes.Buffer
	res, err := demo(context.Background(), filepath.Join(t.TempDir(), "gone"), config{}, newPrinter(&out, term.Never), true)
	if err == nil {
		res.close()
		t.Fatal("demo in a missing directory succeeded")
	}
	if strings.Contains(out.String(), "charged exactly once") {
		t.Errorf("the demo claimed success:\n%s", out.String())
	}
}

// TestDemoStopsOnCancel: a cancelled context ends the demo with an error before it can claim a
// single charge.
func TestDemoStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	res, err := demo(ctx, t.TempDir(), config{}, newPrinter(&out, term.Never), true)
	if err == nil {
		res.close()
		t.Fatalf("a cancelled demo succeeded:\n%s", out.String())
	}
	if strings.Contains(out.String(), "charged exactly once") {
		t.Errorf("the demo claimed success:\n%s", out.String())
	}
}

// TestVerifiersDemo: an approver resolves to its key; an unknown approver or unparsable key text
// resolves to nothing.
func TestVerifiersDemo(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	v := verifiers(map[string]string{"ops": audit.FormatPublicKey(audit.AlgEd25519, pub), "junk": "nope"})
	if got, ok := v("ops"); !ok || got == nil {
		t.Error("ops does not resolve")
	}
	for _, id := range []string{"finance", "junk"} {
		if got, ok := v(id); ok || got != nil {
			t.Errorf("%s resolved to %v", id, got)
		}
	}
}

// TestViewOverDamagedDirectory: the verification step run over a finished demo directory. With the
// right log key everything verifies; with another key the proofs fail but view still reports them
// (the demo then refuses to claim success); a missing input file is an error, not a pass.
func TestViewOverDamagedDirectory(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	res, err := demo(context.Background(), dir, config{}, newPrinter(&out, term.Never), true)
	if err != nil {
		t.Fatalf("demo: %v\n%s", err, out.String())
	}
	res.close()
	seed, err := os.ReadFile(filepath.Join(dir, logKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := hex.DecodeString(strings.TrimSpace(string(seed)))
	logPub := ed25519.NewKeyFromSeed(raw).Public().(ed25519.PublicKey)

	r, err := view(context.Background(), dir, logPub)
	if err != nil || !r.proof.OK || !r.anchored.OK || !r.approvals.OK || r.logPub != audit.FormatPublicKey(audit.AlgEd25519, logPub) {
		t.Fatalf("view with the log key: %+v, %v", r, err)
	}
	r.close()

	other, _, _ := ed25519.GenerateKey(rand.Reader)
	r, err = view(context.Background(), dir, other)
	if err != nil {
		t.Fatal(err)
	}
	if r.proof.OK || r.anchored.OK {
		t.Errorf("proofs verified under a key that did not sign them: evidence %v, anchored %v", r.proof.OK, r.anchored.OK)
	}
	r.close()

	for _, name := range []string{keysFile, anchorFile, journalFile, evidenceFile} {
		path := filepath.Join(dir, name)
		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if r, err := view(context.Background(), dir, logPub); err == nil {
			r.close()
			t.Errorf("view without %s succeeded", name)
		}
		if err := os.WriteFile(path, saved, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(dir, anchorFile), "")
	if r, err := view(context.Background(), dir, logPub); err == nil {
		r.close()
		t.Error("view with an empty anchor succeeded")
	}
}
