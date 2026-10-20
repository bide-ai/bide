// Package stopcmd is bide stop: the kill switch's command line. It sets a stop (a pause of every run, or of
// one tool, flow or agent, before any side effect that has not begun), lists and shows the stops, lifts one,
// and, in audited mode, signs every record with an enrolled approver's key and keeps the stop trail whole
// (-reaudit, -apply-pending, -upgrade, -downgrade). The engine's side is agent.StopControl and
// agent.StopSwitch; this package is a thin, careful operator surface over them.
//
//	bide stop -db run.db -all -reason "model misbehaving"                       basic: no keys
//	bide stop -db run.db -tool refund -reason "provider incident" -stop-policy P -as ops -key K    audited
//	bide stop -db run.db -list [-json]
//	bide stop -db run.db -lift <id> -reason "provider recovered" [-stop-policy P -as ops -key K]
//
// There is no default scope: a bare bide stop lists the active stops and exits 2. Nothing secret is on the
// command line: -key names a key file, or (bide-hsm stop) an HSM or KMS key reference, and a Postgres journal
// is named by the environment variable that holds its DSN.
package stopcmd

import (
	"bufio"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/cmd/bide/internal/approvecmd"
	"github.com/bide-ai/bide/internal/safefile"
	"github.com/bide-ai/bide/internal/strictjson"
	"github.com/bide-ai/bide/internal/term"
)

// Exit statuses of bide stop.
const (
	exitOK       = 0
	exitFailed   = 1 // refused: a record the control does not accept, no such stop, declined, or an unusable state
	exitUsage    = 2
	exitInput    = 3 // the journal, the policy or the key could not be read or opened
	exitHalfDone = 5 // an audited write landed in one of the control run and the trail only
)

// Exits documents the exit statuses, for bide help and the CLI reference.
const Exits = `0  success
1  refused or failed: the stop control did not accept the record, no such stop, the prompt was declined,
   or the stops cannot be judged (the stops are unknowable); nothing was written unless stated
2  usage error: a bad flag or argument; nothing was read or written. A bare "bide stop" is one
3  the journal, the policy or the key could not be read or opened; nothing was written
5  half done: an audited record landed in the control run or on the stop trail but not both. The
   message says which; "bide stop -reaudit" (a stop in force, trail missing) or "bide stop
   -apply-pending" (a lift on the trail, not in force) completes it. Repeating a stop only reports
   it in force (nothing is written), so it repairs nothing`

// Opened is a journal store opened for bide stop: the store the runs use and, beside it, the event store the
// stop trail lives in.
type Opened struct {
	Store  agent.Store
	Events audit.EventStore
	Close  io.Closer
}

// Stores opens the journals bide stop reads and writes. readOnly is set for -list and -show.
type Stores struct {
	SQLite   func(path string, readOnly bool) (Opened, error)
	Postgres func(dsn string, readOnly bool) (Opened, error)
}

// Test seams: the clock the ages are shown against, the writer's host and user name.
var (
	now      = time.Now
	hostName = os.Hostname
	osUser   = func() string {
		if u, err := user.Current(); err == nil && u.Username != "" {
			return u.Username
		}
		return os.Getenv("USER")
	}
)

