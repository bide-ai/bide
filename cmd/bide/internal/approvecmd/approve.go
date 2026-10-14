// Package approvecmd is bide approve and bide keygen: the command line an approver uses to decide
// on a gated call. bide approve reads the call from the journal, shows it, asks for confirmation,
// signs the decision with the approver's Ed25519 key and records it (agent.SubmitDecision for an
// m-of-n gate, agent.Approve for a single-approver gate). The decision is the library's: this
// package adds no way to record anything the library would not, and the gate re-verifies every
// signature itself when the run resumes.
package approvecmd

import (
	"bufio"
	"cmp"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/approveline"
	"github.com/bide-ai/bide/internal/runview"
	"github.com/bide-ai/bide/internal/term"
)

// Stores opens the journals bide approve reads and writes. It is how the command plugs in store
// backends without this package importing one. readOnly is set for -list, which never writes.
type Stores struct {
	SQLite   func(path string, readOnly bool) (agent.Store, io.Closer, error)
	Postgres func(dsn string, readOnly bool) (agent.Store, io.Closer, error)
}

// now is the clock that stamps a decision's signed time; a test replaces it.
var now = time.Now

// defaultPostgresEnv is the environment variable the listed commands name for a Postgres journal
// opened with a literal -postgres DSN: the command line never carries the DSN.
const defaultPostgresEnv = "BIDE_POSTGRES_DSN"

const usageText = `bide approve: record a signed approval, or a rejection, of a call that waits for a decision

  bide approve -db <file> -run <id> -call <tool-use id> -as <approver> -key <key-file> [flags]
  bide approve -postgres-env <VAR> -run <id> -call <tool-use id> -as <approver> -key <key-file>
  bide approve -db <file> -list

It reads the call from the journal, never from a flag or a notification, and shows what it is
about to sign: the run, the tool, the call's arguments (indented JSON, long ones cut; -full shows
all), the gate (k of n, who has decided, who is pending) and the SHA-256 of the arguments the
signature binds. It then asks you to type your approver id (or y) to confirm; -yes skips the
question, for scripts. The decision is signed with your Ed25519 private key (made by bide keygen),
checked against the call before it is recorded, and recorded with the time you decided.

It refuses, and records nothing, when the call has no waiting gate, the run is not waiting on it,
the gate already passed or was denied, you are not one of the gate's approvers, you already decided
(it says what, and when), or the key file can be read by others (on Unix, as ssh refuses one).

With -approver-keys (a JSON object of approver id to public key, the file bide view reads) it also
refuses a key that is not the one enrolled for you, and prints a verified tally. Without it, the
journal does not hold the approvers' keys, so the tally it prints counts the decisions recorded,
and the gate checks every signature when the run resumes: a decision signed with a key the
application does not enrol is recorded but never counts.

A single-approver gate (agent.Approve) has no approver identity and no signature: bide approve
records the decision as agent.Approve does, takes neither -as nor -key, and says so.

-reject records a rejection: the same checks, the same signature over a denial. One command with a
flag keeps the checks in one place; a rejection is the same act with the opposite byte, and the
confirmation prompt says REJECT in capitals.

After the decision is recorded, re-run the run's root run ID (agent.Run with the same ID) or let
RecoverLoop pick it up: the gate counts the decisions and the call runs or is denied. bide approve
never runs the call.

-list prints the calls in the journal that wait for a decision, each with the bide approve command
that decides it; -json prints them as JSON. A Postgres journal is named by -postgres-env, the name
of the environment variable that holds the DSN, so the password stays out of the argument list,
the shell history and the commands -list prints; -postgres takes the DSN itself, and the listed
commands then name $` + defaultPostgresEnv + `.

Exit status: 0 on success (for -list, even when nothing waits), 1 if the command refused or
failed, 2 for a usage error.

Flags:
`

// refusal is an error the command reports as a refusal: nothing was recorded, and the message says
// why.
type refusal struct{ msg string }

func (e *refusal) Error() string { return e.msg }

func refuse(format string, args ...any) error { return &refusal{fmt.Sprintf(format, args...)} }

