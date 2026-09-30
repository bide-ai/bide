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
	"fmt"
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
// The WithX/Use/SetX builder methods MUTATE the receiver in place and return it for chaining; they
// do not copy. This is deliberate: it lets configuration be applied after construction and after the
// value has been shared, e.g. a.Use(trace.Model(...)) or a.Use(agent.WithRetrieval(...)) wiring a
// cross-cutting concern onto an agent other code already holds. The consequence is that two
// variables assigned from the same New(...) are aliases: reconfiguring one reconfigures both. When
// you need an independently configured variant, construct a fresh Agent rather than expecting a
// builder to fork. (The internal clone/cloneWith, used by RunTyped, do copy; they are not exported.)
//
// An Agent is safe for concurrent Run/Stream/Session calls once configured; the builder methods are
// not safe to call concurrently with a run or with each other. Configure first, then run.
type Agent struct {
	model        Model
	tools        map[string]Tool
	specs        map[string]*ToolSpec // each tool's spec, read once when it was registered; never changed
	specList     []ToolSpec           // specs sorted by name, as model requests are sent them
	dupTool      string               // a tool name New was given more than once; every run fails with ErrConfig
	toolErr      error                // the first tool New refused (checkWrapper); every run fails with it
	store        Durable
	mw           []Middleware
	toolMW       []ToolMiddleware
	sampling     Sampling // generation controls applied to every model call
	maxConc      int      // max concurrent tool calls per turn; 0 = unbounded (default)
	maxTurns     int      // max model turns per run; 0 = unbounded (default)
	tokenBudget  int      // max tokens per run (Usage.TotalTokens); 0 = unbounded (default)
	systemPrompt string   // optional static system message prepended to every model call
	// systemPromptFn, if set, computes the system message per run (dynamic context:
	// current time, tenant, retrieved state). Takes precedence over systemPrompt.
	systemPromptFn func(context.Context) string
	responseFormat *ResponseFormat // native structured-output constraint (see RunTypedNative)
	toolChoice     *ToolChoice     // tool-choice control applied to every model call (see WithToolChoice)
	terminalTool   string          // a successful call to this tool ends the run (RunTyped's final_answer)
	// approverVerifiers resolves an approver id to the verifier for its decision
	// signature; required by any tool with an m-of-n ToolSpec.Approval (see WithApproverVerifiers).
	approverVerifiers ApproverVerifierFor
	// toolErrRedact, if set, chooses the text journaled and sent to the model for a failed tool
	// call (see WithToolErrorRedactor).
	toolErrRedact func(tool string, err error) string
}

