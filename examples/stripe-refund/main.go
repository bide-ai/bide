// Command stripe-refund is a refund agent against Stripe's TEST mode API that survives being killed
// mid-refund. It looks up a payment intent (read-only), asks for 2-of-3 approval when the refund is
// over a threshold, sends the refund (POST /v1/refunds, with an Idempotency-Key and metadata naming
// the run), and writes a receipt. The journal is SQLite.
//
// With -crash-after-send the process dies right after the refund request goes out, before its
// result is journaled: Stripe has the refund, the journal does not know. Run it again and the step
// halts (outcome unknown) instead of refunding twice; the reconciler asks Stripe for the refund
// carrying this step's key, resolves the halt from that record, and the run completes. Exactly one
// refund exists.
//
//	export STRIPE_SECRET_KEY=sk_test_...        # test mode only; anything else is refused
//	go run . run -payment-intent pi_... -crash-after-send    # the process dies mid-refund (exit 3)
//	go run . run -payment-intent pi_...                      # halts, reconciles, completes
//	go run . approve -as ops                                 # when the refund is over -approval-threshold
//
// See README.md, and demo.sh for the whole story in one command.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/store/sqlite"
)

func say(format string, a ...any) { fmt.Printf(format+"\n", a...) }

// drive runs the agent until it finishes, pauses for approval, or halts for good. A halt on the
// refund is reconciled against Stripe (unless -reconcile=false) and the run resumed.
func drive(ctx context.Context, j *agent.Journal, api *stripe, cfg config) error {
	say("run %s: refund on %s (threshold %d, approval 2 of 3 above it)", cfg.runID, cfg.paymentIntent, cfg.threshold)
	for range 3 {
		ag, err := newAgent(j, api, cfg)
		if err != nil {
			return err
		}
		res, err := ag.Run(ctx, cfg.runID, agent.UserText("Refund "+cfg.paymentIntent+"."))
		if halt, ok := errors.AsType[*agent.OutcomeUnknown](err); ok {
			say("status: halted. %s %s (%s) was attempted and its result was never journaled: the outcome is unknown, so it is not sent again.", halt.Op.Kind, halt.Op.ID, halt.Op.ToolName)
			if !cfg.reconcile {
				say("not reconciling (-reconcile=false). Run again with reconciliation on, or resolve it by hand.")
				return nil
			}
			ok, err := reconcileWaiting(ctx, say, api, j, halt, cfg)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			continue
		}
		if pend, ok := errors.AsType[*agent.ApprovalPending](err); ok && pend.Quorum != nil {
			q := pend.Quorum
			say("step 2  approval %s is over the threshold: paused. approved %d of %d, waiting on %s", pend.ToolName, q.Approved, q.Need, strings.Join(q.Pending, ", "))
			say("status: paused. Approve with: go run . approve -db <db> -run %s -as %s", cfg.runID, q.Pending[0])
			return nil
		}
		if err != nil {
			return err
		}
		say("status: done")
		for _, p := range res.Message.Parts {
			if t, ok := p.(agent.Text); ok {
				say("step 4  receipt  %s", t.Text)
			}
		}
		return nil
	}
	return errors.New("the run halted again after reconciling: stopping")
}

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "run" && os.Args[1] != "approve") {
		fatal(errors.New("usage: stripe-refund run -payment-intent pi_... [flags] | approve -as ops|finance|risk"))
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	db := fs.String("db", "stripe-refund.db", "sqlite journal path")
	pi := fs.String("payment-intent", "", "the test-mode payment intent to refund, pi_... (run)")
	runID := fs.String("run", "", "run id (default stripe-refund-<payment intent>)")
	as := fs.String("as", "", "approver id: ops, finance or risk (approve)")
	cfg := config{}
	fs.Int64Var(&cfg.amount, "amount", 0, "amount to refund in minor units (cents); 0 refunds what is left (run)")
	fs.Int64Var(&cfg.threshold, "approval-threshold", 10000, "a refund over this many minor units needs 2-of-3 approval (run)")
	fs.BoolVar(&cfg.crashAfterSend, "crash-after-send", false, "exit right after the refund request is sent, before its result is journaled (run)")
	fs.BoolVar(&cfg.reconcile, "reconcile", true, "on a halted refund, ask Stripe for the refund and resolve the halt (run)")
	fs.DurationVar(&cfg.minHaltAge, "min-halt-age", 2*time.Second, "do not resolve a halt younger than this, so Stripe's record can settle (run)")
	_ = fs.Parse(os.Args[2:])

	cfg.paymentIntent = *pi
	cfg.runID = *runID
	if cfg.runID == "" {
		cfg.runID = "stripe-refund-" + cfg.paymentIntent
	}
	ctx := context.Background()

	var api *stripe
	if cmd == "run" {
		if !strings.HasPrefix(cfg.paymentIntent, "pi_") {
			fatal(errors.New("run: -payment-intent pi_... is required"))
		}
		var err error
		if api, err = newStripe(os.Getenv("STRIPE_API_BASE"), os.Getenv("STRIPE_SECRET_KEY")); err != nil {
			fatal(err)
		}
	} else if *as == "" || *runID == "" && cfg.paymentIntent == "" {
		fatal(errors.New("approve: -as and (-run or -payment-intent) are required"))
	}

	store, err := sqlite.Open(*db)
	if err != nil {
		fatal(err)
	}
	defer store.Close()
	j, err := agent.NewJournal(store)
	if err != nil {
		fatal(err)
	}
	if cmd == "run" {
		err = drive(ctx, j, api, cfg)
	} else {
		err = approve(ctx, j, cfg.runID, *as)
	}
	if err != nil {
		store.Close()
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
