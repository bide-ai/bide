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
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/plan"
)

// Stores opens the stores bide-view reads, read-only. It is how the command plugs in store backends
// (store/sqlite's and store/postgres's OpenReadOnly) without the core module importing one. A nil
// opener means this build cannot read that kind of store.
type Stores struct {
	SQLite   func(path string) (agent.Store, io.Closer, error)
	Postgres func(dsn string) (agent.Store, io.Closer, error)
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// usageText heads bide-view's -h output; the flags follow it.
const usageText = `bide-view: a read-only local web page for one run of a bide journal

  bide-view -db <file> [-run <id>] [flags]
  bide-view -postgres <dsn> [-run <id>] [flags]
  bide-view -journal <export.json> [-journal ...] [-run <id>] [flags]

It shows an agent run's timeline (model turns, tool calls with their Safety, attempts, approvals,
halts, end state) or a plan flow's declared topology with the run's path, and whether the journal
verifies against a signed tree head or an evidence package; with -approver-keys, whether each
m-of-n approval gate's signatures verify (audit.VerifyApprovals), against the policy the gate
recorded or, with -need and -approvers, against the policy you expect. It never writes to the
store, and it serves on a loopback address only. With no -run it lists the runs. Stop it with
Ctrl-C.

Exit status: 0 after a clean stop (or -h), 1 if an input cannot be read or the server fails,
2 for a usage error.

Flags:
`

// flags are bide-view's inputs, as parsed.
type flags struct {
	db, postgres          string
	journals              multi
	topo, digest          string
	sth, evidence, pubkey string
	approverKeys          string
	need                  int
	approvers             string
}

// Main is the bide-view command: it parses args, opens the input, and serves until interrupted.
// It returns the exit status: 0 after a clean shutdown, 2 for a usage error, 1 otherwise.
func Main(args []string, stdout, stderr io.Writer, stores Stores) int {
	var f flags
	fs := flag.NewFlagSet("bide-view", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
	}
	fs.StringVar(&f.db, "db", "", "SQLite journal file to read, opened read-only (sqlite.OpenReadOnly)")
	fs.StringVar(&f.postgres, "postgres", "", "Postgres journal to read, a pgx DSN such as postgres://user:pass@host:5432/db, read in sessions the server holds read-only (postgres.OpenReadOnly)")
	fs.Var(&f.journals, "journal", "exported journal (audit.ExportJournal as JSON) to read; repeatable")
	run := fs.String("run", "", "run to open (default: the run list)")
	fs.StringVar(&f.topo, "topology", "", "declared flow topology: JSON of flow.Topology(), to draw a flow run against")
	fs.StringVar(&f.digest, "digest", "", "flow.Digest() of the declared flow, compared with the digest the run journaled")
	fs.StringVar(&f.sth, "sth", "", "signed tree head (audit.SignedTreeHead JSON) to verify the journal against; needs -pubkey")
	fs.StringVar(&f.evidence, "evidence", "", "evidence package (audit.EvidencePackage JSON) to verify; needs -pubkey")
	fs.StringVar(&f.pubkey, "pubkey", "", "log public key as <scheme>:<hex>, or bare ed25519 hex, or a file holding it")
	fs.StringVar(&f.approverKeys, "approver-keys", "", `approver public keys, a JSON object of approver id to key text, e.g. {"ops":"ed25519:ab12..."}, to verify each m-of-n gate's signatures (audit.VerifyApprovals); needs -sth or -evidence`)
	fs.IntVar(&f.need, "need", 0, "the expected policy's k: verify approvals against -need of -approvers instead of the recorded policy; needs -approvers and -approver-keys")
	fs.StringVar(&f.approvers, "approvers", "", "the expected policy's approver ids, comma-separated, in policy order; needs -need")
	addr := fs.String("addr", "127.0.0.1:8484", "address to serve on: a loopback IP (127.0.0.1 or ::1) or localhost, with a port")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usage := func(msg string) int {
		fmt.Fprintln(stderr, "bide-view:", msg)
		fs.Usage()
		return 2
	}
	inputs := 0
	for _, set := range []bool{f.db != "", f.postgres != "", len(f.journals) > 0} {
		if set {
			inputs++
		}
	}
	if inputs != 1 {
		return usage("give exactly one of -db, -postgres and -journal")
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
		fmt.Fprintln(stderr, "bide-view:", err)
		return 1
	}
	if closer != nil {
		defer closer.Close()
	}
	ln, err := Listen(*addr)
	if err != nil {
		fmt.Fprintln(stderr, "bide-view:", err)
		return 2
	}
	link := "http://" + ln.Addr().String() + "/"
	if *run != "" {
		link += "run?id=" + url.QueryEscape(*run)
	}
	fmt.Fprintln(stdout, "bide-view: serving", link, "(read-only; Ctrl-C to stop)")
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
		fmt.Fprintln(stderr, "bide-view:", err)
		return 1
	}
	return 0
}

// options reads every input the flags name, strictly (audit.UnmarshalStrict, as bide-audit reads
// its inputs), into Options.
func options(f flags, stores Stores) (Options, io.Closer, error) {
	var o Options
	var closer io.Closer
	open, what := stores.SQLite, f.db
	if f.postgres != "" {
		open, what = stores.Postgres, "the Postgres journal"
	}
	if what != "" {
		if open == nil {
			return o, nil, errors.New("this build cannot read that store; use -journal")
		}
		s, c, err := open(cmp.Or(f.db, f.postgres))
		if err != nil {
			return o, nil, fmt.Errorf("open %s: %w", what, err)
		}
		j, err := agent.NewJournal(s)
		if err != nil {
			c.Close()
			return o, nil, err
		}
		o.Source, closer = JournalSource{J: j}, c
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
		o.Flows = map[string]*Flow{t.Flow: {Topology: t, Digest: f.digest}}
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
			ap.Expected = &agent.ApprovalPolicy{Need: f.need, Approvers: strings.Split(f.approvers, ",")}
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

// readApproverKeys reads the approver key file bide-audit verify-approvals reads: a JSON object of
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

// maxInput caps an input file, as bide-audit's default -max-input-bytes does.
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

// readKey parses a public key given inline or in a file, as bide-audit's -pubkey does.
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
