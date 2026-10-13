// Package demo is bide demo: it kills an agent mid-payment and shows that bide does not charge
// twice. It needs no API key, no network and no setup:
//
//	bide demo
//
// A payment agent on a scripted model (agenttest) wants to charge $120 for order 42. Its charge
// tool has the default Safety (a side effect) and a 2-of-3 approval gate (ops, finance, risk); the
// demo signs two approvals with keys it generates. The journal is SQLite in a temp directory,
// wrapped in an audited store that signs a tree head after every write. Then:
//
//  1. The agent runs in a child process. The charge reaches a local fake payment provider, and
//     the parent kills the child (os.Process.Kill) before the charge's result is journaled.
//  2. The run resumes from its journal. bide finds the charge's attempt marker with no result and
//     halts with agent.OutcomeUnknown instead of charging again.
//  3. The demo looks the charge up at the provider, records it with agent.ResolveHalt and its
//     evidence, and the run completes. The provider holds exactly one charge.
//  4. It exports an evidence package and the approvers' public keys, verifies them, and serves the
//     run on a loopback address in the bide view page (the timeline, the crashed attempt, the
//     resolution, the gate's signatures and the audit proof), and opens a browser on it.
//
// Flags: -no-browser prints the page's address without opening a browser, -fast drops the pauses
// (for CI), -addr sets the loopback address to serve on, -keep keeps the temp directory and prints
// it, with the bide view command that reopens the run, and -color colors the narration (by default
// on a terminal only).
package demo

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/internal/term"
)

const (
	runID  = "order-42"
	callID = "charge-1" // the tool-use id the scripted model gives its charge call
	amount = 120
	order  = 42
	input  = "Charge order 42 $120."

	// childEnv names the demo's directory in the environment of the child process that drives
	// the agent until it is killed.
	childEnv = "BIDE_DEMO_CHILD"

	// recordEnv names a directory for assets/demo/record.sh, which composes the README's
	// recording: the demo copies the halted journal there (halted.db) and appends the seconds since
	// it started at which the halt and the verified proof were printed (marks.txt).
	recordEnv = "BIDE_DEMO_RECORD"
)

// policy is the gate on the charge: 2 of these 3 approvers must sign off.
var policy = agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}

// The files the demo keeps in its directory.
const (
	journalFile  = "journal.db"
	providerFile = "provider.jsonl"     // the fake payment provider's charges, one JSON line each
	anchorFile   = "anchor.jsonl"       // every signed tree head the audited store published
	logKeyFile   = "log.key"            // the log operator's private key (hex seed), for the child
	keysFile     = "approver-keys.json" // approver id -> public key text
	evidenceFile = "evidence.json"
	configFile   = "config.json"
)

// config is what the parent hands the child through configFile.
type config struct {
	// RetrySafe declares the charge tool Idempotent, which it is not: bide then retries it after
	// the crash instead of halting, and the provider is charged twice. A test sets it to prove the
	// demo's checks catch a double charge.
	RetrySafe bool `json:"retry_safe,omitempty"`
}

// reexec is the command line (without the program) that runs the demo again, in the child process.
var reexec []string

// demoFlags are the demo's inputs, as parsed.
type demoFlags struct {
	noBrowser, fast, keep *bool
	addr                  *string
	color                 *term.Mode
}

func newFlags() (*flag.FlagSet, demoFlags) {
	fs := flag.NewFlagSet("bide demo", flag.ContinueOnError)
	var f demoFlags
	f.noBrowser = fs.Bool("no-browser", false, "print the viewer's address instead of opening a browser")
	f.fast = fs.Bool("fast", false, "no pauses between steps (for CI)")
	f.addr = fs.String("addr", "127.0.0.1:0", "loopback address to serve the viewer on (port 0 picks a free one)")
	f.keep = fs.Bool("keep", false, "keep the demo's directory (the journal, evidence and keys) and print it")
	f.color = term.Flag(fs)
	return fs, f
}

