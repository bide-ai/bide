package runview

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/approveline"
	"github.com/bide-ai/bide/internal/term"
	"github.com/bide-ai/bide/plan"
)

// Stores opens the stores bide view reads, read-only. It is how the command plugs in store backends
// (store/sqlite's and store/postgres's OpenReadOnly) without the core module importing one. A nil
// opener means this build cannot read that kind of store.
type Stores struct {
	SQLite   func(path string) (agent.Store, io.Closer, error)
	Postgres func(dsn string) (agent.Store, io.Closer, error)
	// Events returns the notification trail's store beside a store the opener returned, or nil when
	// the store keeps none. A nil Events means the Needs attention list says nothing about
	// notification.
	Events func(agent.Store) audit.EventStore
	// YAML converts a YAML flow config to the JSON plan reads; nil means this build reads JSON
	// configs only.
	YAML func(data []byte) ([]byte, error)
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// usageText heads bide view's -h output; the flags follow it.
const usageText = `bide view: a read-only local web page for one run of a bide journal

  bide view -db <file> [-run <id>] [flags]
  bide view -postgres <dsn> [-run <id>] [flags]
  bide view -postgres-env <VAR> [-run <id>] [flags]
  bide view -journal <export.json> [-journal ...] [-run <id>] [flags]
  bide view -config <flow.yaml|flow.json> [-check [-strict]]
  bide view -config <flow.yaml|flow.json> -diff <old config|git revision> [-check [-fail-on-weaken]]

It shows an agent run's timeline (model turns, tool calls with their Safety, attempts, approvals,
halts, end state) or a plan flow's declared topology with the run's path (drawn from the topology
the run journaled, its digest checked, unless -topology overrides it), and whether the journal
verifies against a signed tree head or an evidence package; with -approver-keys, whether each
m-of-n approval gate's signatures verify (audit.VerifyApprovals), against the policy the gate
recorded or, with -need and -approvers, against the policy you expect. It never writes to the
store, and it serves on a loopback address only. Stop it with Ctrl-C.

With no -run the page opens on the run list: each run's kind, state (completed, halted: outcome
unknown, waiting for approval, in progress, aborted or cancelled), when it started (as its
run:start records it; none for a run an earlier version journaled) and its record count, newest
first, filtered by state, kind and run ID. Its headline links to the Needs attention list (/attention):
every run waiting for approval, halted with an unknown outcome or aborted, newest first, with the gate's
approvers and the ResolveHalt call, and, from -db or -postgres, whether the notifier told someone and
the sink acknowledged it (the notification trail in the same database). The run list and that list are
built at most once every 2 seconds, so a page that reloads itself costs the store little. A
bide approve command shown there is quoted for a POSIX shell, also for a Windows -db path.

From -db or -postgres, the page of a run in progress follows it (live mode): it polls the store
about once a second for records after the last one it shows, and updates in place until the run
journals its end marker. An exported journal (-journal) is shown as it is.

With -config it shows a flow definition instead, without running anything: the declared graph,
its approval gates, and every problem plan.InspectConfig finds, marked on the node, edge or arm it
belongs to. The check is structural (types and registered blocks are checked when a program embeds
the viewer with its plan.Registry, see package plan/planview). Each node is colored by its crash
behavior (side effect, read-only, idempotent, or unknown without the Registry), and the config's
assert lines are decided on the graph, a violated one's counterexample path drawn in red. With
-check it serves nothing and prints one "file:node: message" line per problem ("file:from->to:
message" for an edge or arm), a violated assertion among them.

With -diff it compares the -config flow with an earlier version of it: the page draws the new flow
with added nodes and edges in green, removed ones as dashed red ghosts and changed nodes in amber,
and calls out an approval gate whose policy was weakened (removed, its need lowered, or an approver
added without raising the need). -diff takes a file, or, when no file has that name, a git
revision read with git show: <rev>:<path>, or a bare <rev> for the -config file's path at that
revision. With -check it serves nothing and prints one line per change and a summary line.

Exit status: 0 after a clean stop (or -h, or a -check that found no errors; warnings do not
count unless -strict; for -diff -check, a diff is not a failure), 1 if an input cannot be read,
the server fails, -check found errors (or, with -strict, any problem), or -diff -check
-fail-on-weaken found a weakened approval policy, 2 for a usage error.

Flags:
`

// flags are bide view's inputs, as parsed.
type flags struct {
	db, postgres          string
	postgresEnv           string
	config                string
	check                 bool
	strict                bool
	diff                  string
	failOnWeaken          bool
	journals              multi
	topo, digest          string
	sth, evidence, pubkey string
	approverKeys          string
	need                  int
	approvers             string
	excludePrincipal      bool
}

// Main is the bide view command: it parses args, opens the input, and serves until interrupted.
// It returns the exit status: 0 after a clean shutdown, 2 for a usage error, 1 otherwise.
func Main(args []string, stdout, stderr io.Writer, stores Stores) int {
	var f flags
	fs := flag.NewFlagSet("bide view", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
	}
	fs.StringVar(&f.db, "db", "", "SQLite journal file to read, opened read-only (sqlite.OpenReadOnly)")
	fs.StringVar(&f.postgres, "postgres", "", "Postgres journal to read, a pgx DSN such as postgres://user:pass@host:5432/db, read in sessions the server holds read-only (postgres.OpenReadOnly)")
	fs.StringVar(&f.postgresEnv, "postgres-env", "", "name of the environment variable that holds the Postgres DSN, instead of -postgres: keeps the password out of the argument list, and is the name the Needs attention list's bide approve commands use")
	fs.Var(&f.journals, "journal", "exported journal (audit.ExportJournal as JSON) to read; repeatable")
	fs.StringVar(&f.config, "config", "", "a flow config (.json, or .yaml/.yml) to show instead of runs: its declared graph and its problems; nothing runs")
	fs.BoolVar(&f.check, "check", false, "with -config: serve nothing, print each problem as file:node: message, and exit 1 if any is an error; with -diff, print each change instead")
	fs.BoolVar(&f.strict, "strict", false, "with -check: exit 1 on a warning too")
	fs.StringVar(&f.diff, "diff", "", "with -config: compare it with this earlier version, a file or else a git revision (<rev>:<path>, or <rev> for the -config path at rev); -check prints the changes instead of serving the page")
	fs.BoolVar(&f.failOnWeaken, "fail-on-weaken", false, "with -diff -check: exit 1 if an approval gate was removed or weakened")
	run := fs.String("run", "", "run to open (default: the run list)")
	fs.StringVar(&f.topo, "topology", "", "declared flow topology, JSON of flow.Topology(), to draw a flow run against instead of the topology the run journaled (flow:topology); for a run journaled before that record existed, or to check a run against another flow")
	fs.StringVar(&f.digest, "digest", "", "flow.Digest() of the -topology flow, compared with the digest the run journaled (default: computed from -topology when it carries the digest's inputs)")
	fs.StringVar(&f.sth, "sth", "", "signed tree head (audit.SignedTreeHead JSON) to verify the journal against; needs -pubkey")
	fs.StringVar(&f.evidence, "evidence", "", "evidence package (audit.EvidencePackage JSON) to verify; needs -pubkey")
	fs.StringVar(&f.pubkey, "pubkey", "", "log public key as <scheme>:<hex>, or bare ed25519 hex, or a file holding it")
	fs.StringVar(&f.approverKeys, "approver-keys", "", `approver public keys, a JSON object of approver id to key text, e.g. {"ops":"ed25519:ab12..."}, to verify each m-of-n gate's signatures (audit.VerifyApprovals); needs -sth or -evidence`)
	fs.IntVar(&f.need, "need", 0, "the expected policy's k: verify approvals against -need of -approvers instead of the recorded policy; needs -approvers and -approver-keys")
	fs.StringVar(&f.approvers, "approvers", "", "the expected policy's approver ids, comma-separated, in policy order; needs -need")
	fs.BoolVar(&f.excludePrincipal, "exclude-principal", false, "the expected policy excludes the run's requesters (its principal and those of the runs above it) from approving (separation of duties); needs -need")
	addr := fs.String("addr", "127.0.0.1:8484", "address to serve on: a loopback IP (127.0.0.1 or ::1) or localhost, with a port")
	color := term.Flag(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	st := term.For(stdout, *color)
	es := term.For(stderr, *color) // error lines on stderr
	usage := func(msg string) int {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide view:", msg)
		fs.Usage()
		return 2
	}
	if f.check && f.config == "" {
		return usage("-check needs -config")
	}
	if f.strict && !f.check {
		return usage("-strict needs -check")
	}
	if f.diff != "" && f.config == "" {
		return usage("-diff needs -config")
	}
	if f.failOnWeaken && (f.diff == "" || !f.check) {
		return usage("-fail-on-weaken needs -diff and -check")
	}
	if f.strict && f.diff != "" {
		return usage("-strict checks one config's problems; -diff compares two (run bide check without -diff for the problems)")
	}
	if f.config != "" {
		if f.db != "" || f.postgres != "" || f.postgresEnv != "" || len(f.journals) > 0 || *run != "" || f.sth != "" || f.evidence != "" || f.topo != "" {
			return usage("-config shows a flow definition; it takes none of the run flags")
		}
		c, err := readConfig(f.config, stores.YAML)
		if err != nil {
			fmt.Fprintf(stderr, "%s%s: %v\n", es.ErrorPrefix(), f.config, err)
			return 1
		}
		if f.diff != "" {
			old, err := readOld(f.diff, f.config, stores.YAML)
			if err != nil {
				fmt.Fprintln(stderr, es.ErrorPrefix()+"bide view:", err)
				return 1
			}
			d := Diff{Old: old, New: c}
			if _, err := BuildDiff(d); err != nil {
				fmt.Fprintln(stderr, es.ErrorPrefix()+"bide view:", err)
				return 1
			}
			if !f.check {
				return serve(*addr, "", Options{Diff: &d}, nil, st, es, stdout, stderr)
			}
			weakened, _ := CheckDiffStyled(stdout, d, st)
			if weakened && f.failOnWeaken {
				return 1
			}
			return 0
		}
		if f.check {
			c.Strict = f.strict
			if CheckStyled(stdout, c, st) {
				return 1
			}
			return 0
		}
		return serve(*addr, "", Options{Config: &c}, nil, st, es, stdout, stderr)
	}
	inputs := 0
	for _, set := range []bool{f.db != "", f.postgres != "" || f.postgresEnv != "", len(f.journals) > 0} {
		if set {
			inputs++
		}
	}
	if inputs != 1 {
		return usage("give exactly one of -db, -postgres (or -postgres-env) and -journal")
	}
	if f.postgresEnv != "" && !approveline.ValidEnvName(f.postgresEnv) {
		return usage(approveline.NotAnEnvName) // the value is not echoed: it may be a DSN with a password
	}
	if f.postgres != "" && f.postgresEnv != "" {
		return usage("give -postgres or -postgres-env, not both")
	}
	if (f.sth != "" || f.evidence != "") && f.pubkey == "" {
		return usage("-sth and -evidence need -pubkey")
	}
	if f.approverKeys != "" && f.sth == "" && f.evidence == "" {
		return usage("-approver-keys needs -sth or -evidence (and -pubkey)")
	}
	if (f.need != 0) != (f.approvers != "") {
		return usage("-need and -approvers go together")
	}
	if f.excludePrincipal && f.need == 0 {
		return usage("-exclude-principal needs -need and -approvers")
	}
	if f.need != 0 && f.approverKeys == "" {
		return usage("-need and -approvers need -approver-keys")
	}
	if f.need != 0 {
		if err := (agent.ApprovalPolicy{Need: f.need, Approvers: strings.Split(f.approvers, ",")}).Validate(); err != nil {
			return usage("-need and -approvers: " + err.Error())
		}
	}
	if f.digest != "" && f.topo == "" {
		return usage("-digest needs -topology")
	}
	o, closer, err := options(f, stores)
	if err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide view:", err)
		return 1
	}
	return serve(*addr, *run, o, closer, st, es, stdout, stderr)
}

// serve listens on addr (a loopback address) and serves o until interrupted. It prints the page's
// address in st's colors.
func serve(addr, run string, o Options, closer io.Closer, st, es term.Style, stdout, stderr io.Writer) int {
	if closer != nil {
		defer closer.Close()
	}
	ln, err := Listen(addr)
	if err != nil {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide view:", err)
		return 2
	}
	link := "http://" + ln.Addr().String() + "/"
	if run != "" {
		link += "run?id=" + url.QueryEscape(run)
	}
	fmt.Fprint(stdout, servingLine(st, link))
	srv := &http.Server{Handler: Handler(o), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, es.ErrorPrefix()+"bide view:", err)
		return 1
	}
	return 0
}

