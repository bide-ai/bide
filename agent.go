// Package agent is a durability-first, Go-idiomatic toolkit for building AI agents.
//
// The agent loop journals every step to a Durable store via named-step memoization
// (Durable.Do) and resumes after a crash — reusing recorded steps, re-running
// retry-safe tools, halting (rather than double-executing) a non-idempotent tool whose
// outcome is unknown, and pausing durably for human approval when a tool requires it.
// Orchestration is plain Go (Option B); the model call is wrapped by a func(Handler)
// Handler middleware chain.
//
// Scope: this resumes AROUND tool boundaries, not the internals of a single in-flight
// tool call. The honest promise is "survives crashes around tool calls, never
// double-fires an unsafe side effect" — not "mid-tool-call resume".
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// ModelHandler generates one assistant turn. Middleware wraps it.
type ModelHandler func(context.Context, Request) (Message, Usage, error)

// Middleware wraps a ModelHandler — the net/http-style func(Handler) Handler chain, at
// the SEMANTIC layer (it sees messages, tool calls, token usage — not bytes). Batteries
// live in the middleware/ package (Retry, TokenBudget, ...).
type Middleware func(ModelHandler) ModelHandler

// Agent binds a model, a tool set, a durable store, and a middleware chain.
type Agent struct {
	model   Model
	tools   map[string]Tool
	store   Durable
	mw      []Middleware
	maxConc int // max concurrent tool calls per turn; 0 = unbounded (default)
}

// New constructs an Agent.
func New(model Model, store Durable, tools ...Tool) *Agent {
	m := make(map[string]Tool, len(tools))
	for _, t := range tools {
		m[t.Name()] = t
	}
	return &Agent{model: model, tools: m, store: store}
}

// Use appends middleware wrapping the model call (first added = outermost). Returns the
// agent for chaining.
func (a *Agent) Use(mw ...Middleware) *Agent {
	a.mw = append(a.mw, mw...)
	return a
}

// SetMaxConcurrency bounds how many tool calls run in parallel within a single turn.
// n <= 0 means unbounded (the default). Returns the agent for chaining; use
// SetMaxConcurrency(1) to force fully sequential tool execution.
func (a *Agent) SetMaxConcurrency(n int) *Agent {
	a.maxConc = n
	return a
}

func (a *Agent) generate(ctx context.Context, req Request) (Message, Usage, error) {
	h := ModelHandler(func(ctx context.Context, req Request) (Message, Usage, error) {
		return Generate(ctx, a.model, req)
	})
	for i := len(a.mw) - 1; i >= 0; i-- {
		h = a.mw[i](h)
	}
	return h(ctx, req)
}

// Run drives the agent to completion for runID, resuming from the journal if steps
// already exist. Completed steps are reused; retry-safe tools with no recorded result
// are re-run; a non-retry-safe tool with no result triggers ResumeHalt; a tool that
// requires approval with no recorded decision triggers PendingApproval.
func (a *Agent) Run(ctx context.Context, runID, input string) (Message, error) {
	return a.run(ctx, runID, input, false)
}

