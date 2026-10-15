//go:build ignore

// approve.go writes what the bide approve recording (assets/demo/approve.sh) reviews: a SQLite
// journal holding a refund that waits at a 2-of-3 approval gate (ops, finance, risk) with
// separation of duties, started on ops's behalf, so ops is the requester and cannot approve its
// own request. The real agent runtime runs it, on a scripted model, to the gate. Run it from
// cmd/bide, which carries the modules it imports:
//
//	go run ../../assets/demo/setup/approve.go -dir DIR
//
// DIR must hold approver-keys.json (made from bide keygen's public keys); it gets refund.db.
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
	dir := flag.String("dir", "", "directory holding approver-keys.json; refund.db is written there")
	flag.Parse()
	if err := run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "approve.go:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	var keys map[string]string
	b, err := os.ReadFile(filepath.Join(dir, "approver-keys.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	s, err := sqlite.Open(filepath.Join(dir, "refund.db"))
	if err != nil {
		return err
	}
	defer s.Close()
	j, err := agent.NewJournal(s)
	if err != nil {
		return err
	}
	policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}, ExcludePrincipal: true}
	refund := agent.MustFunc("refund", "refund an order to the customer's card",
		func(context.Context, json.RawMessage) (string, error) { return "refunded", nil },
		agent.WithApproval(&policy))
	model := agenttest.NewScriptedModel(
		agenttest.ToolTurn("c2", "refund", `{"order":"1049","amount":120,"currency":"USD","reason":"damaged on arrival"}`),
		agenttest.TextTurn("Refunded $120 for order 1049."))
	verify := func(id string) (agent.ApproverVerifier, bool) {
		k, ok := keys[id]
		if !ok {
			return nil, false
		}
		v, err := audit.ParsePublicKey(k)
		return v, err == nil
	}
	a, err := agent.New(model, j, agent.WithTools(refund), agent.WithApproverVerifiers(verify))
	if err != nil {
		return err
	}
	_, err = a.Run(context.Background(), "refund-1049", agent.UserText("Refund order 1049, $120."),
		agent.WithIdentity(agent.Identity{Actor: "support-agent", OnBehalfOf: "ops"}))
	if !errors.As(err, new(*agent.ApprovalPending)) {
		return fmt.Errorf("the run did not pause at the gate: %v", err)
	}
	return nil
}
