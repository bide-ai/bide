// Package agent is a durability-first, Go-idiomatic toolkit for building AI agents.
//
// The agent loop journals every step to a Durable store via named-step memoization
// (Durable.Do) and resumes after a crash — reusing recorded steps, re-running
// retry-safe tools, halting (rather than double-executing) a non-idempotent tool whose
// outcome is unknown, and pausing durably for human approval when a tool requires it.
// Orchestration is plain Go (Option B); the model call is wrapped by a func(Handler)
// Handler middleware chain. Reliability wrappers ship in the middleware package:
// per-attempt timeouts, classified retry with backoff, hedged model calls
// (middleware.Hedge), and rate limiting.
//
// Scope: this resumes AROUND tool boundaries, not the internals of a single in-flight
// tool call. The precise promise is "survives crashes around tool calls, never
// double-fires an unsafe side effect" — not "mid-tool-call resume".
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// addUsage accumulates src into dst field-by-field.
func addUsage(dst *Usage, src Usage) {
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheWriteTokens += src.CacheWriteTokens
}

// Agent binds a model, a tool set, a durable store, and a middleware chain.
//
// Build an Agent with Build and options, and derive a differently configured one with With,
// which copies it: an agent built that way is never changed, so it is safe for concurrent
// Run/Stream/Session calls and for concurrent With calls.
//
// The transitional builder methods (WithMaxTurns, Use, UseTool, SetMaxConcurrency and the rest)
// MUTATE the receiver in place and return it for chaining; they do not copy. Two variables
// assigned from the same New(...) are aliases: reconfiguring one reconfigures both. They are not
// safe to call concurrently with a run or with each other, and the 1.0 rewrite removes them in
// favour of the options.
type Agent struct {
	model    Model
	tools    map[string]Tool
	specs    map[string]*ToolSpec // each tool's spec, read once when it was registered; never changed
	specList []ToolSpec           // specs sorted by name, as model requests are sent them
	// toolErr is the first problem New found with its tools (a duplicate name, a wrapper it
	// cannot honor, an invalid approval policy): every run fails with it. Build and With return
	// such a problem instead, so an agent they make never has one.
	toolErr      error
	store        Durable
	mw           []Middleware
	toolMW       []ToolMiddleware
	sampling     Sampling // generation controls applied to every model call
	maxConc      int      // max concurrent tool calls per turn; 0 = unbounded (default)
	maxTurns     int      // max model turns per run; 0 = unbounded (default)
	tokenBudget  int      // max tokens per run (Usage.TotalTokens); 0 = unbounded (default)
	systemPrompt string   // optional static system message prepended to every model call
	// systemPromptFn, if set, computes the system message per drive of a run (dynamic context:
	// current time, tenant, retrieved state). It shares one slot with systemPrompt: it takes
	// precedence over the text, and setting the text clears it, so the later of the two wins.
	systemPromptFn func(context.Context, RunInfo) (string, error)
	responseFormat *ResponseFormat  // native structured-output constraint (see RunTypedNative)
	toolChoice     *ToolChoice      // tool-choice control applied to every model call (see WithToolChoice)
	terminalTool   string           // a successful call to this tool ends the run (RunTyped's final_answer)
	retrievals     []retrievalLayer // WithRetrieval steps, in the order given
	// identity, waker and clock are the agent's defaults for a run whose context carries none
	// (see runDefaults).
	identity *Identity
	waker    Waker
	clock    func() time.Time
	// approverVerifiers resolves an approver id to the verifier for its decision
	// signature; required by any tool with an m-of-n ToolSpec.Approval (see WithApproverVerifiers).
	approverVerifiers ApproverVerifierFor
	// toolErrRedact, if set, chooses the text journaled and sent to the model for a failed tool
	// call (see WithToolErrorRedactor).
	toolErrRedact func(tool string, err error) string
}

// systemMessage returns the system prompt for this drive of run: the dynamic function's if one
// is set, else the static string.
func (a *Agent) systemMessage(ctx context.Context, run RunInfo) (string, error) {
	if a.systemPromptFn != nil {
		s, err := a.systemPromptFn(ctx, run)
		if err != nil {
			return "", fmt.Errorf("agent: system prompt for run %s: %w", run.RunID, err)
		}
		return s, nil
	}
	return a.systemPrompt, nil
}