// flags are bide approve's inputs, as parsed.
type flags struct {
	db, postgres, postgresEnv string
	run, call, as, key        string
	approverKeys              string
	reject, yes, full         bool
	list, asJSON              bool
}

// Main is the bide approve command: it parses args and decides on, or lists, calls that wait for
// a decision. Confirmation is read from in. It returns the exit status: 0 on success, 1 on a
// refusal or a failure, 2 for a usage error.
func Main(args []string, in io.Reader, stdout, stderr io.Writer, stores Stores) int {
	var f flags
	fs := flag.NewFlagSet("bide approve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageText)
		fs.PrintDefaults()
	}
	fs.StringVar(&f.db, "db", "", "SQLite journal file")
	fs.StringVar(&f.postgres, "postgres", "", "Postgres journal, a pgx DSN such as postgres://user:pass@host:5432/db; prefer -postgres-env, which keeps the password out of the argument list")
	fs.StringVar(&f.postgresEnv, "postgres-env", "", "name of the environment variable that holds the Postgres DSN")
	fs.StringVar(&f.run, "run", "", "the run whose journal holds the call: the pause's run ID")
	fs.StringVar(&f.call, "call", "", "the call's tool-use ID (for a plan flow node, its key, such as node:review)")
	fs.StringVar(&f.as, "as", "", "your approver id, exactly as the gate's policy lists it (not for a single-approver gate)")
	fs.StringVar(&f.key, "key", "", "your Ed25519 private key file, as bide keygen writes it (not for a single-approver gate); on Unix it must not be readable by group or others")
	fs.StringVar(&f.approverKeys, "approver-keys", "", `approver public keys, a JSON object of approver id to key text, e.g. {"ops":"ed25519:ab12..."}: refuse a -key that is not the one enrolled for -as, and verify the tally`)
	fs.BoolVar(&f.reject, "reject", false, "record a rejection instead of an approval")
	fs.BoolVar(&f.yes, "yes", false, "do not ask for confirmation (for scripts); what is signed is still shown")
	fs.BoolVar(&f.full, "full", false, "show all of the call's arguments, not the first lines")
	fs.BoolVar(&f.list, "list", false, "list the calls that wait for a decision, each with its bide approve command; nothing is recorded")
	fs.BoolVar(&f.asJSON, "json", false, "with -list: print the waiting calls as JSON")
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	st, es := term.For(stdout, *color), term.For(stderr, *color)
	usage := func(msg string) int {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide approve:", msg)
		fs.Usage()
		return 2
	}
	if fs.NArg() > 0 {
		return usage(fmt.Sprintf("unexpected argument %q", fs.Arg(0)))
	}
	if msg := f.check(); msg != "" {
		return usage(msg)
	}
	// No signal handling: Ctrl-C at the prompt ends the command, and a decision is one journal write,
	// so an interrupt leaves it recorded or not recorded.
	ctx := context.Background()
	c := &cmd{f: f, in: bufio.NewReader(in), out: stdout, st: st, stores: stores}
	var err error
	if f.list {
		err = c.list(ctx)
	} else {
		err = c.approve(ctx)
	}
	if err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide approve:", es.Hint(err.Error(), "; "))
		return 1
	}
	return 0
}

// check reports a usage error in the flags, or "".
func (f flags) check() string {
	n := 0
	for _, s := range []string{f.db, f.postgres, f.postgresEnv} {
		if s != "" {
			n++
		}
	}
	if n != 1 {
		return "give exactly one of -db, -postgres and -postgres-env"
	}
	if f.list {
		for name, set := range map[string]bool{"-run": f.run != "", "-call": f.call != "", "-as": f.as != "", "-key": f.key != "", "-reject": f.reject, "-yes": f.yes, "-full": f.full, "-approver-keys": f.approverKeys != ""} {
			if set {
				return name + " does not go with -list"
			}
		}
		return ""
	}
	if f.asJSON {
		return "-json goes with -list"
	}
	switch {
	case f.run == "":
		return "no -run given"
	case f.call == "":
		return "no -call given (bide approve -list shows the calls that wait)"
	}
	if (f.as == "") != (f.key == "") {
		return "-as and -key go together"
	}
	return ""
}