// Main runs the demo with the command line args (without the program and the subcommand) and
// returns the exit status: 0 after a clean stop or -h, 1 if the demo fails, 2 for a usage error.
// self is the command line that runs the demo again ("demo" for the bide program): the demo
// re-executes the program with it, and with the child's environment variable set, to drive the
// agent in a child process it can kill. Main is that child when the variable is set.
func Main(args, self []string, stdout, stderr io.Writer) int {
	if dir := os.Getenv(childEnv); dir != "" {
		return child(dir)
	}
	reexec = self
	fs, f := newFlags()
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "bide demo: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	noBrowser, fast, addr, keep := f.noBrowser, f.fast, f.addr, f.keep

	p := newPrinter(stdout, *f.color)
	dir, err := os.MkdirTemp("", "bide-demo-*")
	if err != nil {
		p.fatal(err)
	}
	if !*keep {
		defer os.RemoveAll(dir)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := demo(ctx, dir, config{}, p, *fast)
	if err != nil {
		p.fatal(err)
	}
	ln, err := runview.Listen(*addr)
	if err != nil {
		p.fatal(err)
	}
	link := "http://" + ln.Addr().String() + "/run?id=" + runID
	p.line("")
	p.line(p.bold("viewer") + "  " + p.st.Path(link))
	p.line(p.dim("        (Ctrl-C to quit)"))
	if *keep {
		p.line(p.dim(fmt.Sprintf("kept    %s\n        bide view -db %s -run %s -evidence %s -pubkey %s -approver-keys %s",
			dir, filepath.Join(dir, journalFile), runID, filepath.Join(dir, evidenceFile), res.logPub, filepath.Join(dir, keysFile))))
	}
	if !*noBrowser {
		openBrowser(link)
	}
	srv := &http.Server{Handler: res.handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		p.fatal(err)
	}
	res.close()
	return 0
}

// result is what a finished demo leaves for main to serve and for the test to check.
type result struct {
	handler   http.Handler   // the bide view page over the run, read-only
	close     func()         // closes the read-only journal the handler reads
	charges   int            // charges the provider holds
	state     string         // the run's end state
	proof     *runview.Proof // the exported evidence, checked as bide view -evidence does
	anchored  *runview.Proof // the last head the audited store anchored, as bide view -sth does
	approvals runview.ApprovalCheck
	logPub    string // the log public key, as bide view -pubkey takes it
}

// demo runs the whole story in dir and returns the viewer over the finished run. It fails unless
// the provider holds exactly one charge, the run completed, and the proof and the gate's
// signatures verify.
func demo(ctx context.Context, dir string, cfg config, p *printer, fast bool) (*result, error) {
	pause := func(d time.Duration) {
		if !fast {
			time.Sleep(d)
		}
	}
	rec := newRecorder()
	p.line(p.bold("bide crash demo") + p.dim("  no double charges"))
	pause(500 * time.Millisecond)

	// Keys: the log operator signs every tree head and the evidence; each approver signs their
	// decision. All are generated for this run and live only in dir.
	logPub, logPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	approverKeys := map[string]ed25519.PrivateKey{}
	keyText := map[string]string{}
	for _, id := range policy.Approvers {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		approverKeys[id], keyText[id] = priv, audit.FormatPublicKey(audit.AlgEd25519, pub)
	}
	if err := writeJSON(filepath.Join(dir, keysFile), keyText); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, logKeyFile), []byte(hex.EncodeToString(logPriv.Seed())), 0o600); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, configFile), cfg); err != nil {
		return nil, err
	}

	// 1. The agent asks to charge; the gate pauses it; two approvers sign.
	p.step(1, "Approve", p.cyan, fmt.Sprintf("the agent asks to charge $%d", amount))
	j, closeJ, err := openJournal(dir)
	if err != nil {
		return nil, err
	}
	_, err = newAgent(j, dir, cfg, nil).Run(ctx, runID, agent.UserText(input))
	pend, ok := errors.AsType[*agent.ApprovalPending](err)
	if !ok {
		closeJ()
		return nil, fmt.Errorf("the first run did not pause at the approval gate: %v", err)
	}
	p.more(fmt.Sprintf("gate %d of %d (%s)", policy.Need, len(policy.Approvers), strings.Join(policy.Approvers, ", ")))
	subject := agent.ApprovalSubject{RunID: runID, ToolUseID: pend.ToolUseID, ToolName: pend.ToolName, Args: pend.Args}
	for _, id := range []string{"ops", "finance"} {
		pause(500 * time.Millisecond)
		sig := ed25519.Sign(approverKeys[id], agent.ApprovalDecisionBytes(subject, id, true))
		d := agent.Decision{RunID: runID, ToolUseID: pend.ToolUseID, ApproverID: id, Approved: true, Alg: audit.AlgEd25519, Signature: sig}
		if err := agent.SubmitDecision(ctx, j, d); err != nil {
			closeJ()
			return nil, err
		}
		p.more(fmt.Sprintf("%-8s %s", id, p.green("approved, signed")))
	}
	closeJ()
	pause(600 * time.Millisecond)

	// 2. The agent runs in a child process; the parent kills it after the charge, before the
	// charge's result is journaled.
	if err := crash(ctx, dir, p, pause); err != nil {
		return nil, err
	}
	pause(1000 * time.Millisecond)

	// 3. The run resumes from its journal and halts: the charge's outcome is unknown.
	p.step(3, "Restart", p.yellow, "the run resumes from its journal")
	pause(700 * time.Millisecond)
	j, closeJ, err = openJournal(dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeJ != nil {
			closeJ()
		}
	}()
	_, err = newAgent(j, dir, cfg, nil).Run(ctx, runID, agent.UserText(input))
	halt, halted := errors.AsType[*agent.OutcomeUnknown](err)
	charges, cerr := providerCharges(dir)
	if cerr != nil {
		return nil, cerr
	}
	if !halted {
		return nil, fmt.Errorf("the resumed run did not halt with an unknown outcome (err: %v); the provider holds %d charges", err, len(charges))
	}
	p.more(p.yellow(p.bold("halted")) + p.yellow(": outcome unknown: not charging again"))
	p.more(fmt.Sprintf("provider charges: %d", len(charges)))
	if rec.on() {
		closeJ()
		if err := rec.snapshot(dir, "halted"); err != nil {
			return nil, err
		}
		if j, closeJ, err = openJournal(dir); err != nil {
			return nil, err
		}
	}
	pause(1300 * time.Millisecond)

	// 4. Look the charge up at the provider, record it, and finish the run.
	p.step(4, "Resolve", p.blue, fmt.Sprintf("look up order %d at the provider", order))
	pause(900 * time.Millisecond)
	var found *charge
	for i := range charges {
		if charges[i].Order == order {
			found = &charges[i]
		}
	}
	if found == nil {
		return nil, fmt.Errorf("the provider holds no charge for order %d", order)
	}
	p.more(fmt.Sprintf("charge %s found ($%d, %s)", p.bold(found.ID), found.Amount, found.Status))
	out := agent.Outcome{
		Result:   receipt(*found),
		Evidence: map[string]string{"provider_charge": found.ID, "status": found.Status, "source": "provider lookup by order"},
	}
	if err := agent.ResolveHalt(ctx, j, halt.Ref(), out); err != nil {
		return nil, err
	}
	pause(600 * time.Millisecond)
	p.more(p.blue(p.bold("resolved")) + " with ResolveHalt + evidence")
	if _, err := newAgent(j, dir, cfg, nil).Run(ctx, runID, agent.UserText(input)); err != nil {
		return nil, err
	}
	st, err := agent.Status(ctx, j, runID)
	if err != nil {
		return nil, err
	}
	p.more(p.blue("run " + string(st.State)))
	pause(1200 * time.Millisecond)

	// 5. Export the evidence and the approvers' public keys, and verify them as bide view does.
	p.step(5, "Prove", p.green, "evidence exported, signed")
	signer := audit.Ed25519Signer{Priv: logPriv}
	pkg, err := audit.Evidence(ctx, j, runID, signer, time.Now().UnixNano(), audit.WithToolCall(callID))
	if err != nil {
		return nil, err
	}
	decisions, err := audit.ApprovalEvidence(ctx, j, runID, callID, pkg.STH)
	if err != nil {
		return nil, err
	}
	// The call itself is already packaged. Reseal: the package changed after Evidence sealed it.
	pkg.Actions = append(pkg.Actions, decisions[:len(decisions)-1]...)
	if err := pkg.Seal(signer); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, evidenceFile), pkg); err != nil {
		return nil, err
	}
	closeJ()
	closeJ = nil

	res, err := view(ctx, dir, logPub)
	if err != nil {
		return nil, err
	}
	pause(700 * time.Millisecond)
	p.more("anchored  " + proofText(p, res.anchored))
	pause(500 * time.Millisecond)
	p.more("evidence  " + proofText(p, res.proof))
	pause(500 * time.Millisecond)
	p.more("approvals " + approvalText(p, res.approvals))
	if err := rec.mark("verified"); err != nil {
		res.close()
		return nil, err
	}

	charges, err = providerCharges(dir)
	if err != nil {
		res.close()
		return nil, err
	}
	res.charges, res.state = len(charges), string(st.State)
	var problems []string
	if res.charges != 1 {
		problems = append(problems, fmt.Sprintf("the provider holds %d charges, want exactly 1", res.charges))
	}
	if st.State != agent.RunCompleted {
		problems = append(problems, "the run is "+string(st.State)+", not completed")
	}
	if !res.proof.OK {
		problems = append(problems, "the evidence does not verify: "+strings.Join(res.proof.Problems, "; "))
	}
	if !res.anchored.OK {
		problems = append(problems, "the anchored head does not verify: "+strings.Join(res.anchored.Problems, "; "))
	}
	if !res.approvals.OK {
		problems = append(problems, "the approvals' signatures do not verify")
	}
	if len(problems) > 0 {
		res.close()
		return nil, errors.New(strings.Join(problems, "; "))
	}
	pause(500 * time.Millisecond)
	p.line("")
	p.line(p.st.Pass() + p.green(p.bold("charged exactly once.")) + p.dim(fmt.Sprintf("  provider charges: %d", res.charges)))
	return res, nil
}

