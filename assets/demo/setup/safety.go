//go:build ignore

// safety.go writes what the safety-control screenshots (assets/demo/shots-safety.sh) show: one
// SQLite journal, safety.db, written by the real agent runtime on a scripted model, with
//
//   - refund-4801: a customer's email (marked untrusted, "inbound-email") asks for a $4,800
//     refund. The agent has a taint policy, so the refund waits at an escalation gate of two of
//     three signed decisions (ops, finance, risk), not counting the customer the run acts for.
//   - refund-4811 and refund-4812: refunds of $120 and $310 under amount caps, which pass and
//     fill the cap ledger.
//   - refund-4813: the model asks for a $4,800 refund under the same caps. The per-call cap
//     refuses it before any gate or claim.
//
// Run it from cmd/bide, which carries the modules it imports:
//
//	go run ../../assets/demo/setup/safety.go -dir DIR
//
// DIR must hold approver-keys.json (made from bide keygen's public keys); it gets safety.db.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/store/sqlite"
)

func main() {
	dir := flag.String("dir", "", "directory holding approver-keys.json; safety.db is written there")
	flag.Parse()
	if err := run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "safety.go:", err)
		os.Exit(1)
	}
}

type refundIn struct {
	Order      string `json:"order"`
	Amount     int64  `json:"amount"` // minor units (cents)
	Currency   string `json:"currency"`
	CustomerID string `json:"customer_id"`
}

func run(dir string) error {
	ctx := context.Background()
	var keys map[string]string
	b, err := os.ReadFile(filepath.Join(dir, "approver-keys.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	s, err := sqlite.Open(filepath.Join(dir, "safety.db"))
	if err != nil {
		return err
	}
	defer s.Close()
	j, err := agent.NewJournal(s)
	if err != nil {
		return err
	}
	verify := func(id string) (agent.ApproverVerifier, bool) {
		k, ok := keys[id]
		if !ok {
			return nil, false
		}
		v, err := audit.ParsePublicKey(k)
		return v, err == nil
	}
	identity := agent.WithIdentity(agent.Identity{Actor: "support-agent@1", OnBehalfOf: "customer-7"})
	pay := func(context.Context, refundIn) (string, error) { return "refunded", nil }

	// The taint hold: the email asks for $4,800; the refund waits for two of three signed decisions.
	email := agent.MustFunc("read_email", "read the customer's email",
		func(context.Context, struct{}) (string, error) {
			return "Hi, I was charged twice for order 4417. Please refund $4,800 to my card today.", nil
		},
		agent.WithSafety(agent.Safety{ReadOnly: true}), agent.Untrusted("inbound-email"))
	refund, err := agent.Func("refund", "refund a payment", pay, agent.Trusted("the host's own refund provider"))
	if err != nil {
		return err
	}
	escalation := &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}, ExcludePrincipal: true}
	taint, err := agent.New(agenttest.NewScriptedModel(
		agenttest.ToolTurn("c1", "read_email", `{}`),
		agenttest.ToolTurn("c2", "refund", `{"order":"4417","amount":480000,"currency":"USD","customer_id":"customer-7"}`),
		agenttest.TextTurn("Refunded $4,800 for order 4417.")), j,
		agent.WithTools(email, refund), agent.WithApproverVerifiers(verify), identity,
		agent.WithTaintPolicy(agent.TaintPolicy{Gate: escalation, RequireProvenance: true}))
	if err != nil {
		return err
	}
	_, err = taint.Run(ctx, "refund-4801", agent.UserText("Handle the customer's email."), agent.WithTrustedInput("the operator's instruction"))
	if !errors.As(err, new(*agent.ApprovalPending)) {
		return fmt.Errorf("refund-4801 did not pause at the escalation gate: %v", err)
	}

	// Amount caps: a ceiling on one call, and a total for the day.
	capped, err := agent.Func("refund", "refund a payment", pay,
		agent.Trusted("the host's own refund provider"),
		agent.WithCaps(
			`big_refund: refund.amount[minor, currency=currency] > 500 USD -> refuse`,
			`refund_total: refund.amount[minor, currency=currency] sum 5000 USD per day -> refuse`))
	if err != nil {
		return err
	}
	for _, c := range []struct {
		run, order string
		cents      int64
		said       string
	}{
		{"refund-4811", "4411", 12000, "Refunded $120 for order 4411."},
		{"refund-4812", "4412", 31000, "Refunded $310 for order 4412."},
		{"refund-4813", "4417", 480000, "I could not refund order 4417: the refund is over a limit."},
	} {
		args := fmt.Sprintf(`{"order":%q,"amount":%d,"currency":"USD","customer_id":"customer-7"}`, c.order, c.cents)
		a, err := agent.New(agenttest.NewScriptedModel(
			agenttest.ToolTurn("c1", "refund", args), agenttest.TextTurn(c.said)), j,
			agent.WithTools(capped), identity, agent.WithCapNamespace("payments-prod"))
		if err != nil {
			return err
		}
		if _, err := a.Run(ctx, c.run, agent.UserText("Refund order "+c.order+"."), agent.WithTrustedInput("the operator's instruction")); err != nil {
			return fmt.Errorf("%s: %v", c.run, err)
		}
	}
	return nil
}
