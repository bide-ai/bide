// Package samplecmd is bide sample: it writes a realistic, varied journal produced by the real
// runtime (internal/sample), with the evidence, keys and flow configs that go with it, and opens
// bide view on it. It needs no API key, no network and no code:
//
//	bide sample                       # writes ./sample.db and its files, then opens bide view
//	bide sample -out DIR -no-browser  # writes into DIR and prints the bide view address
//	bide sample -live                 # also keeps one run progressing while you watch
//
// The runs are the same every time (run IDs, states, timestamps, records), but for the random salt
// each record carries, so digests differ. Only the -live run uses real time. The page is served read-only, on a loopback address.
package samplecmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/internal/sample"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/store/sqlite"
)

// The files bide sample writes, relative to its directory.
const (
	journalFile = "sample.db"
	keysFile    = "approver-keys.json"
	logKeyFile  = "log.pub"
	readmeFile  = "README.txt"
	flowsDir    = "flows"
)

// livePace is how long each step of the -live run takes; notify returns the context a stop signal
// cancels. Tests replace both.
var (
	livePace = 4 * time.Second
	notify   = func() (context.Context, context.CancelFunc) {
		return signal.NotifyContext(context.Background(), os.Interrupt)
	}
)

// Main runs bide sample with args (after "sample") and returns the exit status: 0 after a clean
// stop or -h, 1 if the sample could not be written or served, 2 for a usage error.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bide sample", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: bide sample [-out <dir>] [-no-browser] [-live] [-force]

Writes a sample journal into dir (default: the current directory) and opens bide view on it. No
API key, no network and no code needed: the runs are produced by the real runtime, driven by a
scripted model and fake tools. dir holds:

  sample.db             a SQLite journal of 14 runs: refund agent runs (completed, halted with an
                        unknown outcome, halted and resolved, waiting at a 2-of-3 approval gate,
                        approved, in progress, aborted, cancelled), plan flow runs, and a run of
                        durable steps only
  *.evidence.json       evidence packages for two runs, for bide audit and bide view -evidence
  log.pub               the public key the evidence is signed with
  approver-keys.json    the approvers' public keys, for bide view -approver-keys
  flows/                three flow configs for bide view -config, bide check and bide check -diff
  README.txt            what is inside, and the commands to try

The runs are the same on every call: the same run IDs, states, start times and records, apart
from the random salt each record carries (so the digests over the records, and the evidence that
holds them, differ). The keys come from fixed public seeds: do not use them for anything real. The page is served read-only on
a loopback address; stop it with Ctrl-C.

`)
		fs.PrintDefaults()
	}
	out := fs.String("out", ".", "directory to write the sample into (created if missing)")
	noBrowser := fs.Bool("no-browser", false, "print the viewer's address instead of opening a browser")
	live := fs.Bool("live", false, "also keep one run, "+sample.LiveRun+", progressing while you watch: it pauses at its approval gate, gets its two approvals and completes (about 20 seconds)")
	force := fs.Bool("force", false, "replace an existing "+journalFile)
	addr := fs.String("addr", "127.0.0.1:0", "loopback address to serve the viewer on (port 0 picks a free one)")
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "bide sample: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	st := term.For(stdout, *color)
	fail := func(err error) int {
		fmt.Fprintln(stderr, "bide sample:", err)
		return 1
	}

	res, names, err := WriteFiles(*out, *force)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "%s  %d runs written by the real runtime\n", st.Bold("bide sample"), len(res.Runs))
	for _, f := range names {
		fmt.Fprintf(stdout, "  %s\n", st.Path(filepath.Join(*out, f)))
	}
	fmt.Fprintln(stdout, st.Dim("  README.txt lists what is inside and the commands to try"))

	dbPath := filepath.Join(*out, journalFile)
	rs, err := sqlite.OpenReadOnly(dbPath)
	if err != nil {
		return fail(err)
	}
	defer rs.Close()
	j, err := agent.NewJournal(rs)
	if err != nil {
		return fail(err)
	}
	logV, err := audit.ParsePublicKey(res.LogPub)
	if err != nil {
		return fail(err)
	}
	ap := runview.Approvals{Keys: func(id string) (agent.ApproverVerifier, bool) {
		k, ok := res.ApproverKeys[id]
		if !ok {
			return nil, false
		}
		v, err := audit.ParsePublicKey(k)
		return v, err == nil
	}}
	opts := runview.Options{Source: runview.JournalSource{J: j}, Events: rs.EventStore()}
	for _, pkg := range res.Evidence {
		opts.Provers = append(opts.Provers, runview.EvidenceProver(pkg, logV, ap))
	}
	ln, err := runview.Listen(*addr)
	if err != nil {
		return fail(err)
	}
	link := "http://" + ln.Addr().String() + "/"
	fmt.Fprintf(stdout, "\n%s  %s\n", st.Bold("viewer"), st.Path(link))
	fmt.Fprintln(stdout, st.Dim("        (read-only; Ctrl-C to quit)"))
	if *live {
		fmt.Fprintf(stdout, "%s   %s\n", st.Bold("live"), st.Path(link+"run?id="+sample.LiveRun))
	}
	if !*noBrowser {
		openBrowser(link)
	}

	ctx, stop := notify()
	defer stop()
	if *live {
		ws, err := sqlite.Open(dbPath)
		if err != nil {
			return fail(err)
		}
		defer ws.Close()
		go func() {
			if err := sample.Live(ctx, ws, livePace); err != nil && ctx.Err() == nil {
				fmt.Fprintln(stderr, "bide sample: live run:", err)
			}
		}()
	}
	srv := &http.Server{Handler: runview.Handler(opts), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fail(err)
	}
	return 0
}

// WriteFiles writes the sample's files into dir, and returns what the runs made and the names of
// the files, relative to dir. An existing journal is an error unless force replaces it.
func WriteFiles(dir string, force bool) (*sample.Result, []string, error) {
	db := filepath.Join(dir, journalFile)
	if _, err := os.Stat(db); err == nil {
		if !force {
			return nil, nil, fmt.Errorf("%s exists; remove it, pick another -out, or pass -force to replace it", db)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(db + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, nil, err
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, flowsDir), 0o755); err != nil {
		return nil, nil, err
	}
	s, err := sqlite.Open(db)
	if err != nil {
		return nil, nil, err
	}
	res, err := sample.Write(context.Background(), s, "")
	if err == nil {
		err = writeTrail(s)
	}
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, nil, err
	}
	files := map[string][]byte{
		logKeyFile: []byte(res.LogPub + "\n"),
		readmeFile: []byte(readme(res)),
	}
	for run, pkg := range res.Evidence {
		if files[run+".evidence.json"], err = jsonMarshal(pkg); err != nil {
			return nil, nil, err
		}
	}
	if files[keysFile], err = jsonMarshal(res.ApproverKeys); err != nil {
		return nil, nil, err
	}
	entries, err := sample.Flows.ReadDir("flows")
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		b, err := sample.Flows.ReadFile("flows/" + e.Name())
		if err != nil {
			return nil, nil, err
		}
		files[filepath.Join(flowsDir, e.Name())] = b
	}
	names := []string{journalFile}
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names[1:])
	for _, name := range names[1:] {
		if err := os.WriteFile(filepath.Join(dir, name), files[name], 0o644); err != nil {
			return nil, nil, err
		}
	}
	return res, names, nil
}

// writeTrail runs the notifier's sweeps over the sample journal in s, into the notification trail
// of the same database (see sample.WriteNotifyTrail).
func writeTrail(s *sqlite.Store) error {
	j, err := agent.NewJournal(s)
	if err != nil {
		return err
	}
	return sample.WriteNotifyTrail(context.Background(), j, s.EventStore(), "")
}

// readme is README.txt: what the directory holds and the commands to try, from its directory.
func readme(res *sample.Result) string {
	var b strings.Builder
	b.WriteString(`This directory was written by bide sample. Everything in it was produced by the real