// crash runs the agent in a child process and kills it once the charge has reached the provider,
// before the child journals the charge's result.
func crash(ctx context.Context, dir string, p *printer, pause func(time.Duration)) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, exe, reexec...)
	cmd.Env = append(os.Environ(), childEnv+"="+dir)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	p.step(2, "Crash", p.red, fmt.Sprintf("agent runs in child process %d", cmd.Process.Pid))
	pause(700 * time.Millisecond)
	p.more(fmt.Sprintf("charging $%d ...", amount))
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("the child exited before charging (%v)", err)
	}
	pause(900 * time.Millisecond)
	p.more("the provider took it: " + p.bold(strings.TrimSpace(line)))
	if err := cmd.Process.Kill(); err != nil {
		return err
	}
	_ = cmd.Wait()
	pause(400 * time.Millisecond)
	p.more(p.red(p.bold("process killed mid-charge")))
	p.more(p.dim("(before bide journaled the result)"))
	return nil
}

// child drives the run in the child process. The charge tool reports each charge on stdout and
// then blocks, so the parent kills the process before the charge's result is journaled.
func child(dir string) int {
	var cfg config
	if err := readJSON(filepath.Join(dir, configFile), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "bide demo child:", err)
		return 1
	}
	j, closeJ, err := openJournal(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bide demo child:", err)
		return 1
	}
	defer closeJ()
	charged := func(c charge) {
		fmt.Println(c.ID)
		time.Sleep(time.Hour) // the parent kills the process here
	}
	_, err = newAgent(j, dir, cfg, charged).Run(context.Background(), runID, agent.UserText(input))
	fmt.Fprintln(os.Stderr, "bide demo child: the run returned before the parent killed it:", err)
	return 1
}