// SamplingOption sets one field of the Sampling config; see Temperature, TopP,
// MaxTokens, Stop, Seed.
type SamplingOption func(*Sampling)

// Temperature sets the sampling temperature (0 = most deterministic).
func Temperature(v float64) SamplingOption { return func(s *Sampling) { s.Temperature = &v } }

// TopP sets nucleus-sampling top-p.
func TopP(v float64) SamplingOption { return func(s *Sampling) { s.TopP = &v } }

// MaxTokens caps generated tokens, overriding the model adapter's construction default.
func MaxTokens(v int) SamplingOption { return func(s *Sampling) { s.MaxTokens = &v } }

// Stop sets stop sequences.
func Stop(seqs ...string) SamplingOption { return func(s *Sampling) { s.Stop = seqs } }

// Seed sets a best-effort determinism seed (honored by providers that support it).
func Seed(v int64) SamplingOption { return func(s *Sampling) { s.Seed = &v } }

// New constructs an Agent over model, store and tools. It panics if model or store is nil. A
// problem with the tools (two with one name, an invalid approval policy, a wrapper the agent
// cannot honor) is not reported here: every run fails with it, as ErrConfig. Build reports these
// when the agent is built, and refuses more: the reserved name "final_answer", an input schema
// that is not a JSON object, and every other configuration problem.
//
// Deprecated: transitional; the 1.0 rewrite removes it, and renames Build to New. Use Build.
func New(model Model, store Durable, tools ...Tool) *Agent {
	if model == nil {
		panic("agent: New requires a non-nil Model")
	}
	if store == nil {
		panic("agent: New requires a non-nil Durable store")
	}
	a := newAgent(model, store)
	for _, t := range tools {
		if err := a.addTool(t, false); err != nil && a.toolErr == nil {
			a.toolErr = err
		}
	}
	a.sortSpecs()
	return a
}

// addTool registers t, reading its spec once: every decision about its calls reads this copy. It
// refuses a nil tool, a name the agent already has, a wrapper the agent cannot honor
// (checkWrapper), and an approval policy no option would build (a SingleApproval whose fields
// were changed), each with ErrConfig. strict (Build and With) also refuses the name RunTyped
// reserves and an input schema that is not a JSON object; New, which is transitional, does not,
// so the tools it was always given keep working until the 1.0 rewrite moves them to Build. A
// refused tool is not registered.
func (a *Agent) addTool(t Tool, strict bool) error {
	if isNil(t) {
		return fmt.Errorf("agent: nil tool: %w", ErrConfig)
	}
	s := SpecOf(t)
	if err := checkWrapper(t, s); err != nil {
		return err
	}
	if s.Approval != nil {
		if err := checkApproval(s.Approval); err != nil {
			return fmt.Errorf("agent: tool %q: %w", s.Name, err)
		}
	}
	switch _, taken := a.specs[s.Name]; {
	case taken && !strict:
		a.tools[s.Name], a.specs[s.Name] = t, &s // as New always did: the run fails with the error
		return fmt.Errorf("agent: two tools are named %q: %w", s.Name, ErrConfig)
	case taken:
		// The model calls a tool by name, so one of the two could never be called, and which one
		// a call reached would depend on the order the host listed them in: a host that adds tools
		// from a runtime source such as an MCP server after its own would send the model's call,
		// arguments and all, to the server.
		return fmt.Errorf("agent: two tools are named %q: %w", s.Name, ErrConfig)
	case !strict:
	case s.Name == finalAnswerTool:
		return fmt.Errorf("agent: tool name %q is reserved for RunTyped's answer: %w", s.Name, ErrConfig)
	default:
		if err := checkInputSchema(s); err != nil {
			return err
		}
	}
	a.tools[s.Name], a.specs[s.Name] = t, &s
	return nil
}

