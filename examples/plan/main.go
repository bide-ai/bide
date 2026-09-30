// Command plan shows the plan flow builder driving a small order-triage flow
// durably against an on-disk SQLite journal. It builds a realistic multi-step flow
// (two Step nodes and a Switch with When/Else), then, for one run id:
//
//   - prints flow.RenderMermaid(), the DECLARED topology;
//   - flow.Run(...) to a typed Receipt and prints it;
//   - flow.Conform(...) and prints whether the journaled run followed the declared graph;
//   - proves CRYPTOGRAPHIC conformance: commits to the run's journal with a signed
//     audit tree head, obtains an RFC 6962 inclusion proof for the flow:digest record
//     Run journaled before any node, and checks the proven digest equals flow.Digest() (see
//     proveTopologyConformance). This is the offline-verifiable "the run followed the
//     signed diagram" story.
//
// The point it demonstrates is the substrate guarantee the plan surface inherits for
// free: a non-idempotent side effect (reserving inventory, modelled as one appended
// witness line) fires AT MOST ONCE across a crash. Run drives every node as an agent.Step,
// whose attempt claim guards its body, so a node whose attempt was recorded but whose
// result was lost to a crash HALTS the resumed run (*agent.OutcomeUnknown) rather than
// re-firing the body.
//
// With no flags it is a clean demo end to end (no API key, no network):
//
//	go run .
//
// The flags exist so the cross-process e2e (e2e_test.go) can crash a chosen Step in one
// process and resume in a fresh one against the same journal file:
//
//	-db PATH       sqlite journal path (default: a temp file, removed on clean exit)
//	-run ID        run id to drive (default: "triage-demo")
//	-witness PATH  file the reserve step appends one line to per real reservation
//	-amount N      order amount; > 100 routes to the rush arm that reserves (default 500)
//	-crash POINT   inject a crash: "during-reserve" fires the reserve side effect then
//	               os.Exit(1) BEFORE its result is journaled (leaves an attempt with no
//	               result, so a resumed Run halts at reserve); "before-finalize" lets
//	               reserve commit, then os.Exit(1) at the start of finalize (leaves
//	               finalize attempted-but-unfinished)
//	-resolve       on resume: if Run halts at the side-effect-free finalize step, record
//	               finalize's result with agent.ResolveHaltRef (the documented resolution
//	               of a halt) and re-run to completion
//	-config        build the flow by plan.Load-ing the declarative config (declarativeConfig) instead
//	               of the code builder. The config-loaded flow uses the SAME node names and
//	               topology as the code-built flow, so it produces the SAME journal keys and
//	               the SAME flow.Digest(): a fresh-process resume off the same journal aligns
//	               node-for-node and the committed digest matches. All other flags apply
//	               unchanged, so the cross-process crash/resume harness drives a config-loaded
//	               flow exactly as it drives the code-built one.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/plan"
	"github.com/bide-ai/bide/store/sqlite"
)

// Order is the flow input: a single incoming order to triage.
type Order struct {
	ID     string `json:"id"`
	Amount int    `json:"amount"`
}

// Assessment is what the classify step produces from an Order: the routing decision the
// Switch reads. Rush selects the reserve-and-finalize path.
type Assessment struct {
	OrderID string `json:"order_id"`
	Amount  int    `json:"amount"`
	Rush    bool   `json:"rush"`
}

// Reservation is the reserve step's output: the reference for the inventory hold it took.
// Producing it is the flow's one non-idempotent effect.
type Reservation struct {
	OrderID string `json:"order_id"`
	Ref     string `json:"ref"`
}

// Receipt is the flow output: the terminal outcome for the order on either arm.
type Receipt struct {
	OrderID  string `json:"order_id"`
	Outcome  string `json:"outcome"`
	Detail   string `json:"detail"`
	Reserved bool   `json:"reserved"`
}

// config holds the run parameters parsed from flags, so both the demo path and the e2e
// harness drive main through the same code.
type config struct {
	db       string
	runID    string
	witness  string
	amount   int
	crash    string
	resolve  bool
	loadFlow bool
}