// isTerminal reports whether the command's input is a terminal; a test replaces it. A character device is not
// enough: a cron job's standard input is often /dev/null, which is one, and must not be waited on.
var isTerminal = func(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok || !term.IsTerminal(f) {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
}

const maxPolicyFile = 1 << 20

const usageText = `bide stop: stop agents now (a pause that resumes when the stop is lifted), and list and lift stops

  bide stop -db <file> -all|-tool <name>|-flow <name>|-agent <name> -reason <text> [-yes]
  bide stop -db <file> -list [-json]
  bide stop -db <file> -show <id>
  bide stop -db <file> -lift <id> -reason <text> [-yes]
  bide stop -postgres-env <VAR> ...

A stop is a pause, not a failure: a call that has begun finishes, a call that has not begun does not start,
and the runs carry on when the stop is lifted. There is no default scope: a bare "bide stop" lists the active
stops and exits 2, and never sets one. -tool, -flow and -agent repeat (one stop each).

Basic mode (no -stop-policy) needs no keys: anyone who can reach the journal sets and lifts a stop, and who
did it is asserted, not authenticated. Audited mode (-stop-policy FILE, the deployment's policy) signs every
record with -key as the enrolled approver -as, lifts by the policy's quorum (each lifter runs -lift), and
writes the signed stop trail that "bide audit stops" verifies. Give every process the same mode: a basic
process that meets signed records holds side-effect calls until it is given the policy.

An -agent stop covers runs whose identity (agent.WithIdentity) names that agent; a run with no identity is
never covered by one. Use -all or -tool to cover those.

Operations on an audited deployment:
  -reaudit         append to the trail the records the control run has and the trail lacks (after exit 5)
  -apply-pending   make effective the signed lifts (and policy or mode records) that sit on the trail and never
                   took effect, after showing each one's signer and age; signed, so who applied what is evidence
  -upgrade         move to audited mode: adopts the active plain stops as signed ones, then records the change
  -downgrade       record a move back to basic; refused while any stop is active

The reason is shown in logs, notifications and the viewer: put no secret and no customer data in it. A stop is
written without a prompt when -yes is given or standard input is not a terminal, and asked about once on a
terminal. A lift, -apply-pending, -upgrade and -downgrade ask on a terminal and refuse without one unless -yes
is given. The journal is named by -db (an existing SQLite file) or -postgres-env (the variable that holds the
DSN: it never goes on the command line).

Flags:
`

type nameList []string

func (n *nameList) String() string { return strings.Join(*n, ",") }
func (n *nameList) Set(s string) error {
	*n = append(*n, s)
	return nil
}

type flags struct {
	db, postgresEnv       string
	all, yes, asJSON      bool
	tools, flows, agents  nameList
	reason, as, key       string
	blockComp             bool
	list                  bool
	show, lift            string
	reaudit, applyPending bool
	upgrade, downgrade    bool
	stopPolicy, stopRoot  string
	color                 *term.Mode
	setScopes, verbs      int
}

type cmd struct {
	f         flags
	fs        *flag.FlagSet
	in        *bufio.Reader
	inRaw     io.Reader
	out       io.Writer
	errw      io.Writer
	st        term.Style
	stores    Stores
	src       approvecmd.KeySource
	announced map[string]bool // the stops already reported in force (before their trail write)
	j         *agent.Journal  // the journal the control runs over, for the checks that read a second view of it
}

// Main runs bide stop with a key file as the key source.
func Main(args []string, in io.Reader, stdout, stderr io.Writer, stores Stores) int {
	return MainWith(args, in, stdout, stderr, stores, FileKeys())
}

// FileKeys is the key source of bide stop: -key names a key file bide keygen wrote.
func FileKeys() approvecmd.KeySource {
	src := approvecmd.FileKeys()
	src.Prog = "bide stop"
	src.KeyHelp = "audited mode: your private key file, as bide keygen writes it; on Unix it must not be readable by group or others"
	src.KeyNoun = "your private key file"
	return src
}

// MainWith is Main with the approver's key taken from src (bide-hsm stop: a key in an HSM or a KMS).
func MainWith(args []string, in io.Reader, stdout, stderr io.Writer, stores Stores, src approvecmd.KeySource) int {
	c := &cmd{inRaw: in, in: bufio.NewReader(in), out: stdout, errw: stderr, stores: stores, src: src, announced: map[string]bool{}}
	f := &c.f
	c.fs = flag.NewFlagSet(src.Prog, flag.ContinueOnError)
	fs := c.fs
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), strings.ReplaceAll(usageText, "bide stop", src.Prog))
		fs.PrintDefaults()
	}
	fs.StringVar(&f.db, "db", "", "SQLite journal file (must exist)")
	fs.StringVar(&f.postgresEnv, "postgres-env", "", "name of the environment variable that holds the Postgres DSN (a DSN never goes on the command line)")
	fs.BoolVar(&f.all, "all", false, "set a stop that covers every run")
	fs.Var(&f.tools, "tool", "set a stop that holds calls to this tool, by exact name (repeat for several)")
	fs.Var(&f.flows, "flow", "set a stop that holds this plan flow, by its name (repeat for several)")
	fs.Var(&f.agents, "agent", "set a stop that holds runs of this agent, by its identity's actor name (repeat for several); runs with no identity are not covered")
	fs.StringVar(&f.reason, "reason", "", "why (at most 1 KiB, shown in logs, notifications and the viewer: no secrets, no customer data); required to set or lift")
	fs.BoolVar(&f.blockComp, "block-compensation", false, "with a stop: hold a saga's compensation too (by default a rollback continues)")
	fs.BoolVar(&f.list, "list", false, "list the stops: active ones with who set them and their lifts, the rejected records, the records waiting on the trail; nothing is written")
	fs.BoolVar(&f.asJSON, "json", false, "with -list: print the state as JSON")
	fs.StringVar(&f.show, "show", "", "show one stop, by its ID or an unambiguous prefix of at least 8 characters, with its lift records")
	fs.StringVar(&f.lift, "lift", "", "add your lift of this stop (its ID or an unambiguous prefix of at least 8 characters); audited mode lifts by the policy's quorum")
	fs.BoolVar(&f.reaudit, "reaudit", false, "audited: append to the stop trail what the control run has and the trail lacks")
	fs.BoolVar(&f.applyPending, "apply-pending", false, "audited: make effective the signed records on the trail that never took effect, after showing each one's signer and age (signed, so it is evidence)")
	fs.BoolVar(&f.upgrade, "upgrade", false, "move the deployment to audited mode: adopt the active stops as signed ones and record the change (needs -stop-policy, -as, -key)")
	fs.BoolVar(&f.downgrade, "downgrade", false, "record a move back to basic mode (refused while any stop is active)")
	fs.StringVar(&f.stopPolicy, "stop-policy", "", "audited mode: the stop policy file (JSON: approvers' public keys, setters, lifters, lift quorum, deployment ID) the deployment pins; without it this is basic mode")
	fs.StringVar(&f.stopRoot, "stop-root", "", "audited mode: the digest of the deployment's root policy, when -stop-policy is a later policy the chain reaches")
	fs.StringVar(&f.as, "as", "", "audited mode: your approver id, as the stop policy lists it; basic mode: the name recorded (default your OS user; asserted, not authenticated)")
	fs.StringVar(&f.key, "key", "", src.KeyHelp)
	fs.BoolVar(&f.yes, "yes", false, "do not ask for confirmation (for scripts); what is signed is still shown")
	if src.Flags != nil {
		src.Flags(fs)
	}
	f.color = term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	c.st = term.For(stdout, *f.color)
	es := term.For(stderr, *f.color)
	if msg := c.check(); msg != "" {
		fmt.Fprintf(stderr, "%s%s: %s\n", es.ErrorPrefix(), src.Prog, msg)
		if c.f.verbs+c.f.setScopes == 0 && (c.f.db != "" || c.f.postgresEnv != "") {
			// A bare command lists, so nobody mistakes it for a stop; it is still a usage error.
			if code := c.listBare(); code != exitOK {
				return code
			}
		}
		return exitUsage
	}
	return c.run(context.Background(), es)
}

