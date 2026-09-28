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
// live in the middleware/ package (Retry, TokenBudget, ...).
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
	store        Durable
	mw           []Middleware
	toolMW       []ToolMiddleware
	sampling     Sampling // generation controls applied to every model call
	maxConc      int      // max concurrent tool calls per turn; 0 = unbounded (default)
	maxTurns     int      // max model turns per run; 0 = unbounded (default)
	systemPrompt string   // optional static system message prepended to every model call
	// systemPromptFn, if set, computes the system message per run (dynamic context:
	// current time, tenant, retrieved state). Takes precedence over systemPrompt.
	systemPromptFn func(context.Context) string
	responseFormat *ResponseFormat // native structured-output constraint (see RunTypedNative)
	toolChoice     *ToolChoice     // tool-choice control applied to every model call (see WithToolChoice)
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
func New(model Model, store Durable, tools ...Tool) *Agent {
	if model == nil {
		panic("agent: New requires a non-nil Model")
	}
	if store == nil {
		panic("agent: New requires a non-nil Durable store")
	}
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

// WithMaxTurns caps the number of model turns a single run may take, so a model that
// keeps calling tools can't loop forever. n <= 0 means unbounded (the default). When the
// cap is reached the run returns ErrMaxTurns (category ErrBudget). Returns the agent for
// chaining. The cap is per run (per Session.Send turn), not per session.
func (a *Agent) WithMaxTurns(n int) *Agent {
	a.maxTurns = n
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
		// When a token sink is installed (Agent.Stream), stream the model call and
		// forward deltas as they arrive while still assembling the message for the
		// journal; otherwise take the plain blocking drain. Middleware wraps this
		// either way and sees the assembled message + usage — streaming stays below it.
		if sink := modelSink(ctx); sink != nil {
			s, err := a.model.Stream(ctx, req)
			if err != nil {
				return Message{}, Usage{}, err
			}
			return s.drain(sink)
		}
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
func (a *Agent) run(ctx context.Context, runID string, seed []Message, saga bool, emit func(AgentEvent)) (Message, Usage, int, error) {
	if runID == "" {
		// An empty runID would key every run to the same journal, silently cross-contaminating
		// their memoized steps. Reject it rather than corrupt the log.
		return Message{}, Usage{}, 0, fmt.Errorf("run: empty runID: %w", ErrConfig)
	}
	fire := func(e AgentEvent) {
		if emit != nil {
			emit(e)
		}
	}
	toolH := a.toolHandler() // tool-middleware chain, built once for this run

	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return Message{}, Usage{}, 0, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}

	msgs := []Message{}
	if sys := a.systemMessage(ctx); sys != "" {
		msgs = append(msgs, SystemText(sys))
	}
	msgs = append(msgs, seed...)
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
			return Message{}, Usage{}, 0, &ResumeHalt{RunID: runID, ToolUseID: id, ToolName: name}
		}
	}

	var totalUsage Usage // accumulated token usage across live model turns
	var liveTurns int    // number of live (non-replayed) model calls this run

	for {
		// If the last turn is an assistant message with tool calls still pending (a
		// resumed journal), execute those; otherwise ask the model for the next turn.
		var asst Message
		if n := len(msgs); n > 0 && msgs[n-1].Role == RoleAssistant && pending(msgs[n-1], done) {
			asst = msgs[n-1]
		} else {
			// Safety valve: cap model turns so a model that keeps calling tools can't loop
			// forever. modelSeq counts turns including replayed ones, so a resumed run that
			// already hit the cap stops immediately.
			if a.maxTurns > 0 && modelSeq >= a.maxTurns {
				return Message{}, totalUsage, liveTurns, fmt.Errorf("run %s: %w (%d turns)", runID, ErrMaxTurns, modelSeq)
			}
			fire(TurnStarted{Seq: modelSeq})
			// Install the token sink so a live (non-replayed) model call forwards its
			// deltas as ModelEvents. On memoized replay store.Do skips the fn, so no
			// sink fires — an AssistantTurn{Replayed:true} was emitted during resume.
			genCtx := ctx
			if emit != nil {
				genCtx = withModelSink(ctx, func(ev Event) { fire(ModelEvent{Event: ev}) })
			}
			var turnUsage Usage
			rec, err := a.store.Do(genCtx, runID, fmt.Sprintf("@llm/%d", modelSeq),
				func(ctx context.Context) (Record, error) {
					m, u, e := a.generate(ctx, Request{Messages: msgs, Tools: a.toolList(), Sampling: a.sampling, ResponseFormat: a.responseFormat, ToolChoice: a.toolChoice})
					if e != nil {
						return Record{}, e
					}
					turnUsage = u
					return Record{Kind: StepModel, Message: &m}, nil
				})
			if err != nil {
				return Message{}, totalUsage, liveTurns, fmt.Errorf("generate (run %s): %w (%w)", runID, err, ErrModel)
			}
			addUsage(&totalUsage, turnUsage)
			liveTurns++
			asst = *rec.Message
			modelSeq++
			msgs = append(msgs, asst)
			fire(AssistantTurn{Message: asst, Replayed: false})
		}

		uses := asst.toolUses()
		if len(uses) == 0 {
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
			if t.Safety().RequiresApproval {
				if !decided[tu.ID] {
					fire(ApprovalRequired{ToolUseID: tu.ID, Name: tu.Name, Args: tu.Args})
					return Message{}, totalUsage, liveTurns, &PendingApproval{RunID: runID, ToolUseID: tu.ID, ToolName: tu.Name, Args: tu.Args}
				}
				if !approvals[tu.ID] { // denied — record a denial and let the model react
					const denied = `"tool call denied by human"`
					if _, err := a.store.Do(ctx, runID, tu.ID, func(context.Context) (Record, error) {
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
		// failure in saga mode cancels siblings via the errgroup context.
		g, gctx := errgroup.WithContext(ctx)
		if a.maxConc > 0 {
			g.SetLimit(a.maxConc)
		}
		for _, c := range toRun {
			g.Go(func() error {
				sctx := withRunScope(gctx, runID+"/"+c.tu.ID) // hierarchical sub-run ID
				sctx = withRunContext(sctx, a.store, runID)   // lets the tool call Interrupt
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
				fire(ToolStarted{ToolUseID: c.tu.ID, Name: c.tu.Name, Args: c.tu.Args})
				var toolCallErr error
				// Journal the tool's OUTCOME under a non-cancellable context: the tool itself still
				// runs under sctx (a saga sibling's failure cancels it, as intended), but once it has
				// run, recording its result must not be cancelled by that sibling: otherwise a fired
				// side effect is left with no recorded outcome and resume would halt on it (or, worse,
				// re-fire it). The attempt marker above stays on gctx: if we are cancelled before it
				// commits, the tool has not started, so there is nothing to record.
				rec, err := a.store.Do(context.WithoutCancel(gctx), runID, c.tu.ID, func(context.Context) (Record, error) {
					res, callErr := toolH(sctx, c.tu)
					r := Record{Kind: StepToolResult, ToolUseID: c.tu.ID}
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
					fire(ToolCompleted{ToolUseID: c.tu.ID, Name: c.tu.Name, Result: rec.Result, IsError: true})
					return &sagaTrip{toolName: c.tu.Name, toolUseID: c.tu.ID, cause: toolCallErr}
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
	runScopeKey   ctxKey = 0
	sagaKey       ctxKey = 1
	modelSinkKey  ctxKey = 2
	runContextKey ctxKey = 3
)

// runCtx carries the store + runID into a tool's context so Interrupt can journal and
// read its resume value without the tool holding those handles.
type runCtx struct {
	store Durable
	runID string
}

func withRunContext(ctx context.Context, store Durable, runID string) context.Context {
	return context.WithValue(ctx, runContextKey, runCtx{store: store, runID: runID})
}

func runContext(ctx context.Context) (Durable, string, bool) {
	rc, ok := ctx.Value(runContextKey).(runCtx)
	return rc.store, rc.runID, ok
}

func withModelSink(ctx context.Context, sink func(Event)) context.Context {
	return context.WithValue(ctx, modelSinkKey, sink)
}

// modelSink returns the live token sink installed by Agent.Stream, or nil for a
// blocking Run. The base model handler forwards each stream Event to it.
func modelSink(ctx context.Context) func(Event) {
	s, _ := ctx.Value(modelSinkKey).(func(Event))
	return s
}

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