// newAgent builds the payment agent over j, with dir's provider and approver keys. after, if set,
// is called after each charge reaches the provider, before the tool returns.
func newAgent(j *agent.Journal, dir string, cfg config, after func(charge)) *agent.Agent {
	opts := []agent.ToolOption{agent.WithApproval(&policy)}
	if cfg.RetrySafe {
		opts = append(opts, agent.WithSafety(agent.Safety{Idempotent: true}))
	}
	chargeTool := agent.MustFunc("charge", "charge a customer for an order",
		func(_ context.Context, in chargeArgs) (string, error) {
			c, err := providerCharge(dir, in)
			if err != nil {
				return "", err
			}
			if after != nil {
				after(c)
			}
			return receipt(c), nil
		}, opts...)
	keys := map[string]string{}
	if err := readJSON(filepath.Join(dir, keysFile), &keys); err != nil {
		panic(err)
	}
	model := agenttest.NewScriptedModel(
		agenttest.ToolTurn(callID, "charge", fmt.Sprintf(`{"order":%d,"amount":%d}`, order, amount)),
		agenttest.TextTurn(fmt.Sprintf("Charged $%d for order %d.", amount, order)),
	)
	return agenttest.MustNew(model, j, agent.WithTools(chargeTool), agent.WithApproverVerifiers(verifiers(keys)))
}