// cmd is one bide approve invocation.
type cmd struct {
	f      flags
	in     *bufio.Reader
	out    io.Writer
	st     term.Style
	stores Stores
}

// open opens the journal the flags name. A SQLite file must already exist: a mistyped path must not
// create an empty journal and report that nothing waits in it.
func (c *cmd) open(readOnly bool) (*agent.Journal, io.Closer, approveline.Journal, error) {
	var (
		s     agent.Store
		close io.Closer
		err   error
		tgt   approveline.Journal
	)
	switch {
	case c.f.db != "":
		if c.stores.SQLite == nil {
			return nil, nil, tgt, errors.New("this build cannot open a SQLite journal")
		}
		if _, serr := os.Stat(c.f.db); serr != nil {
			return nil, nil, tgt, fmt.Errorf("journal %s: %w", c.f.db, unwrapPath(serr))
		}
		tgt = approveline.Journal{SQLite: c.f.db}
		s, close, err = c.stores.SQLite(c.f.db, readOnly)
	default:
		dsn, env := c.f.postgres, cmp.Or(c.f.postgresEnv, defaultPostgresEnv)
		if c.f.postgresEnv != "" {
			if dsn = os.Getenv(c.f.postgresEnv); dsn == "" {
				return nil, nil, tgt, fmt.Errorf("-postgres-env %s: the variable is empty or not set", c.f.postgresEnv)
			}
		}
		if c.stores.Postgres == nil {
			return nil, nil, tgt, errors.New("this build cannot open a Postgres journal")
		}
		tgt = approveline.Journal{PostgresEnv: env}
		s, close, err = c.stores.Postgres(dsn, readOnly)
		if err != nil {
			err = errors.New("cannot open the Postgres journal (the error is not shown, as it can hold the DSN)")
		}
	}
	if err != nil {
		return nil, nil, tgt, fmt.Errorf("open journal: %w", err)
	}
	j, err := agent.NewJournal(s)
	if err != nil {
		close.Close()
		return nil, nil, tgt, err
	}
	return j, close, tgt, nil
}

// gateOf is the waiting gate on call in run's records, and the call it gates, from the same
// records: what is shown and what is signed are read once, here.
type gateOf struct {
	recs []agent.Record
	view *runview.View
	gate runview.Gate
	call agent.ToolUse
}

// load reads run's journal and finds the gate on call. It refuses a run with no records and a
// call with no gate.
func load(ctx context.Context, j *agent.Journal, run, call string) (*gateOf, error) {
	recs, err := j.History(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("read run %s: %w", safe(run), err)
	}
	if len(recs) == 0 {
		return nil, refuse("run %s is not in this journal", safe(run))
	}
	v := runview.Build(ctx, run, recs, nil)
	_, tu, found := agent.FindToolCall(recs, call)
	i := slices.IndexFunc(v.Gates, func(g runview.Gate) bool { return g.ToolUseID == call })
	if !found || i < 0 {
		var ids []string
		for _, g := range v.Gates {
			ids = append(ids, safe(g.ToolUseID))
		}
		if len(ids) == 0 {
			return nil, refuse("run %s has no gated call %s: it has no approval gate at all", safe(run), safe(call))
		}
		return nil, refuse("run %s has no approval gate on call %s; its gated calls: %s", safe(run), safe(call), strings.Join(ids, ", "))
	}
	return &gateOf{recs: recs, view: v, gate: v.Gates[i], call: tu}, nil
}

// subject is what the approver signs a decision on, from the records read.
func (g *gateOf) subject(run string) agent.ApprovalSubject {
	return agent.ApprovalSubject{RunID: run, ToolUseID: g.call.ID, ToolName: g.call.Name, Args: g.call.Args}
}

// verdict is what a recorded decision of an approver says, for a message.
func verdict(approved bool) string {
	if approved {
		return "approved"
	}
	return "denied"
}

