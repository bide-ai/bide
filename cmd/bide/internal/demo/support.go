package demo

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/notify"
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
	as.OnError(func(runID string, err error) { fmt.Fprintf(os.Stderr, "bide demo: anchoring %s: %v\n", runID, err) })
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

// view opens the journal at journalPath read-only, as bide view does, and checks the run against
// dir's exported evidence, the approvers' public keys and the last head the audited store anchored.
func view(ctx context.Context, dir, journalPath string, logPub ed25519.PublicKey) (*result, error) {
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
	s, err := sqlite.OpenReadOnly(journalPath)
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
		return p.st.Pass() + p.green(fmt.Sprintf("verified (%d records)", pr.Covers))
	}
	return p.red(pr.Summary)
}

// approvalText is the gate's signature check: "2 of 3: signatures verified" when it holds, and the
// page's full text (in red) when it does not.
func approvalText(p *printer, a runview.ApprovalCheck) string {
	if a.OK {
		text, _, _ := strings.Cut(a.Text, " (")
		return p.st.Pass() + p.green(text)
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

// readStrict reads an audit artifact as bide view and bide audit do (audit.UnmarshalStrict).
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

// printer writes the demo's narration, colored as term.For decides (a terminal, NO_COLOR, -color).
// Each step has its color: the crash red, the halt amber, the resolution blue, the proof green.
type printer struct {
	out io.Writer
	st  term.Style
}

func newPrinter(out io.Writer, m term.Mode) *printer { return &printer{out: out, st: term.For(out, m)} }

func (p *printer) line(s string) { fmt.Fprintln(p.out, s) }

// step prints a numbered step's first line: the number and a short label in the step's color, and
// what happens.
func (p *printer) step(n int, label string, color func(string) string, what string) {
	p.line("")
	pad := ""
	if len(label) > 7 {
		pad = " " // a label wider than the column still keeps two spaces from the text
	}
	p.line(fmt.Sprintf("%s %s%s %s", color(fmt.Sprint(n)), p.bold(color(fmt.Sprintf("%-7s", label))), pad, what))
}

// more prints a line under the current step, aligned with its text.
func (p *printer) more(s string) { p.line(strings.Repeat(" ", 10) + s) }

func (p *printer) fatal(err error) {
	p.line("")
	p.line(p.st.Cross() + p.red(p.bold("demo failed: ")) + err.Error())
	os.Exit(1)
}

func (p *printer) bold(s string) string   { return p.st.Bold(s) }
func (p *printer) dim(s string) string    { return p.st.Dim(s) }
func (p *printer) red(s string) string    { return p.st.Fail(s) }
func (p *printer) green(s string) string  { return p.st.OK(s) }
func (p *printer) yellow(s string) string { return p.st.Warn(s) }
func (p *printer) blue(s string) string   { return p.st.Blue(s) }
func (p *printer) cyan(s string) string   { return p.st.Cyan(s) }

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

// snapshot copies the journal in dir to <name>.db in the recorder's directory, and marks the event.
// It does nothing when the recorder is off.
func (r recorder) snapshot(dir, name string) error {
	if !r.on() {
		return nil
	}
	if err := r.mark(name); err != nil {
		return err
	}
	return copyJournal(filepath.Join(dir, journalFile), filepath.Join(r.dir, name+".db"))
}

// copyJournal writes a consistent copy of the SQLite journal src to dst (which must not exist),
// including what its write-ahead log holds, as a file copy of a journal a killed process left would
// not.
func copyJournal(src, dst string) error {
	db, err := sql.Open("sqlite", src)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec("VACUUM INTO '" + strings.ReplaceAll(dst, "'", "''") + "'")
	return err
}

// withoutBide replays the crash without bide, for contrast: an agent with no journal charges, dies
// after the provider took the charge, and on restart has nothing that says it already did, so it
// charges again. It uses a fake provider of its own (in dir/naive) and returns the charges it
// holds at the end.
func withoutBide(dir string, p *printer, pause func(time.Duration)) (int, error) {
	pdir := filepath.Join(dir, naiveDir)
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		return 0, err
	}
	p.step(0, "Without bide", p.red, "restart retries the charge")
	pause(700 * time.Millisecond)
	in := chargeArgs{Order: order, Amount: amount}
	first, err := providerCharge(pdir, in)
	if err != nil {
		return 0, err
	}
	p.more(fmt.Sprintf("charged $%d (%s), then the process died", amount, first.ID))
	pause(1000 * time.Millisecond)
	// The restarted process has no journal: it knows nothing of the first attempt, so it asks again.
	if _, err := providerCharge(pdir, in); err != nil {
		return 0, err
	}
	all, err := providerCharges(pdir)
	if err != nil {
		return 0, err
	}
	p.more(p.red(fmt.Sprintf("provider charges: %d", len(all))) + "  " + p.st.Cross() + p.red(p.bold("double charge")))
	if len(all) != 2 {
		return 0, fmt.Errorf("the run without bide left %d charges, want 2", len(all))
	}
	return len(all), nil
}

// receiver is the in-process HTTPS endpoint the demo's webhook notifier delivers to. It accepts a
// delivery only if its HMAC signature verifies (notify.Verify, as a real receiver would).
type receiver struct {
	srv      *httptest.Server
	secret   string
	notifier agent.Notifier
	got      chan notify.Payload
	rejected chan error
}

func newReceiver() (*receiver, error) {
	var seed [16]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	r := &receiver{secret: hex.EncodeToString(seed[:]), got: make(chan notify.Payload, 8), rejected: make(chan error, 8)}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := notify.Verify(req, []string{r.secret}, 0); err != nil {
			r.rejected <- err
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		var pl notify.Payload
		if err := json.NewDecoder(req.Body).Decode(&pl); err != nil {
			r.rejected <- err
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		r.got <- pl
		w.WriteHeader(http.StatusNoContent)
	}))
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.srv.Certificate().Raw})
	w, err := notify.NewWebhook(r.srv.URL, notify.WithSecrets(r.secret, ""), notify.WithServerCAPEM(ca))
	if err != nil {
		r.srv.Close()
		return nil, err
	}
	r.notifier = w
	return r, nil
}

