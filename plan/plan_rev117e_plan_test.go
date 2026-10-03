package plan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// A RegisterTool the agent's wrapper check refuses is not recorded on the Registry, unlike a
// duplicate registration, so Load (for a caller that did not check the return) reports an
// unknown block rather than the refusal.
func TestRev117e_RegisterToolRefusalSurfacesAtLoad(t *testing.T) {
	sub := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("x")), agenttest.MemJournal())
	reg := NewRegistry()
	_ = reg.RegisterTool[int, int]("delegate", timedSubWrap{agent.MustSubAgent("delegate", "", sub)})
	cfg := `{"version":1,"flow":"f","in":"int","out":"int","entry":"delegate",
	  "nodes":[{"name":"delegate","block":"delegate"}],"wiring":[]}`
	_, err := Load[int, int]([]byte(cfg), reg)
	if err == nil || !strings.Contains(err.Error(), "Timeout") {
		t.Fatalf("Load = %v; want the RegisterTool refusal (a Timeout over a sub-agent), as a duplicate registration surfaces", err)
	}
}

// The plan timeout rule matches the agent's: a result after the deadline is returned; an error
// after the flow's own context is done (cancelled, or its earlier deadline passed) is the flow's
// cancellation, not an unknown outcome; an error after the tool's own deadline is unknown.
func TestRev117e_TimeoutRuleParity(t *testing.T) {
	mk := func(f func(ctx context.Context) (int, error)) agent.Tool {
		return agent.MustFunc("t", "", func(ctx context.Context, _ int) (int, error) { return f(ctx) }, agent.WithTimeout(20*time.Millisecond))
	}
	args, _ := json.Marshal(1)
	// result after the tool's deadline
	raw, err := callTool(context.Background(), mk(func(ctx context.Context) (int, error) { <-ctx.Done(); return 7, nil }), 20*time.Millisecond, args)
	if err != nil || string(raw) != "7" {
		t.Errorf("late result: %s, %v", raw, err)
	}
	// error after parent cancellation, before the tool's deadline
	pctx, cancel := context.WithCancel(context.Background())
	_, err = callTool(pctx, mk(func(ctx context.Context) (int, error) { cancel(); return 0, errors.New("boom") }), 20*time.Millisecond, args)
	if errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Errorf("error after parent cancel: %v; want the plain error", err)
	}
	// parent deadline earlier than the tool's
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer dcancel()
	_, err = callTool(dctx, mk(func(ctx context.Context) (int, error) { <-ctx.Done(); return 0, ctx.Err() }), time.Second, args)
	if errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Errorf("error after the parent's deadline: %v; want the plain error", err)
	}
	// error after the tool's deadline, judged by the deadline before the timer fires
	_, err = callTool(context.Background(), mk(func(ctx context.Context) (int, error) {
		dl, _ := ctx.Deadline()
		time.Sleep(time.Until(dl))
		return 0, errors.New("late")
	}), 20*time.Millisecond, args)
	if !errors.Is(err, agent.ErrToolOutcomeUnknown) {
		t.Errorf("error at the tool's deadline: %v; want ErrToolOutcomeUnknown", err)
	}
}