func main() {
	cfg := parseFlags()

	dbPath, cleanup, err := resolveDBPath(cfg.db)
	if err != nil {
		fatal(err)
	}
	defer cleanup()

	flow, err := selectFlow(cfg)
	if err != nil {
		fatal(err)
	}

	// The declared topology, before any run: the diagram the flow author wrote.
	fmt.Println("Declared topology (flow.RenderMermaid):")
	fmt.Println(flow.RenderMermaid())

	store, err := sqlite.Open(dbPath)
	if err != nil {
		fatal(fmt.Errorf("open sqlite store at %s: %w", dbPath, err))
	}
	defer store.Close()

	ctx := context.Background()
	order := Order{ID: cfg.runID, Amount: cfg.amount}

	out, runErr := flow.Run(ctx, store, cfg.runID, order)

	// A resumed run may halt with an unknown outcome (an attempt recorded, its result
	// lost to a crash). A node halts as the Step named by its node key ("node:<name>").
	// Report it, and optionally resolve a halt at the side-effect-free finalize step with
	// agent.ResolveHaltRef, exactly as the OutcomeUnknown doc prescribes.
	if halt, ok := errors.AsType[*agent.OutcomeUnknown](runErr); ok {
		fmt.Printf("Run halted at step %q: %v\n", halt.Op.ID, halt)
		if cfg.resolve && halt.Op.ID == "node:finalize" {
			out, runErr = resolveFinalize(ctx, flow, store, halt.Ref(), order)
		} else {
			reportConform(ctx, flow, store, cfg.runID)
			return
		}
	}
	if runErr != nil {
		fatal(fmt.Errorf("run %q: %w", cfg.runID, runErr))
	}

	fmt.Printf("Run output (typed Receipt): %+v\n", out)
	reportConform(ctx, flow, store, cfg.runID)
	proveTopologyConformance(ctx, flow, store, cfg.runID)

	// The declarative demonstration: the same triage flow authored as declarative config,
	// loaded, run, conformed, and shown to share the code-built flow's topology Digest.
	// It runs only on the clean demo path (no crash injection), against its own in-memory
	// store, so it never perturbs the crash/resume e2e that drives the sqlite journal.
	if cfg.crash == "" {
		demoDeclarative(ctx, flow)
		// The config surface beyond the linear case: a fan-in (join) diamond and a
		// bounded loop (loop_max back-edge), each authored as data, run, conformed, and
		// shown to share the code-built flow's topology Digest. Like demoDeclarative, they run
		// only on the clean path against their own in-memory stores.
		demoDeclarativeJoin(ctx)
		demoDeclarativeLoop(ctx)
	}
}

// proveTopologyConformance demonstrates the offline-verifiable "the run followed the
// signed diagram" story, tying three pieces together:
//
//  1. the DECLARED topology, hashed to flow.Digest();
//  2. the audit layer's signed tree head (STH) over this run's journal, which commits
//     to the whole history, including the flow:digest record Run wrote before any node;
//  3. an RFC 6962 inclusion proof that the flow:digest record is in the tree the STH
//     signed, plus a Conform pass over the same run.
//
// An auditor, given only the signed tree head, the proof bundle, and the signer's
// public key (obtained out of band), can verify OFFLINE that a run committed to THIS
// topology: the bundle's signature is authentic, the flow:digest record is included
// under the signed root, and the proven digest equals the declared flow's Digest().
// The signing key here is generated for the demo; a real deployment anchors the STH
// and its key in a separate trust domain (see the audit package security model).
func proveTopologyConformance(ctx context.Context, flow *plan.Flow[Order, Receipt], store agent.Durable, runID string) {
	// The declared topology digest: the fingerprint of the diagram the author wrote.
	declared := flow.Digest()

	// Commit to the run's journal with a signed tree head. In production the key is
	// held by a separate trust domain and the STH is anchored out of band; here we
	// generate a demo key so the example is self-contained (no network, no key file).
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		fatal(fmt.Errorf("generate demo signing key: %w", err))
	}
	th, err := audit.NewTreeHead(ctx, store, runID, time.Now().UnixNano())
	if err != nil {
		fatal(fmt.Errorf("build tree head over run %q: %w", runID, err))
	}
	sth := audit.SignTreeHead(th, priv)

	// Locate the flow:digest record's index in the journal (Run writes it after the
	// journal header and the run's start, so it is index 2, but resolve it by name to stay
	// robust), then prove its inclusion under the signed tree head.
	idx, err := digestRecordIndex(ctx, store, runID)
	if err != nil {
		fatal(err)
	}
	bundle, err := audit.ProveRecord(ctx, store, runID, idx, sth)
	if err != nil {
		fatal(fmt.Errorf("prove flow:digest inclusion: %w", err))
	}

	// Verify OFFLINE: (1) the bundle is authentic under the signer's public key and the
	// flow:digest record is included under the signed root; (2) the proven digest equals
	// the declared topology's Digest(). Together these prove the run followed THIS diagram.
	ok, err := bundle.Verify(pub)
	if err != nil {
		fatal(fmt.Errorf("verify proof bundle: %w", err))
	}
	if !ok {
		fmt.Println("Cryptographic conformance: FAILED (the inclusion proof did not verify under the signing key).")
		return
	}
	proven, err := decodeJournaledDigest(bundle.Record.Result)
	if err != nil {
		fatal(fmt.Errorf("decode proven digest: %w", err))
	}
	if proven != declared {
		fmt.Printf("Cryptographic conformance: FAILED (proven digest %s != declared %s: the run followed a different topology).\n", proven, declared)
		return
	}
	fmt.Printf("Cryptographic conformance: the run committed to the declared topology under the signed tree head (digest %s).\n", declared)
}

