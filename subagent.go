package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dayna/go-agents/schema"
)

// SubAgent wraps an agent as a Tool so a parent can delegate to it — the multi-agent
// primitive, as a durable tree rather than a fragile handoff.
//
// The sub-run journals under a hierarchical, deterministic ID (parentRunID/toolUseID) —
// so give `sub` the SAME Durable store as the parent for a unified journal. Then a crash
// ANYWHERE in the tree resumes the whole tree precisely: completed sub-agents are reused,
// the in-flight one resumes from its own journal, and ResumeHalt / PendingApproval from
// deep in the tree propagate up (approve, re-run the root, and it resumes down the path).
//
// This is what the incumbents can't do: ADK/agenticenv can't recover sub-agents across a
// restart, and Eino doesn't unify nested state into the parent checkpoint.
func SubAgent(name, description string, sub *Agent) Tool {
	s, _ := schema.For[subAgentArgs]()
	return &subAgentTool{name: name, description: description, sub: sub, argsSchema: s}
}

type subAgentArgs struct {
	Task string `json:"task" desc:"the task or question to delegate to this sub-agent"`
}

type subAgentTool struct {
	name, description string
	sub               *Agent
	argsSchema        json.RawMessage
}

func (t *subAgentTool) Name() string               { return t.name }
func (t *subAgentTool) Description() string         { return t.description }
func (t *subAgentTool) ArgsSchema() json.RawMessage { return t.argsSchema }

// Idempotent: re-running a sub-agent call on resume RESUMES the sub-run from its journal
// (it doesn't restart it), so it's safe to retry. Any unsafe write inside the sub-run
// halts via the sub-run's own ResumeHalt, which propagates up here.
func (t *subAgentTool) Safety() Safety { return Safety{Idempotent: true} }

func (t *subAgentTool) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var in subAgentArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
	}
	subRunID := RunScope(ctx) // parentRunID/toolUseID — stable + unique per call site
	if subRunID == "" {
		subRunID = "sub/" + t.name
	}
	// Run the sub-agent on its OWN goroutine (fresh, small stack) rather than recursing on
	// the parent's stack — so a deep agent tree is N shallow stacks, not one that balloons
	// to gigabytes. Idiomatic Go, and it makes sub-agents ctx-cancellable for free.
	// In a saga, the sub-agent runs transactionally too: a failure inside it rolls back its
	// own writes and returns *SagaAborted, which propagates up to abort the whole tree.
	type result struct {
		msg Message
		err error
	}
	ch := make(chan result, 1) // buffered so the child never blocks if we've already returned
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: fmt.Errorf("sub-agent %q panicked: %v (%w)", t.name, r, ErrTool)}
			}
		}()
		var m Message
		var e error
		if InSaga(ctx) {
			m, e = t.sub.RunSaga(ctx, subRunID, in.Task)
		} else {
			m, e = t.sub.Run(ctx, subRunID, in.Task)
		}
		ch <- result{msg: m, err: e}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-ch:
		if out.err != nil {
			return nil, out.err // SagaAborted / ResumeHalt / PendingApproval propagate up
		}
		return json.Marshal(firstText(out.msg))
	}
}