// preflight refuses a decision that cannot count or cannot be made: the run ended, the gate is
// decided, the call is not waiting, the approver is not one of the gate's, or already decided. It
// reads the journal only. verifierFor, when non-nil, tells a valid earlier decision of the approver
// from one that does not verify.
func (g *gateOf) preflight(run, as string, verifierFor agent.ApproverVerifierFor) error {
	gt := g.gate
	switch g.view.State {
	case string(agent.RunCompleted), string(agent.RunAborted), string(agent.RunCancelled):
		return refuse("run %s has ended (%s); it does not wait on call %s", safe(run), g.view.StateText, safe(gt.ToolUseID))
	}
	if gt.Tallied {
		out := "passed"
		if len(gt.ApprovedBy) < gt.Need {
			out = "was denied"
		}
		return refuse("the gate on call %s already decided: it %s (%s)", safe(gt.ToolUseID), out, safe(gt.Status))
	}
	if gt.Single && len(gt.Decisions) > 0 {
		d := gt.Decisions[0]
		return refuse("the single-approver gate on call %s already has its decision: %s (recorded %s)", safe(gt.ToolUseID), verdict(d.Approved), cmp.Or(d.RecordedAt, "at an unknown time"))
	}
	if !gt.Waiting {
		return refuse("run %s is not waiting on call %s: the call has a result, or the run never paused at its gate (no approval-pending record)", safe(run), safe(gt.ToolUseID))
	}
	if gt.Single {
		return nil
	}
	if len(gt.Approvers) == 0 {
		return refuse("the journal does not record the policy of the gate on call %s, so approvers cannot be checked", safe(gt.ToolUseID))
	}
	if !slices.Contains(gt.Approvers, as) {
		return refuse("%q is not an approver of the gate on call %s; its approvers are: %s", safe(as), safe(gt.ToolUseID), safe(strings.Join(gt.Approvers, ", ")))
	}
	// With the approvers' keys, the standing is the one the gate computes (TallyApprovals); without
	// them it is the decisions recorded, valid or not, and the refusal says so.
	counted := map[string]bool{}
	approved, denied := len(gt.ApprovedBy), len(gt.DeniedBy)
	hint := "; the decisions are counted unverified: give -approver-keys to check their signatures"
	if verifierFor != nil {
		t, checks := agent.TallyApprovals(g.recs, g.subject(run), agent.ApprovalPolicy{Need: gt.Need, Approvers: gt.Approvers}, verifierFor)
		for _, c := range checks {
			if c.Counted {
				counted[c.Approver] = true
			}
		}
		approved, denied, hint = t.Approved, t.Denied, ""
	}
	switch {
	case approved >= gt.Need:
		return refuse("the gate on call %s already has its %d approval(s) recorded; nothing more is needed: re-run run %s%s", safe(gt.ToolUseID), gt.Need, safe(run), hint)
	case len(gt.Approvers)-denied < gt.Need:
		return refuse("the gate on call %s can no longer pass (%d denied of %d approvers, %d needed): re-run run %s and the call is denied%s", safe(gt.ToolUseID), denied, len(gt.Approvers), gt.Need, safe(run), hint)
	}
	for _, d := range gt.Decisions {
		if d.Approver != as || d.DecidedAt == "" { // a decision with no signed time is of the earlier format, which does not count
			continue
		}
		if verifierFor != nil && !counted[as] {
			continue // it does not verify, so it holds nobody's place: the approver can decide
		}
		return refuse("%s already decided on call %s: %s at %s (recorded %s)%s", safe(as), safe(gt.ToolUseID), verdict(d.Approved), safe(d.DecidedAt), safe(cmp.Or(d.RecordedAt, "at an unknown time")), hint)
	}
	return nil
}

// approverKeys reads -approver-keys: id -> verifier. It fails closed: an entry that is not a key
// is an error, not a skipped approver.
func approverKeys(path string) (map[string]audit.Verifier, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("-approver-keys: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, fmt.Errorf("-approver-keys %s: %w", path, err)
	}
	if len(b) > 1<<20 {
		return nil, fmt.Errorf("-approver-keys %s is larger than 1 MiB", path)
	}
	var raw map[string]string
	if err := audit.UnmarshalStrict(b, &raw); err != nil {
		return nil, fmt.Errorf("-approver-keys %s: %w", path, err)
	}
	keys := make(map[string]audit.Verifier, len(raw))
	for id, text := range raw {
		v, err := audit.ParsePublicKey(text)
		if err != nil {
			return nil, fmt.Errorf("-approver-keys %s: the key of %q: %w", path, safe(id), err)
		}
		keys[id] = v
	}
	return keys, nil
}

