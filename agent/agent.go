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
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"
)

// addUsage accumulates src into dst field-by-field.
func addUsage(dst *Usage, src Usage) {
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.CacheReadTokens += src.CacheReadTokens
	dst.CacheWriteTokens += src.CacheWriteTokens
}

// ModelHandler generates one assistant turn. Middleware wraps it.
type ModelHandler func(context.Context, Request) (Message, Usage, error)

// Middleware wraps a ModelHandler — the net/http-style func(Handler) Handler chain, at
// the SEMANTIC layer (it sees messages, tool calls, token usage — not bytes). Batteries
// live in the middleware/ package (Retry, RateLimit, Cost, ...).
type Middleware func(ModelHandler) ModelHandler

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
	dupTool      string // a tool name New was given more than once; every run fails with ErrConfig
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
	// signature; required by any tool with a non-nil Safety.Approval (see WithApproverVerifiers).
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
func (a *Agent) WithSystemPrompt(s string) *Agent {
	a.systemPrompt = s
	return a
}

// WithSystemPromptFunc sets a system message computed per run, so it can inject dynamic
// context (current date, tenant, retrieved state) each turn. It takes precedence over
// WithSystemPrompt. Returns the agent for chaining.
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
	dup := ""
	for _, t := range tools {
		if _, taken := m[t.Name()]; taken && dup == "" {
			dup = t.Name()
		}
		m[t.Name()] = t
	}
	return &Agent{model: model, tools: m, dupTool: dup, store: store}
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
	return nil
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
// cancellation) is not counted. Like WithMaxTurns it applies per run (per Session.Send turn), and a
// sub-agent's run has its own budget.
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
// the verifier for its signature. Required whenever any tool carries a non-nil
// Safety.Approval: a gated call with no resolver configured fails with ErrConfig rather than
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

// generate runs one model call through the middleware chain. usedIDs holds the tool-use IDs
// already in the run's conversation; a turn that reuses one is rejected (see checkToolUseIDs).
// The innermost handler checks each model response, below middleware, so a retry middleware
// sees the fault, and the check still holds when a middleware trims the history it sends. The
// response the chain returns is checked again, since a middleware can return one it did not get
// from the handler it wraps (a fallback, a cache), and that response is what the run records.
//
// The innermost handler sends every request of the call: it goes to the model WithModel set, if
// any, and runs the ModelCallHooks middleware installed around it. meter receives every
// request's usage, so the run can record what the call spent beyond the response it returns.
func (a *Agent) generate(ctx context.Context, req Request, usedIDs map[string]bool, meter *spendMeter) (Message, Usage, error) {
	ctx = inModelCall(ctx, meter)
	h := ModelHandler(func(ctx context.Context, req Request) (Message, Usage, error) {
		hooks := modelHooks(ctx)
		for _, hk := range hooks {
			if hk.Before != nil {
				if err := hk.Before(ctx); err != nil {
					return Message{}, Usage{}, err
				}
			}
		}
		msg, u, err := a.send(ctx, modelFor(ctx, a.model), req)
		for _, hk := range hooks {
			if hk.After != nil {
				hk.After(u)
			}
		}
		if err != nil {
			return msg, u, err
		}
		if err := checkToolUseIDs(msg, usedIDs); err != nil {
			return Message{}, u, err
		}
		return msg, u, nil
	})
	for i := len(a.mw) - 1; i >= 0; i-- {
		h = a.mw[i](h)
	}
	msg, u, err := h(ctx, req)
	if err != nil {
		return msg, u, err
	}
	if err := u.Validate(); err != nil {
		return Message{}, Usage{}, err
	}
	if err := checkToolUseIDs(msg, usedIDs); err != nil {
		return Message{}, u, err
	}
	return msg, u, nil
}

// send makes one model request. When a token sink is installed (Agent.Stream), it streams the
// call and forwards deltas as they arrive while still assembling the message for the journal;
// otherwise it takes the plain blocking drain. Middleware wraps this either way and sees the
// assembled message and usage: streaming stays below it.
func (a *Agent) send(ctx context.Context, m Model, req Request) (Message, Usage, error) {
	sink := modelSink(ctx)
	if sink == nil {
		return Generate(ctx, m, req)
	}
	sink(attemptStart{}) // a new attempt: any deltas an earlier one streamed are discarded
	s, err := m.Stream(ctx, req)
	if err != nil {
		return Message{}, Usage{}, err
	}
	return s.drain(sink)
}

