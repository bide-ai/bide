package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

func rev117eResult(t *testing.T, store *agent.Journal, runID, id string) (agent.Record, bool) {
	t.Helper()
	recs, err := store.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Name == agent.ToolResultStep(id) {
			return r, true
		}
	}
	return agent.Record{}, false
}

// README.md "Write your own" snippet, verbatim: a denial that does not wrap ErrToolNotCalled.
// The README says "the tool never executes; the model sees the error and reacts". For a side
// effect the run instead fails with ErrToolOutcomeUnknown, records nothing, and a resume halts.
func TestRev117eDocs_ReadmeRequireTagHaltsSideEffect(t *testing.T) {
	authorized := func(context.Context, string) bool { return false }
	RequireTag := func(tag string) agent.ToolMiddleware {
		return func(next agent.ToolHandler) agent.ToolHandler {
			return func(ctx context.Context, call agent.ToolCall) (json.RawMessage, error) {
				if !authorized(ctx, tag) {
					return nil, fmt.Errorf("tool %q denied: %w", call.Use.Name, agent.ErrTool)
				}
				return next(ctx, call)
			}
		}
	}
	tool := agent.Func("refund", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { return "ran", nil })
	store := agenttest.MemJournal()
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "refund", `{}`), agent.TextTurn("done"))
	_, err := agenttest.MustNew(m, store, agent.WithTools(tool), agent.WithToolMiddleware(RequireTag("x"))).Run(context.Background(), "r1", "go")
	_, recorded := rev117eResult(t, store, "r1", "c1")
	if !errors.Is(err, agent.ErrToolOutcomeUnknown) || recorded {
		t.Fatalf("Run = %v, recorded %v; README claims the model sees the error", err, recorded)
	}
	_, err = agenttest.MustNew(m, store, agent.WithTools(tool)).Run(context.Background(), "r1", "go")
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("resume = %v, want *OutcomeUnknown", err)
	}
}

// GUARANTEE.md / CHANGELOG: a side effect that returns an error after its WithTimeout deadline
// records nothing and fails with ErrToolOutcomeUnknown; a retry-safe one records the error; a
// result after the deadline is recorded.
func TestRev117eDocs_TimeoutRule(t *testing.T) {
	late := func(ok bool) func(ctx context.Context, _ struct{}) (string, error) {
		return func(ctx context.Context, _ struct{}) (string, error) {
			<-ctx.Done()
			if ok {
				return "late but done", nil
			}
			return "", errors.New("late failure")
		}
	}
	for _, tc := range []struct {
		name         string
		safety       agent.Safety
		ok           bool
		wantUnknown  bool
		wantRecorded bool
		wantIsError  bool
	}{
		{"side effect, late error", agent.Safety{}, false, true, false, false},
		{"retry-safe, late error", agent.Safety{Idempotent: true}, false, false, true, true},
		{"side effect, late result", agent.Safety{}, true, false, true, false},
	} {
		// In a synctest bubble the deadline passes only once the tool blocks on its context, so the
		// call always reaches the tool; on the wall clock it can pass during dispatch, and the call
		// is then refused as not started.
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				tool := agent.Func("t", "", tc.safety, func(ctx context.Context, a struct{}) (string, error) { calls.Add(1); return late(tc.ok)(ctx, a) }, agent.WithTimeout(10*time.Millisecond))
				store := agenttest.MemJournal()
				m := agent.NewScriptedModel(agent.ToolTurn("c1", "t", `{}`), agent.TextTurn("done"))
				_, err := agenttest.MustNew(m, store, agent.WithTools(tool)).Run(context.Background(), "r1", "go")
				rec, recorded := rev117eResult(t, store, "r1", "c1")
				if calls.Load() != 1 || errors.Is(err, agent.ErrToolOutcomeUnknown) != tc.wantUnknown || recorded != tc.wantRecorded || recorded && rec.IsError != tc.wantIsError {
					t.Fatalf("calls %d, Run = %v, recorded %v, is_error %v", calls.Load(), err, recorded, rec.IsError)
				}
				if recorded && rec.IsError && !strings.Contains(string(rec.Result), "after its 10ms timeout") {
					t.Fatalf("recorded %s; want the late error", rec.Result)
				}
			})
		})
	}
}