func (a *Agent) run(ctx context.Context, runID, input string, saga bool) (Message, error) {
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return Message{}, fmt.Errorf("load history %s: %w", runID, err)
	}

	msgs := []Message{UserText(input)}
	done := map[string]bool{}      // tool-use IDs with a recorded result
	attempted := map[string]bool{} // tool-use IDs we recorded an attempt marker for (started a side effect)
	decided := map[string]bool{}   // tool-use IDs with a recorded approval decision
	approvals := map[string]bool{} // tool-use ID -> approve(true)/deny(false)
	modelSeq := 0
	for _, r := range recs {
		switch r.Kind {
		case StepModel:
			modelSeq++
			if r.Message != nil {
				msgs = append(msgs, *r.Message)
			}
		case StepToolResult:
			done[r.ToolUseID] = true
			msgs = append(msgs, Message{Role: RoleTool, Parts: []Part{
				ToolResult{ToolUseID: r.ToolUseID, Result: r.Result, IsError: r.IsError},
			}})
		case StepAttempt:
			attempted[r.ToolUseID] = true
		case StepSagaFail:
			done[r.ToolUseID] = true // the failing step is durably resolved (no ResumeHalt)
		case StepApproval:
			decided[r.ToolUseID] = true
			approvals[r.ToolUseID] = r.Approved
		}
	}

	// Resume safety gate: a non-retriable tool that we ATTEMPTED (recorded a start marker
	// for) but has no recorded result crashed mid-side-effect → unknown outcome → halt.
	// A tool that was never attempted never ran its side effect, so it's safe to run now
	// (not a halt); one awaiting approval re-surfaces as PendingApproval in the loop.
	for id := range attempted {
		if done[id] {
			continue
		}
		name, ok := toolNameFor(recs, id)
		if !ok {
			continue
		}
		if t, ok := a.tools[name]; ok && !t.Safety().retriableOnResume() {
			return Message{}, &ResumeHalt{RunID: runID, ToolUseID: id, ToolName: name}
		}
	}

	for {
		// If the last turn is an assistant message with tool calls still pending (a
		// resumed journal), execute those; otherwise ask the model for the next turn.
		var asst Message
		if n := len(msgs); n > 0 && msgs[n-1].Role == RoleAssistant && pending(msgs[n-1], done) {
			asst = msgs[n-1]
		} else {
			rec, err := a.store.Do(ctx, runID, fmt.Sprintf("@llm/%d", modelSeq),
				func(ctx context.Context) (Record, error) {
					m, _, e := a.generate(ctx, Request{Messages: msgs, Tools: a.toolList()})
					if e != nil {
						return Record{}, e
					}
					return Record{Kind: StepModel, Message: &m}, nil
				})
			if err != nil {
				return Message{}, fmt.Errorf("generate: %w", err)
			}
			asst = *rec.Message
			modelSeq++
			msgs = append(msgs, asst)
		}

		uses := asst.toolUses()
		if len(uses) == 0 {
			return asst, nil // final answer
		}

		// Pre-pass (sequential): resolve human-in-the-loop approvals and collect the tools
		// to execute. results[i] holds the tool-result message for uses[i], so the
		// conversation is assembled in deterministic uses-order regardless of which tool
		// finishes first.
		type call struct {
			idx int
			tu  ToolUse
			t   Tool
		}
		var toRun []call
		results := make([]*Message, len(uses))
		for i, tu := range uses {
			if done[tu.ID] {
				continue // already recorded (resumed turn) — its result is already in msgs
			}
			t, ok := a.tools[tu.Name]
			if !ok {
				return Message{}, fmt.Errorf("model called unknown tool %q", tu.Name)
			}
			if t.Safety().RequiresApproval {
				if !decided[tu.ID] {
					return Message{}, &PendingApproval{RunID: runID, ToolUseID: tu.ID, ToolName: tu.Name, Args: tu.Args}
				}
				if !approvals[tu.ID] { // denied — record a denial and let the model react
					const denied = `"tool call denied by human"`
					if _, err := a.store.Do(ctx, runID, tu.ID, func(context.Context) (Record, error) {
						return Record{Kind: StepToolResult, ToolUseID: tu.ID, IsError: true, Result: json.RawMessage(denied)}, nil
					}); err != nil {
						return Message{}, err
					}
					done[tu.ID] = true
					results[i] = &Message{Role: RoleTool, Parts: []Part{ToolResult{ToolUseID: tu.ID, Result: json.RawMessage(denied), IsError: true}}}
					continue
				}
			}
			toRun = append(toRun, call{idx: i, tu: tu, t: t})
		}

		// Execute the ready tools CONCURRENTLY (Go's strength; single-flight-safe). First
		// failure in saga mode cancels siblings via the errgroup context.
		g, gctx := errgroup.WithContext(ctx)
		if a.maxConc > 0 {
			g.SetLimit(a.maxConc)
		}
		for _, c := range toRun {
			c := c
			g.Go(func() error {
				sctx := withRunScope(gctx, runID+"/"+c.tu.ID) // hierarchical sub-run ID
				if saga {
					sctx = withSaga(sctx)
				}
				// Attempt marker before a non-retriable side effect (crash-mid-write → halt).
				if !c.t.Safety().retriableOnResume() {
					if _, err := a.store.Do(gctx, runID, "attempt:"+c.tu.ID, func(context.Context) (Record, error) {
						return Record{Kind: StepAttempt, ToolUseID: c.tu.ID}, nil
					}); err != nil {
						return err
					}
				}
				var toolCallErr error
				rec, err := a.store.Do(gctx, runID, c.tu.ID, func(context.Context) (Record, error) {
					res, callErr := c.t.Call(sctx, c.tu.Args)
					r := Record{Kind: StepToolResult, ToolUseID: c.tu.ID}
					if callErr != nil {
						if saga {
							toolCallErr = callErr
							return Record{Kind: StepSagaFail, ToolUseID: c.tu.ID, Result: mustJSON(callErr.Error())}, nil
						}
						r.IsError = true
						r.Result, _ = json.Marshal(callErr.Error())
					} else {
						r.Result = res
					}
					return r, nil
				})
				if saga && toolCallErr != nil {
					return &sagaTrip{toolName: c.tu.Name, toolUseID: c.tu.ID, cause: toolCallErr}
				}
				if err != nil {
					return fmt.Errorf("tool %q: %w", c.tu.Name, err)
				}
				results[c.idx] = &Message{Role: RoleTool, Parts: []Part{ToolResult{ToolUseID: c.tu.ID, Result: rec.Result, IsError: rec.IsError}}}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			var trip *sagaTrip
			if errors.As(err, &trip) {
				return Message{}, trip // RunSaga catches → compensates
			}
			return Message{}, err
		}

		// Append results in deterministic uses-order.
		for i, tu := range uses {
			if results[i] != nil {
				done[tu.ID] = true
				msgs = append(msgs, *results[i])
			}
		}
	}
}

func (a *Agent) toolList() []Tool {
	out := make([]Tool, 0, len(a.tools))
	for _, t := range a.tools {
		out = append(out, t)
	}
	return out
}

type ctxKey int

const (
	runScopeKey ctxKey = 0
	sagaKey     ctxKey = 1
)

func withRunScope(ctx context.Context, scope string) context.Context {
	return context.WithValue(ctx, runScopeKey, scope)
}

// RunScope returns the hierarchical run scope for the current tool execution
// (parentRunID/toolUseID). Sub-agents use it to derive a stable, resumable sub-run ID.
func RunScope(ctx context.Context) string {
	s, _ := ctx.Value(runScopeKey).(string)
	return s
}

func withSaga(ctx context.Context) context.Context { return context.WithValue(ctx, sagaKey, true) }

// InSaga reports whether the current tool execution is inside a saga run. Sub-agents use
// it to run transactionally (RunSaga) so a failure deep in the tree aborts the whole tree.
func InSaga(ctx context.Context) bool {
	v, _ := ctx.Value(sagaKey).(bool)
	return v
}

// firstText returns the first Text part of a message (the assistant's answer).
func firstText(m Message) string {
	for _, p := range m.Parts {
		if t, ok := p.(Text); ok {
			return t.Text
		}
	}
	return ""
}

// toolNameFor finds the tool name for a tool-use ID across the journaled model turns.
func toolNameFor(recs []Record, id string) (string, bool) {
	for _, r := range recs {
		if r.Kind != StepModel || r.Message == nil {
			continue
		}
		for _, tu := range r.Message.toolUses() {
			if tu.ID == id {
				return tu.Name, true
			}
		}
	}
	return "", false
}

// pending reports whether an assistant message has tool calls without recorded results.
func pending(m Message, done map[string]bool) bool {
	for _, tu := range m.toolUses() {
		if !done[tu.ID] {
			return true
		}
	}
	return false
}