// verifiers resolves approver ids to the keys read.
func verifiers(keys map[string]audit.Verifier) agent.ApproverVerifierFor {
	return func(id string) (agent.ApproverVerifier, bool) {
		v, ok := keys[id]
		return v, ok
	}
}

// approve is the whole of bide approve for one call.
func (c *cmd) approve(ctx context.Context) error {
	f := c.f
	j, closer, _, err := c.open(false)
	if err != nil {
		return err
	}
	defer closer.Close()
	g, err := load(ctx, j, f.run, f.call)
	if err != nil {
		return err
	}
	single := g.gate.Single
	if single && (f.as != "" || f.key != "") {
		return refuse("the gate on call %s is a single-approver gate (agent.Approve): its decision carries no approver id and no signature, so -as and -key do not apply; drop them", safe(f.call))
	}
	if !single && (f.as == "" || f.key == "") {
		return refuse("the gate on call %s is a %s gate: give -as (your approver id) and -key (your private key file)", safe(f.call), gateShape(g.gate))
	}
	var keys map[string]audit.Verifier
	var verifierFor agent.ApproverVerifierFor
	if f.approverKeys != "" {
		if keys, err = approverKeys(f.approverKeys); err != nil {
			return err
		}
		verifierFor = verifiers(keys)
	}
	if err := g.preflight(f.run, f.as, verifierFor); err != nil {
		return err
	}
	var priv ed25519.PrivateKey
	if !single {
		if priv, err = LoadKey(f.key); err != nil {
			return err
		}
		if err := checkEnrolled(keys, f.as, priv); err != nil {
			return err
		}
	}

	s := g.subject(f.run)
	c.show(g, s, priv)
	if !c.confirm(single) {
		return refuse("not confirmed; nothing was recorded")
	}

	if single {
		if err := agent.Approve(ctx, j, f.run, f.call, !f.reject); err != nil {
			return fmt.Errorf("record the decision: %w", err)
		}
		fmt.Fprintf(c.out, "\n%s%s the call, unsigned (a single-approver gate records no signature)\n", c.st.Pass(), c.verb(!f.reject))
	} else {
		at := now()
		msg := agent.ApprovalDecisionBytes(s, f.as, !f.reject, at)
		if !digestAt(msg, s, argsDigest(s)) {
			return errors.New("internal error: the decision bytes do not carry the shown arguments digest; nothing was signed")
		}
		sig := ed25519.Sign(priv, msg)
		d := agent.Decision{RunID: f.run, ToolUseID: f.call, ApproverID: f.as, Approved: !f.reject, DecidedAt: at, Alg: audit.AlgEd25519, Signature: sig}
		// The check verifies the signature against the call as the journal holds it, with the
		// public key derived from the private key, so a decision that does not verify is never
		// recorded; the gate re-verifies every record itself against the application's keys.
		own := func(id string) (agent.ApproverVerifier, bool) {
			if id != f.as {
				return nil, false
			}
			return audit.Ed25519Verifier{Pub: priv.Public().(ed25519.PublicKey)}, true
		}
		if err := agent.SubmitDecision(ctx, j, d, agent.WithDecisionCheck(own)); err != nil {
			return fmt.Errorf("the decision was not recorded: %w", err)
		}
		fmt.Fprintf(c.out, "\n%s%s call %s as %s, signed and recorded (decided %s)\n", c.st.Pass(), c.verb(!f.reject), safe(f.call), c.st.Bold(f.as), agent.FormatTimestamp(at))
	}
	return c.after(ctx, j, verifierFor)
}

// verb is the past tense of the decision, colored.
func (c *cmd) verb(approved bool) string {
	if approved {
		return c.st.OK("Approved")
	}
	return c.st.Fail("Rejected")
}