// checkInputSchema refuses a spec whose Input is not a JSON object schema: a JSON object whose
// "type", if it has one, is "object". Providers take a tool's arguments only as an object.
func checkInputSchema(s ToolSpec) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(s.Input, &obj); err != nil || obj == nil {
		return fmt.Errorf("agent: tool %q: input schema is not a JSON object: %w", s.Name, ErrConfig)
	}
	if t, ok := obj["type"]; ok {
		var typ string
		if err := json.Unmarshal(t, &typ); err != nil || typ != "object" {
			return fmt.Errorf("agent: tool %q: input schema type is %s, not \"object\": %w", s.Name, t, ErrConfig)
		}
	}
	return nil
}

// checkTools reports the first problem New found with its tools (see New). It is an error rather
// than a panic because a tool list read from a server at run time is data, not code.
func (a *Agent) checkTools() error { return a.toolErr }

// Use appends middleware wrapping the model call (first added = outermost). Returns the
// agent for chaining.
//
// Deprecated: transitional; the 1.0 rewrite removes it. Use the WithMiddleware option.
func (a *Agent) Use(mw ...Middleware) *Agent {
	a.mw = append(a.mw, mw...)
	return a
}

// WithSampling sets generation controls applied to every model call (last write wins per
// field). Returns the agent for chaining.
//
// Deprecated: transitional; the 1.0 rewrite removes it. Use the WithSampling option.
func (a *Agent) WithSampling(opts ...SamplingOption) *Agent {
	for _, o := range opts {
		o(&a.sampling)
	}
	return a
}

// WithToolChoice sets the tool-choice control applied to every model call (see
// ToolChoice for the modes). Returns the agent for chaining.
//
// CAVEAT: forcing Mode "required" or "tool" on every turn of the multi-turn loop keeps
// the model from ever producing a final answer (it must always call a tool), so the run
// cannot terminate normally. Reserve those modes for single-turn or typed/structured
// calls; "auto" (the default when unset) is the norm for the agent loop.
//
// Deprecated: transitional; the 1.0 rewrite removes it. Use the WithToolChoice option.
func (a *Agent) WithToolChoice(tc ToolChoice) *Agent {
	a.toolChoice = &tc
	return a
}

// WithSystemPrompt sets a system message that is prepended to the conversation on every
// model turn. The message is re-seeded on each Run (including resumes), so it is always
// present regardless of journal replay. It fills the slot WithSystemPromptFunc fills: the later
// of the two wins. Returns the agent for chaining.
//
// The system message is configuration, not journal: it is not recorded, and the model turns a
// drive makes are sent the message the agent holds at that drive. A run resumed after the prompt
// was changed sends its remaining turns the new prompt beside turns that answered the old one.
// Its recorded turns are not affected, and neither is any decision the loop makes on resume.
//
// Deprecated: transitional; the 1.0 rewrite removes it. Use the WithSystemPrompt option.
func (a *Agent) WithSystemPrompt(s string) *Agent {
	a.systemPrompt, a.systemPromptFn = s, nil
	return a
}

// WithSystemPromptFunc sets a system message computed per run, so it can inject dynamic
// context (current date, tenant, retrieved state) each turn. It fills the slot WithSystemPrompt
// fills: the later of the two wins. Returns the agent for chaining.
//
// fn is called once per drive of a run (each Run, Stream, or resume), not once per run, and its
// result is not journaled (see WithSystemPrompt): a run resumed later is sent what fn returns
// then. Context that the run's later turns must see unchanged belongs in the input, which is
// journaled (see RunStart), or in a tool result.
//
// Deprecated: transitional; the 1.0 rewrite removes it. Use the WithSystemPromptFunc option,
// whose function is also given the run's RunInfo and may fail.
func (a *Agent) WithSystemPromptFunc(fn func(context.Context) string) *Agent {
	a.systemPromptFn = func(ctx context.Context, _ RunInfo) (string, error) { return fn(ctx), nil }
	return a
}

// WithMaxTurns caps the number of model turns a single run may take, so a model that
// keeps calling tools can't loop forever. n <= 0 means unbounded (the default). When the
// cap is reached the run returns ErrMaxTurns (category ErrBudget). Returns the agent for
// chaining. The cap is per run (per Session.Send turn), not per session.
//
// Deprecated: transitional; the 1.0 rewrite removes it. Use the WithMaxTurns option, which
// refuses a negative n.
func (a *Agent) WithMaxTurns(n int) *Agent {
	a.maxTurns = max(n, 0)
	return a
}

