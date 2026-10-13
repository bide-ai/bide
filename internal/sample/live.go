package sample

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

// LiveRun is the ID of the run Live drives.
const LiveRun = "refund-live"

// Live drives one more refund run, LiveRun, in real time against s, so a page following it (bide
// view, live mode) can be watched: the run starts and its first model turn is held, it takes the
// lookup, pauses at the 2-of-3 gate, gets the two signed approvals one pace apart, refunds and
// completes. Each step takes about pace. Unlike Write it uses the wall clock and real randomness,
// so its records differ from run to run; it returns when the run completes or ctx is done.
func Live(ctx context.Context, s agent.Store, pace time.Duration) error {
	j, err := agent.NewJournal(s)
	if err != nil {
		return err
	}
	keys := map[string]ed25519.PrivateKey{}
	text := map[string]string{}
	for _, id := range policy.Approvers {
		k := keyFor(id)
		keys[id] = k
		text[id] = audit.FormatPublicKey(audit.AlgEd25519, k.Public().(ed25519.PublicKey))
	}
	e := &env{j: j, approvers: keys, verify: verifiers(text)}
	run := func() (*agent.ApprovalPending, error) {
		a := agenttest.MustNew(slowModel{refundModel(), pace}, j, agent.WithTools(refundTools(&policy)...), agent.WithApproverVerifiers(e.verify))
		_, err := a.Run(ctx, LiveRun, agent.UserText("Refund order 1050."))
		if p, ok := errors.AsType[*agent.ApprovalPending](err); ok {
			return p, nil
		}
		return nil, err
	}
	p, err := run()
	if err != nil || p == nil {
		return fmt.Errorf("sample: live run did not pause at its gate (%v)", err)
	}
	for _, who := range []string{"ops", "finance"} {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pace):
		}
		if err := e.decide(ctx, p, who); err != nil {
			return err
		}
	}
	_, err = run()
	return err
}

// slowModel holds each model turn for delay before it answers.
type slowModel struct {
	agent.Model
	delay time.Duration
}

func (m slowModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(m.delay):
	}
	return m.Model.Stream(ctx, req)
}