func (r *receiver) close() { r.srv.Close() }

// verified returns the halt notification the receiver accepted, and fails if it received none or
// refused one.
func (r *receiver) verified() (*notify.Payload, error) {
	select {
	case err := <-r.rejected:
		return nil, fmt.Errorf("the receiver refused the halt notification: %w", err)
	default:
	}
	select {
	case pl := <-r.got:
		if pl.Kind != string(agent.NotificationOutcomeUnknown) || pl.RunID != runID {
			return nil, fmt.Errorf("the receiver got a %s notification for %s, want outcome_unknown for %s", pl.Kind, pl.RunID, runID)
		}
		return &pl, nil
	default:
		return nil, errors.New("the receiver got no halt notification")
	}
}

// tamper changes the amount of the charge request ($120 to $1,200) in a copy of the journal,
// leaving the journal itself alone, and checks the copy as bide view does against the evidence
// and the anchored head, which sign the original. It fails if the copy still verifies.
func tamper(ctx context.Context, dir string, logPub ed25519.PublicKey, res *result, p *printer, pause func(time.Duration)) error {
	copyPath := filepath.Join(dir, tamperedFile)
	if err := copyJournal(filepath.Join(dir, journalFile), copyPath); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", copyPath)
	if err != nil {
		return err
	}
	var seq int
	err = db.QueryRow(`SELECT seq FROM bide_steps WHERE run_id = ? AND name = '@llm/0'`, runID).Scan(&seq)
	if err == nil {
		var r sql.Result
		r, err = db.Exec(`UPDATE bide_steps SET data = replace(data, ?, ?) WHERE seq = ?`,
			fmt.Sprintf(`"amount":%d}`, amount), fmt.Sprintf(`"amount":%d}`, amount*10), seq)
		if err == nil {
			if n, _ := r.RowsAffected(); n != 1 {
				err = fmt.Errorf("the charge request in the copy holds no amount of %d", amount)
			}
		}
	}
	db.Close()
	if err != nil {
		return err
	}
	p.step(6, "Tamper", p.red, fmt.Sprintf("change record %d: $%d \u2192 $%s", seq, amount, thousands(amount*10)))
	pause(900 * time.Millisecond)
	bad, err := view(ctx, dir, copyPath, logPub)
	if err != nil {
		return err
	}
	bad.close()
	res.tampered, res.tamperedSeq = bad.proof, seq
	if bad.proof.OK {
		return errors.New("the evidence still verifies against the tampered journal")
	}
	if len(bad.proof.Problems) == 0 {
		return errors.New("the tampered journal failed verification without a reason")
	}
	// The viewer's proof carries its problems as text, so the typed error comes from the same call
	// it makes: the inclusion proof of the last record under the anchored head.
	perr := proveAnchored(ctx, dir, copyPath)
	res.tamperErr = perr
	p.more(p.st.Cross() + p.red(p.bold("verify failed")+verdictReason(perr)))
	pause(500 * time.Millisecond)
	why := bad.proof.Problems[0]
	if perr != nil {
		why = perr.Error()
	}
	why = strings.TrimPrefix(why, "the signed head does not commit to this journal: ")
	why, _, _ = strings.Cut(strings.TrimPrefix(why, "audit: "), " (")
	p.more(p.dim(shorten(why, 60)))
	return nil
}

// proveAnchored is the check at the heart of the viewer's proof: the inclusion proof of the last
// record the anchored head covers, built from the journal at journalPath. It returns its error.
func proveAnchored(ctx context.Context, dir, journalPath string) error {
	sth, err := fileAnchor(filepath.Join(dir, anchorFile)).latest()
	if err != nil {
		return err
	}
	s, err := sqlite.OpenReadOnly(journalPath)
	if err != nil {
		return err
	}
	defer s.Close()
	j, err := agent.NewJournal(s)
	if err != nil {
		return err
	}
	_, err = audit.ProveRecord(ctx, j, runID, sth.Size-1, sth)
	return err
}

// verdictReason is a plain-language reason for a failed verification, chosen by the error's type:
// empty (just "verify failed") for an error it has no wording for.
func verdictReason(err error) string {
	if errors.Is(err, audit.ErrRootMismatch) {
		return ": journal changed since it was signed"
	}
	return ""
}

// thousands formats n with a comma between thousands.
func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// shorten cuts s to at most n runes, ending in an ellipsis when it cuts.
func shorten(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "\u2026"
}
