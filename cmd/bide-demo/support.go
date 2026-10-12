package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/store/sqlite"
)

// openJournal opens dir's SQLite journal behind an audited store: every write is followed by a
// signed tree head, published to anchorFile. Close the returned func when done.
func openJournal(dir string) (*agent.Journal, func(), error) {
	seed, err := os.ReadFile(filepath.Join(dir, logKeyFile))
	if err != nil {
		return nil, nil, err
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(seed)))
	if err != nil || len(b) != ed25519.SeedSize {
		return nil, nil, fmt.Errorf("%s: not an ed25519 seed", logKeyFile)
	}
	s, err := sqlite.Open(filepath.Join(dir, journalFile))
	if err != nil {
		return nil, nil, err
	}
	as, err := audit.NewAuditedStore(s, audit.Ed25519Signer{Priv: ed25519.NewKeyFromSeed(b)}, fileAnchor(filepath.Join(dir, anchorFile)))
	if err != nil {
		s.Close()
		return nil, nil, err
	}
	as.OnError(func(runID string, err error) { fmt.Fprintf(os.Stderr, "bide-demo: anchoring %s: %v\n", runID, err) })
	j, err := agent.NewJournal(as)
	if err != nil {
		s.Close()
		return nil, nil, err
	}
	return j, func() { s.Close() }, nil
}

// fileAnchor is an audit.Anchor that appends each signed tree head to a file, one JSON line each:
// a stand-in for an external transparency log that outlives the process that wrote it.
type fileAnchor string

var anchorMu sync.Mutex