// check validates the flags and returns a usage message, or "".
func (c *cmd) check() string {
	f := &c.f
	if fs := c.fs; fs.NArg() > 0 {
		if c.src.NoEchoArgs {
			return "unexpected argument (the command takes flags only; a key reference goes after -key)"
		}
		return fmt.Sprintf("unexpected argument %q (the command takes flags only)", fs.Arg(0))
	}
	f.setScopes = len(f.tools) + len(f.flows) + len(f.agents)
	if f.all {
		f.setScopes++
	}
	for _, b := range []bool{f.list, f.show != "", f.lift != "", f.reaudit, f.applyPending, f.upgrade, f.downgrade} {
		if b {
			f.verbs++
		}
	}
	switch {
	case f.db != "" && f.postgresEnv != "":
		return "give one of -db and -postgres-env, not both"
	case f.db == "" && f.postgresEnv == "":
		return "no journal: give -db <file> or -postgres-env <VAR>"
	case f.setScopes > 0 && f.verbs > 0, f.verbs > 1:
		return "give one thing to do: a scope to stop (-all, -tool, -flow, -agent), or one of -list, -show, -lift, -reaudit, -apply-pending, -upgrade, -downgrade"
	case f.setScopes+f.verbs == 0:
		return "no scope given: there is no default, and a bare command never sets a stop. Give -all, -tool, -flow or -agent to stop, or -list; the active stops are:"
	case f.asJSON && !f.list:
		return "-json goes with -list"
	case f.all && f.setScopes > 1:
		return "-all covers everything: it does not combine with -tool, -flow or -agent"
	case f.stopRoot != "" && f.stopPolicy == "":
		return "-stop-root goes with -stop-policy"
	case f.key != "" && f.stopPolicy == "":
		return "-key signs audited records: give -stop-policy too, or drop -key (basic mode records are not signed)"
	case f.blockComp && f.setScopes == 0:
		return "-block-compensation goes with a stop"
	}
	writes := f.setScopes > 0 || f.lift != "" || f.applyPending || f.upgrade || f.downgrade
	if writes && f.reason == "" {
		return "-reason is required (it is part of the record)"
	}
	if f.reason != "" && !writes {
		return "-reason goes with a stop, -lift, -apply-pending, -upgrade or -downgrade"
	}
	audited := f.stopPolicy != ""
	switch {
	case (f.reaudit || f.applyPending || f.upgrade || f.downgrade) && !audited:
		return "this operation is for an audited deployment: give -stop-policy"
	case audited && writes && (f.as == "" || f.key == ""):
		return "audited mode signs: give -as (your approver id) and -key (" + c.src.KeyNoun + ")"
	case f.show != "" && len(f.show) < 8, f.lift != "" && len(f.lift) < 8:
		return "a stop ID, or a prefix of at least 8 characters"
	}
	return ""
}

