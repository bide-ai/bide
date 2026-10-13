package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

const (
	lookupID = "lookup-1" // the tool-use ids the scripted model gives its two calls
	refundID = "refund-1"
)

// policy is the gate on a refund over the threshold: 2 of these 3 approvers must sign off.
var policy = agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}

// demoKey derives a fixed ed25519 key from a name, so separate processes agree on the approvers'
// keys. WARNING: DEMO ONLY. Each private key is SHA-256 of a public string, so anyone can sign as
// any approver. Real approvers hold their own randomly generated keys.
func demoKey(name string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("bide examples/stripe-refund " + name))
	return ed25519.NewKeyFromSeed(seed[:])
}

func approverVerifiers() agent.ApproverVerifierFor {
	keys := map[string]ed25519.PublicKey{}
	for _, id := range policy.Approvers {
		keys[id] = demoKey("approver " + id).Public().(ed25519.PublicKey)
	}
	return func(id string) (agent.ApproverVerifier, bool) {
		pub, ok := keys[id]
		if !ok {
			return nil, false
		}
		return audit.Ed25519Verifier{Pub: pub}, true
	}
}

// config is everything one invocation needs; a restart passes the same values.
type config struct {
	runID          string
	paymentIntent  string
	amount         int64 // minor units; 0 refunds what is left
	threshold      int64 // a refund over this many minor units needs 2-of-3 approval
	crashAfterSend bool
	reconcile      bool
	minHaltAge     time.Duration
}

// refundKey is the Idempotency-Key of a refund and the value a reconciler finds it by. It names
// the run and the tool call, so it is the same on every attempt of this refund and different for
// any other.
func refundKey(runID, callID string) string { return runID + "/" + callID }

type lookupArgs struct {
	PaymentIntent string `json:"payment_intent"`
}

// lookupResult is what the read-only lookup reports.
type lookupResult struct {
	PaymentIntent string `json:"payment_intent"`
	Currency      string `json:"currency"`
	Received      int64  `json:"received"`
	Refunded      int64  `json:"refunded"`
	Refundable    int64  `json:"refundable"`
}

type refundArgs struct {
	PaymentIntent  string `json:"payment_intent"`
	Amount         int64  `json:"amount"`
	IdempotencyKey string `json:"idempotency_key"`
}