// servingLine tells where the page is: the address to open in bold cyan, when st colors.
func servingLine(st term.Style, link string) string {
	return "bide view: serving " + st.Path(link) + " " + st.Dim("(read-only; Ctrl-C to stop)") + "\n"
}

// options reads every input the flags name, strictly (audit.UnmarshalStrict, as bide audit reads
// its inputs), into Options.
func options(f flags, stores Stores) (Options, io.Closer, error) {
	var o Options
	var closer io.Closer
	open, what := stores.SQLite, f.db
	dsn := f.postgres
	if f.postgresEnv != "" {
		if dsn = os.Getenv(f.postgresEnv); dsn == "" {
			return o, nil, fmt.Errorf("-postgres-env %s: the variable is empty or not set", f.postgresEnv)
		}
	}
	if dsn != "" {
		open, what = stores.Postgres, "the Postgres journal"
	}
	if what != "" {
		if open == nil {
			return o, nil, errors.New("this build cannot read that store; use -journal")
		}
		s, c, err := open(cmp.Or(f.db, dsn))
		if err != nil {
			return o, nil, fmt.Errorf("open %s: %w", what, err)
		}
		j, err := agent.NewJournal(s)
		if err != nil {
			c.Close()
			return o, nil, err
		}
		o.Source, closer = JournalSource{J: j}, c
		o.ApproveCommand = approveCommand(f)
		if stores.Events != nil {
			o.Events = stores.Events(s)
		}
	} else {
		x := Exports{}
		for _, path := range f.journals {
			var exp audit.JournalExport
			if err := readStrict(path, &exp); err != nil {
				return o, nil, err
			}
			recs, err := exp.Journal()
			if err != nil {
				return o, nil, fmt.Errorf("%s: %w", path, err)
			}
			x[exp.RunID] = recs
		}
		o.Source = x
	}
	if f.topo != "" {
		var t plan.Topology
		if err := readStrict(f.topo, &t); err != nil {
			return o, closer, err
		}
		fl := &Flow{Topology: t, Digest: f.digest, Source: "from -topology"}
		if fl.Digest == "" && t.InCanonical != "" {
			// A topology exported by a version that writes the digest's inputs gives its own digest.
			if d, err := plan.TopologyDigest(t); err == nil {
				fl.Digest, fl.Source = d, "from -topology (digest computed from it)"
			}
		}
		o.Flows = map[string]*Flow{t.Flow: fl}
	}
	if f.pubkey != "" {
		v, err := readKey(f.pubkey)
		if err != nil {
			return o, closer, err
		}
		var ap Approvals
		if f.approverKeys != "" {
			if ap.Keys, err = readApproverKeys(f.approverKeys); err != nil {
				return o, closer, err
			}
		}
		if f.need != 0 {
			ap.Expected = &agent.ApprovalPolicy{Need: f.need, Approvers: strings.Split(f.approvers, ","), ExcludePrincipal: f.excludePrincipal}
		}
		if f.sth != "" {
			var h audit.SignedTreeHead
			if err := readStrict(f.sth, &h); err != nil {
				return o, closer, err
			}
			o.Provers = append(o.Provers, HeadProver(h, v, ap))
		}
		if f.evidence != "" {
			var pkg audit.EvidencePackage
			if err := readStrict(f.evidence, &pkg); err != nil {
				return o, closer, err
			}
			o.Provers = append(o.Provers, EvidenceProver(pkg, v, ap))
		}
	}
	return o, closer, nil
}