// digestRecordIndex returns the journal index of the reserved flow:digest record for
// runID, so audit.ProveRecord can build an inclusion proof for it. It errors if no
// such record exists (the run never started, or was journaled without Run).
func digestRecordIndex(ctx context.Context, store agent.Durable, runID string) (int, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return 0, fmt.Errorf("load history for run %q: %w", runID, err)
	}
	for i, r := range recs {
		if r.Name == "flow:digest" {
			return i, nil
		}
	}
	return 0, fmt.Errorf("run %q has no flow:digest record", runID)
}

// decodeJournaledDigest reads the hex topology digest from a flow:digest record's
// Result, which Run JSON-encodes as a string.
func decodeJournaledDigest(raw json.RawMessage) (string, error) {
	var digest string
	if err := json.Unmarshal(raw, &digest); err != nil {
		return "", err
	}
	return digest, nil
}

// buildFlow assembles the order-triage flow: classify the order, then Switch on the
// assessment to either reserve-and-finalize (the rush arm, which fires the one
// non-idempotent side effect) or decline (a side-effect-free terminal). Build validates
// the whole graph and freezes it.
func buildFlow(cfg config) (*plan.Flow[Order, Receipt], error) {
	b := plan.New[Order, Receipt]("order-triage")

	// classify is the entry step: it consumes the flow input Order and produces the
	// Assessment the Switch routes on. It has no external side effect.
	classify := b.Step("classify", func(_ context.Context, o Order) (Assessment, error) {
		return Assessment{OrderID: o.ID, Amount: o.Amount, Rush: o.Amount > 100}, nil
	})

	// reserve is the non-idempotent step: it takes an inventory hold, modelled as one
	// appended witness line, then produces a Reservation. The crash flag injects a
	// failure here so the e2e can observe at-most-once across a process boundary.
	reserve := b.Step("reserve", func(_ context.Context, a Assessment) (Reservation, error) {
		appendWitness(cfg.witness, "reserved "+a.OrderID)
		if cfg.crash == "during-reserve" {
			// The effect above already fired. Exit BEFORE returning, so Run records no
			// result: a resumed Run finds the attempt marker with no result and halts
			// rather than re-firing the hold. This is the crown-jewel property.
			os.Exit(1)
		}
		return Reservation{OrderID: a.OrderID, Ref: "hold-" + a.OrderID}, nil
	})

	// finalize is the rush-arm terminal: it turns the Reservation into the flow output.
	// It is side-effect-free, so a halt here is safe to resolve out of band.
	finalize := b.Step("finalize", func(_ context.Context, r Reservation) (Receipt, error) {
		if cfg.crash == "before-finalize" {
			// reserve has already committed its result by now; crash at the start of
			// finalize leaves finalize attempted-but-unfinished for the resume to handle.
			os.Exit(1)
		}
		return Receipt{OrderID: r.OrderID, Outcome: "reserved", Detail: r.Ref, Reserved: true}, nil
	})

	// decline is the Else-arm terminal: a small order is declined with no side effect.
	decline := b.Step("decline", func(_ context.Context, a Assessment) (Receipt, error) {
		return Receipt{OrderID: a.OrderID, Outcome: "declined", Detail: "below rush threshold"}, nil
	})

	// Route on the assessment: a rush order reserves, everything else declines.
	b.Switch(classify,
		plan.When(func(a Assessment) bool { return a.Rush }, reserve).Named("rush"), // the config's predicate name, so the digests match
		plan.Else(decline),
	)
	// The reserve arm continues to finalize; the decline arm is already terminal.
	b.Edge(reserve, finalize)

	flow, err := b.Build()
	if err != nil {
		return nil, fmt.Errorf("build flow: %w", err)
	}
	return flow, nil
}