func (c *cmd) listBare() int {
	ctx := context.Background()
	o, err := c.open(true)
	if err != nil {
		fmt.Fprintf(c.errw, "%s%s: %v\n", term.For(c.errw, *c.f.color).ErrorPrefix(), c.src.Prog, err)
		return exitInput
	}
	defer o.Close.Close()
	if code := c.printState(ctx, o, false); code != exitOK {
		return code
	}
	return exitOK
}

func (c *cmd) fail(code int, format string, a ...any) int {
	fmt.Fprintf(c.errw, "%s%s: %s\n", term.For(c.errw, *c.f.color).ErrorPrefix(), c.src.Prog, fmt.Sprintf(format, a...))
	return code
}

func unwrapPath(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	return err
}

// open opens the journal the flags name. A SQLite file must already exist: a mistyped path must not create an
// empty journal and report that nothing is stopped in it.
func (c *cmd) open(readOnly bool) (Opened, error) {
	if c.f.db != "" {
		if c.stores.SQLite == nil {
			return Opened{}, errors.New("this build cannot open a SQLite journal")
		}
		if _, err := os.Stat(c.f.db); err != nil {
			return Opened{}, fmt.Errorf("journal %s: %w", c.f.db, unwrapPath(err))
		}
		o, err := c.stores.SQLite(c.f.db, readOnly)
		if err != nil {
			return Opened{}, fmt.Errorf("open journal: %w", err)
		}
		return o, nil
	}
	dsn := os.Getenv(c.f.postgresEnv)
	if dsn == "" {
		return Opened{}, fmt.Errorf("-postgres-env %s: the variable is empty or not set", c.f.postgresEnv)
	}
	if c.stores.Postgres == nil {
		return Opened{}, errors.New("this build cannot open a Postgres journal")
	}
	o, err := c.stores.Postgres(dsn, readOnly)
	if err != nil {
		return Opened{}, errors.New("open journal: cannot open the Postgres journal (the error is not shown, as it can hold the DSN)")
	}
	return o, nil
}