// Publish appends sth to the file.
func (a fileAnchor) Publish(_ context.Context, runID string, sth audit.SignedTreeHead) error {
	b, err := json.Marshal(sth)
	if err != nil {
		return err
	}
	anchorMu.Lock()
	defer anchorMu.Unlock()
	f, err := os.OpenFile(string(a), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// latest returns the last head the anchor holds.
func (a fileAnchor) latest() (audit.SignedTreeHead, error) {
	var sth audit.SignedTreeHead
	b, err := os.ReadFile(string(a))
	if err != nil {
		return sth, err
	}
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	if len(lines) == 0 || len(lines[0]) == 0 {
		return sth, errors.New("the anchor holds no signed tree head")
	}
	return sth, json.Unmarshal(lines[len(lines)-1], &sth)
}

// chargeArgs is what the model passes the charge tool.
type chargeArgs struct {
	Order  int `json:"order"`
	Amount int `json:"amount"`
}

// charge is one charge the fake payment provider holds.
type charge struct {
	ID     string `json:"id"`
	Order  int    `json:"order"`
	Amount int    `json:"amount"`
	Status string `json:"status"`
}

// receipt is what the charge tool returns for c.
func receipt(c charge) string {
	return fmt.Sprintf("charged $%d for order %d (%s)", c.Amount, c.Order, c.ID)
}

// providerCharge charges at the fake provider: it appends a charge to providerFile and returns it.
// Like a real provider without an idempotency key, it charges again every time it is called.
func providerCharge(dir string, in chargeArgs) (charge, error) {
	all, err := providerCharges(dir)
	if err != nil {
		return charge{}, err
	}
	c := charge{ID: fmt.Sprintf("ch_%d", len(all)+1), Order: in.Order, Amount: in.Amount, Status: "succeeded"}
	b, err := json.Marshal(c)
	if err != nil {
		return charge{}, err
	}
	f, err := os.OpenFile(filepath.Join(dir, providerFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return charge{}, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return charge{}, err
	}
	return c, f.Close()
}

// providerCharges lists the charges the fake provider holds.
func providerCharges(dir string) ([]charge, error) {
	f, err := os.Open(filepath.Join(dir, providerFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []charge
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c charge
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, sc.Err()
}

// verifiers resolves approver ids to the public keys in keys (approver id -> key text).
func verifiers(keys map[string]string) agent.ApproverVerifierFor {
	return func(id string) (agent.ApproverVerifier, bool) {
		k, ok := keys[id]
		if !ok {
			return nil, false
		}
		v, err := audit.ParsePublicKey(k)
		if err != nil {
			return nil, false
		}
		return v, true
	}
}

// view opens dir's journal read-only, as bide-view does, and checks the run against the exported
// evidence, the approvers' public keys and the last head the audited store anchored.
func view(ctx context.Context, dir string, logPub ed25519.PublicKey) (*result, error) {
	var pkg audit.EvidencePackage
	if err := readStrict(filepath.Join(dir, evidenceFile), &pkg); err != nil {
		return nil, err
	}
	var keys map[string]string
	if err := readStrict(filepath.Join(dir, keysFile), &keys); err != nil {
		return nil, err
	}
	anchored, err := fileAnchor(filepath.Join(dir, anchorFile)).latest()
	if err != nil {
		return nil, err
	}
	logV := audit.Ed25519Verifier{Pub: logPub}
	ap := runview.Approvals{Keys: verifiers(keys)}
	s, err := sqlite.OpenReadOnly(filepath.Join(dir, journalFile))
	if err != nil {
		return nil, err
	}
	j, err := agent.NewJournal(s)
	if err != nil {
		s.Close()
		return nil, err
	}
	recs, err := j.History(ctx, runID)
	if err != nil {
		s.Close()
		return nil, err
	}
	res := &result{
		handler: runview.Handler(runview.Options{
			Source:  runview.JournalSource{J: j},
			Provers: []runview.Prover{runview.EvidenceProver(pkg, logV, ap)},
		}),
		close:  func() { s.Close() },
		logPub: audit.FormatPublicKey(audit.AlgEd25519, logPub),
	}
	res.proof = runview.EvidenceProver(pkg, logV, ap)(ctx, runID, recs)
	res.anchored = runview.HeadProver(anchored, logV, runview.Approvals{})(ctx, runID, recs)
	res.approvals = res.proof.Approvals[callID]
	return res, nil
}

// proofText is a proof's verdict in a few words: what it covers when it verifies, and its summary
// (in red) when it does not.
func proofText(p *printer, pr *runview.Proof) string {
	if pr.OK {
		return p.green(fmt.Sprintf("verified (%d records)", pr.Covers))
	}
	return p.red(pr.Summary)
}

// approvalText is the gate's signature check: "2 of 3: signatures verified" when it holds, and the
// page's full text (in red) when it does not.
func approvalText(p *printer, a runview.ApprovalCheck) string {
	if a.OK {
		text, _, _ := strings.Cut(a.Text, " (")
		return p.green(text)
	}
	return p.red(a.Text)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// readStrict reads an audit artifact as bide-view and bide-audit do (audit.UnmarshalStrict).
func readStrict(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return audit.UnmarshalStrict(b, v)
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

// printer writes the demo's narration, colored when out is a terminal and NO_COLOR is unset.
type printer struct {
	out   io.Writer
	color bool
}

func newPrinter(out io.Writer) *printer {
	p := &printer{out: out}
	if f, ok := out.(*os.File); ok && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			p.color = true
		}
	}
	return p
}

func (p *printer) line(s string) { fmt.Fprintln(p.out, s) }

// step prints a numbered step's first line: the number, a short label, and what happens.
func (p *printer) step(n int, label, what string) {
	p.line("")
	p.line(fmt.Sprintf("%s %s %s", p.cyan(fmt.Sprint(n)), p.bold(fmt.Sprintf("%-7s", label)), what))
}

// more prints a line under the current step, aligned with its text.
func (p *printer) more(s string) { p.line(strings.Repeat(" ", 10) + s) }

func (p *printer) fatal(err error) {
	p.line("")
	p.line(p.red(p.bold("demo failed: ")) + err.Error())
	os.Exit(1)
}

func (p *printer) sgr(code, s string) string {
	if !p.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p *printer) bold(s string) string   { return p.sgr("1", s) }
func (p *printer) dim(s string) string    { return p.sgr("2", s) }
func (p *printer) red(s string) string    { return p.sgr("31", s) }
func (p *printer) green(s string) string  { return p.sgr("32", s) }
func (p *printer) yellow(s string) string { return p.sgr("33", s) }
func (p *printer) cyan(s string) string   { return p.sgr("36", s) }

// recorder serves assets/demo/record.sh (see recordEnv); it does nothing when recordEnv is unset.
type recorder struct {
	dir   string
	start time.Time
}

func newRecorder() recorder { return recorder{dir: os.Getenv(recordEnv), start: time.Now()} }

func (r recorder) on() bool { return r.dir != "" }

// mark appends the event and the seconds since the demo started to marks.txt.
func (r recorder) mark(event string) error {
	if !r.on() {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(r.dir, "marks.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fmt.Fprintf(f, "%s %.2f\n", event, time.Since(r.start).Seconds())
	return f.Close()
}

// snapshot copies the closed journal in dir to <name>.db in the recorder's directory, and marks
// the event.
func (r recorder) snapshot(dir, name string) error {
	if err := r.mark(name); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(dir, journalFile))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.dir, name+".db"), b, 0o644)
}