// readConfig reads a flow config file, converting a .yaml or .yml file to JSON with toJSON.
func readConfig(path string, toJSON func([]byte) ([]byte, error)) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxInput+1))
	if err != nil {
		return Config{}, err
	}
	if len(b) > maxInput {
		return Config{}, fmt.Errorf("larger than %d bytes", maxInput)
	}
	return configFrom(path, path, b, toJSON)
}

// configFrom is the config named name, read from path's bytes b: a .yaml or .yml path's are
// converted to JSON with toJSON.
func configFrom(name, path string, b []byte, toJSON func([]byte) ([]byte, error)) (Config, error) {
	if ext := strings.ToLower(filepath.Ext(path)); ext == ".yaml" || ext == ".yml" {
		if toJSON == nil {
			return Config{}, errors.New("this build reads JSON configs only")
		}
		var err error
		if b, err = toJSON(b); err != nil {
			return Config{}, fmt.Errorf("YAML: %w", err)
		}
	}
	return Config{Name: name, Data: b}, nil
}

// readApproverKeys reads the approver key file bide audit verify-approvals reads: a JSON object of
// approver id to public key text. audit.VerifyApprovals checks the keys against each gate's policy
// (agent.ApprovalPolicy.ValidateKeys: no two approvers on one key).
func readApproverKeys(path string) (agent.ApproverVerifierFor, error) {
	var text map[string]string
	if err := readStrict(path, &text); err != nil {
		return nil, err
	}
	keys := make(map[string]audit.Verifier, len(text))
	for id, k := range text {
		v, err := audit.ParsePublicKey(k)
		if err != nil {
			return nil, fmt.Errorf("%s: approver %q key: %w", path, id, err)
		}
		keys[id] = v
	}
	return func(id string) (agent.ApproverVerifier, bool) {
		v, ok := keys[id]
		return v, ok
	}, nil
}

