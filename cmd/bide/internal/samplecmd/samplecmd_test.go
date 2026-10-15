package samplecmd

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/internal/sample"
	"github.com/bide-ai/bide/store/sqlite"
)

var update = flag.Bool("update", false, "rewrite testdata/runs.golden")

// runsOf reads the run list of dir's journal as bide view serves it.
func runsOf(t *testing.T, dir string) runview.RunList {
	t.Helper()
	s, err := sqlite.OpenReadOnly(filepath.Join(dir, journalFile))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j, err := agent.NewJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	return listOf(t, runview.Handler(runview.Options{Source: runview.JournalSource{J: j}}))
}

func listOf(t *testing.T, h http.Handler) runview.RunList {
	t.Helper()
	var l runview.RunList
	rec := &recorder{}
	h.ServeHTTP(rec, mustGet("/runs.json"))
	if rec.code != 200 || json.Unmarshal(rec.body.Bytes(), &l) != nil {
		t.Fatalf("GET /runs.json = %d: %s", rec.code, rec.body.String())
	}
	return l
}

// TestRunSet: bide sample -out dir writes the expected run set: each run's ID, kind, state and
// record count. The golden file is testdata/runs.golden (go test -update rewrites it).
func TestRunSet(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := WriteFiles(dir, false); err != nil {
		t.Fatal(err)
	}
	l := runsOf(t, dir)
	if l.Err != "" {
		t.Fatal(l.Err)
	}
	var b strings.Builder
	for _, r := range l.Rows {
		fmt.Fprintf(&b, "%s\tkind=%s\tstate=%s\tgroup=%s\trecords=%d\n", r.ID, r.Kind, r.State, r.Group, r.Records)
	}
	got := b.String()
	golden := filepath.Join("testdata", "runs.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (go test -update writes it)", err)
	}
	// A Windows checkout may hold the golden with CRLF line endings.
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Fatalf("the sample's runs changed (go test -update rewrites the golden if that is intended):\n%s\nwant:\n%s", got, want)
	}
}

// entries is every entry of every run of dir's journal, in order, as text, without the random
// salts and claim IDs the runtime draws (and the SQLite file itself holds the freed pages of lease
// rows, with random holders).
func entries(t *testing.T, dir string) string {
	t.Helper()
	s, err := sqlite.OpenReadOnly(filepath.Join(dir, journalFile))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var b strings.Builder
	for _, r := range runsOf(t, dir).Rows {
		for e, err := range s.Load(context.Background(), r.ID, -1) {
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s\t%d\t%s\t%s\n", r.ID, e.Seq, e.Name, sample.Unsalted(e.Data))
		}
	}
	return b.String()
}

