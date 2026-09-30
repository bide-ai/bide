package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/bide-ai/bide/internal/strictjson"
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
// The answer is the arguments the final_answer tool accepted: what it received, after any tool
// middleware (a middleware that rewrites arguments rewrites the answer, as it would for any
// tool). The tool decodes them strictly as T (see Func: a missing required field, an unknown or
// case-variant name, a duplicate name, and so on are a tool error the model corrects) and
// journals them as its result, {"accepted": <arguments>}. RunTyped reads that JOURNALED result,
// not a value captured live, so it is resume-safe: on resume, or when the run already finished,
// the answer is recovered from the log without re-running anything. (A run journaled before
// final_answer recorded its arguments has the result {}; its answer is the model's arguments,
// decoded with encoding/json as the tool then accepted them.)
//
// Only if the model never makes an accepted final_answer call (it answers in plain text
// instead) does RunTyped parse the text of the run's final turn (the message Run returns) as T,
// strictly too; a text that does not decode is ErrProtocol.
//
// T must be a type whose schema is a JSON object (a struct, a pointer to one, or a map): the
// answer is a tool call's arguments, which providers take only as an object. Any other T is an
// ErrConfig before the run starts. Because Go methods cannot add type parameters, this is a
// package function: agent.RunTyped[MyResult](ctx, a, id, in).
func RunTyped[T any](ctx context.Context, a *Agent, runID, input string) (T, error) {
	var zero T
	sch, err := schema.For[T]()
	if err != nil {
		return zero, fmt.Errorf("typed: schema for %T: %w (%w)", zero, err, ErrConfig)
	}
	var shape struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(sch, &shape) // For's own output, always a JSON object
	if shape.Type != "object" {
		return zero, fmt.Errorf("typed: %T is not a JSON object, and a final_answer tool call's arguments must be one (wrap it in a struct field): %w", zero, ErrConfig)
	}
	if _, taken := a.tools[finalAnswerTool]; taken {
		return zero, fmt.Errorf("typed: agent already registers a %q tool, which RunTyped needs: %w", finalAnswerTool, ErrConfig)
	}

	// The terminal tool: its arguments decode strictly as T, so a malformed call is rejected
	// with ErrToolArgs and the model self-corrects. Its result journals the arguments it
	// accepted, which RunTyped reads back (below), so resume works.
	respond := &answerTool[T]{schema: sch}

	instruction := "Use the available tools to do any needed work, then call the " + finalAnswerTool +
		" tool exactly once with the final answer. Calling it completes the task; do not add further prose."

	// A successful final_answer call ends the run: the answer exists, so the model is not asked
	// for another turn (which could only repeat the answer, or run into the turn cap).
	typed := a.cloneWith(respond, injectSystem(instruction))
	typed.terminalTool = finalAnswerTool
	final, err := typed.Run(ctx, runID, input)
	if err != nil {
		return zero, err
	}

	// The answer is the journaled result of the first final_answer call (in the model's order)
	// the tool accepted (resume-safe). A call the tool rejected (arguments that do not decode as
	// T) is not an answer. Only if no call was accepted does the final turn's text stand in.
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return zero, fmt.Errorf("typed: load history %s: %w (%w)", runID, err, ErrStorage)
	}
	accepted := map[string]json.RawMessage{}
	for _, r := range recs {
		if r.Kind == StepToolResult && !r.IsError {
			accepted[r.ToolUseID] = r.Result
		}
	}
	var out T
	for _, r := range recs {
		if r.Kind != StepModel || r.Message == nil {
			continue
		}
		for _, tu := range r.Message.toolUses() {
			if result, ok := accepted[tu.ID]; ok && tu.Name == finalAnswerTool {
				if err := readAnswer(result, tu.Args, &out); err != nil {
					return zero, fmt.Errorf("typed: decode final_answer into %T: %w (%w)", zero, err, ErrProtocol)
				}
				return out, nil
			}
		}
	}
	// The final turn, not an earlier one: text written beside a tool call is not the answer.
	raw := json.RawMessage(firstText(final))
	if len(raw) == 0 {
		// The model neither called the final_answer tool nor produced any text to parse: there is
		// no answer to decode. Report that directly rather than surfacing an opaque JSON error on "".
		return zero, fmt.Errorf("typed: run produced no final_answer tool call and no text answer to decode into %T: %w", zero, ErrProtocol)
	}
	if err := strictjson.Unmarshal(raw, &out, argsOptions); err != nil {
		return zero, fmt.Errorf("typed: decode answer into %T (the model answered in text that is not valid JSON for this type): %w (%w)", zero, err, ErrProtocol)
	}
	return out, nil
}

