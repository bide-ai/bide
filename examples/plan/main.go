// Command plan shows the rung-1 plan flow builder driving a small order-triage flow
// durably against an on-disk SQLite journal. It builds a realistic multi-step flow
// (two Step nodes and a Switch with When/Else), then, for one run id:
//
//   - prints flow.RenderMermaid(), the DECLARED topology;
//   - flow.Run(...) to a typed Receipt and prints it;
//   - flow.Conform(...) and prints whether the journaled run followed the declared graph.
//
// The point it demonstrates is the substrate guarantee the plan surface inherits for
// free: a non-idempotent side effect (reserving inventory, modelled as one appended
// witness line) fires AT MOST ONCE across a crash. Run drives every node under an
// attempt/result guard, so a node whose attempt was recorded but whose result was lost
// to a crash HALTS the resumed run (*plan.HaltAmbiguous) rather than re-firing the body.
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
//	               finalize's result out of band (the documented HaltAmbiguous resolution)
//	               and re-run to completion
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/plan"
	"github.com/dayna/go-agents/store/sqlite"
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
	db      string
	runID   string
	witness string
	amount  int
	crash   string
	resolve bool
}

func main() {
	cfg := parseFlags()

	dbPath, cleanup, err := resolveDBPath(cfg.db)
	if err != nil {
		fatal(err)
	}
	defer cleanup()

	flow, err := buildFlow(cfg)
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
	// lost to a crash). Report it, and optionally resolve a halt at the side-effect-free
	// finalize step out of band, exactly as the HaltAmbiguous doc prescribes.
	var halt *plan.HaltAmbiguous
	if errors.As(runErr, &halt) {
		fmt.Printf("Run halted at step %q: %v\n", halt.Step, halt)
		if cfg.resolve && halt.Step == "finalize" {
			out, runErr = resolveFinalize(ctx, flow, store, cfg.runID, order)
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
}

// buildFlow assembles the order-triage flow: classify the order, then Switch on the
// assessment to either reserve-and-finalize (the rush arm, which fires the one
// non-idempotent side effect) or decline (a side-effect-free terminal). Build validates
// the whole graph and freezes it.
func buildFlow(cfg config) (*plan.Flow[Order, Receipt], error) {
	b := plan.New[Order, Receipt]("order-triage")

	// classify is the entry step: it consumes the flow input Order and produces the
	// Assessment the Switch routes on. It has no external side effect.
	classify := b.Step("classify", func(o Order) (Assessment, error) {
		return Assessment{OrderID: o.ID, Amount: o.Amount, Rush: o.Amount > 100}, nil
	})

	// reserve is the non-idempotent step: it takes an inventory hold, modelled as one
	// appended witness line, then produces a Reservation. The crash flag injects a
	// failure here so the e2e can observe at-most-once across a process boundary.
	reserve := b.Step("reserve", func(a Assessment) (Reservation, error) {
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
	finalize := b.Step("finalize", func(r Reservation) (Receipt, error) {
		if cfg.crash == "before-finalize" {
			// reserve has already committed its result by now; crash at the start of
			// finalize leaves finalize attempted-but-unfinished for the resume to handle.
			os.Exit(1)
		}
		return Receipt{OrderID: r.OrderID, Outcome: "reserved", Detail: r.Ref, Reserved: true}, nil
	})

	// decline is the Else-arm terminal: a small order is declined with no side effect.
	decline := b.Step("decline", func(a Assessment) (Receipt, error) {
		return Receipt{OrderID: a.OrderID, Outcome: "declined", Detail: "below rush threshold"}, nil
	})

	// Route on the assessment: a rush order reserves, everything else declines.
	b.Switch(classify,
		plan.When(func(a Assessment) bool { return a.Rush }, reserve),
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

// resolveFinalize handles a resumed run that halted at the side-effect-free finalize
// step: finalize took no external action, so its result is safe to record out of band
// (the resolution the HaltAmbiguous doc prescribes). It records finalize's result under
// the step's own journal name, then re-runs so Run replays the now-complete journal to a
// typed output. It never re-runs the reserve side effect, which already committed.
func resolveFinalize(ctx context.Context, flow *plan.Flow[Order, Receipt], store *sqlite.Store, runID string, order Order) (Receipt, error) {
	receipt := Receipt{OrderID: order.ID, Outcome: "reserved", Detail: "hold-" + order.ID, Reserved: true}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return Receipt{}, fmt.Errorf("encode resolved finalize result: %w", err)
	}
	if _, err := store.Do(ctx, runID, "finalize", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: encoded}, nil
	}); err != nil {
		return Receipt{}, fmt.Errorf("record resolved finalize result: %w", err)
	}
	fmt.Println("Resolved the finalize halt out of band; re-running to completion.")
	return flow.Run(ctx, store, runID, order)
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