// Run drives the agent to completion for runID, resuming from the journal if steps
// already exist. Completed steps are reused; retry-safe tools with no recorded result
// are re-run; a non-retry-safe tool with no result triggers ResumeHalt; a tool that
// requires approval with no recorded decision triggers PendingApproval. A run that already
// finished is final: Run returns its recorded answer without calling the model, whatever
// input is passed, so retrying a completed run never repeats its side effects.
func (a *Agent) Run(ctx context.Context, runID, input string) (Message, error) {
	msg, _, _, err := a.run(ctx, runID, []Message{UserText(input)}, false, nil)
	return msg, err
}

// run is the single loop shared by Run/RunSaga (emit == nil) and Stream/StreamSaga
// (emit receives lifecycle events). It drives one durable run seeded with `seed` — the
// conversation to start from: a single user turn for Run, or the full transcript plus
// the new user turn for a Session turn. The system prompt, if set, is prepended ahead of
// the seed. Durability, resume, and side-effect safety are identical regardless of emit.
// It returns the final message, accumulated token usage across all model turns, the
// number of live model turns (replayed journal turns are not counted), and any error.
func (a *Agent) run(ctx context.Context, runID string, seed []Message, saga bool, emit func(AgentEvent)) (Message, usageTotals, int, error) {
	if err := checkRunID(ctx, runID); err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	if err := a.checkTools(); err != nil {
		return Message{}, usageTotals{}, 0, err
	}
	fire := func(e AgentEvent) {
		if emit != nil {
			emit(e)
		}
	}
	toolH := a.toolHandler() // tool-middleware chain, built once for this run

	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return Message{}, usageTotals{}, 0, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}

	msgs := []Message{}
	if sys := a.systemMessage(ctx); sys != "" {
		msgs = append(msgs, SystemText(sys))
	}
	msgs = append(msgs, seed...)
	done := map[string]bool{}           // tool-use IDs with a recorded result
	attempted := map[string]bool{}      // tool-use IDs we recorded an attempt marker for (started a side effect)
	attemptedAtMs := map[string]int64{} // tool-use ID -> attempt marker's Unix-millis timestamp
	decided := map[string]bool{}        // tool-use IDs with a recorded approval decision
	approvals := map[string]bool{}      // tool-use ID -> approve(true)/deny(false)
	modelSeq := 0
	var runUsage Usage // token usage across all of this run's model calls, replayed and live
	spendSeq := 0      // spend records ("@spend/<n>") already in the journal
	for _, r := range recs {
		// Discarded spend: on a model step, its turn's other requests; on a spend record, a
		// failed model call's requests (see recordSpend).
		if r.DiscardedUsage != nil {
			addUsage(&runUsage, *r.DiscardedUsage)
		}
		if strings.HasPrefix(r.Name, spendStepPrefix) {
			spendSeq++
		}
		switch r.Kind {
		case StepModel:
			modelSeq++
			if r.Usage != nil {
				addUsage(&runUsage, *r.Usage)
			}
			if r.Message != nil {
				msgs = append(msgs, *r.Message)
				fire(AssistantTurn{Message: *r.Message, Replayed: true})
			}
		case StepToolResult:
			done[r.ToolUseID] = true
			msgs = append(msgs, Message{Role: RoleTool, Parts: []Part{
				ToolResult{ToolUseID: r.ToolUseID, Result: r.Result, IsError: r.IsError},
			}})
			name, _ := toolNameFor(recs, r.ToolUseID)
			fire(ToolCompleted{ToolUseID: r.ToolUseID, Name: name, Result: r.Result, IsError: r.IsError})
		case StepAttempt:
			if !isToolAttempt(r) {
				continue // a Step's marker, not a call's
			}
			attempted[r.ToolUseID] = true
			attemptedAtMs[r.ToolUseID] = r.AttemptedAt
		case StepSagaFail:
			done[r.ToolUseID] = true // the failing step is durably resolved (no ResumeHalt)
		case StepApproval:
			if r.Approver != "" {
				continue // a per-approver m-of-n decision (ApproveAs); tallied by the quorum gate, not here
			}
			decided[r.ToolUseID] = true
			approvals[r.ToolUseID] = r.Approved
		}
	}

	// A finished run is final: return its recorded answer without asking the model for
	// another turn. Re-invoking a finished run is routine (a client retrying after a lost
	// response, a redelivered job, a sub-agent or session turn re-entered on resume), and a
	// fresh model turn could request tools again under NEW tool-use ids, which at-most-once
	// (keyed by tool-use id) would not recognize as repeats. The input is not consulted.
	if final, ok := completedAnswer(recs); ok {
		fire(Finished{Final: final})
		return final, usageTotals{}, 0, nil
	}

	// Resume safety gate: a tool call that we ATTEMPTED (recorded a start marker for) but has
	// no recorded result crashed mid-side-effect → unknown outcome → halt. A tool that was never
	// attempted never ran its side effect, so it's safe to run now (not a halt); one awaiting
	// approval re-surfaces as PendingApproval in the loop.
	//
	// The marker is the call's recorded safety: one is written only for a call that was not
	// retry-safe when it fired, in this version and every earlier one. So the halt goes by the
	// marker, not by the tool's safety now: a tool relabelled retry-safe since (a trusted MCP
	// server's new annotations, a code change), or no longer registered at all, still halts,
	// rather than run a side effect a second time.
	for id := range attempted {
		if done[id] {
			continue
		}
		name, ok := toolNameFor(recs, id)
		if !ok {
			continue
		}
		var attemptedAt time.Time
		if ms := attemptedAtMs[id]; ms != 0 {
			attemptedAt = time.UnixMilli(ms)
		}
		return Message{}, usageTotals{}, 0, &ResumeHalt{RunID: runID, RootRunID: rootRunID(ctx, runID), ToolUseID: id, ToolName: name, AttemptedAt: attemptedAt}
	}

	var totalUsage usageTotals // accumulated token usage across live model turns
	meter := &spendMeter{}     // usage of every model request this invocation sends
	var liveTurns int          // number of live (non-replayed) model calls this run

	for {
		// If the last turn is an assistant message with tool calls still pending (a
		// resumed journal), execute those; otherwise ask the model for the next turn.
		//
		// A replayed assistant turn with no tool calls is the run's final answer: the crash came
		// after it was recorded and before the completion marker. It is taken as the turn too, so
		// the run finishes with it (below) rather than ask the model for another turn, which could
		// answer differently or call tools under new tool-use ids. A live turn like it returns in
		// the same iteration, so only the first iteration of a resume sees one.
		var asst Message
		terminal := false // the latest turn's terminal-tool call succeeded, which ends the run
		if n := len(msgs); n > 0 && msgs[n-1].Role == RoleAssistant && (pending(msgs[n-1], done) || len(msgs[n-1].toolUses()) == 0) {
			asst = msgs[n-1]
		} else if last, ok := terminalCallDone(msgs, a.terminalTool); ok {
			asst, terminal = last, true
		} else {
			// Safety valve: cap model turns so a model that keeps calling tools can't loop
			// forever. modelSeq counts turns including replayed ones, so a resumed run that
			// already hit the cap stops immediately.
			// A cancelled run stops before asking for another turn, rather than relying on the
			// model adapter to notice the cancellation.
			if err := ctx.Err(); err != nil {
				return Message{}, totalUsage, liveTurns, err
			}
			if a.maxTurns > 0 && modelSeq >= a.maxTurns {
				return Message{}, totalUsage, liveTurns, fmt.Errorf("run %s: %w (%d turns)", runID, ErrMaxTurns, modelSeq)
			}
			if a.tokenBudget > 0 && runUsage.TotalTokens() >= a.tokenBudget {
				return Message{}, totalUsage, liveTurns, fmt.Errorf("run %s: %d tokens used, budget %d: %w", runID, runUsage.TotalTokens(), a.tokenBudget, ErrBudgetExceeded)
			}
			fire(TurnStarted{Seq: modelSeq})
			// Install the token sink so a live (non-replayed) model call forwards its
			// deltas as ModelEvents. On memoized replay store.Do skips the fn, so no
			// sink fires — an AssistantTurn{Replayed:true} was emitted during resume.
			genCtx := ctx
			if emit != nil {
				genCtx = withModelSink(ctx, turnSink(modelSeq, fire))
			}
			genCtx = withModelRun(genCtx, a.store, runID) // model middleware can journal a step of this run (WithRetrieval)
			var turnUsage Usage
			rec, err := a.store.Do(genCtx, runID, modelStep(modelSeq),
				func(ctx context.Context) (Record, error) {
					m, u, e := a.generate(ctx, Request{Messages: msgs, Tools: a.toolList(), Sampling: a.sampling, ResponseFormat: a.responseFormat, ToolChoice: a.toolChoice}, toolUseIDs(msgs), meter)
					if e != nil {
						return Record{}, e
					}
					turnUsage = u
					r := Record{Kind: StepModel, Message: &m, Usage: &u}
					// The turn recorded one response; every other request it sent was billed too.
					if d := discardedSpend(meter.take(), u); d != (Usage{}) {
						r.DiscardedUsage = &d
					}
					return r, nil
				})
			if err != nil {
				err = fmt.Errorf("generate (run %s): %w (%w)", runID, err, ErrModel)
				// The call failed for good, but its requests were billed: journal their spend so the
				// budget counts it on this and every later invocation of the run.
				if spent := meter.take(); spent != (Usage{}) {
					if serr := a.recordSpend(ctx, runID, spendSeq, spent); serr != nil {
						err = errors.Join(err, serr)
					}
				}
				return Message{}, totalUsage, liveTurns, err
			}
			addUsage(&totalUsage.answer, turnUsage)
			addUsage(&totalUsage.spend, turnUsage)
			if rec.Usage != nil {
				addUsage(&runUsage, *rec.Usage)
			}
			if rec.DiscardedUsage != nil {
				addUsage(&runUsage, *rec.DiscardedUsage)
				addUsage(&totalUsage.spend, *rec.DiscardedUsage)
			}
			liveTurns++
			asst = *rec.Message
			modelSeq++
			msgs = append(msgs, asst)
			fire(AssistantTurn{Message: asst, Replayed: false})
		}

		uses := asst.toolUses()
		if len(uses) == 0 || terminal {
			// Terminal: record a durable completion marker so a crash-recovery supervisor
			// can skip this run (see IsComplete / Recover). Appended only at the terminal,
			// so it never shifts an earlier record's index; at-most-once by name, so a
			// replay of a finished run does not add a second one.
			if _, err := a.store.Do(ctx, runID, runCompleteStep, func(context.Context) (Record, error) {
				return Record{Kind: StepValue}, nil
			}); err != nil {
				return Message{}, totalUsage, liveTurns, fmt.Errorf("mark complete (run %s): %w (%w)", runID, err, ErrStorage)
			}
			fire(Finished{Final: asst})
			return asst, totalUsage, liveTurns, nil // final answer
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
				return Message{}, totalUsage, liveTurns, fmt.Errorf("model called unknown tool %q: %w", tu.Name, ErrUnknownTool)
			}
			if safety := t.Safety(); safety.RequiresApproval || safety.Approval != nil {
				var approved bool
				if pol := safety.Approval; pol != nil {
					// m-of-n: the decision is the tally over the journaled per-approver records.
					tally, final, err := a.quorumTally(ctx, runID, tu, pol)
					if err != nil {
						return Message{}, totalUsage, liveTurns, err
					}
					if !final {
						evTally := tally // the event gets its own copy; PendingApproval keeps tally
						evTally.Pending = append([]string(nil), tally.Pending...)
						fire(ApprovalRequired{ToolUseID: tu.ID, Name: tu.Name, Args: tu.Args, Quorum: &evTally})
						return Message{}, totalUsage, liveTurns, &PendingApproval{RunID: runID, RootRunID: rootRunID(ctx, runID), ToolUseID: tu.ID, ToolName: tu.Name, Args: tu.Args, Quorum: &tally}
					}
					approved = tally.Passed()
				} else {
					if !decided[tu.ID] {
						fire(ApprovalRequired{ToolUseID: tu.ID, Name: tu.Name, Args: tu.Args})
						return Message{}, totalUsage, liveTurns, &PendingApproval{RunID: runID, RootRunID: rootRunID(ctx, runID), ToolUseID: tu.ID, ToolName: tu.Name, Args: tu.Args}
					}
					approved = approvals[tu.ID]
				}
				if !approved { // denied — record a denial and let the model react
					const denied = `"tool call denied by human"`
					if _, err := a.store.Do(ctx, runID, ToolResultStep(tu.ID), func(context.Context) (Record, error) {
						return Record{Kind: StepToolResult, ToolUseID: tu.ID, IsError: true, Result: json.RawMessage(denied)}, nil
					}); err != nil {
						return Message{}, totalUsage, liveTurns, err
					}
					done[tu.ID] = true
					results[i] = &Message{Role: RoleTool, Parts: []Part{ToolResult{ToolUseID: tu.ID, Result: json.RawMessage(denied), IsError: true}}}
					fire(ToolCompleted{ToolUseID: tu.ID, Name: tu.Name, Result: json.RawMessage(denied), IsError: true})
					continue
				}
			}
			toRun = append(toRun, call{idx: i, tu: tu, t: t})
		}

		// Execute the ready tools CONCURRENTLY (Go's strength; single-flight-safe). First
		// failure in saga mode cancels siblings via the errgroup context. A pause or halt
		// (Interrupt, Sleep, Await, approval, ResumeHalt) does not: it is held until every
		// sibling has finished and recorded its outcome, since a routine pause must not cut
		// off a side effect in flight and leave it with an unknown outcome.
		g, gctx := errgroup.WithContext(ctx)
		if a.maxConc > 0 {
			g.SetLimit(a.maxConc)
		}
		var (
			pauseMu  sync.Mutex
			pauseIdx = -1
			pauseErr error
		)
		for _, c := range toRun {
			g.Go(func() (err error) {
				defer func() {
					if err != nil && isPause(err) {
						pauseMu.Lock()
						if pauseIdx < 0 || c.idx < pauseIdx { // report the first call's pause
							pauseIdx, pauseErr = c.idx, err
						}
						pauseMu.Unlock()
						err = nil
					}
				}()
				sctx := withRunScope(gctx, SubRunID(runID, c.tu.ID)) // hierarchical sub-run ID
				sctx = withRunContext(sctx, a.store, runID)          // lets the tool call Interrupt
				if saga {
					sctx = withSaga(sctx)
				}
				// Attempt marker before a non-retriable side effect (crash-mid-write → halt),
				// written as an exclusive claim: if another driver of this run claimed the call
				// first (overlapping drivers, e.g. after a lease lapsed), it owns the side effect
				// and this driver halts rather than run it a second time.
				if !c.t.Safety().retriableOnResume() {
					won, got, err := ClaimAttempt(gctx, a.store, runID, toolAttemptStep(c.tu.ID),
						Record{Kind: StepAttempt, ToolUseID: c.tu.ID, AttemptedAt: time.Now().UnixMilli()})
					if err != nil {
						return err
					}
					if !won {
						var at time.Time
						if got.AttemptedAt != 0 {
							at = time.UnixMilli(got.AttemptedAt)
						}
						return &ResumeHalt{RunID: runID, RootRunID: rootRunID(ctx, runID), ToolUseID: c.tu.ID, ToolName: c.tu.Name, AttemptedAt: at}
					}
				}
				fire(ToolStarted{ToolUseID: c.tu.ID, Name: c.tu.Name, Args: c.tu.Args})
				var toolCallErr error
				// Journal the tool's OUTCOME under a non-cancellable context: the tool itself still
				// runs under sctx (a saga sibling's failure cancels it, as intended), but once it has
				// run, recording its result must not be cancelled by that sibling: otherwise a fired
				// side effect is left with no recorded outcome and resume would halt on it (or, worse,
				// re-fire it). The attempt marker above stays on gctx: if we are cancelled before it
				// commits, the tool has not started, so there is nothing to record.
				rec, err := a.store.Do(context.WithoutCancel(gctx), runID, ToolResultStep(c.tu.ID), func(context.Context) (Record, error) {
					res, callErr := toolH(sctx, c.tu)
					r := Record{Kind: StepToolResult, ToolUseID: c.tu.ID}
					if callErr != nil && sctx.Err() != nil {
						// The call was cancelled (the run was cancelled, or a sibling paused or
						// failed the group) before it could report back, so its outcome is
						// unknown, not failed: a request may already have reached a provider.
						// Record nothing. A retry-safe tool re-runs on resume; a non-retriable
						// one has its attempt marker and no result, so resume halts for
						// confirmation instead of the journal claiming a failure a retry would
						// repeat.
						return Record{}, callErr
					}
					if callErr != nil && errors.Is(callErr, ErrToolOutcomeUnknown) && !c.t.Safety().retriableOnResume() {
						// The tool cannot tell whether its side effect took place (its connection
						// dropped after the request went out). Recording a failure would tell the
						// model it did not, and invite it to ask again. Record nothing: the attempt
						// marker stays without a result, so a resume halts for confirmation.
						return Record{}, callErr
					}
					if callErr != nil {
						// A ResumeHalt or PendingApproval raised INSIDE this tool (a sub-agent
						// whose own tool halted or needs approval) is a control-flow signal for
						// the whole tree, not a tool failure: record nothing and propagate it up
						// unchanged, so the parent surfaces it and does NOT mark the run complete.
						// It is not gated on retry-safety: the pause lives in the sub-run's
						// journal, and re-driving this tool re-enters that sub-run rather than
						// re-firing a side effect here (see subagent.go).
						var subHalt *ResumeHalt
						var subApproval *PendingApproval
						if errors.As(callErr, &subHalt) || errors.As(callErr, &subApproval) {
							return Record{}, callErr
						}
						// An Interrupt or a durable Sleep pauses the run: record nothing and
						// propagate, so the tool re-runs and resolves on resume. Requires a
						// retry-safe tool (else its attempt marker would halt the resume instead).
						var intr *Interrupted
						var slp *Sleeping
						var awt *Awaiting
						if errors.As(callErr, &intr) || errors.As(callErr, &slp) || errors.As(callErr, &awt) {
							if !c.t.Safety().retriableOnResume() {
								return Record{}, fmt.Errorf("agent: tool %q paused (Interrupt, Sleep, or Await) but is not retry-safe (mark it ReadOnly or Idempotent): %w", c.tu.Name, ErrConfig)
							}
							return Record{}, callErr
						}
						if saga {
							toolCallErr = callErr
							return Record{Kind: StepSagaFail, ToolUseID: c.tu.ID, Result: mustJSON(toolErrorText(a.toolErrRedact, c.tu.Name, callErr))}, nil
						}
						// The model reads the error text as written, less any credential in a URL (and
						// whatever else the agent's tool-error redactor removes): see toolErrorText.
						r.IsError = true
						r.Result, _ = marshalJournal(toolErrorText(a.toolErrRedact, c.tu.Name, callErr))
					} else {
						r.Result = res
					}
					return r, nil
				})
				if saga && toolCallErr != nil {
					fire(ToolCompleted{ToolUseID: c.tu.ID, Name: c.tu.Name, Result: rec.Result, IsError: true})
					var journaled string
					_ = json.Unmarshal(rec.Result, &journaled)
					return &sagaTrip{toolName: c.tu.Name, toolUseID: c.tu.ID, cause: toolCallErr, journaled: journaled}
				}
				if err != nil {
					var subHalt *ResumeHalt
					var subApproval *PendingApproval
					var intr *Interrupted
					var slp *Sleeping
					var awt *Awaiting
					if errors.As(err, &subHalt) || errors.As(err, &subApproval) ||
						errors.As(err, &intr) || errors.As(err, &slp) || errors.As(err, &awt) {
						return err // propagate the pause / sub-tree halt unwrapped
					}
					if sctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
						return err // a cancellation, not a tool fault: surface it as one
					}
					return fmt.Errorf("tool %q: %w (%w)", c.tu.Name, err, ErrTool)
				}
				results[c.idx] = &Message{Role: RoleTool, Parts: []Part{ToolResult{ToolUseID: c.tu.ID, Result: rec.Result, IsError: rec.IsError}}}
				fire(ToolCompleted{ToolUseID: c.tu.ID, Name: c.tu.Name, Result: rec.Result, IsError: rec.IsError})
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			var trip *sagaTrip
			if errors.As(err, &trip) {
				return Message{}, totalUsage, liveTurns, trip // RunSaga catches → compensates
			}
			return Message{}, totalUsage, liveTurns, err
		}
		if pauseErr != nil {
			return Message{}, totalUsage, liveTurns, pauseErr
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

// quorumTally evaluates the m-of-n gate for tu. It re-reads the run's journal (the
// authoritative source of the decisions ApproveAs recorded) and counts it with
// TallyApprovals, the same rule offline verification runs. final reports whether the gate
// has reached a terminal outcome: passed (Need approvals) or unreachable (too few approvers
// remain who have not validly denied). A non-final tally means the run must stay paused.
//
// Only a terminal tally is journaled, as a StepValue named ApprovalTallyStep(tu.ID), before
// the tool runs. Steps are append-once by name, so writing a still-pending count would freeze
// a stale tally; recording only the terminal outcome keeps one final, provable record per
// resolved gate, carrying the policy it enforced and every decision record it read. Once that
// record exists it is authoritative: a replay reuses it rather than recounting, so the outcome
// cannot drift if keys or policy change later.
func (a *Agent) quorumTally(ctx context.Context, runID string, tu ToolUse, pol *ApprovalPolicy) (ApprovalTally, bool, error) {
	if err := pol.Validate(); err != nil {
		return ApprovalTally{}, false, fmt.Errorf("agent: tool %q: %w", tu.Name, err)
	}
	if a.approverVerifiers == nil {
		return ApprovalTally{}, false, fmt.Errorf("agent: tool %q has an m-of-n Approval policy but no approver verifiers are configured (see WithApproverVerifiers): %w", tu.Name, ErrConfig)
	}
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return ApprovalTally{}, false, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	name := ApprovalTallyStep(tu.ID)
	for _, r := range recs {
		if r.Name == name && r.Kind == StepValue {
			var t ApprovalTally
			if err := json.Unmarshal(r.Result, &t); err != nil {
				return ApprovalTally{}, false, fmt.Errorf("decode %s (run %s): %w (%w)", name, runID, err, ErrStorage)
			}
			return t, true, nil
		}
	}
	subject := ApprovalSubject{RunID: runID, ToolUseID: tu.ID, ToolName: tu.Name, Args: tu.Args}
	tally, _ := TallyApprovals(recs, subject, *pol, a.approverVerifiers)
	if !tally.Passed() && !tally.Unreachable() {
		return tally, false, nil
	}
	tally, err = step(ctx, a.store, runID, name, func(context.Context) (ApprovalTally, error) { return tally, nil }, StepSafety(Safety{ReadOnly: true}))
	if err != nil {
		return ApprovalTally{}, false, fmt.Errorf("record %s (run %s): %w (%w)", name, runID, err, ErrStorage)
	}
	return tally, true, nil
}

// toolUseIDs returns the IDs of every tool call in msgs.
func toolUseIDs(msgs []Message) map[string]bool {
	ids := map[string]bool{}
	for _, m := range msgs {
		for _, tu := range m.toolUses() {
			ids[tu.ID] = true
		}
	}
	return ids
}

// checkToolUseIDs rejects a live model turn whose tool calls cannot each be keyed by their own
// ID: a call with no ID, an ID already used earlier in the conversation (used), an ID that
// appears twice in the turn, or an ID that is not valid UTF-8 (the journal's JSON cannot hold
// it, so the replayed ID would differ from the live one). The loop records each call's result
// and journal step under its ID, so a reused ID would pass a new call off as one already done.
// Any other ID is safe: the keys and sub-run ID derived from it encode it (see encodeID). Only
// live turns are checked; a turn replayed from the journal is taken as recorded.
func checkToolUseIDs(m Message, used map[string]bool) error {
	seen := map[string]bool{}
	for _, tu := range m.toolUses() {
		switch {
		case tu.ID == "":
			return fmt.Errorf("model called tool %q with no tool-use id: %w", tu.Name, ErrToolUseIDReused)
		case !utf8.ValidString(tu.ID):
			return fmt.Errorf("model called tool %q with tool-use id %q, which is not valid UTF-8: %w", tu.Name, tu.ID, ErrToolUseIDReused)
		case used[tu.ID]:
			return fmt.Errorf("model called tool %q with tool-use id %q from an earlier turn: %w", tu.Name, tu.ID, ErrToolUseIDReused)
		case seen[tu.ID]:
			return fmt.Errorf("model called tool %q with tool-use id %q twice in one turn: %w", tu.Name, tu.ID, ErrToolUseIDReused)
		}
		seen[tu.ID] = true
	}
	return nil
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
	runScopeKey      ctxKey = 0
	sagaKey          ctxKey = 1
	modelSinkKey     ctxKey = 2
	runContextKey    ctxKey = 3
	onceScopeKey     ctxKey = 4
	modelHooksKey    ctxKey = 5 // []ModelCallHook; present only in an agent model call's context
	modelOverrideKey ctxKey = 6 // Model set by WithModel
)

// runCtx carries the store + runID into a tool's context so Interrupt can journal and
// read its resume value without the tool holding those handles.
type runCtx struct {
	store Durable
	runID string
	root  string // the top-level run; a sub-agent's runs inherit it
}

func withRunContext(ctx context.Context, store Durable, runID string) context.Context {
	return context.WithValue(ctx, runContextKey, runCtx{store: store, runID: runID, root: rootRunID(ctx, runID)})
}

// rootRunID is the top-level run for a run with this ID reached through ctx: the root recorded
// by an enclosing run (a sub-agent is called from its parent's tool context), or runID itself.
func rootRunID(ctx context.Context, runID string) string {
	if rc, ok := ctx.Value(runContextKey).(runCtx); ok && rc.root != "" {
		return rc.root
	}
	return runID
}

func runContext(ctx context.Context) (Durable, string, bool) {
	rc, ok := ctx.Value(runContextKey).(runCtx)
	return rc.store, rc.runID, ok
}

func withModelSink(ctx context.Context, sink func(Event)) context.Context {
	return context.WithValue(ctx, modelSinkKey, sink)
}

// attemptStart is sent to a run's model sink when a model attempt starts delivering a
// response: the model handler starting a call, or EmitMessage. The sink does not forward it; it
// marks where the previous attempt, if any, ended.
type attemptStart struct{}

func (attemptStart) event() {}

// turnSink returns the model sink for turn seq. It forwards each model Event as a ModelEvent.
// On an attemptStart that follows an attempt which streamed deltas, it emits TurnRestarted,
// since those deltas are not part of the response the turn records: a middleware such as Retry
// is calling the model again, or delivering a response from elsewhere.
func turnSink(seq int, fire func(AgentEvent)) func(Event) {
	var (
		mu       sync.Mutex
		streamed bool // the current attempt has forwarded a delta
	)
	return func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := ev.(attemptStart); ok {
			if streamed {
				streamed = false
				fire(TurnRestarted{Seq: seq})
			}
			return
		}
		streamed = true
		fire(ModelEvent{Event: ev})
	}
}

// modelSink returns the live token sink installed by Agent.Stream, or nil for a
// blocking Run. The base model handler forwards each stream Event to it.
func modelSink(ctx context.Context) func(Event) {
	s, _ := ctx.Value(modelSinkKey).(func(Event))
	return s
}

// DetachModelSink returns ctx with the run's token sink removed, and that sink (nil when the run
// is not streaming). Agent.Stream installs the sink so a model's token deltas reach the caller as
// they arrive. Middleware that sends one model call to several targets must detach it before
// calling them: otherwise a target that loses streams text the run never records, and may keep
// sending after the run has ended. Once it has chosen the response to return, the middleware can
// deliver it to the caller with EmitMessage.
func DetachModelSink(ctx context.Context) (context.Context, func(Event)) {
	return context.WithValue(ctx, modelSinkKey, (func(Event))(nil)), modelSink(ctx)
}

// EmitMessage delivers a model call's response, m and its usage u, to sink as the events a
// model would have streamed for it, ending with a Finish that carries u. It does nothing when
// sink is nil. See DetachModelSink.
func EmitMessage(sink func(Event), m Message, u Usage) {
	if sink == nil {
		return
	}
	sink(attemptStart{}) // m replaces whatever an earlier attempt streamed
	for _, e := range emitsFor(m, u) {
		sink(e.Event)
	}
}

func withRunScope(ctx context.Context, scope string) context.Context {
	return withOnceScope(context.WithValue(ctx, runScopeKey, scope), scope)
}

// RunScope returns the hierarchical run scope for the current tool execution
// (SubRunID: the parent run ID, '>', the encoded tool-use ID). Sub-agents use it to derive a stable, resumable sub-run ID.
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
// terminalCallDone reports whether the latest assistant turn in msgs called the terminal tool
// named tool and that call succeeded (its result is recorded and is not an error), and returns
// that turn. The result is read from msgs, so a resumed run that crashed after recording the
// call ends the same way a live one does. With no terminal tool (tool is ""), no call matches.
func terminalCallDone(msgs []Message, tool string) (Message, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != RoleAssistant {
			continue
		}
		calls := map[string]bool{}
		for _, tu := range msgs[i].toolUses() {
			if tu.Name == tool {
				calls[tu.ID] = true
			}
		}
		for _, m := range msgs[i+1:] {
			for _, p := range m.Parts {
				if tr, ok := p.(ToolResult); ok && calls[tr.ToolUseID] && !tr.IsError {
					return msgs[i], true
				}
			}
		}
		return Message{}, false
	}
	return Message{}, false
}

func pending(m Message, done map[string]bool) bool {
	for _, tu := range m.toolUses() {
		if !done[tu.ID] {
			return true
		}
	}
	return false
}