// selectFlow returns the flow the harness runs: the code-built triage flow by default, or
// the config-loaded equivalent when -config is set. Both describe the identical topology
// (same node names, same wiring), so they share journal keys and flow.Digest(); the only
// difference is authorship (Go builder vs plan.Load of declarativeConfig).
func selectFlow(cfg config) (*plan.Flow[Order, Receipt], error) {
	if cfg.loadFlow {
		return buildFlowFromConfig(cfg)
	}
	return buildFlow(cfg)
}

// buildFlowFromConfig builds the order-triage flow by plan.Load-ing declarativeConfig against a
// registry whose block bodies are the HARNESS variants: reserve appends the witness line
// and honors the crash flag, and finalize honors the crash flag, exactly as buildFlow's
// steps do. The registry reuses the same node names (classify/reserve/finalize/decline plus
// the rush predicate) declarativeConfig references, so the loaded flow produces the SAME journal
// keys and the SAME Digest() as the code-built flow: a fresh process can resume the same run
// id off the same journal and the committed topology digest matches. The block BODIES differ
// from buildDeclarativeRegistry's clean demo variants (they take the witness/crash side effects the
// crash/resume e2e observes), which is sound because the topology Digest commits to node
// names, kinds, and I/O types, not to node bodies.
func buildFlowFromConfig(cfg config) (*plan.Flow[Order, Receipt], error) {
	reg := plan.NewRegistry()

	// classify: Order -> Assessment, the entry step the Switch routes on. Side-effect-free,
	// identical to the code-built classify body.
	if err := plan.RegisterStep(reg, "classify", func(_ context.Context, o Order) (Assessment, error) {
		return Assessment{OrderID: o.ID, Amount: o.Amount, Rush: o.Amount > 100}, nil
	}); err != nil {
		return nil, fmt.Errorf("register classify: %w", err)
	}

	// reserve: Assessment -> Reservation, the one non-idempotent step. It appends the witness
	// line then, under -crash during-reserve, exits BEFORE returning so Run records no result,
	// exactly as buildFlow's reserve does. This is the effect the e2e proves fires at most once.
	if err := plan.RegisterStep(reg, "reserve", func(_ context.Context, a Assessment) (Reservation, error) {
		appendWitness(cfg.witness, "reserved "+a.OrderID)
		if cfg.crash == "during-reserve" {
			os.Exit(1)
		}
		return Reservation{OrderID: a.OrderID, Ref: "hold-" + a.OrderID}, nil
	}); err != nil {
		return nil, fmt.Errorf("register reserve: %w", err)
	}

	// finalize: Reservation -> Receipt, the side-effect-free rush-arm terminal. Under -crash
	// before-finalize it exits at the start (after reserve has committed), leaving finalize
	// attempted-but-unfinished for the resume, exactly as buildFlow's finalize does.
	if err := plan.RegisterStep(reg, "finalize", func(_ context.Context, r Reservation) (Receipt, error) {
		if cfg.crash == "before-finalize" {
			os.Exit(1)
		}
		return Receipt{OrderID: r.OrderID, Outcome: "reserved", Detail: r.Ref, Reserved: true}, nil
	}); err != nil {
		return nil, fmt.Errorf("register finalize: %w", err)
	}

	// decline: Assessment -> Receipt, the Else-arm terminal, side-effect-free.
	if err := plan.RegisterStep(reg, "decline", func(_ context.Context, a Assessment) (Receipt, error) {
		return Receipt{OrderID: a.OrderID, Outcome: "declined", Detail: "below rush threshold"}, nil
	}); err != nil {
		return nil, fmt.Errorf("register decline: %w", err)
	}

	// rush: the Switch predicate over Assessment, identical to the code-built When.
	if err := plan.RegisterPredicate(reg, "rush", func(a Assessment) bool { return a.Rush }); err != nil {
		return nil, fmt.Errorf("register rush: %w", err)
	}

	flow, err := plan.Load[Order, Receipt]([]byte(declarativeConfig), reg)
	if err != nil {
		return nil, fmt.Errorf("load declarative config: %w", err)
	}
	return flow, nil
}

