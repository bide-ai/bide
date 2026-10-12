package runview

import (
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

// OpenStore opens a store for reading, at path. It is how a command plugs in a store backend
// (store/sqlite's OpenReadOnly) without the core module importing one.
type OpenStore func(path string) (agent.Store, io.Closer, error)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// usageText heads bide-view's -h output; the flags follow it.
const usageText = `bide-view: a read-only local web page for one run of a bide journal

  bide-view -db <file> [-run <id>] [flags]
  bide-view -journal <export.json> [-journal ...] [-run <id>] [flags]

It shows an agent run's timeline (model turns, tool calls with their Safety, attempts, approvals,
halts, end state) or a plan flow's declared topology with the run's path, and whether the journal
verifies against a signed tree head or an evidence package. It never writes to the store, and it
serves on a loopback address only. With no -run it lists the runs. Stop it with Ctrl-C.

Exit status: 0 after a clean stop (or -h), 1 if an input cannot be read or the server fails,
2 for a usage error.

Flags:
`

// Main is the bide-view command: it parses args, opens the input, and serves until interrupted.
// It returns the exit status: 0 after a clean shutdown, 2 for a usage error, 1 otherwise.
func Main(args []string, stdout, stderr io.Writer, open OpenStore) int {
	fs := flag.NewFlagSet("bide-view", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
	}
	db := fs.String("db", "", "SQLite journal file to read, opened read-only (sqlite.OpenReadOnly)")
	var journals multi
	fs.Var(&journals, "journal", "exported journal (audit.ExportJournal as JSON) to read instead of -db; repeatable")
	run := fs.String("run", "", "run to open (default: the run list)")
	topo := fs.String("topology", "", "declared flow topology: JSON of flow.Topology(), to draw a flow run against")
	digest := fs.String("digest", "", "flow.Digest() of the declared flow, compared with the digest the run journaled")
	sth := fs.String("sth", "", "signed tree head (audit.SignedTreeHead JSON) to verify the journal against; needs -pubkey")
	evidence := fs.String("evidence", "", "evidence package (audit.EvidencePackage JSON) to verify; needs -pubkey")
	pubkey := fs.String("pubkey", "", "log public key as <scheme>:<hex>, or bare ed25519 hex, or a file holding it")
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
	if (*db == "") == (len(journals) == 0) {
		return usage("give exactly one of -db and -journal")
	}
	if (*sth != "" || *evidence != "") && *pubkey == "" {
		return usage("-sth and -evidence need -pubkey")
	}
	if *digest != "" && *topo == "" {
		return usage("-digest needs -topology")
	}
	o, closer, err := options(*db, journals, *topo, *digest, *sth, *evidence, *pubkey, open)
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
func options(db string, journals []string, topo, digest, sth, evidence, pubkey string, open OpenStore) (Options, io.Closer, error) {
	var o Options
	var closer io.Closer
	if db != "" {
		if open == nil {
			return o, nil, errors.New("this build reads no store; use -journal")
		}
		s, c, err := open(db)
		if err != nil {
			return o, nil, fmt.Errorf("open %s: %w", db, err)
		}
		j, err := agent.NewJournal(s)
		if err != nil {
			c.Close()
			return o, nil, err
		}
		o.Source, closer = JournalSource{J: j}, c
	} else {
		x := Exports{}
		for _, path := range journals {
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
	if topo != "" {
		var t plan.Topology
		if err := readStrict(topo, &t); err != nil {
			return o, closer, err
		}
		o.Flows = map[string]*Flow{t.Flow: {Topology: t, Digest: digest}}
	}
	if pubkey != "" {
		v, err := readKey(pubkey)
		if err != nil {
			return o, closer, err
		}
		if sth != "" {
			var h audit.SignedTreeHead
			if err := readStrict(sth, &h); err != nil {
				return o, closer, err
			}
			o.Provers = append(o.Provers, HeadProver(h, v))
		}
		if evidence != "" {
			var pkg audit.EvidencePackage
			if err := readStrict(evidence, &pkg); err != nil {
				return o, closer, err
			}
			o.Provers = append(o.Provers, EvidenceProver(pkg, v))
		}
	}
	return o, closer, nil
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