bide runtime, driven by a scripted model and fake tools: no API key, no network, no
hand-written records. bide sample writes the same runs every time (run IDs, states, times and
records), but for the random salt each record carries, so the digests over them differ.

DO NOT USE THESE KEYS FOR ANYTHING REAL. They come from fixed seeds that are public (in the bide
source), and they sign nothing but this sample.

RUNS in sample.db

`)
	for _, r := range res.Runs {
		fmt.Fprintf(&b, "  %-15s %s\n", r.ID, r.Note)
	}
	b.WriteString(`
FILES

  sample.db                    the journal (SQLite), with the notification trail of a notifier's two sweeps
  refund-1041.evidence.json    evidence for refund-1041: the completed refund, proven
  refund-1049.evidence.json    evidence for refund-1049, with its gate's two signatures
  log.pub                      the public key both evidence packages are signed with
  approver-keys.json           the approvers' (ops, finance, risk) public keys
  flows/refund-v1.yaml         the refund flow: review needs 2 of 3 approvers; 4 assertions hold
  flows/refund-v2.yaml         the same flow with review lowered to 1 of 3: a weakened gate
  flows/refund-bypass.yaml     a vip arm skips review for an ungated charge step: a violated assertion

TRY (from this directory)

  Browse the runs (the run list, then a run page); its Needs attention list shows what was
  notified, acknowledged, failing and overdue:
    bide view -db sample.db

  One run, with its evidence and its gate's signatures verified:
    bide view -db sample.db -run refund-1049 -evidence refund-1049.evidence.json -pubkey log.pub -approver-keys approver-keys.json

  Verify the evidence offline, with no store and no server:
    bide audit verify-evidence -evidence refund-1041.evidence.json -pubkey log.pub
    bide audit verify-evidence -evidence refund-1049.evidence.json -pubkey log.pub

  The cap ledger the two refunds above are counted in (500 USD a day, 240 USD used), audited with
  no signed head to compare against:
    bide audit caps -db sample.db -unanchored

  A flow config: its graph, its crash-behavior colors and its assertions:
    bide view -config flows/refund-v1.yaml

  Check a flow config, for CI (exit 1 on the violated assertion, with a counterexample path):
    bide check flows/refund-v1.yaml
    bide check flows/refund-bypass.yaml

  Compare two versions of a flow (exit 1 when a gate was weakened):
    bide check flows/refund-v2.yaml -diff flows/refund-v1.yaml
    bide check flows/refund-v2.yaml -diff flows/refund-v1.yaml -check -fail-on-weaken

  Watch a run progress, in live mode: bide sample -live

Guides: https://bide-ai.com (the run viewer guide covers each page).
`)
	return b.String()
}

// openBrowser opens url in the default browser, best effort.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

func jsonMarshal(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	return append(b, '\n'), err
}
