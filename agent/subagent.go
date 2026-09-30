package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bide-ai/bide/schema"
)

// SubAgent wraps an agent as a Tool so a parent can delegate to it — the multi-agent
// primitive, as a durable tree rather than a fragile handoff.
//
// The sub-run journals under a hierarchical, deterministic ID (SubRunID(parentRunID, toolUseID)),
// so give `sub` the SAME Durable store as the parent for a unified journal. Then a crash
// ANYWHERE in the tree resumes the whole tree precisely: completed sub-agents are reused,
// the in-flight one resumes from its own journal, and OutcomeUnknown / ApprovalPending from
// deep in the tree propagate up (approve, re-run the root, and it resumes down the path).
//
// opts set the rest of the tool's spec: WithApproval, so the parent waits for a human before it
// delegates, WithTitle and WithOutputSchema. SubAgent refuses WithSafety, since a sub-agent call
// re-enters its sub-run, whose own calls carry their safety, and WithTimeout, since a deadline
// would cut the sub-run off mid-call and record the delegation as failed while the sub-run's own
// outcome is unknown: bound the sub-agent's tools instead. It panics, with an error wrapping
// ErrConfig, on a refused or invalid option, as Func does.
//
// This is what the incumbents can't do: ADK/agenticenv can't recover sub-agents across a
// restart, and Eino doesn't unify nested state into the parent checkpoint.
func SubAgent(name, description string, sub *Agent, opts ...ToolOption) Tool {
	s, _ := schema.For[subAgentArgs]()
	// Idempotent: re-running a sub-agent call on resume RESUMES the sub-run from its journal
	// (it doesn't restart it), and a sub-run that already finished returns its recorded answer,
	// so it's safe to retry. Any unsafe write inside the sub-run halts via the sub-run's own
	// OutcomeUnknown, which propagates up here.
	c := toolConfig{spec: ToolSpec{Name: name, Description: description, Input: s, Safety: Safety{Idempotent: true}}}
	if err := applyToolOptions(&c, opts); err != nil {
		panic(err)
	}
	switch {
	case c.safetySet:
		panic(fmt.Errorf("agent: SubAgent %q: WithSafety does not apply to a sub-agent, whose sub-run's calls carry their own safety: %w", name, ErrConfig))
	case c.timeoutSet:
		panic(fmt.Errorf("agent: SubAgent %q: WithTimeout does not apply to a sub-agent, whose sub-run it would cut off mid-call; give its tools timeouts: %w", name, ErrConfig))
	}
	return &subAgentTool{spec: c.spec, sub: sub}
}

type subAgentArgs struct {
	Task string `json:"task" desc:"the task or question to delegate to this sub-agent"`
}

type subAgentTool struct {
	spec ToolSpec
	sub  *Agent
}

func (t *subAgentTool) Name() string                { return t.spec.Name }
func (t *subAgentTool) Description() string         { return t.spec.Description }
func (t *subAgentTool) ArgsSchema() json.RawMessage { return t.spec.Input }
func (t *subAgentTool) Safety() Safety              { return t.spec.Safety }

// Spec returns the tool's spec, with a copy of its approval policy.
func (t *subAgentTool) Spec() ToolSpec {
	s := t.spec
	s.Approval = s.Approval.clone()
	return s
}

func (t *subAgentTool) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var in subAgentArgs
	if err := decodeArgs(args, &in); err != nil {
		return nil, fmt.Errorf("decode args for sub-agent %q: %w (%w)", t.spec.Name, err, ErrToolArgs)
	}
	subRunID := RunScope(ctx) // SubRunID(parentRunID, toolUseID): stable and unique per call site
	if subRunID == "" {
		// Fallback for a SubAgent tool invoked outside the agent loop (which always sets the run
		// scope, agent.go withRunScope). This id is NOT unique per call: two calls to a same-named
		// sub-agent would share one journal and the second would memoize to the first's result. Drive
		// sub-agents through Agent.Run/RunSaga (the normal path) so each call gets a distinct scope.
		subRunID = "sub/" + t.spec.Name
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
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: fmt.Errorf("sub-agent %q panicked: %v (%w)", t.spec.Name, r, ErrTool)}
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

	// Wait for the child even when ctx is cancelled, as the loop waits for any tool call: the
	// child runs under the same ctx and stops as promptly as its own tools do, and returning
	// before it had would leave the sub-run's work in flight after the parent's Run returned.
	out := <-ch
	if out.err != nil {
		var sa *SagaAborted
		if !errors.As(out.err, &sa) && (errors.Is(out.err, ErrStorage) || errors.Is(out.err, ErrToolOutcomeUnknown)) {
			// The sub-run stopped short of a verdict: its journal could not be read or written,
			// or one of its calls lost its answer (it records nothing, and its resume halts for
			// the outcome). This call has no outcome to record either. The parent records nothing
			// and stops; resuming it re-enters the sub-run, which carries on from its journal.
			return nil, &subRunUnfinished{err: out.err}
		}
		return nil, out.err // SagaAborted / OutcomeUnknown / ApprovalPending / cancellation propagate up
	}
	return marshalJournal(firstText(out.msg)) // not HTML-escaped: the parent model reads it as written
}

// asSubAgent returns the SubAgent tool t is, or wraps. A tool that wraps another (as
// audit.AttenuatingSubAgent wraps a SubAgent) says so with an Unwrap() Tool method, which is
// followed as errors.As follows Unwrap, so a saga rollback and the run's budget recurse into the
// sub-run of a wrapped sub-agent as they do into a plain one's.
func asSubAgent(t Tool) (*subAgentTool, bool) {
	for range 64 { // a bound, so a wrapper that unwraps to itself cannot loop forever
		if s, ok := t.(*subAgentTool); ok {
			return s, true
		}
		u, ok := t.(interface{ Unwrap() Tool })
		if !ok {
			return nil, false
		}
		if t = u.Unwrap(); t == nil {
			return nil, false
		}
	}
	return nil, false
}

// subRunUnfinished is a sub-agent call whose sub-run stopped short of a verdict: its journal could
// not be read or written, or one of its calls lost its answer (ErrToolOutcomeUnknown). The loop
// records no result for it, as for a cancelled call, so neither is journaled as the sub-agent's
// answer nor, in a saga, taken for the step failure that aborts the transaction.
type subRunUnfinished struct{ err error }

func (e *subRunUnfinished) Error() string { return e.err.Error() }
func (e *subRunUnfinished) Unwrap() error { return e.err }