// systemMessage returns the system prompt for this run — the dynamic function if set,
// else the static string.
func (a *Agent) systemMessage(ctx context.Context) string {
	if a.systemPromptFn != nil {
		return a.systemPromptFn(ctx)
	}
	return a.systemPrompt
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

// WithSampling sets generation controls applied to every model call (last write wins per
// field). Returns the agent for chaining: New(...).WithSampling(agent.Temperature(0), agent.MaxTokens(500)).
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
func (a *Agent) WithToolChoice(tc ToolChoice) *Agent {
	a.toolChoice = &tc
	return a
}

// WithSystemPrompt sets a system message that is prepended to the conversation on every
// model turn. The message is re-seeded on each Run (including resumes), so it is always
// present regardless of journal replay. Returns the agent for chaining:
// New(...).WithSystemPrompt("you are a concise assistant").
//
// The system message is configuration, not journal: it is not recorded, and the model turns a
// drive makes are sent the message the agent holds at that drive. A run resumed after the prompt
// was changed sends its remaining turns the new prompt beside turns that answered the old one.
// Its recorded turns are not affected, and neither is any decision the loop makes on resume.
func (a *Agent) WithSystemPrompt(s string) *Agent {
	a.systemPrompt = s
	return a
}

// WithSystemPromptFunc sets a system message computed per run, so it can inject dynamic
// context (current date, tenant, retrieved state) each turn. It takes precedence over
// WithSystemPrompt. Returns the agent for chaining.
//
// fn is called once per drive of a run (each Run, Stream, or resume), not once per run, and its
// result is not journaled (see WithSystemPrompt): a run resumed later is sent what fn returns
// then. Context that the run's later turns must see unchanged belongs in the input, which is
// journaled (see RunStart), or in a tool result.
func (a *Agent) WithSystemPromptFunc(fn func(context.Context) string) *Agent {
	a.systemPromptFn = fn
	return a
}

// New constructs an Agent. It panics if model or store is nil: both are load-bearing on every run
// (the model drives turns, the store journals them for at-most-once resume), so a nil is a
// construction-time programmer error, not a runtime condition to thread through every call.
// Tool names must be unique: if two tools share a name, every run fails with ErrConfig.
func New(model Model, store Durable, tools ...Tool) *Agent {
	if model == nil {
		panic("agent: New requires a non-nil Model")
	}
	if store == nil {
		panic("agent: New requires a non-nil Durable store")
	}
	m := make(map[string]Tool, len(tools))
	specs := make(map[string]*ToolSpec, len(tools))
	dup := ""
	var toolErr error
	for _, t := range tools {
		s := SpecOf(t) // read once: every decision about the tool's calls reads this copy
		if err := checkWrapper(t, s); err != nil && toolErr == nil {
			toolErr = err
		}
		if _, taken := m[s.Name]; taken && dup == "" {
			dup = s.Name
		}
		m[s.Name], specs[s.Name] = t, &s // a tool with a Spec method is called by its spec's name
	}
	a := &Agent{model: model, tools: m, specs: specs, dupTool: dup, toolErr: toolErr, store: store}
	a.sortSpecs()
	return a
}

// checkTools reports a tool name New was given twice. The model calls a tool by name, so one of
// the two could never be called, and which one a call reaches would depend on the order the host
// listed them in: a host that adds tools from a runtime source such as an MCP server after its
// own would send the model's call, arguments and all, to the server. It is an error rather than
// a panic because a tool list read from a server at run time is data, not code.
func (a *Agent) checkTools() error {
	if a.dupTool != "" {
		return fmt.Errorf("agent: two tools are named %q: %w", a.dupTool, ErrConfig)
	}
	return a.toolErr
}

// Use appends middleware wrapping the model call (first added = outermost). Returns the
// agent for chaining.
func (a *Agent) Use(mw ...Middleware) *Agent {
	a.mw = append(a.mw, mw...)
	return a
}

// WithMaxTurns caps the number of model turns a single run may take, so a model that
// keeps calling tools can't loop forever. n <= 0 means unbounded (the default). When the
// cap is reached the run returns ErrMaxTurns (category ErrBudget). Returns the agent for
// chaining. The cap is per run (per Session.Send turn), not per session.
func (a *Agent) WithMaxTurns(n int) *Agent {
	a.maxTurns = n
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
func (a *Agent) WithTokenBudget(max int) *Agent {
	a.tokenBudget = max
	return a
}

// SetMaxConcurrency bounds how many tool calls run in parallel within a single turn.
// n <= 0 means unbounded (the default). Returns the agent for chaining; use
// SetMaxConcurrency(1) to force fully sequential tool execution.
func (a *Agent) SetMaxConcurrency(n int) *Agent {
	a.maxConc = n
	return a
}

// WithApproverVerifiers configures how the m-of-n approval gate resolves an approver id to
// the verifier for its signature. Required whenever any tool carries an m-of-n
// ToolSpec.Approval (see WithApproval): a gated call with no resolver configured fails with ErrConfig rather than
// silently counting zero decisions. Returns the agent for chaining.
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
func (a *Agent) WithToolErrorRedactor(fn func(tool string, err error) string) *Agent {
	a.toolErrRedact = fn
	return a
}
