// Package runlistdemo writes a journal of runs in every state the bide view run list tells apart:
// completed, halted (outcome unknown), waiting for approval, in progress, aborted and cancelled,
// agent runs and plan flow runs, and one run journaled as a version that recorded no start time. The runview and cmd/bide tests read it back through each store,
// and it is the journal the run viewer guide's screenshots are taken of:
//
//	BIDE_RUNLIST_DEMO=$PWD/runs.db go test ./cmd/bide -run TestRunListDemo
//	bide view -db runs.db
//
// It drives real runs with scripted models (no API key), losing one write where a crash is part of
// the story. Only tests import it.
package runlistdemo

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"regexp"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/plan"
)

// Run is one run Write journals, and the state group the run list puts it in.
type Run struct {
	ID    string
	Kind  string // "agent" or "flow"
	Group string // the run list's state filter group
}

// Write journals the demo runs into s, each run ID prefixed with prefix, and returns them.
func Write(ctx context.Context, s agent.Store, prefix string) ([]Run, error) {
	cs := &crashStore{Store: s}
	j, err := agent.NewJournal(cs)
	if err != nil {
		return nil, err
	}
	// Each agent run starts a few minutes after the one before it, by its clock, so the list's
	// start times differ; a flow run takes the wall clock (plan has no clock option).
	base := time.Now().UTC().Add(-time.Duration(len(demos)) * 7 * time.Minute)
	var runs []Run
	for i, d := range demos {
		id := prefix + d.id
		at := base.Add(time.Duration(i) * 7 * time.Minute)
		if err := d.write(ctx, j, cs, id, agent.WithClock(func() time.Time { return at })); err != nil {
			return runs, fmt.Errorf("runlistdemo: %s: %w", id, err)
		}
		runs = append(runs, Run{ID: id, Kind: d.kind, Group: d.group})
	}
	return runs, nil
}

var demos = []struct {
	id, kind, group string
	write           func(ctx context.Context, j *agent.Journal, cs *crashStore, id string, clock agent.RunOption) error
}{
	{"refund-0999", "agent", "completed", func(ctx context.Context, j *agent.Journal, cs *crashStore, id string, clock agent.RunOption) error {
		// Journaled as a version that recorded no start time writes run:start.
		cs.noStartTime = true
		defer func() { cs.noStartTime = false }()
		_, err := refundAgent(j, nil).Run(ctx, id, agent.UserText("Refund order 999."), clock)
		return err
	}},
	{"refund-1041", "agent", "completed", func(ctx context.Context, j *agent.Journal, _ *crashStore, id string, clock agent.RunOption) error {
		_, err := refundAgent(j, nil).Run(ctx, id, agent.UserText("Refund order 1041."), clock)
		return err
	}},
	{"refund-1042", "agent", "halted", func(ctx context.Context, j *agent.Journal, cs *crashStore, id string, clock agent.RunOption) error {
		// The refund fires and the process dies before its result is journaled: the resume halts.
		cs.lose = "tool:c2"
		_, err := refundAgent(j, nil).Run(ctx, id, agent.UserText("Refund order 1042."), clock)
		cs.lose = ""
		if !errors.Is(err, errCrash) {
			return fmt.Errorf("crashing run = %v, want the simulated crash", err)
		}
		_, err = refundAgent(j, nil).Run(ctx, id, agent.UserText("Refund order 1042."), clock)
		return want[*agent.OutcomeUnknown](err)
	}},
	{"refund-1043", "agent", "waiting", func(ctx context.Context, j *agent.Journal, _ *crashStore, id string, clock agent.RunOption) error {
		_, err := refundAgent(j, agent.SingleApproval()).Run(ctx, id, agent.UserText("Refund order 1043."), clock)
		return want[*agent.ApprovalPending](err)
	}},
	{"refund-1044", "agent", "in-progress", func(ctx context.Context, j *agent.Journal, cs *crashStore, id string, clock agent.RunOption) error {
		// The process dies after the read-only lookup ran, before its result is journaled: no
		// side effect started, so a resume would simply run the lookup again.
		cs.lose = "tool:c1"
		_, err := refundAgent(j, nil).Run(ctx, id, agent.UserText("Refund order 1044."), clock)
		cs.lose = ""
		if !errors.Is(err, errCrash) {
			return fmt.Errorf("crashing run = %v, want the simulated crash", err)
		}
		return nil
	}},
	{"refund-1045", "agent", "failed", func(ctx context.Context, j *agent.Journal, _ *crashStore, id string, clock agent.RunOption) error {
		// An operator cancels the refund while it waits for approval.
		if _, err := refundAgent(j, agent.SingleApproval()).Run(ctx, id, agent.UserText("Refund order 1045."), clock); want[*agent.ApprovalPending](err) != nil {
			return err
		}
		return agent.Cancel(ctx, j, id, "customer withdrew the request")
	}},
	{"ship-2001", "agent", "failed", func(ctx context.Context, j *agent.Journal, _ *crashStore, id string, clock agent.RunOption) error {
		// A saga: the label is bought, the carrier booking fails, and the label is voided.
		label := agent.MustCompensatedFunc("buy_label", "buy a shipping label",
			func(context.Context, struct{}) (string, error) { return "label L-77", nil },
			func(context.Context, struct{}, string) error { return nil })
		book := agent.MustFunc("book_carrier", "book the carrier", func(context.Context, struct{}) (string, error) {
			return "", errors.New("carrier unavailable")
		})
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "buy_label", `{}`), agenttest.ToolTurn("c2", "book_carrier", `{}`), agenttest.TextTurn("done"))
		a, err := agent.New(m, j, agent.WithTools(label, book))
		if err != nil {
			return err
		}
		_, err = a.Run(ctx, id, agent.UserText("Ship order 2001."), agent.WithSaga(), clock)
		return want[*agent.SagaAborted](err)
	}},
	{"triage-3001", "flow", "completed", func(ctx context.Context, j *agent.Journal, _ *crashStore, id string, clock agent.RunOption) error {
		_, err := refundFlow().Run(ctx, j, id, order{ID: "o-3001", Amount: 40})
		return err
	}},
	{"triage-3002", "flow", "completed", func(ctx context.Context, j *agent.Journal, _ *crashStore, id string, clock agent.RunOption) error {
		_, err := refundFlow().Run(ctx, j, id, order{ID: "o-3002", Amount: 180})
		return err
	}},
	{"triage-3003", "flow", "halted", func(ctx context.Context, j *agent.Journal, cs *crashStore, id string, clock agent.RunOption) error {
		cs.lose = "node:charge"
		_, err := refundFlow().Run(ctx, j, id, order{ID: "o-3003", Amount: 250})
		cs.lose = ""
		if !errors.Is(err, errCrash) {
			return fmt.Errorf("crashing run = %v, want the simulated crash", err)
		}
		_, err = refundFlow().Run(ctx, j, id, order{ID: "o-3003", Amount: 250})
		return want[*agent.OutcomeUnknown](err)
	}},
}

