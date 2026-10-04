package middleware_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/middleware"
)

// flakyModel fails its first `fail` calls (reporting usage), then answers.
type flakyModel struct {
	name  string
	fail  int32
	calls atomic.Int32
	spent atomic.Int64
}

func (m *flakyModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	n := m.calls.Add(1)
	ch := make(chan agent.Emit, 4)
	m.spent.Add(int64(billed.InputTokens + billed.OutputTokens))
	if n <= m.fail {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "bad"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: agent.FinishStop, Usage: billed}}
		ch <- agent.Emit{Err: errors.New("boom")}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: m.name}}
		ch <- agent.Emit{Event: agent.Finish{Reason: agent.FinishStop, Usage: billed}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

type hookCount struct {
	mu            sync.Mutex
	before, after map[int]int
}

func (h *hookCount) mw(next agent.ModelHandler) agent.ModelHandler {
	hk := agent.ModelCallHook{
		Before: func(_ context.Context, c agent.ModelCall) error {
			h.mu.Lock()
			h.before[c.Attempt()]++
			h.mu.Unlock()
			return nil
		},
		After: func(_ context.Context, c agent.ModelCall, _ agent.ModelAttempt) {
			h.mu.Lock()
			h.after[c.Attempt()]++
			h.mu.Unlock()
		},
	}
	return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
		return next(ctx, call.AddHook(hk))
	}
}

func appender(next agent.ModelHandler) agent.ModelHandler {
	return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
		call.Request.Messages = append(call.Request.Messages, agent.SystemText("x"))
		call.Request.Tools = append(call.Request.Tools, call.Request.Tools...)
		return next(ctx, call)
	}
}

func perms(n int) [][]int {
	if n == 1 {
		return [][]int{{0}}
	}
	var out [][]int
	for _, p := range perms(n - 1) {
		for i := 0; i <= len(p); i++ {
			q := slices.Insert(slices.Clone(p), i, n-1)
			out = append(out, q)
		}
	}
	return out
}

func TestStress_HooksOncePerRequestEveryOrder(t *testing.T) {
	for pi, p := range perms(5) {
		primary := &flakyModel{name: "p", fail: 1}
		backup := &flakyModel{name: "b", fail: 1}
		hc := &hookCount{before: map[int]int{}, after: map[int]int{}}
		var meter middleware.CostMeter
		all := []agent.Middleware{
			middleware.Retry(3, middleware.WithBackoff(0, 0)),
			middleware.Hedge(0, backup),
			middleware.RateLimit(middleware.NewRateLimiter(time.Microsecond, 1000)),
			middleware.Cost(&meter, middleware.Rates{InputPer1M: 1e6}),
			hc.mw,
		}
		var mws []agent.Middleware
		for _, i := range p {
			mws = append(mws, all[i], appender)
		}
		tool := agent.MustFunc("t", "d", func(context.Context, struct{}) (string, error) { return "", nil }, agent.WithSafety(agent.Safety{ReadOnly: true}))
		a := agenttest.MustNew(primary, agenttest.MemJournal(), agent.WithTools(tool), agent.WithMiddleware(mws...))
		as := a.Stream(context.Background(), fmt.Sprint("r", pi), agent.UserText("q"))
		restarts := 0
		for ev := range as.Events() {
			if _, ok := ev.(agent.TurnRestarted); ok {
				restarts++
			}
		}
		if _, err := as.Result(); err != nil {
			t.Fatalf("perm %v: %v", p, err)
		}
		sent := int(primary.calls.Load() + backup.calls.Load())
		deadline := time.Now().Add(2 * time.Second)
		for {
			hc.mu.Lock()
			na, nb := 0, 0
			for _, v := range hc.after {
				na += v
			}
			for _, v := range hc.before {
				nb += v
			}
			hc.mu.Unlock()
			if (na == nb && na >= sent) || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		hc.mu.Lock()
		// Numbers can have gaps: a request an outer hook's Before refused (a hedge loser's RateLimit
		// wait sees its cancelled context) keeps its number, and the hooks after it never see it.
		for k := range hc.before {
			if hc.before[k] != 1 || hc.after[k] != 1 {
				t.Errorf("perm %v: attempt %d before=%d after=%d (sent %d) %v %v", p, k, hc.before[k], hc.after[k], sent, hc.before, hc.after)
			}
		}
		if len(hc.before) < sent {
			t.Errorf("perm %v: %d numbered, %d sent", p, len(hc.before), sent)
		}
		hc.mu.Unlock()
		if got, want := meter.Snapshot().Spend.InputTokens+meter.Snapshot().Spend.OutputTokens, int(primary.spent.Load()+backup.spent.Load()); got != want {
			t.Errorf("perm %v: cost spend %d want %d", p, got, want)
		}
	}
}