// WithTokenBudget caps the tokens a single run may use: once the run's model calls have used
// max tokens or more (Usage.TotalTokens, cached input included), the run makes no further
// model call and returns ErrBudgetExceeded (category ErrBudget). A call's usage is known only
// after it returns, so the call that crosses max completes; the next one is refused. max <= 0
// means unbounded (the default). Returns the agent for chaining.
//
// The budget counts every model request the run sent, not only the responses it recorded: a
// failed attempt a middleware retried, a losing hedge target, and a model call that failed for
// good all count (see Result.Spend). It is durable: each turn's usage and discarded spend are
// journaled with it, a failed call's spend is journaled as its own record, and the count is
// rebuilt from the journal, so a resumed run, on any process, is held to what it has already
// used. A request still running when the run returns (a hedge loser whose model ignores
// cancellation) is not counted. Like WithMaxTurns it applies per run (per Session.Send turn).
//
// The budget covers the run's agent tree: every run started from its tool calls (sub-agents,
// and theirs) counts against it, and each of those runs checks it, with its own budget if it has
// one, before each model call. A resumed tree first counts the journals of its sub-agents cut
// off mid-run, so it is held to everything it used before any of it calls the model again.
// Overshoot: a check sees every call that has returned, so the tree passes max only by calls in
// flight when it reached max. Each agent run has at most one model call in flight, so with k
// runs of the tree calling the model at that moment (k parallel sub-agents; 1 with none) the
// tree uses less than max plus k calls' usage.
//
// Deprecated: transitional; the 1.0 rewrite removes it. Use the WithTokenBudget option, which
// refuses a negative max.
func (a *Agent) WithTokenBudget(max int) *Agent {
	if max < 0 {
		max = 0
	}
	a.tokenBudget = max
	return a
}

// SetMaxConcurrency bounds how many tool calls run in parallel within a single turn.
// n <= 0 means unbounded (the default). Returns the agent for chaining; use
// SetMaxConcurrency(1) to force fully sequential tool execution.
//
// Deprecated: transitional; the 1.0 rewrite removes it. Use the WithMaxConcurrency option, which
// refuses a negative n.
func (a *Agent) SetMaxConcurrency(n int) *Agent {
	a.maxConc = max(n, 0)
	return a
}

// WithApproverVerifiers configures how the m-of-n approval gate resolves an approver id to
// the verifier for its signature. Required whenever any tool carries an m-of-n
// ToolSpec.Approval (see WithApproval): a gated call with no resolver configured fails with ErrConfig rather than
// silently counting zero decisions. Returns the agent for chaining.
//
// Deprecated: transitional; the 1.0 rewrite removes it. Use the WithApproverVerifiers option,
// with which Build and With check every m-of-n policy when the agent is built.
func (a *Agent) WithApproverVerifiers(fn ApproverVerifierFor) *Agent {
	a.approverVerifiers = fn
	return a
}

// WithToolErrorRedactor sets the text recorded for a tool call that fails. A failed call's
// error text is journaled as the call's result, sent to the model, and hashed into the audit
// trail, where a proof can disclose it, so it must not carry secrets. By default the text is the
// error's own, with every URL in it redacted: its userinfo, each query parameter's value, and
// its fragment become REDACTED (a net/http *url.Error quotes the whole request URL, API key
// parameter included). fn receives the tool's name and its error and returns the text to record
// instead; the same URL redaction then applies to what fn returns. Use it to scrub what URL
// redaction cannot know about (an account number, a token in a header echoed back), and keep
// the text useful: the model reads it to decide what to do next.
//
// It applies to every failed call, a sub-agent's included (a sub-agent's failure is its tool
// call's error), and to the failure a saga journals. The error the caller gets back
// (SagaAborted.Cause, for one) is the tool's own. Returns the agent for chaining.
//
// Deprecated: transitional; the 1.0 rewrite removes it. Use the WithToolErrorRedactor option.
func (a *Agent) WithToolErrorRedactor(fn func(tool string, err error) string) *Agent {
	a.toolErrRedact = fn
	return a
}