// refundResult is what the refund returns, and what a reconciler records when it finds the refund
// at Stripe, so a reconciled step and a clean one read alike.
type refundResult struct {
	Refund   string `json:"refund"`
	Status   string `json:"status"`
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// model is a scripted, stateless stand-in for an LLM: look the payment up, refund it (through the
// gated tool above the threshold), then write the receipt. Stateless, so a restarted run needs no
// per-run script: it reads what the journal already holds.
type model struct{ cfg config }

func (m model) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit, 2)
	call := func(id, name string, args any) {
		b, _ := json.Marshal(args)
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: id, Name: name, ArgsFragment: b}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	}
	say := func(s string) {
		ch <- agent.Emit{Event: agent.TextDelta{Text: s}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	results := toolResults(req.Messages)
	look, ok := results[lookupID]
	switch {
	case !ok:
		call(lookupID, "lookup", lookupArgs{m.cfg.paymentIntent})
	case look.IsError:
		say("No refund: " + resultText(look))
	default:
		var l lookupResult
		_ = json.Unmarshal(look.Result, &l)
		amount := m.cfg.amount
		if amount == 0 {
			amount = l.Refundable
		}
		ref, done := results[refundID]
		switch {
		case done && ref.IsError:
			say("Refund failed: " + resultText(ref))
		case done:
			var r refundResult
			_ = json.Unmarshal(ref.Result, &r)
			say(fmt.Sprintf("Receipt: refunded %d %s on %s as %s (%s).", r.Amount, r.Currency, l.PaymentIntent, r.Refund, r.Status))
		case amount <= 0 || amount > l.Refundable:
			say(fmt.Sprintf("No refund: %d requested, %d refundable on %s.", amount, l.Refundable, l.PaymentIntent))
		default:
			tool := "refund"
			if amount > m.cfg.threshold {
				tool = "refund_large" // over the threshold: the same refund behind the approval gate
			}
			call(refundID, tool, refundArgs{PaymentIntent: l.PaymentIntent, Amount: amount, IdempotencyKey: refundKey(m.cfg.runID, refundID)})
		}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

func toolResults(msgs []agent.Message) map[string]agent.ToolResult {
	out := map[string]agent.ToolResult{}
	for _, m := range msgs {
		for _, p := range m.Parts {
			if tr, ok := p.(agent.ToolResult); ok {
				out[tr.ToolUseID] = tr
			}
		}
	}
	return out
}

func resultText(tr agent.ToolResult) string {
	var s string
	if json.Unmarshal(tr.Result, &s) != nil {
		s = string(tr.Result)
	}
	return s
}

// newAgent builds the agent: a read-only lookup, and the refund as two tools that share one
// implementation, the gated one for amounts over the threshold.
func newAgent(j *agent.Journal, api *stripe, cfg config) (*agent.Agent, error) {
	lookup, err := agent.Func("lookup", "look up a payment intent and what is left to refund",
		func(ctx context.Context, in lookupArgs) (lookupResult, error) { return doLookup(ctx, api, in) },
		agent.WithSafety(agent.Safety{ReadOnly: true}))
	if err != nil {
		return nil, err
	}
	do := func(ctx context.Context, in refundArgs) (refundResult, error) { return doRefund(ctx, api, cfg, in) }
	small, err := agent.Func("refund", "refund a payment (at or under the approval threshold)", do)
	if err != nil {
		return nil, err
	}
	large, err := agent.Func("refund_large", "refund a payment over the threshold: needs 2 of 3 approvers", do, agent.WithApproval(&policy))
	if err != nil {
		return nil, err
	}
	return agent.New(model{cfg}, j, agent.WithTools(lookup, small, large), agent.WithApproverVerifiers(approverVerifiers()))
}

func doLookup(ctx context.Context, api *stripe, in lookupArgs) (lookupResult, error) {
	pi, err := api.paymentIntent(ctx, in.PaymentIntent)
	if err != nil {
		return lookupResult{}, err
	}
	if pi.Status != "succeeded" || pi.LatestCharge == "" {
		return lookupResult{}, fmt.Errorf("%s is %q, not a succeeded payment: nothing to refund", pi.ID, pi.Status)
	}
	ch, err := api.charge(ctx, pi.LatestCharge)
	if err != nil {
		return lookupResult{}, err
	}
	l := lookupResult{PaymentIntent: pi.ID, Currency: pi.Currency, Received: pi.AmountReceived, Refunded: ch.AmountRefunded,
		Refundable: pi.AmountReceived - ch.AmountRefunded}
	say("step 1  lookup   %s: received %d, already refunded %d, refundable %d (%s, minor units)", l.PaymentIntent, l.Received, l.Refunded, l.Refundable, l.Currency)
	return l, nil
}

// doRefund is the side effect. It sends POST /v1/refunds with an Idempotency-Key and metadata that
// names this run and call, and returns what Stripe answered. With -crash-after-send the process
// dies right after the request went out and before the answer can be journaled: the unknown
// outcome this example exists to resolve.
func doRefund(ctx context.Context, api *stripe, cfg config, in refundArgs) (refundResult, error) {
	meta := map[string]string{"bide_run": cfg.runID, "bide_call": refundID, "bide_key": in.IdempotencyKey}
	say("step 3  refund   POST /v1/refunds %s amount %d, Idempotency-Key %s", in.PaymentIntent, in.Amount, in.IdempotencyKey)
	r, err := api.createRefund(ctx, in.PaymentIntent, in.Amount, in.IdempotencyKey, meta)
	if cfg.crashAfterSend {
		say("        -crash-after-send: the refund request was sent; the process dies before its result is journaled")
		os.Exit(3)
	}
	if err != nil {
		// A 4xx answer is Stripe refusing: the refund did not happen, a known failure. Anything
		// else (a dropped connection, a timeout, a 5xx) leaves it unknown whether the refund
		// exists, so the call says so and a restart halts instead of sending it again.
		if ae, ok := errors.AsType[*apiError](err); ok && ae.Status/100 == 4 {
			return refundResult{}, err
		}
		return refundResult{}, fmt.Errorf("refund request to Stripe: %w: %w", err, agent.ErrToolOutcomeUnknown)
	}
	say("        Stripe answered: %s %s", r.ID, r.Status)
	return refundResult{Refund: r.ID, Status: r.Status, Amount: r.Amount, Currency: r.Currency}, nil
}

// reconcile resolves a halted refund from Stripe's own record. It lists the payment intent's
// refunds and looks for the one carrying this call's key in its metadata. Found: it records that
// refund as the step's outcome, with what it read as evidence, and reports true. Not found: it
// resolves nothing, since an absent record does not prove the refund was never sent, and reports
// false for a human to decide.
func reconcile(ctx context.Context, w func(string, ...any), api *stripe, j *agent.Journal, halt *agent.OutcomeUnknown, cfg config) (bool, error) {
	key := refundKey(halt.RunID, halt.Op.ID)
	w("reconcile: asking Stripe for the refunds of %s that carry metadata bide_key=%s", cfg.paymentIntent, key)
	refunds, err := api.listRefunds(ctx, cfg.paymentIntent)
	if err != nil {
		return false, err
	}
	var found *refund
	for i := range refunds {
		if refunds[i].Metadata["bide_key"] == key {
			found = &refunds[i]
			break
		}
	}
	if found == nil {
		w("reconcile: no refund at Stripe carries that key. Not resolving: the request may still be in flight, or never left. A human decides.")
		return false, nil
	}
	w("reconcile: found %s at Stripe: %d %s, status %s", found.ID, found.Amount, found.Currency, found.Status)
	out := agent.Outcome{
		Result:   refundResult{Refund: found.ID, Status: found.Status, Amount: found.Amount, Currency: found.Currency},
		Evidence: map[string]any{"source": "GET /v1/refunds?payment_intent=" + cfg.paymentIntent, "matched": "metadata.bide_key=" + key, "refund": found},
	}
	if found.Status == "failed" || found.Status == "canceled" {
		out.Result, out.IsError = "refund "+found.ID+" is "+found.Status+" at Stripe", true
	}
	if err := agent.ResolveHalt(ctx, j, halt.Ref(), out, agent.WithMinHaltAge(cfg.minHaltAge)); err != nil {
		return false, err
	}
	w("reconcile: resolved %s from Stripe's record (reconciled, with evidence)", halt.Op.ID)
	return true, nil
}

// reconcileWaiting is reconcile that waits out a halt too young to resolve (the provider's record
// can lag the send), then tries again.
func reconcileWaiting(ctx context.Context, w func(string, ...any), api *stripe, j *agent.Journal, halt *agent.OutcomeUnknown, cfg config) (bool, error) {
	// Do not even ask before the record can have settled: asking early reads "absent".
	if age := time.Since(halt.AttemptedAt); !halt.AttemptedAt.IsZero() && age < cfg.minHaltAge {
		wait := cfg.minHaltAge - age
		w("reconcile: the refund was attempted %s ago; waiting %s for Stripe's record to settle", age.Round(time.Millisecond), wait.Round(time.Millisecond))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	for {
		ok, err := reconcile(ctx, w, api, j, halt, cfg)
		young, tooYoung := errors.AsType[*agent.HaltTooYoung](err)
		if !tooYoung {
			return ok, err
		}
		wait := young.Min - young.Age + 50*time.Millisecond
		w("reconcile: the halt is %s old; waiting %s for Stripe's record to settle", young.Age.Round(time.Millisecond), wait.Round(time.Millisecond))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// approve records one approver's signed decision on the paused refund call. The approver signs
// exactly what the gate shows: run, call id, tool and arguments.
func approve(ctx context.Context, j *agent.Journal, runID, as string) error {
	recs, err := j.History(ctx, runID)
	if err != nil {
		return err
	}
	_, call, ok := agent.FindToolCall(recs, refundID)
	if !ok {
		return fmt.Errorf("no pending call %s in run %s (run the agent first, with an amount over the threshold)", refundID, runID)
	}
	subject := agent.ApprovalSubject{RunID: runID, ToolUseID: refundID, ToolName: call.Name, Args: call.Args}
	decidedAt := time.Now() // when the approver decided, by their clock: signed with the decision
	sig := ed25519.Sign(demoKey("approver "+as), agent.ApprovalDecisionBytes(subject, as, true, decidedAt))
	d := agent.Decision{RunID: runID, ToolUseID: refundID, ApproverID: as, Approved: true, DecidedAt: decidedAt, Alg: audit.AlgEd25519, Signature: sig}
	if err := agent.SubmitDecision(ctx, j, d, agent.WithDecisionCheck(approverVerifiers())); err != nil {
		if errors.Is(err, agent.ErrInvalidApproval) || errors.Is(err, agent.ErrAlreadyDecided) {
			say("refused: %v", err)
			return nil
		}
		return err
	}
	var args refundArgs
	_ = json.Unmarshal(call.Args, &args)
	say("recorded: %s approved %s (amount %d)", as, call.Name, args.Amount)
	return nil
}