// TestDeterministic: two writes produce the same files and the same journal entries, byte for
// byte but for the random salts and claim IDs: run IDs, times, kinds, states and the record
// sequence. The evidence packages hold digests over salted records, so they differ.
func TestDeterministic(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	_, names, err := WriteFiles(a, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteFiles(b, false); err != nil {
		t.Fatal(err)
	}
	if x, y := entries(t, a), entries(t, b); x != y || len(x) < 10000 {
		t.Errorf("the journal entries differ between two writes (%d and %d bytes)", len(x), len(y))
	}
	for _, name := range names {
		if name == journalFile || strings.HasSuffix(name, ".evidence.json") {
			continue
		}
		x, err := os.ReadFile(filepath.Join(a, name))
		if err != nil {
			t.Fatal(err)
		}
		y, err := os.ReadFile(filepath.Join(b, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(x, y) {
			t.Errorf("%s differs between two writes", name)
		}
	}
	if !slices.Contains(names, journalFile) || len(names) < 9 {
		t.Errorf("files written: %v", names)
	}
}

// TestForce: a second write into the same directory refuses to replace the journal, and -force
// replaces it with the same entries.
func TestForce(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := WriteFiles(dir, false); err != nil {
		t.Fatal(err)
	}
	first := entries(t, dir)
	if _, _, err := WriteFiles(dir, false); err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("second write = %v, want an error naming -force", err)
	}
	if _, _, err := WriteFiles(dir, true); err != nil {
		t.Fatal(err)
	}
	if again := entries(t, dir); first != again {
		t.Error("-force wrote a different journal")
	}
}

// TestUsage: a stray argument and an unknown flag are usage errors.
func TestUsage(t *testing.T) {
	for _, args := range [][]string{{"stray"}, {"-nope"}} {
		if code := Main(args, io.Discard, io.Discard); code != 2 {
			t.Errorf("bide sample %v = %d, want 2", args, code)
		}
	}
	if code := Main([]string{"-h"}, io.Discard, io.Discard); code != 0 {
		t.Errorf("bide sample -h = %d, want 0", code)
	}
}

// syncBuffer is a bytes.Buffer safe for the server goroutine to write while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var viewerLine = regexp.MustCompile(`viewer\s+(http://\S+/)`)

// serveWait is how long the tests give bide sample to write its files and start serving: it
// writes 14 runs through an audited store, and a loaded Windows runner needs far longer than a
// laptop does (the wait is on the viewer's address, so a fast machine never waits it out).
var serveWait = 2 * time.Minute

// running counts the bide sample commands start has begun and not seen return.
var running atomic.Int32

// start runs bide sample with args and returns the viewer's address once it prints it, and a stop
// that cancels the run and returns its exit status (idempotent). If the viewer does not come up
// within wait, or the command exits first, start stops the command, waits for it to return (so
// its files are closed) and returns an error: no path leaves bide sample running over the
// directory a test is about to remove.
func start(wait time.Duration, args ...string) (link string, stop func() int, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	old := notify
	notify = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	out := &syncBuffer{}
	done := make(chan int, 1)
	running.Add(1)
	// The count drops before the status is sent, so whoever receives the status sees the command
	// as no longer running (a deferred decrement would run after the send).
	go func() { code := Main(args, out, out); running.Add(-1); done <- code }()
	var once sync.Once
	var code int
	stop = func() int {
		once.Do(func() { cancel(); code = <-done; notify = old })
		return code
	}
	deadline := time.Now().Add(wait)
	for {
		if m := viewerLine.FindStringSubmatch(out.String()); m != nil {
			return m[1], stop, nil
		}
		select {
		case c := <-done:
			once.Do(func() { code = c; cancel(); notify = old })
			return "", stop, fmt.Errorf("bide sample exited %d before serving:\n%s", c, out.String())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			text := out.String()
			stop()
			return "", stop, fmt.Errorf("bide sample did not serve within %v:\n%s", wait, text)
		}
	}
}

// serve runs bide sample with args until stop, and returns the viewer's address. The command is
// also stopped when the test ends, however it ends, before the test's directory is removed.
func serve(t *testing.T, args ...string) (link string, stop func() int) {
	t.Helper()
	link, stop, err := start(serveWait, args...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop() })
	return link, stop
}

// A sample that does not come up in time is stopped, and its files are closed, before start
// returns: the directory can be removed at once, which Windows refuses while sample.db is open.
func TestStartFailureStopsTheCommand(t *testing.T) {
	dir := t.TempDir()
	_, stop, err := start(time.Nanosecond, "-out", filepath.Join(dir, "s"), "-no-browser", "-addr", "127.0.0.1:0")
	if err == nil {
		stop()
		t.Skip("the sample served within a nanosecond")
	}
	if !strings.Contains(err.Error(), "did not serve") {
		t.Fatalf("start = %v, want the did-not-serve error", err)
	}
	if n := running.Load(); n != 0 {
		t.Fatalf("%d bide sample command(s) still running after the failed start", n)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("the directory could not be removed after the failed start (is sample.db still open?): %v", err)
	}
	if code := stop(); code != 0 && code != 1 {
		t.Errorf("stop after the failure = %d", code)
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// TestServe: the viewer serves the sample read-only, and the two runs that have evidence show it
// verifying, the approved run's gate signatures too.
func TestServe(t *testing.T) {
	dir := t.TempDir()
	link, stop := serve(t, "-out", dir, "-no-browser", "-addr", "127.0.0.1:0")
	var l runview.RunList
	getJSON(t, link+"runs.json", &l)
	if len(l.Rows) != 14 {
		t.Errorf("run list holds %d runs, want 14", len(l.Rows))
	}
	for _, id := range []string{"refund-1041", "refund-1049"} {
		var v struct {
			State  string
			Proofs []runview.Proof `json:"proofs"`
			Proof  *runview.Proof  `json:"proof"`
		}
		getJSON(t, link+"run.json?id="+id, &v)
		p := v.Proof
		if p == nil && len(v.Proofs) > 0 {
			p = &v.Proofs[0]
		}
		if v.State != "completed" || p == nil || !p.OK {
			t.Errorf("%s: state %q, proof %+v; want completed and a verified proof", id, v.State, p)
		}
		if id == "refund-1049" && p != nil {
			if len(p.Approvals) == 0 {
				t.Errorf("%s: no approval check", id)
			}
			for call, a := range p.Approvals {
				if !a.OK {
					t.Errorf("%s: approvals of %s do not verify: %s", id, call, a.Text)
				}
			}
		}
	}
	// The page serves the sample's notification trail: a halt that is failing and overdue, a gate
	// that was acknowledged.
	var a runview.AttentionList
	getJSON(t, link+"attention.json", &a)
	if !a.Trail || a.TrailUnread != 0 {
		t.Errorf("the Needs attention list has no whole notify trail: %+v", a)
	}
	for _, it := range a.Items {
		switch {
		case it.RunID == "refund-1042" && (it.Notify == nil || it.Notify.State != runview.NotifyFailing || !it.Notify.Overdue):
			t.Errorf("refund-1042 notify = %+v, want failing and overdue", it.Notify)
		case it.RunID == "refund-1043" && (it.Notify == nil || it.Notify.State != runview.NotifyAcked):
			t.Errorf("refund-1043 notify = %+v, want acked", it.Notify)
		}
	}
	if code := stop(); code != 0 {
		t.Errorf("exit %d after the stop", code)
	}
}

// TestLive: with -live, one more run progresses to its gate and completes, and the others stay.
func TestLive(t *testing.T) {
	old := livePace
	livePace = 30 * time.Millisecond
	t.Cleanup(func() { livePace = old })
	dir := t.TempDir()
	link, stop := serve(t, "-out", dir, "-no-browser", "-live")
	defer stop()
	deadline := time.Now().Add(serveWait)
	for {
		var v struct{ State string }
		if resp, err := http.Get(link + "run.json?id=" + sample.LiveRun); err == nil {
			json.NewDecoder(resp.Body).Decode(&v)
			resp.Body.Close()
		}
		if v.State == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the live run is %q, want completed", v.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var l runview.RunList
	getJSON(t, link+"runs.json", &l)
	if len(l.Rows) != 15 {
		t.Errorf("run list holds %d runs, want the sample's 14 and the live one", len(l.Rows))
	}
}

// recorder is a minimal http.ResponseWriter.
type recorder struct {
	code int
	hdr  http.Header
	body bytes.Buffer
}

func (r *recorder) Header() http.Header {
	if r.hdr == nil {
		r.hdr = http.Header{}
	}
	return r.hdr
}
func (r *recorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	return r.body.Write(p)
}
func (r *recorder) WriteHeader(c int) { r.code = c }

func mustGet(target string) *http.Request {
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		panic(err)
	}
	req.Host = "127.0.0.1:8484"
	return req
}