func gateShape(g runview.Gate) string {
	if g.Single {
		return "single-approver"
	}
	return fmt.Sprintf("%d-of-%d", g.Need, len(g.Approvers))
}

// checkEnrolled refuses a private key whose public key is not the one -approver-keys enrols for as.
func checkEnrolled(keys map[string]audit.Verifier, as string, priv ed25519.PrivateKey) error {
	if keys == nil {
		return nil
	}
	v, ok := keys[as]
	if !ok {
		return refuse("-approver-keys has no key for %q", safe(as))
	}
	have := publicKeyText(priv)
	want := audit.FormatPublicKey(v.Alg(), v.PublicKey())
	if have != want {
		return refuse("the public key of your -key (%s) is not the key enrolled for %q in -approver-keys (%s); a decision signed with it would be recorded and never count", have, safe(as), want)
	}
	return nil
}

// show prints what is about to be signed, all of it read from the journal.
func (c *cmd) show(g *gateOf, s agent.ApprovalSubject, priv ed25519.PrivateKey) {
	st, gt := c.st, g.gate
	action := st.Bold("Approve this call?")
	if c.f.reject {
		action = st.BoldFail("REJECT this call?")
	}
	fmt.Fprintf(c.out, "%s %s\n\n", action, st.Dim("(read from the journal)"))
	row := func(k, v string) { fmt.Fprintf(c.out, "  %-10s %s\n", k, v) }
	row("run", safe(s.RunID))
	row("call", safe(s.ToolUseID))
	row("tool", st.Bold(safe(s.ToolName)))
	if gt.Single {
		row("gate", "one approver, one decision; the decision is recorded without a signature")
	} else {
		row("gate", fmt.Sprintf("%d of %d must approve: %s", gt.Need, len(gt.Approvers), safe(strings.Join(gt.Approvers, ", "))))
		for _, d := range gt.Decisions {
			if d.Approver == "" || d.DecidedAt == "" {
				continue
			}
			row("decided", fmt.Sprintf("%s %s at %s", safe(d.Approver), verdict(d.Approved), safe(d.DecidedAt)))
		}
		row("pending", safe(strings.Join(gt.Pending, ", ")))
		row("you", fmt.Sprintf("%s, with key %s", st.Bold(c.f.as), publicKeyText(priv)))
		if c.f.approverKeys == "" {
			row("", st.Warn("this key is not checked against the key enrolled for you (no -approver-keys): if the application enrols another, the decision is recorded and never counts"))
		}
	}
	if gt.AskedAt != "" {
		row("asked", safe(gt.AskedAt))
	}
	text := prettyArgs(g.call.Args)
	hidden := 0
	if !c.f.full {
		text, hidden = clip(text)
	}
	fmt.Fprintf(c.out, "\n  arguments\n")
	for _, l := range strings.Split(text, "\n") {
		fmt.Fprintf(c.out, "    %s\n", l)
	}
	if hidden > 0 {
		fmt.Fprintf(c.out, "    %s\n", st.Warn(fmt.Sprintf("[%d more bytes not shown; the signature binds all of them; -full shows everything]", hidden)))
	}
	if gt.Single {
		fmt.Fprintln(c.out)
		return
	}
	fmt.Fprintf(c.out, "\n  %s %s\n", "args SHA-256", st.Bold(argsDigest(s)))
	fmt.Fprintf(c.out, "  %s\n\n", st.Dim("your signature binds this digest with the run, call, tool and your id, your decision and its time, and nothing else"))
}

// confirm asks the approver to confirm, unless -yes. For an m-of-n gate the approver types their
// id (or y); a single-approver gate has no id, so y. Anything else, and end of input, declines.
func (c *cmd) confirm(single bool) bool {
	if c.f.yes {
		return true
	}
	what := "approve"
	if c.f.reject {
		what = "REJECT"
	}
	if single {
		fmt.Fprintf(c.out, "Type y to %s: ", what)
	} else {
		fmt.Fprintf(c.out, "Type %s (or y) to %s: ", c.f.as, what)
	}
	line, _ := c.in.ReadString('\n')
	line = strings.TrimSpace(line)
	return line == "y" || line == "Y" || line == "yes" || (!single && line == c.f.as)
}