// loadPolicy reads the stop policy file named by -stop-policy.
func (c *cmd) loadPolicy() (agent.StopPolicy, agent.StopPolicyDoc, error) {
	b, err := safefile.ReadOrStdin("-stop-policy", c.f.stopPolicy, maxPolicyFile)
	if err != nil {
		var refused *safefile.RefusedError
		if errors.As(err, &refused) && refused.Kind == safefile.TooLarge {
			return agent.StopPolicy{}, agent.StopPolicyDoc{}, fmt.Errorf("-stop-policy %s: larger than %d bytes: not a stop policy", c.f.stopPolicy, maxPolicyFile)
		}
		return agent.StopPolicy{}, agent.StopPolicyDoc{}, fmt.Errorf("-stop-policy %s: %w", c.f.stopPolicy, unwrapPath(err))
	}
	var doc agent.StopPolicyDoc
	if err := strictjson.Unmarshal(b, &doc, nil); err != nil {
		return agent.StopPolicy{}, doc, fmt.Errorf("-stop-policy %s: %w", c.f.stopPolicy, err)
	}
	pol, err := audit.StopPolicyFromDoc(doc)
	if err != nil {
		return agent.StopPolicy{}, doc, fmt.Errorf("-stop-policy %s: %w", c.f.stopPolicy, err)
	}
	return pol, doc, nil
}

// control builds the stop control over o in the mode the flags name; an audited control also keeps the stop trail.
func (c *cmd) control(o Opened) (*agent.StopControl, *agent.StopPolicyDoc, error) {
	j, err := agent.NewJournal(o.Store)
	if err != nil {
		return nil, nil, err
	}
	if c.f.stopPolicy == "" {
		ctl, err := agent.NewStopControl(j, agent.WithStopMode(agent.StopBasic))
		return ctl, nil, err
	}
	pol, doc, err := c.loadPolicy()
	if err != nil {
		return nil, nil, &inputError{err}
	}
	opts := []agent.StopOption{agent.WithStopMode(agent.StopAudited), agent.WithStopPolicy(pol), agent.WithStopVerifier(audit.StopVerifierFromAudit)}
	if c.f.stopRoot != "" {
		opts = append(opts, agent.WithStopRoot(c.f.stopRoot))
	}
	if o.Events != nil {
		opts = append(opts, agent.WithStopTrail(audit.NewStopTrail(o.Events)))
		// A stop is in force when it is in the control run: say so before the trail write, which can wait on
		// other operators (and, if it gives up, is exit 5 and -reaudit).
		opts = append(opts, agent.WithStopInForce(func(e agent.StopEntry) {
			fmt.Fprintf(c.out, "stop %s in force: %s (writing the audit trail)\n", c.st.Bold(e.ID), e.Scope)
			c.announced[e.ID] = true
		}))
	}
	ctl, err := agent.NewStopControl(j, opts...)
	return ctl, &doc, err
}

// inputError marks an error as exit status 3: an input that could not be read or used.
type inputError struct{ error }

func (e *inputError) Unwrap() error { return e.error }

func (c *cmd) run(ctx context.Context, es term.Style) int {
	readOnly := c.f.list || c.f.show != ""
	o, err := c.open(readOnly)
	if err != nil {
		return c.fail(exitInput, "%v", err)
	}
	defer o.Close.Close()
	switch {
	case c.f.list:
		return c.printState(ctx, o, true)
	case c.f.show != "":
		return c.showStop(ctx, o)
	}
	ctl, doc, err := c.control(o)
	if err != nil {
		return c.errCode(err)
	}
	c.j, err = agent.NewJournal(o.Store)
	jr := c.j
	if err != nil {
		return c.errCode(err)
	}
	// The key is loaded after the arguments are validated and the journal is open, so a bad flag never
	// touches an HSM, and what is signed is shown before the signer is used.
	var signer audit.Signer
	if c.f.stopPolicy != "" && !c.f.reaudit {
		if signer, err = c.src.Load(c.f.key); err != nil {
			return c.fail(exitInput, "%v", err)
		}
		if cl, ok := signer.(io.Closer); ok {
			defer cl.Close()
		}
		if n, ok := signer.(interface{ Notice() string }); ok && n.Notice() != "" {
			fmt.Fprintf(c.out, "key        %s\n", es.Warn(n.Notice()))
		}
	}
	by, host := c.identity()
	switch {
	case c.f.setScopes > 0:
		return c.setStops(ctx, ctl, doc, signer, by, host)
	case c.f.lift != "":
		return c.liftStop(ctx, ctl, jr, doc, signer, by, host)
	case c.f.reaudit:
		return c.reauditTrail(ctx, ctl)
	case c.f.applyPending:
		return c.applyPending(ctx, ctl, signer, by, host)
	case c.f.upgrade, c.f.downgrade:
		return c.changeMode(ctx, ctl, o, signer, by, host)
	}
	return exitUsage
}