// tool_middleware.go: a middleware that turns a side effect's success into an error makes the
// outcome unknown; for a retry-safe tool it is recorded as a failure.
func TestRev117eDocs_SuccessTurnedError(t *testing.T) {
	mw := func(next agent.ToolHandler) agent.ToolHandler {
		return func(ctx context.Context, call agent.ToolCall) (json.RawMessage, error) {
			if _, err := next(ctx, call); err != nil {
				return nil, err
			}
			return nil, errors.New("rejected by middleware")
		}
	}
	for _, tc := range []struct {
		safety      agent.Safety
		wantUnknown bool
	}{{agent.Safety{}, true}, {agent.Safety{Idempotent: true}, false}} {
		tool := agent.Func("t", "", tc.safety, func(context.Context, struct{}) (string, error) { return "ok", nil })
		store := agenttest.MemJournal()
		m := agent.NewScriptedModel(agent.ToolTurn("c1", "t", `{}`), agent.TextTurn("done"))
		_, err := agenttest.MustNew(m, store, agent.WithTools(tool), agent.WithToolMiddleware(mw)).Run(context.Background(), "r1", "go")
		_, recorded := rev117eResult(t, store, "r1", "c1")
		if errors.Is(err, agent.ErrToolOutcomeUnknown) != tc.wantUnknown || recorded == tc.wantUnknown {
			t.Fatalf("%+v: Run = %v, recorded %v", tc.safety, err, recorded)
		}
	}
}

// subagent.go / CHANGELOG: SubAgent refuses WithSafety and WithTimeout with an ErrConfig panic,
// and takes WithApproval.
func TestRev117eDocs_SubAgentOptions(t *testing.T) {
	sub := agenttest.MustNew(agent.NewScriptedModel(agent.TextTurn("x")), agenttest.MemJournal())
	for name, opt := range map[string]agent.ToolOption{
		"safety":  agent.WithSafety(agent.Safety{ReadOnly: true}),
		"timeout": agent.WithTimeout(time.Second),
	} {
		func() {
			defer func() {
				err, _ := recover().(error)
				if !errors.Is(err, agent.ErrConfig) {
					t.Fatalf("%s: recover = %v, want ErrConfig", name, err)
				}
			}()
			agent.SubAgent("s", "", sub, opt)
		}()
	}
	if s := agent.SpecOf(agent.SubAgent("s", "", sub, agent.WithApproval(agent.SingleApproval()))); s.Approval == nil {
		t.Fatal("SubAgent dropped WithApproval")
	}
}

// CHANGELOG: SingleApproval encodes as {"single":true}; a {Need:1} literal is ErrConfig; the
// decoder refuses "single" beside other members.
func TestRev117eDocs_ApprovalWire(t *testing.T) {
	b, err := json.Marshal(agent.SingleApproval())
	if err != nil || string(b) != `{"single":true}` {
		t.Fatalf("Marshal = %s, %v", b, err)
	}
	func() {
		defer func() {
			err, _ := recover().(error)
			if !errors.Is(err, agent.ErrConfig) {
				t.Fatalf("recover = %v, want ErrConfig", err)
			}
		}()
		agent.Func("t", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { return "", nil }, agent.WithApproval(&agent.ApprovalPolicy{Need: 1}))
	}()
	var p agent.ApprovalPolicy
	if err := json.Unmarshal([]byte(`{"single":true,"need":1}`), &p); !errors.Is(err, agent.ErrProtocol) {
		t.Fatalf("Unmarshal single+need = %v, want ErrProtocol", err)
	}
	b, _ = json.Marshal(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}})
	if !strings.Contains(string(b), `"need":2`) {
		t.Fatalf("m-of-n = %s", b)
	}
}

// README.md "Write your own" snippet as corrected: a denial that wraps ErrToolNotCalled is
// recorded as a failure the model sees, the tool never runs, and the run completes.
func TestRev117eDocs_ReadmeRequireTagWithNotCalled(t *testing.T) {
	authorized := func(context.Context, string) bool { return false }
	RequireTag := func(tag string) agent.ToolMiddleware {
		return func(next agent.ToolHandler) agent.ToolHandler {
			return func(ctx context.Context, call agent.ToolCall) (json.RawMessage, error) {
				if !authorized(ctx, tag) {
					return nil, fmt.Errorf("tool %q denied: %w", call.Use.Name, agent.ErrToolNotCalled)
				}
				return next(ctx, call)
			}
		}
	}
	ran := false
	tool := agent.Func("refund", "", agent.Safety{}, func(context.Context, struct{}) (string, error) { ran = true; return "ran", nil })
	store := agenttest.MemJournal()
	m := agent.NewScriptedModel(agent.ToolTurn("c1", "refund", `{}`), agent.TextTurn("done"))
	_, err := agenttest.MustNew(m, store, agent.WithTools(tool), agent.WithToolMiddleware(RequireTag("x"))).Run(context.Background(), "r1", "go")
	rec, recorded := rev117eResult(t, store, "r1", "c1")
	if err != nil || ran || !recorded || !rec.IsError {
		t.Fatalf("Run = %v, ran %v, recorded %v (is_error %v)", err, ran, recorded, rec.IsError)
	}
}