// maxInput caps an input file, as bide audit's default -max-input-bytes does.
const maxInput = 256 << 20

func readStrict(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxInput+1))
	if err != nil {
		return err
	}
	if len(b) > maxInput {
		return fmt.Errorf("%s is larger than %d bytes", path, maxInput)
	}
	if err := audit.UnmarshalStrict(b, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// readKey parses a public key given inline or in a file, as bide audit's -pubkey does.
func readKey(s string) (audit.Verifier, error) {
	if v, err := audit.ParsePublicKey(s); err == nil {
		return v, nil
	} else if errors.Is(err, audit.ErrWeakKey) {
		return nil, err
	}
	b, err := os.ReadFile(s)
	if err != nil {
		return nil, fmt.Errorf("public key: %q is neither a key nor a readable file", s)
	}
	return audit.ParsePublicKey(strings.TrimSpace(string(b)))
}

// approveCommand writes the bide approve command line the Needs attention list shows for a waiting
// call, for the journal the flags name: -db by its absolute path, -postgres-env by the same variable,
// and a raw -postgres DSN by the placeholder "<VAR>", never the DSN, which holds a password. The
// approver's own id, key and keys files are left as placeholders (approveline.Command). It refuses an
// id with a control, bidirectional or invisible character (approveline.ErrUnsafe).
func approveCommand(f flags) func(runID, toolUseID string, single bool) (string, error) {
	var j approveline.Journal
	switch {
	case f.db != "":
		j.SQLite = f.db
		if abs, err := filepath.Abs(f.db); err == nil {
			j.SQLite = abs
		}
	case f.postgresEnv != "":
		j.PostgresEnv = f.postgresEnv
	default:
		j.PostgresEnv = "<VAR>"
	}
	return func(runID, toolUseID string, single bool) (string, error) {
		return approveline.Command(j, runID, toolUseID, "", "", "", single)
	}
}