// identity is who the record names and the host: the approver id in audited mode, else -as or the OS user.
func (c *cmd) identity() (by, host string) {
	host, _ = hostName()
	if c.f.stopPolicy != "" {
		return c.f.as, host
	}
	return cmp.Or(c.f.as, osUser(), "unknown"), host
}

// errCode maps an error from the stop control to an exit status, and prints it.
func (c *cmd) errCode(err error) int {
	if ie, ok := errors.AsType[*inputError](err); ok {
		return c.fail(exitInput, "%v", ie.error)
	}
	if te, ok := errors.AsType[*agent.StopTrailError](err); ok && te.StoreWritten {
		return c.fail(exitHalfDone, "%v\nthe stop is in force; run %s -reaudit (same -db, -stop-policy) to put it on the trail. Do not repeat the stop: that would set a second one", te, c.src.Prog)
	}
	return c.fail(exitFailed, "%v", err)
}

// confirm asks on a terminal, unless -yes. A stop is written without asking when stdin is not a terminal (an
// incident is the wrong time to be refused by a script's missing -yes); everything that lifts or changes the
// deployment's configuration is refused without a terminal unless -yes is given.
func (c *cmd) confirm(what string, ask bool) (ok, usage bool) {
	if c.f.yes {
		return true, false
	}
	tty := isTerminal(c.inRaw)
	if !tty {
		if ask {
			fmt.Fprintf(c.errw, "%s%s: %s needs a terminal to confirm, or -yes (what is signed is shown first)\n", term.For(c.errw, *c.f.color).ErrorPrefix(), c.src.Prog, what)
			return false, true
		}
		return true, false
	}
	fmt.Fprintf(c.out, "Type y to %s: ", what)
	line, _ := c.in.ReadString('\n')
	switch strings.TrimSpace(line) {
	case "y", "Y", "yes":
		return true, false
	}
	return false, false
}

// preflight refuses, before anything is written, an approver id or key the pinned policy does not enrol for
// role. It applies only while no policy transition has taken effect: after one, the policy in force is not the
// file's, and the control judges the record itself.
func (c *cmd) preflight(ctx context.Context, ctl *agent.StopControl, doc *agent.StopPolicyDoc, signer audit.Signer, role string) error {
	if doc == nil || signer == nil {
		return nil
	}
	st, err := ctl.State(ctx)
	if err != nil {
		return err
	}
	if st.PolicySeq != 0 {
		return nil
	}
	ap, ok := doc.Approvers[c.f.as]
	switch {
	case !ok:
		return fmt.Errorf("-as %q is not an approver of the stop policy (%s)", c.f.as, c.f.stopPolicy)
	case ap.Alg != signer.Alg() || ap.Key != hex.EncodeToString(signer.PublicKey()):
		return fmt.Errorf("the key given is not the key the stop policy enrols for %q", c.f.as)
	}
	list := doc.Setters
	if role == "lifter" {
		list = doc.Lifters
	}
	if !slices.Contains(list, c.f.as) {
		return fmt.Errorf("%q is not an enrolled %s in the stop policy", c.f.as, role)
	}
	return nil
}

func (c *cmd) modeLine(st agent.StopState) string {
	if st.Mode == agent.StopAudited {
		return fmt.Sprintf("audited (policy %s, transitions %d)", short(st.PolicyDigest), st.PolicySeq)
	}
	return "basic (store access is trusted; who set a stop is asserted, not authenticated)"
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func ago(at string) string {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return "at an unknown time"
	}
	return age(now().Sub(t)) + " ago"
}

func age(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