// after reads the call's journal again and prints the gate's standing and what to do next. With the
// approvers' keys the tally is verified here (agent.TallyApprovals, the rule the gate runs);
// without them it counts the decisions recorded.
func (c *cmd) after(ctx context.Context, j *agent.Journal, verifierFor agent.ApproverVerifierFor) error {
	f, st := c.f, c.st
	g, err := load(ctx, j, f.run, f.call)
	if err != nil {
		return fmt.Errorf("the decision was recorded; reading the gate again failed: %w", err)
	}
	gt := g.gate
	passed, denied := false, false
	switch {
	case gt.Single:
		passed, denied = len(gt.Decisions) > 0 && gt.Decisions[0].Approved, len(gt.Decisions) > 0 && !gt.Decisions[0].Approved
		fmt.Fprintf(c.out, "%s\n", gt.Status)
	case verifierFor != nil:
		t, _ := agent.TallyApprovals(g.recs, g.subject(f.run), agent.ApprovalPolicy{Need: gt.Need, Approvers: gt.Approvers}, verifierFor)
		passed, denied = t.Passed(), t.Unreachable()
		fmt.Fprintf(c.out, "verified tally: %d of %d needed approved%s%s\n", t.Approved, t.Need, names(" by", t.ApprovedBy), names("; denied by", t.DeniedBy))
		if len(t.Pending) > 0 && !passed && !denied {
			fmt.Fprintf(c.out, "still pending: %s\n", safe(strings.Join(t.Pending, ", ")))
		}
	default:
		passed, denied = len(gt.ApprovedBy) >= gt.Need, len(gt.Approvers)-len(gt.DeniedBy) < gt.Need
		fmt.Fprintf(c.out, "recorded decisions: %d of %d needed approved%s%s\n", len(gt.ApprovedBy), gt.Need, names(" by", gt.ApprovedBy), names("; denied by", gt.DeniedBy))
		if len(gt.Pending) > 0 && !passed && !denied {
			fmt.Fprintf(c.out, "still pending: %s\n", safe(strings.Join(gt.Pending, ", ")))
		}
		fmt.Fprintln(c.out, st.Dim("(counted from the journal; the gate checks every signature when the run resumes. Give -approver-keys to verify them here.)"))
	}
	switch {
	case passed:
		fmt.Fprintf(c.out, "\n%s the gate has its approvals. Next: re-run run %s (agent.Run with the same run ID, or let RecoverLoop resume it; for a sub-agent's run, its root run ID); the call then runs.\n", st.Pass(), st.Bold(safe(f.run)))
	case denied:
		fmt.Fprintf(c.out, "\n%s the gate can no longer pass. Next: re-run run %s; the call is denied and the model is told so.\n", st.Cross(), st.Bold(safe(f.run)))
	default:
		fmt.Fprintf(c.out, "\nNext: the other pending approvers decide. When the gate has its decisions, re-run run %s (or let RecoverLoop resume it).\n", st.Bold(safe(f.run)))
	}
	return nil
}

func names(label string, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return label + " " + safe(strings.Join(ids, ", "))
}

// waiting is one call that waits for a decision, as -list reports it.
type waiting struct {
	Run        string   `json:"run"`
	Call       string   `json:"call"`
	Tool       string   `json:"tool"`
	Single     bool     `json:"single,omitempty"`
	Need       int      `json:"need,omitempty"`
	Approvers  []string `json:"approvers,omitempty"`
	ApprovedBy []string `json:"approved_by,omitempty"`
	DeniedBy   []string `json:"denied_by,omitempty"`
	Pending    []string `json:"pending,omitempty"`
	AskedAt    string   `json:"asked_at,omitempty"`
	// Settled says the recorded decisions already settle the gate ("approved": it has its approvals;
	// "denied": it can no longer pass): the run only needs re-running, and no command is offered.
	// Counted from the journal as recorded, unverified: the gate checks every signature on resume.
	Settled  string   `json:"settled,omitempty"`
	Commands []string `json:"commands"`
}

