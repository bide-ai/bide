package middleware_test

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/bide-ai/bide/agent"
)

// gateModel answers "name" with usage u. When gate is non-nil it first waits for gate to close,
// ignoring its context (a loser that finishes anyway: late, or at the same instant as the winner).
// honorCtx: if set, on ctx.Done it waits for the gate and then fails with partial usage.
type gateModel struct {
	name     string
	u        agent.Usage
	gate     chan struct{}
	honorCtx bool
	started  chan struct{}
	once     sync.Once
	calls    atomic.Int32
	describe *agent.ModelInfo
}

func (m *gateModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	m.calls.Add(1)
	ch := make(chan agent.Emit, 8)
	go func() {
		defer close(ch)
		if m.started != nil {
			m.once.Do(func() { close(m.started) })
		}
		ch <- agent.Emit{Event: agent.TextDelta{Text: m.name + "-partial"}}
		if m.gate != nil {
			if m.honorCtx {
				select {
				case <-m.gate:
				case <-ctx.Done():
					<-m.gate
					ch <- agent.Emit{Event: agent.Finish{Reason: agent.FinishStop, Usage: m.u}}
					ch <- agent.Emit{Err: ctx.Err()}
					return
				}
			} else {
				<-m.gate
			}
		}
		ch <- agent.Emit{Event: agent.TextDelta{Text: "!"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: agent.FinishStop, Raw: "end_turn", Usage: m.u}}
	}()
	return agent.NewStream(ch), nil
}

type describedModel struct {
	*gateModel
	info agent.ModelInfo
}

func (d describedModel) Describe() agent.ModelInfo { return d.info }
