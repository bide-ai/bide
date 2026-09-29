package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bide-ai/bide/schema"
)

// finalAnswerTool is the synthetic tool RunTyped injects to collect the structured result.
const finalAnswerTool = "final_answer"

// RunTyped runs the agent to completion like Run, but returns a typed result T instead of
// a free-form Message. It injects a synthetic final_answer tool whose JSON schema is
// derived from T (via the schema package) and instructs the model to call it once, with
// the structured answer, when its work is done. Real tools still run first, so a
// tool-using agent can do work and then answer typed. The first final_answer call the tool
// accepts ends the run: the model is not asked for another turn.
//
// The value is decoded from the JOURNALED tool-call args, not captured live, so it is
// resume-safe: on resume the answer is recovered from the log without re-running anything.
// Only if the model never makes an accepted final_answer call (it answers in plain text
// instead) does RunTyped parse its last text as T.
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

	// A successful final_answer call ends the run: the answer exists, so the model is not asked
	// for another turn (which could only repeat the answer, or run into the turn cap).
	typed := a.cloneWith(respond, injectSystem(instruction))
	typed.terminalTool = finalAnswerTool
	if _, err := typed.Run(ctx, runID, input); err != nil {
		return zero, err
	}

	// The answer is the journaled args of the first final_answer call the tool accepted
	// (resume-safe). A call the tool rejected (arguments that do not decode as T) is not an
	// answer. Only if no call was accepted does the model's last text stand in for one.
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return zero, fmt.Errorf("typed: load history %s: %w (%w)", runID, err, ErrStorage)
	}
	accepted := map[string]bool{}
	for _, r := range recs {
		if r.Kind == StepToolResult && !r.IsError {
			accepted[r.ToolUseID] = true
		}
	}
	var raw json.RawMessage
	answered := false
	var lastText string
	for _, r := range recs {
		if r.Kind != StepModel || r.Message == nil {
			continue
		}
		for _, tu := range r.Message.toolUses() {
			if !answered && tu.Name == finalAnswerTool && accepted[tu.ID] {
				raw, answered = tu.Args, true
			}
		}
		if t := firstText(*r.Message); t != "" {
			lastText = t
		}
	}
	var out T
	if answered {
		// Decode exactly as the tool did: empty arguments are the zero value.
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &out); err != nil {
				return zero, fmt.Errorf("typed: decode final_answer into %T: %w (%w)", zero, err, ErrProtocol)
			}
		}
		return out, nil
	}
	raw = json.RawMessage(lastText)
	if len(raw) == 0 {
		// The model neither called the final_answer tool nor produced any text to parse: there is
		// no answer to decode. Report that directly rather than surfacing an opaque JSON error on "".
		return zero, fmt.Errorf("typed: run produced no final_answer tool call and no text answer to decode into %T: %w", zero, ErrProtocol)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("typed: decode answer into %T (the model answered in text that is not valid JSON for this type): %w (%w)", zero, err, ErrProtocol)
	}
	return out, nil
}

// RunTypedNative is like RunTyped but uses the provider's NATIVE structured-output
// constraint (a JSON-schema response format) instead of the injected final_answer tool:
// it sets Request.ResponseFormat from T's schema and decodes the model's direct JSON
// output. Prefer it on OpenAI-compatible providers with strict structured outputs (schema
// adherence is enforced provider-side, no tool round-trip). Providers that don't support
// response formats (e.g. Anthropic) ignore the constraint — use the provider-agnostic
// RunTyped there. Package function (Go methods can't add type parameters).
func RunTypedNative[T any](ctx context.Context, a *Agent, runID, input string) (T, error) {
	var zero T
	sch, err := schema.For[T]()
	if err != nil {
		return zero, fmt.Errorf("typed: schema for %T: %w (%w)", zero, err, ErrConfig)
	}
	c := a.clone()
	c.responseFormat = &ResponseFormat{Name: "response", Schema: sch}
	msg, err := c.Run(ctx, runID, input)
	if err != nil {
		return zero, err
	}
	var out T
	if err := json.Unmarshal([]byte(firstText(msg)), &out); err != nil {
		return zero, fmt.Errorf("typed: decode native structured output into %T: %w (%w)", zero, err, ErrProtocol)
	}
	return out, nil
}

// clone returns a shallow copy of the agent (shared model/store; copied tool map and
// middleware slices), leaving the caller's agent untouched.
func (a *Agent) clone() *Agent {
	c := *a // copies every value field and pointer, so a newly added option field can't be forgotten
	// Deep-copy only the reference types a derived agent must not share with its parent.
	c.tools = make(map[string]Tool, len(a.tools)+1)
	for k, v := range a.tools {
		c.tools[k] = v
	}
	c.mw = append([]Middleware(nil), a.mw...)
	c.toolMW = append([]ToolMiddleware(nil), a.toolMW...)
	return &c
}

// cloneWith returns a clone with one extra tool and appended model middleware.
func (a *Agent) cloneWith(extra Tool, mw ...Middleware) *Agent {
	c := a.clone()
	c.tools[extra.Name()] = extra
	c.mw = append(c.mw, mw...)
	return c
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