// want is nil when err is a T, and an error saying so otherwise.
func want[T error](err error) error {
	if _, ok := errors.AsType[T](err); ok {
		return nil
	}
	var zero T
	return fmt.Errorf("run = %v, want %T", err, zero)
}

// refundAgent looks an order up (read-only) and refunds it (a side effect, gated when gate is set).
func refundAgent(j *agent.Journal, gate *agent.ApprovalPolicy) *agent.Agent {
	lookup := agent.MustFunc("lookup", "look up an order", func(context.Context, struct{}) (string, error) {
		return "order: $120, paid by card", nil
	}, agent.WithSafety(agent.Safety{ReadOnly: true}))
	opts := []agent.ToolOption{}
	if gate != nil {
		opts = append(opts, agent.WithApproval(gate))
	}
	refund := agent.MustFunc("refund", "refund an order", func(context.Context, struct{}) (string, error) {
		return "refunded $120", nil
	}, opts...)
	m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "lookup", `{}`), agenttest.ToolTurn("c2", "refund", `{}`), agenttest.TextTurn("Refunded."))
	return agenttest.MustNew(m, j, agent.WithTools(lookup, refund))
}

type order struct {
	ID     string `json:"id"`
	Amount int    `json:"amount"`
}

// refundFlow is validate -> Switch(amount > 100: charge -> receipt, else: skip).
func refundFlow() *plan.Flow[order, string] {
	b := plan.New[order, string]("refund-triage")
	validate := b.Step("validate", func(_ context.Context, o order) (order, error) { return o, nil }, plan.ReadOnly())
	charge := b.Step("charge", func(_ context.Context, o order) (order, error) { return o, nil })
	receipt := b.Step("receipt", func(_ context.Context, o order) (string, error) { return "refunded " + o.ID, nil }, plan.ReadOnly())
	skip := b.Step("skip", func(_ context.Context, o order) (string, error) { return "nothing to refund", nil }, plan.ReadOnly())
	b.Switch(validate, plan.When(func(o order) bool { return o.Amount > 100 }, charge).Named("large"), plan.Else(skip))
	b.Edge(charge, receipt)
	return agenttest.Must(b.Build())
}

var errCrash = errors.New("simulated crash")

// crashStore fails the insert of the record named lose, when set: the write a crash loses. With
// noStartTime it writes run:start without its start time, as an earlier version wrote it.
type crashStore struct {
	agent.Store
	lose        string
	noStartTime bool
}

var startTime = regexp.MustCompile(`,"started_at":"[^"]*"`)

func (s *crashStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if s.lose != "" && name == s.lose {
		return agent.Entry{}, false, errCrash
	}
	if s.noStartTime && name == "run:start" {
		data = startTime.ReplaceAll(data, nil)
	}
	return s.Store.Insert(ctx, runID, name, data)
}

func (s *crashStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return s.Store.Load(ctx, runID, after)
}

func (s *crashStore) Unwrap() agent.Store { return s.Store }