// list prints the calls that wait for a decision in the journal.
func (c *cmd) list(ctx context.Context) error {
	j, closer, tgt, err := c.open(true)
	if err != nil {
		return err
	}
	defer closer.Close()
	l, ok := agent.Capability[agent.Lister](j.Store())
	if !ok {
		return errors.New("this journal's store cannot list runs; use -run with -call, or bide view")
	}
	var runs []string
	for id, err := range l.Runs(ctx, agent.RunFilter{ExcludeHolding: []string{"run:complete", "run:aborted", "run:cancelled"}}) {
		if err != nil {
			return fmt.Errorf("list runs: %w", err)
		}
		runs = append(runs, id)
	}
	slices.Sort(runs)
	var out []waiting
	for _, run := range runs {
		recs, err := j.History(ctx, run)
		if err != nil {
			return fmt.Errorf("read run %s: %w", safe(run), err)
		}
		v := runview.Build(ctx, run, recs, nil)
		for _, g := range v.Gates {
			if !g.Waiting || v.State == string(agent.RunCompleted) || v.State == string(agent.RunAborted) || v.State == string(agent.RunCancelled) {
				continue
			}
			w := waiting{Run: run, Call: g.ToolUseID, Tool: g.ToolName, Single: g.Single, Need: g.Need, Approvers: g.Approvers, ApprovedBy: g.ApprovedBy, DeniedBy: g.DeniedBy, Pending: g.Pending, AskedAt: g.AskedAt}
			switch {
			case g.Single:
				w.Commands = []string{approveline.Command(tgt, run, g.ToolUseID, "", "", true)}
			case len(g.ApprovedBy) >= g.Need && g.Need > 0:
				w.Settled = "approved"
			case g.Need > 0 && len(g.Approvers)-len(g.DeniedBy) < g.Need:
				w.Settled = "denied"
			default:
				for _, id := range g.Pending {
					w.Commands = append(w.Commands, approveline.Command(tgt, run, g.ToolUseID, id, "", false))
				}
			}
			if w.Commands == nil {
				w.Commands = []string{}
			}
			out = append(out, w)
		}
	}
	if c.f.asJSON {
		if out == nil {
			out = []waiting{}
		}
		enc := json.NewEncoder(c.out)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	st := c.st
	if len(out) == 0 {
		fmt.Fprintln(c.out, "no call waits for a decision in this journal")
		return nil
	}
	fmt.Fprintf(c.out, "%s\n", st.Heading(fmt.Sprintf("%d call(s) wait for a decision:", len(out))))
	for _, w := range out {
		fmt.Fprintf(c.out, "\n  run %s  call %s  tool %s\n", st.Bold(safe(w.Run)), st.Bold(safe(w.Call)), st.Bold(safe(w.Tool)))
		if w.Single {
			fmt.Fprintf(c.out, "    one approver, one decision (unsigned)\n")
		} else {
			fmt.Fprintf(c.out, "    %d of %d: %s%s\n", w.Need, len(w.Approvers), safe(strings.Join(w.Approvers, ", ")), names("; approved by", w.ApprovedBy)+names("; denied by", w.DeniedBy))
			switch w.Settled {
			case "approved":
				fmt.Fprintf(c.out, "    %s\n", st.OK("approvals recorded: re-run the run and the call runs (agent.Run with the same run ID, or RecoverLoop)"))
			case "denied":
				fmt.Fprintf(c.out, "    %s\n", st.Fail("can no longer pass: re-run the run and the call is denied"))
			default:
				fmt.Fprintf(c.out, "    pending: %s\n", safe(strings.Join(w.Pending, ", ")))
			}
		}
		for _, cmdline := range w.Commands {
			fmt.Fprintf(c.out, "    %s\n", st.Path(cmdline))
		}
	}
	if c.f.postgres != "" {
		fmt.Fprintf(c.out, "\n%s\n", st.Dim("Set $"+defaultPostgresEnv+" to the DSN, or use -postgres-env with your own variable: the commands above never carry the DSN."))
	}
	fmt.Fprintf(c.out, "\n%s\n", st.Dim("Replace '"+approveline.KeyFilePlaceholder+"' with the path of your private key (bide keygen makes one). bide approve shows what it signs and asks first."))
	return nil
}