// resolveFinalize handles a resumed run that halted at the side-effect-free finalize
// step: finalize took no external action, so its output is safe to record out of band.
// agent.ResolveHaltRef records it as the node's result (the resolution the OutcomeUnknown
// doc prescribes), then Run replays the now-complete journal to a typed output. It never
// re-runs the reserve side effect, which already committed.
func resolveFinalize(ctx context.Context, flow *plan.Flow[Order, Receipt], store *sqlite.Store, ref agent.HaltRef, order Order) (Receipt, error) {
	receipt := Receipt{OrderID: order.ID, Outcome: "reserved", Detail: "hold-" + order.ID, Reserved: true}
	if err := agent.ResolveHaltRef(ctx, store, ref, agent.Outcome{Result: receipt}); err != nil {
		return Receipt{}, fmt.Errorf("resolve the finalize halt: %w", err)
	}
	fmt.Println("Resolved the finalize halt out of band; re-running to completion.")
	return flow.Run(ctx, store, ref.RunID, order)
}

// reportConform prints whether the journaled run for runID followed the declared graph.
func reportConform(ctx context.Context, flow *plan.Flow[Order, Receipt], store *sqlite.Store, runID string) {
	ok, diffs, err := flow.Conform(ctx, store, runID)
	if err != nil {
		fatal(fmt.Errorf("conform %q: %w", runID, err))
	}
	if ok {
		fmt.Println("Conform: the run followed the declared graph (no diffs).")
		return
	}
	fmt.Printf("Conform: the run DIVERGED from the declared graph: %v\n", diffs)
}

// appendWitness records one line for an observable, non-idempotent effect, so a test in a
// separate process can count how many times the effect actually fired. An empty path
// (the plain demo) records nothing.
func appendWitness(path, line string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fatal(fmt.Errorf("open witness %s: %w", path, err))
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, line); err != nil {
		fatal(fmt.Errorf("append witness %s: %w", path, err))
	}
}

// parseFlags reads the run parameters. Defaults make `go run .` a clean demo.
func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.db, "db", "", "sqlite journal path (default: a temp file removed on clean exit)")
	flag.StringVar(&cfg.runID, "run", "triage-demo", "run id to drive")
	flag.StringVar(&cfg.witness, "witness", "", "file the reserve step appends one line to per real reservation")
	flag.IntVar(&cfg.amount, "amount", 500, "order amount; over 100 routes to the reserving rush arm")
	flag.StringVar(&cfg.crash, "crash", "", `inject a crash: "during-reserve" or "before-finalize"`)
	flag.BoolVar(&cfg.resolve, "resolve", false, "on resume, resolve a finalize halt out of band and complete")
	flag.BoolVar(&cfg.loadFlow, "config", false, "build the flow by plan.Load-ing the declarative config instead of the code builder (same topology, same journal keys, same Digest)")
	flag.Parse()
	return cfg
}

// resolveDBPath returns the journal path to use. When -db is empty (the plain demo) it
// creates a temp file and returns a cleanup that removes it on a clean exit; when -db is
// set (the e2e, which reopens the same file across processes) it returns that path and a
// no-op cleanup so the file persists between processes.
func resolveDBPath(db string) (string, func(), error) {
	if db != "" {
		return db, func() {}, nil
	}
	f, err := os.CreateTemp("", "plan-example-*.db")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temp sqlite file: %w", err)
	}
	path := f.Name()
	f.Close()
	return path, func() { os.Remove(path) }, nil
}

// fatal prints err and exits non-zero. It is the example's single error boundary.
func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