// answerTool is RunTyped's final_answer tool. Its arguments decode strictly as T, and its result
// records the arguments it accepted, as the tool received them after any tool middleware.
type answerTool[T any] struct {
	schema json.RawMessage
}

// answerRecord is the journaled result of an accepted final_answer call.
type answerRecord struct {
	Accepted json.RawMessage `json:"accepted"`
}

// answerToolDescription is what the model is told the final_answer tool is for.
const answerToolDescription = "Call this exactly once, with the final answer structured per the schema, to complete the task."

func (t *answerTool[T]) Name() string                { return finalAnswerTool }
func (t *answerTool[T]) Description() string         { return answerToolDescription }
func (t *answerTool[T]) Safety() Safety              { return Safety{ReadOnly: true} }
func (t *answerTool[T]) ArgsSchema() json.RawMessage { return t.schema }

// Spec returns the final_answer tool's spec: read-only, ungated, with T's schema as its input.
func (t *answerTool[T]) Spec() ToolSpec {
	return ToolSpec{Name: finalAnswerTool, Description: answerToolDescription, Input: t.schema, Safety: Safety{ReadOnly: true}}
}

func (t *answerTool[T]) Call(_ context.Context, args json.RawMessage) (json.RawMessage, error) {
	var v T
	if err := decodeArgs(args, &v); err != nil {
		return nil, fmt.Errorf("decode args for tool %q: %w (%w)", finalAnswerTool, err, ErrToolArgs)
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	return marshalJournal(answerRecord{Accepted: args})
}

// readAnswer decodes into out the answer an accepted final_answer call journaled: result, or, for
// a run journaled before the tool recorded its arguments (result {}), the model's arguments args,
// decoded with encoding/json as the tool then accepted them.
func readAnswer(result, args json.RawMessage, out any) error {
	if bytes.Equal(bytes.TrimSpace(result), []byte("{}")) {
		if len(args) == 0 {
			return nil
		}
		return json.Unmarshal(args, out)
	}
	var rec answerRecord
	if err := strictjson.Unmarshal(result, &rec, argsOptions); err != nil {
		return fmt.Errorf("the journaled result is not an accepted answer: %w", err)
	}
	return decodeArgs(rec.Accepted, out)
}

// RunTypedNative is like RunTyped but uses the provider's NATIVE structured-output
// constraint (a JSON-schema response format) instead of the injected final_answer tool:
// it sets Request.ResponseFormat from T's schema and decodes the model's direct JSON
// output. Prefer it on OpenAI-compatible providers with strict structured outputs (schema
// adherence is enforced provider-side, no tool round-trip). An adapter that does not support
// response formats (Anthropic) fails the run with ErrConfig before calling the model; use
// the provider-agnostic RunTyped there. Package function (Go methods can't add type parameters).
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
	if err := strictjson.Unmarshal([]byte(firstText(msg)), &out, argsOptions); err != nil {
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
	c.indexTools()
	return &c
}

// cloneWith returns a clone with one extra tool and appended model middleware.
func (a *Agent) cloneWith(extra Tool, mw ...Middleware) *Agent {
	c := a.clone()
	c.tools[SpecOf(extra).Name] = extra
	c.indexTools()
	c.mw = append(c.mw, mw...)
	return c
}

// injectSystem is model middleware that prepends a system message to each model call —
// Run seeds only a user turn, so this is how RunTyped steers without a system-prompt API.
func injectSystem(s string) Middleware {
	sys := SystemText(s)
	return func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			call.Request.Messages = append([]Message{sys}, call.Request.Messages...)
			return next(ctx, call)
		}
	}
}
