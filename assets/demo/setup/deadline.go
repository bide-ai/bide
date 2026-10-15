//go:build ignore

// deadline.go writes the journal behind the run viewer guide's deadline screenshots: refunds that
// wait at 2-of-3 approval gates (ops, finance, risk) with a deadline (ApprovalPolicy.Timeout), one
// due within the hour, one due tomorrow, one overdue (its deadline passed and no drive has recorded
// the expiry), and one that expired: a drive found the deadline passed with one approval of two, and
// recorded the expiry, while a second approval, stamped and recorded a second before the deadline,
// was being written (the race the gate resolves as the expiry; the page labels that decision "not
// counted: written after the gate expired"). The real agent runtime runs them on a scripted model with a controllable
// clock, so the times are relative to now. Run it from cmd/bide, which carries the modules it imports:
//
//	go run ../../assets/demo/setup/deadline.go -dir DIR
//
// DIR gets deadline.db.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/store/sqlite"
)

func main() {
	dir := flag.String("dir", "", "directory that gets deadline.db")
	flag.Parse()
	if err := run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "deadline.go:", err)
		os.Exit(1)
	}
}

// injectBefore runs inject once, just before the first Insert of name: a write that lands between
// the gate's read of the journal and its own write.
type injectBefore struct {
	agent.Store
	name   string
	inject func() error
	once   sync.Once
	err    error
}

func (s *injectBefore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if name == s.name {
		s.once.Do(func() { s.err = s.inject() })
		if s.err != nil {
			return agent.Entry{}, false, s.err
		}
	}
	return s.Store.Insert(ctx, runID, name, data)
}

func key(id string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte("bide deadline demo " + id))
	return ed25519.NewKeyFromSeed(seed[:])
}

func run(dir string) error {
	ctx := context.Background()
	s, err := sqlite.Open(filepath.Join(dir, "deadline.db"))
	if err != nil {
		return err
	}
	defer s.Close()
	j, err := agent.NewJournal(s)
	if err != nil {
		return err
	}
	verify := func(id string) (agent.ApproverVerifier, bool) {
		return audit.Ed25519Verifier{Pub: key(id).Public().(ed25519.PublicKey)}, true
	}
	now := time.Now().UTC().Truncate(time.Second)
	clock := now
	at := func(t time.Time) { clock = t }
	clk := func() time.Time { return clock }

	// ask runs one refund to its gate at the time asked, under a gate with the timeout.
	type refund struct {
		id, order string
		timeout   time.Duration
		asked     time.Duration // how long before now it asked
	}
	ask := func(r refund) error {
		policy := agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}, ExcludePrincipal: true, Timeout: r.timeout}
		tool := agent.MustFunc("refund", "refund an order to the customer's card",
			func(context.Context, json.RawMessage) (string, error) { return "refunded", nil }, agent.WithApproval(&policy))
		model := agenttest.NewScriptedModel(
			agenttest.ToolTurn("c1", "refund", fmt.Sprintf(`{"order":%q,"amount":120,"currency":"USD"}`, r.order)),
			agenttest.TextTurn("Refunded order "+r.order+"."))
		a, err := agent.New(model, j, agent.WithTools(tool), agent.WithApproverVerifiers(verify), agent.WithClock(clk))
		if err != nil {
			return err
		}
		at(now.Add(-r.asked))
		_, err = a.Run(ctx, r.id, agent.UserText("Refund order "+r.order+"."), agent.WithIdentity(agent.Identity{Actor: "support-agent", OnBehalfOf: "ops"}))
		if !errors.As(err, new(*agent.ApprovalPending)) {
			return fmt.Errorf("%s did not pause at its gate: %v", r.id, err)
		}
		return nil
	}
	approve := func(id, who string, when time.Time) error {
		recs, err := j.History(ctx, id)
		if err != nil {
			return err
		}
		_, call, _ := agent.FindToolCall(recs, "c1")
		decided := when
		subj := agent.ApprovalSubject{RunID: id, ToolUseID: "c1", ToolName: call.Name, Args: call.Args}
		sig := ed25519.Sign(key(who), agent.ApprovalDecisionBytes(subj, who, true, decided))
		at(when)
		return agent.SubmitDecision(ctx, j, agent.Decision{RunID: id, ToolUseID: "c1", ApproverID: who, Approved: true, DecidedAt: decided, Alg: audit.AlgEd25519, Signature: sig}, agent.WithClock(clk))
	}

	for _, r := range []refund{
		{"refund-2001", "2001", time.Hour, 20 * time.Minute},   // due in 40 minutes
		{"refund-2002", "2002", 24 * time.Hour, 2 * time.Hour}, // due in 22 hours
		{"refund-2003", "2003", time.Hour, 90 * time.Minute},   // overdue by 30 minutes, expiry not recorded
		{"refund-2004", "2004", time.Hour, 3 * time.Hour},      // expired
	} {
		if err := ask(r); err != nil {
			return err
		}
	}
	if err := approve("refund-2001", "finance", now.Add(-10*time.Minute)); err != nil {
		return err
	}
	if err := approve("refund-2004", "finance", now.Add(-170*time.Minute)); err != nil {
		return err
	}
	// A drive after the deadline records the expiry of refund-2004 (the run is over, denied). risk's
	// approval, stamped and recorded a second before the deadline, is written after the gate read
	// the journal and before it recorded the expiry, so the expiry is the outcome and it is not counted.
	racy, err := agent.NewJournal(&injectBefore{Store: s, name: agent.ApprovalTallyStep("c1"), inject: func() error {
		defer at(clock) // the gate's clock goes on from where it was
		return approve("refund-2004", "risk", now.Add(-2*time.Hour-time.Second))
	}})
	if err != nil {
		return err
	}
	tool := agent.MustFunc("refund", "refund an order to the customer's card",
		func(context.Context, json.RawMessage) (string, error) { return "refunded", nil },
		agent.WithApproval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}, ExcludePrincipal: true, Timeout: time.Hour}))
	model := agenttest.NewScriptedModel(agenttest.TextTurn("The refund was not approved in time."))
	a, err := agent.New(model, racy, agent.WithTools(tool), agent.WithApproverVerifiers(verify), agent.WithClock(clk))
	if err != nil {
		return err
	}
	at(now)
	if _, err := a.Run(ctx, "refund-2004", agent.UserText("Refund order 2004."), agent.WithIdentity(agent.Identity{Actor: "support-agent", OnBehalfOf: "ops"})); err != nil {
		return fmt.Errorf("the drive that records the expiry: %v", err)
	}
	return nil
}
