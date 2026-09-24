package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dayna/go-agents/schema"
)

// finalAnswerTool is the synthetic tool RunTyped injects to collect the structured result.
const finalAnswerTool = "final_answer"

// RunTyped runs the agent to completion like Run, but returns a typed result T instead of
// a free-form Message. It injects a synthetic final_answer tool whose JSON schema is
// derived from T (via the schema package) and instructs the model to call it once, with
// the structured answer, when its work is done. Real tools still run first, so a
// tool-using agent can do work and then answer typed.
//
// The value is decoded from the JOURNALED tool-call args, not captured live, so it is
// resume-safe: on resume the answer is recovered from the log without re-running anything.
// If the model answers in plain text instead of calling the tool, RunTyped falls back to
// parsing that text as T.
//
// T is intended to be a struct (the usual structured-output shape). Because Go methods
// cannot add type parameters, this is a package function: agent.RunTyped[MyResult](ctx, a, id, in).
func RunTyped[T any](ctx context.Context, a *Agent, runID, input string) (T, error) {
	var zero T
	if _, err := schema.For[T](); err != nil {
		return zero, fmt.Errorf("typed: schema for %T: %w (%w)", zero, err, ErrConfig)
	}
	if _, taken := a.tools[finalAnswerTool]; taken {
		return zero, fmt.Errorf("typed: agent already registers a %q tool, which RunTyped needs: %w", finalAnswerTool, ErrConfig)
	}

	// The terminal tool: its arg type is T, so a malformed call is rejected with ErrToolArgs
	// and the model self-corrects. Its body is a no-op ack — the answer is read back from the
	// journaled call args (below), never from this closure, so resume works.
	respond := Func(finalAnswerTool,
		"Call this exactly once, with the final answer structured per the schema, to complete the task.",
		Safety{ReadOnly: true},
		func(_ context.Context, _ T) (struct{}, error) { return struct{}{}, nil })

	instruction := "Use the available tools to do any needed work, then call the " + finalAnswerTool +
		" tool exactly once with the final answer. Calling it completes the task; do not add further prose."

	if _, err := a.cloneWith(respond, injectSystem(instruction)).Run(ctx, runID, input); err != nil {
		return zero, err
	}

	// Prefer the journaled final_answer args (resume-safe); fall back to a plain-text JSON
	// answer if the model never called the tool.
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return zero, fmt.Errorf("typed: load history %s: %w (%w)", runID, err, ErrStorage)
	}
	var raw json.RawMessage
	var lastText string
	for _, r := range recs {
		if r.Kind != StepModel || r.Message == nil {
			continue
		}
		for _, tu := range r.Message.toolUses() {
			if tu.Name == finalAnswerTool {
				raw = tu.Args
			}
		}
		if t := firstText(*r.Message); t != "" {
			lastText = t
		}
	}
	if len(raw) == 0 {
		raw = json.RawMessage(lastText)
	}

	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("typed: decode answer into %T: %w (%w)", zero, err, ErrProtocol)
	}
	return out, nil
}

// cloneWith returns a shallow copy of the agent with one extra tool and appended model
// middleware, leaving the caller's agent untouched. Model/store are shared; the tool map
// and middleware slices are copied.
func (a *Agent) cloneWith(extra Tool, mw ...Middleware) *Agent {
	tools := make(map[string]Tool, len(a.tools)+1)
	for k, v := range a.tools {
		tools[k] = v
	}
	tools[extra.Name()] = extra
	return &Agent{
		model:   a.model,
		tools:   tools,
		store:   a.store,
		mw:      append(append([]Middleware(nil), a.mw...), mw...),
		toolMW:  append([]ToolMiddleware(nil), a.toolMW...),
		maxConc: a.maxConc,
	}
}

// injectSystem is model middleware that prepends a system message to each model call —
// Run seeds only a user turn, so this is how RunTyped steers without a system-prompt API.
func injectSystem(s string) Middleware {
	sys := SystemText(s)
	return func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, req Request) (Message, Usage, error) {
			req.Messages = append([]Message{sys}, req.Messages...)
			return next(ctx, req)
		}
	}
}
