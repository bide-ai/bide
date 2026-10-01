package agent

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"
)

// ===========================================================================
// Option scopes
// ===========================================================================
//
// Every option is an interface with an unexported method, so only this package defines options,
// and an option's apply method returns an error, so a bad value fails where the option is used
// (Build, With, a run, Step, and so on) rather than later. A setting that applies at several
// scopes is one constructor whose result type is a combination of those scopes' interfaces (see
// AgentRunOption, ConcurrencyOption, ClockOption, SafetyOption and LeaseControl). Each constructor
// returns its narrowest type, so passing an option where it does not apply is a compile error.
// A later minor release may widen a constructor's result to a larger combination; that change is
// additive.

// Option configures an Agent: Build applies options to a new agent, and With to a copy of one.
// Build an []Option to choose options conditionally; an AgentRunOption fits in one as well.
type Option interface {
	applyAgent(*agentConfig) error
}

// RunOption configures one run. It is the scope of the settings a caller may choose per run,
// which take precedence over the agent's (see Build). The run entry points that take RunOptions
// arrive with the Run API; until then a RunOption value is built and type-checked, and its
// settings are given to the agent with Build or With.
type RunOption interface {
	applyRun(*runConfig) error
}

// ParallelOption configures Parallel.
type ParallelOption interface {
	applyParallel(*parallelConfig) error
}

// StepOption configures Step.
type StepOption interface {
	applyStep(*stepConfig) error
}

// ResolveOption configures ResolveHaltRef (and its wrappers ResolveHalt and ResolveStepHalt).
type ResolveOption interface {
	applyResolve(*resolveConfig) error
}

// LeaseOption configures Lease.
type LeaseOption interface {
	applyLease(*recoverConfig) error
}

// RecoverOption configures Recover. Lease options apply only when the store implements Leaser.
type RecoverOption interface {
	applyRecover(*recoverConfig) error
}

// RecoverLoopOption configures RecoverLoop. Lease options apply only when the store implements
// Leaser.
type RecoverLoopOption interface {
	applyRecoverLoop(*recoverConfig) error
}

// ===========================================================================
// Combination types: the closed list
// ===========================================================================

// AgentRunOption is a setting that applies to an agent (Option) and to one run (RunOption):
// WithMaxTurns, WithTokenBudget, WithSystemPrompt, WithSampling, WithToolChoice, WithWaker and
// WithIdentity.
type AgentRunOption interface {
	Option
	RunOption
}

// ConcurrencyOption is a concurrency cap that applies to an agent's tool calls (Option), to one
// run's (RunOption), and to Parallel's tasks (ParallelOption): WithMaxConcurrency.
type ConcurrencyOption interface {
	Option
	RunOption
	ParallelOption
}

// ClockOption is a clock that applies to an agent (Option), to one run (RunOption), and to a halt
// resolution (ResolveOption): WithClock.
type ClockOption interface {
	Option
	RunOption
	ResolveOption
}

// SafetyOption is a Safety that applies to a tool (ToolOption) and to a step (StepOption):
// WithSafety.
type SafetyOption interface {
	ToolOption
	StepOption
}

// LeaseControl is a lease setting that applies to Lease (LeaseOption), Recover (RecoverOption)
// and RecoverLoop (RecoverLoopOption): WithLeaseHolder and WithLeaseTTL.
type LeaseControl interface {
	LeaseOption
	RecoverOption
	RecoverLoopOption
}

// ===========================================================================
// Configs
// ===========================================================================

// agentConfig is what Option values write: the agent being built (a new one in Build, a deep
// copy in With). Settings are written straight into it, and tools are registered as they come,
// so a duplicate name is refused at the option that repeats it. The checks that depend on more
// than one option (an m-of-n approval needs approver verifiers; a forced tool must be one of the
// agent's) run once every option has been applied (see agentConfig.finish).
type agentConfig struct {
	a *Agent
}

// runConfig is what RunOption values write: the settings a caller chose for one run. A nil
// pointer is a setting the caller left alone, so the agent's value applies (see Build).
type runConfig struct {
	maxTurns, tokenBudget, maxConc *int
	systemPrompt                   *string
	sampling                       *Sampling
	toolChoice                     *ToolChoice
	waker                          Waker
	identity                       *Identity
	clock                          func() time.Time
}

// parallelConfig is what ParallelOption values write.
type parallelConfig struct {
	maxConc int // 0 = one goroutine per task
}

// applyOptions applies opts to cfg in order, through apply, and returns the first error. A nil
// option is ErrConfig, named by its position.
func applyOptions[O comparable, C any](what string, cfg *C, opts []O, apply func(O, *C) error) error {
	var zero O
	for i, o := range opts {
		if o == zero {
			return fmt.Errorf("agent: %s option %d is nil: %w", what, i, ErrConfig)
		}
		if err := apply(o, cfg); err != nil {
			return fmt.Errorf("agent: %s: %w", what, err)
		}
	}
	return nil
}

// ===========================================================================
// Build and With
// ===========================================================================

// Build constructs an Agent over model and the journal j, configured by opts. Every
// configuration problem is an error wrapping ErrConfig, returned here rather than at the first
// run:
//
//   - a nil model, journal, option, tool, middleware or function argument;
//   - two tools with one name, or a tool named "final_answer", which RunTyped reserves;
//   - a tool whose input schema is not a JSON object, or that wraps another in a way the agent
//     cannot honor (see SubAgent);
//   - an invalid approval policy, an m-of-n policy with no WithApproverVerifiers, or one two of
//     whose approvers resolve to one signing key (see ApprovalPolicy.ValidateKeys);
//   - a negative limit (WithMaxTurns, WithTokenBudget, WithMaxConcurrency), a WithRetrieval k
//     below 1, and a WithToolChoice that names no tool of the agent's or has an unknown mode.
//
// Options apply in order, and for any setting the last one given wins. WithSystemPrompt and
// WithSystemPromptFunc fill one slot: the later of the two wins. Precedence for a setting is: a
// value the run was given, then the agent's, then the default. Today a run is given its
// identity, Waker and clock through its context (ContextWithIdentity, ContextWithWaker,
// ContextWithClock, which a sub-agent's run inherits from its parent's); the agent's WithIdentity,
// WithWaker and WithClock apply to a run whose context carries none.
//
// The agent is immutable once built: With returns a configured copy and leaves the agent alone.
// (The builder methods, such as Agent.WithMaxTurns, still change the agent in place; they are
// transitional and are removed by the 1.0 rewrite.)
//
// Deprecated: transitional; renamed by the 1.0 rewrite. Build becomes New(model, j, opts...),
// and the current New is removed.
func Build(model Model, j *Journal, opts ...Option) (*Agent, error) {
	if isNil(model) {
		return nil, fmt.Errorf("agent: Build: nil model: %w", ErrConfig)
	}
	if j == nil {
		return nil, fmt.Errorf("agent: Build: nil journal: %w", ErrConfig)
	}
	a := newAgent(model, j)
	if err := configure(a, opts); err != nil {
		return nil, err
	}
	return a, nil
}

// With returns a copy of a configured by opts, applied over a's configuration as Build applies
// them: a setting given here replaces a's, and tools, middleware and retrievals given here are
// added after a's. The copy shares nothing mutable with a (its tool set, middleware lists,
// sampling and settings are its own), so the two may be used, and configured further, from
// different goroutines. a is never changed. A configuration problem is an error wrapping
// ErrConfig, as for Build, including a tool whose name a already uses.
func (a *Agent) With(opts ...Option) (*Agent, error) {
	if err := a.checkTools(); err != nil {
		return nil, err // an agent New was given a bad tool set: nothing built on it can run
	}
	c := a.clone()
	if err := configure(c, opts); err != nil {
		return nil, err
	}
	return c, nil
}

// Journal returns the journal the agent writes through: the one Build was given, or, for an
// agent made by New, the journal of its store (nil for a Durable with none, such as a test
// wrapper that intercepts Do).
func (a *Agent) Journal() *Journal { return journalOf(a.store) }

// newAgent returns an agent over model and store with no tools and no options.
func newAgent(model Model, store Durable) *Agent {
	return &Agent{model: model, store: store, tools: map[string]Tool{}, specs: map[string]*ToolSpec{}}
}

// configure applies opts to a and runs the checks that span options. a is either new (Build) or
// a's own deep copy (With), so a failure leaves nothing the caller holds half-configured.
func configure(a *Agent, opts []Option) error {
	c := &agentConfig{a: a}
	if err := applyOptions("Build", c, opts, Option.applyAgent); err != nil {
		return err
	}
	return c.finish()
}

// finish runs the checks that depend on more than one option, and rebuilds the sorted spec list
// the model is sent.
func (c *agentConfig) finish() error {
	a := c.a
	if err := a.checkTools(); err != nil {
		return err
	}
	if err := checkForcedTool(a.toolChoice, a.specs); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(a.specs)) {
		s := a.specs[name]
		if s.Approval == nil || s.Approval.single() {
			continue
		}
		if a.approverVerifiers == nil {
			return fmt.Errorf("agent: tool %q has an m-of-n approval policy but the agent has no WithApproverVerifiers: %w", name, ErrConfig)
		}
		if err := s.Approval.ValidateKeys(a.approverVerifiers); err != nil {
			return fmt.Errorf("agent: tool %q: %w", name, err)
		}
	}
	a.sortSpecs()
	return nil
}

// checkToolChoiceValue refuses a tool choice with an unknown mode, mode "tool" with no tool
// name, or a tool name with a mode that forces no tool.
func checkToolChoiceValue(tc ToolChoice) error {
	switch tc.Mode {
	case "", "auto", "none", "required":
		if tc.Name != "" {
			return fmt.Errorf("WithToolChoice: mode %q names tool %q, which only mode \"tool\" forces: %w", tc.Mode, tc.Name, ErrConfig)
		}
	case "tool":
		if tc.Name == "" {
			return fmt.Errorf("WithToolChoice: mode \"tool\" needs a tool name: %w", ErrConfig)
		}
	default:
		return fmt.Errorf("WithToolChoice: mode %q is not one of auto, none, required, tool: %w", tc.Mode, ErrConfig)
	}
	return nil
}

// checkForcedTool refuses a tool choice that forces a tool the agent does not have.
func checkForcedTool(tc *ToolChoice, specs map[string]*ToolSpec) error {
	if tc == nil || tc.Mode != "tool" {
		return nil
	}
	if _, ok := specs[tc.Name]; !ok {
		return fmt.Errorf("agent: tool choice forces tool %q, which the agent does not have: %w", tc.Name, ErrConfig)
	}
	return nil
}

// ===========================================================================
// Agent options
// ===========================================================================

// agentOption is an Option that only applies to an agent.
type agentOption func(*agentConfig) error

func (f agentOption) applyAgent(c *agentConfig) error { return f(c) }

// WithTools registers tools with the agent. Each tool's spec is read once, here, and every
// decision about its calls is made from that copy. A nil tool, two tools with one name (including
// a name the agent already has), the name "final_answer", a non-object input schema, an invalid
// approval policy, and a wrapper the agent cannot honor are ErrConfig.
func WithTools(tools ...Tool) Option {
	return agentOption(func(c *agentConfig) error {
		for _, t := range tools {
			if err := c.a.addTool(t, true); err != nil {
				return err
			}
		}
		return nil
	})
}

// WithMiddleware appends model-call middleware, after any the agent has; the first one added is
// the outermost. A nil middleware is ErrConfig.
func WithMiddleware(mw ...Middleware) Option {
	return agentOption(func(c *agentConfig) error {
		for i, m := range mw {
			if m == nil {
				return fmt.Errorf("WithMiddleware: middleware %d is nil: %w", i, ErrConfig)
			}
		}
		c.a.mw = append(c.a.mw, mw...)
		return nil
	})
}

// WithToolMiddleware appends tool-call middleware, after any the agent has; the first one added
// is the outermost (see ToolMiddleware). A nil middleware is ErrConfig.
func WithToolMiddleware(mw ...ToolMiddleware) Option {
	return agentOption(func(c *agentConfig) error {
		for i, m := range mw {
			if m == nil {
				return fmt.Errorf("WithToolMiddleware: middleware %d is nil: %w", i, ErrConfig)
			}
		}
		c.a.toolMW = append(c.a.toolMW, mw...)
		return nil
	})
}

// WithApproverVerifiers sets how the m-of-n approval gate resolves an approver id to the verifier
// of its decision signatures. An agent with any tool whose ToolSpec.Approval is an m-of-n policy
// needs it: Build and With refuse such an agent without one, and refuse a policy two of whose
// approvers it resolves to one signing key (ApprovalPolicy.ValidateKeys). The gate checks again on
// every evaluation, since fn is a function. A nil fn is ErrConfig.
func WithApproverVerifiers(fn ApproverVerifierFor) Option {
	return agentOption(func(c *agentConfig) error {
		if fn == nil {
			return fmt.Errorf("WithApproverVerifiers: nil resolver: %w", ErrConfig)
		}
		c.a.approverVerifiers = fn
		return nil
	})
}

// WithToolErrorRedactor sets the text recorded for a tool call that fails (see
// Agent.WithToolErrorRedactor for what it covers). A nil fn is ErrConfig.
func WithToolErrorRedactor(fn func(tool string, err error) string) Option {
	return agentOption(func(c *agentConfig) error {
		if fn == nil {
			return fmt.Errorf("WithToolErrorRedactor: nil function: %w", ErrConfig)
		}
		c.a.toolErrRedact = fn
		return nil
	})
}

// WithSystemPromptFunc sets a system message computed for each drive of a run (each Run, Stream
// or resume), from the run's context and its RunInfo, so it can carry dynamic context (the date,
// the tenant, state read from elsewhere). It fills the slot WithSystemPrompt fills: the later of
// the two wins. Its result is not journaled: a run resumed later is sent what fn returns then.
// Context that the run's later turns must see unchanged belongs in the input, which is journaled
// (see RunStart), or in a tool result. An error from fn fails the drive before the model is
// called, wrapped with the run's ID; nothing is recorded for it. A nil fn is ErrConfig.
func WithSystemPromptFunc(fn func(ctx context.Context, run RunInfo) (string, error)) Option {
	return agentOption(func(c *agentConfig) error {
		if fn == nil {
			return fmt.Errorf("WithSystemPromptFunc: nil function: %w", ErrConfig)
		}
		c.a.systemPrompt, c.a.systemPromptFn = "", fn
		return nil
	})
}

// WithRetrieval adds classic retrieval-augmented generation: for each run, the agent retrieves
// the top-k documents for the run's user message, as a journaled engine step, and adds them to
// every model request of the run as a user message placed just before that user message (after
// the system prompt and any earlier turns). Every model middleware sees the request with the
// documents in it.
//
// The step is read-only, so a crash before it is recorded retrieves again; once recorded, every
// later model call of the run (after a crash and resume, or from a second driver) is given the
// documents it recorded, and the journal holds what the model was shown: retrieved text is
// durable content, stored like a tool result. The documents are data the operator does not
// control, so they are not given system authority, and each is written as one line of JSON, so
// no document can forge another entry.
//
// A retrieval error fails the model call, and nothing is recorded, so the next attempt retrieves
// again: have the Retriever return (nil, nil) to degrade to no context instead. A Retriever that
// returns more than k documents is cut to its first k. Several WithRetrieval options retrieve in
// the order given, and their blocks appear in that order. A nil Retriever or a k below 1 is
// ErrConfig.
func WithRetrieval(r Retriever, k int) Option {
	return agentOption(func(c *agentConfig) error {
		if isNil(r) {
			return fmt.Errorf("WithRetrieval: nil Retriever: %w", ErrConfig)
		}
		if k < 1 {
			return fmt.Errorf("WithRetrieval: k must be at least 1, got %d: %w", k, ErrConfig)
		}
		c.a.retrievals = append(c.a.retrievals, retrievalLayer{r: r, k: k})
		return nil
	})
}

// WithOptions groups options into one, applied in order where it appears.
func WithOptions(opts ...Option) Option {
	return agentOption(func(c *agentConfig) error {
		return applyOptions("WithOptions", c, opts, Option.applyAgent)
	})
}

// ===========================================================================
// Agent and run options (AgentRunOption)
// ===========================================================================

// agentRunOption implements AgentRunOption: check validates the value once for both scopes,
// and agent and run write it.
type agentRunOption struct {
	check func() error
	agent func(*Agent)
	run   func(*runConfig)
}

func (o agentRunOption) applyAgent(c *agentConfig) error {
	if err := o.check(); err != nil {
		return err
	}
	o.agent(c.a)
	return nil
}

func (o agentRunOption) applyRun(c *runConfig) error {
	if err := o.check(); err != nil {
		return err
	}
	o.run(c)
	return nil
}

func noCheck() error { return nil }

// nonNegative returns a check that refuses a negative n for option what.
func nonNegative(what string, n int) func() error {
	return func() error {
		if n < 0 {
			return fmt.Errorf("%s(%d): a limit cannot be negative (0 means unbounded): %w", what, n, ErrConfig)
		}
		return nil
	}
}

// WithMaxTurns caps the model turns a run may take, so a model that keeps calling tools cannot
// loop forever. 0 means unbounded (the default); a negative n is ErrConfig. When the cap is
// reached the run returns ErrMaxTurns (category ErrBudget). The cap is per run (per Session.Send
// turn), not per session.
func WithMaxTurns(n int) AgentRunOption {
	return agentRunOption{
		check: nonNegative("WithMaxTurns", n),
		agent: func(a *Agent) { a.maxTurns = n },
		run:   func(c *runConfig) { c.maxTurns = &n },
	}
}

// WithTokenBudget caps the tokens a run, and the runs its tool calls start, may use (see
// Agent.WithTokenBudget for how the budget is counted and enforced). 0 means unbounded (the
// default); a negative max is ErrConfig.
func WithTokenBudget(max int) AgentRunOption {
	return agentRunOption{
		check: nonNegative("WithTokenBudget", max),
		agent: func(a *Agent) { a.tokenBudget = max },
		run:   func(c *runConfig) { c.tokenBudget = &max },
	}
}

// WithSystemPrompt sets a system message prepended to the conversation on every model turn. It
// fills the slot WithSystemPromptFunc fills: the later of the two wins. The message is
// configuration, not journal (see Agent.WithSystemPrompt).
func WithSystemPrompt(s string) AgentRunOption {
	return agentRunOption{
		check: noCheck,
		agent: func(a *Agent) { a.systemPrompt, a.systemPromptFn = s, nil },
		run:   func(c *runConfig) { c.systemPrompt = &s },
	}
}

// WithSampling sets generation controls applied to every model call (Temperature, TopP,
// MaxTokens, Stop, Seed). Each control is its own setting: one this option does not set keeps
// the value an earlier option gave it. A nil control is ErrConfig.
func WithSampling(opts ...SamplingOption) AgentRunOption {
	check := func() error {
		for i, o := range opts {
			if o == nil {
				return fmt.Errorf("WithSampling: control %d is nil: %w", i, ErrConfig)
			}
		}
		return nil
	}
	apply := func(s *Sampling) {
		for _, o := range opts {
			o(s)
		}
	}
	return agentRunOption{
		check: check,
		agent: func(a *Agent) { apply(&a.sampling); a.sampling = cloneSampling(a.sampling) },
		run: func(c *runConfig) {
			if c.sampling == nil {
				c.sampling = &Sampling{}
			}
			apply(c.sampling)
			*c.sampling = cloneSampling(*c.sampling)
		},
	}
}

// WithToolChoice sets the tool-choice control applied to every model call (see ToolChoice and
// Agent.WithToolChoice). A mode that is not "", "auto", "none", "required" or "tool", mode "tool"
// with no Name, a Name with any other mode, and (for an agent) a forced tool the agent does not
// have are ErrConfig.
func WithToolChoice(tc ToolChoice) AgentRunOption {
	return agentRunOption{
		check: func() error { return checkToolChoiceValue(tc) },
		agent: func(a *Agent) { a.toolChoice = &tc },
		run:   func(c *runConfig) { c.toolChoice = &tc },
	}
}

// WithWaker sets the Waker a run's durable Sleep registers its wake with (see Waker). A nil w is
// ErrConfig.
func WithWaker(w Waker) AgentRunOption {
	return agentRunOption{
		check: func() error {
			if isNil(w) {
				return fmt.Errorf("WithWaker: nil Waker: %w", ErrConfig)
			}
			return nil
		},
		agent: func(a *Agent) { a.waker = w },
		run:   func(c *runConfig) { c.waker = w },
	}
}

// WithIdentity sets the acting identity of a run (see Identity): tool calls read it with
// IdentityFrom, and the runs they start inherit it. An empty identity is ErrConfig.
func WithIdentity(id Identity) AgentRunOption {
	return agentRunOption{
		check: func() error {
			if id.Empty() {
				return fmt.Errorf("WithIdentity: empty identity: %w", ErrConfig)
			}
			return nil
		},
		agent: func(a *Agent) { a.identity = &id },
		run:   func(c *runConfig) { c.identity = &id },
	}
}

// ===========================================================================
// WithMaxConcurrency (ConcurrencyOption) and WithClock (ClockOption)
// ===========================================================================

// maxConcurrency implements ConcurrencyOption.
type maxConcurrency int

func (n maxConcurrency) check() error { return nonNegative("WithMaxConcurrency", int(n))() }

func (n maxConcurrency) applyAgent(c *agentConfig) error {
	if err := n.check(); err != nil {
		return err
	}
	c.a.maxConc = int(n)
	return nil
}

func (n maxConcurrency) applyRun(c *runConfig) error {
	if err := n.check(); err != nil {
		return err
	}
	v := int(n)
	c.maxConc = &v
	return nil
}

func (n maxConcurrency) applyParallel(c *parallelConfig) error {
	if err := n.check(); err != nil {
		return err
	}
	c.maxConc = int(n)
	return nil
}

// WithMaxConcurrency caps how many tool calls of one turn run at once (for an agent or a run), or
// how many of Parallel's tasks do. 0 means unbounded (the default); 1 runs them one at a time; a
// negative n is ErrConfig.
func WithMaxConcurrency(n int) ConcurrencyOption { return maxConcurrency(n) }

// clockOption implements ClockOption.
type clockOption func() time.Time

func (f clockOption) check() error {
	if f == nil {
		return fmt.Errorf("WithClock: nil clock: %w", ErrConfig)
	}
	return nil
}

func (f clockOption) applyAgent(c *agentConfig) error {
	if err := f.check(); err != nil {
		return err
	}
	c.a.clock = f
	return nil
}

func (f clockOption) applyRun(c *runConfig) error {
	if err := f.check(); err != nil {
		return err
	}
	c.clock = f
	return nil
}

func (f clockOption) applyResolve(c *resolveConfig) error {
	if err := f.check(); err != nil {
		return err
	}
	c.now = f
	return nil
}

// WithClock sets the clock that reads "now": for a run (an agent's runs, or one run), the clock
// its durable timers (Sleep, WaitUntil, AwaitFor) measure against; for ResolveHaltRef, the clock
// WithMinHaltAge measures against. The default is time.Now. Deployments leave it unset; tests
// pass a controllable clock to advance time deterministically. A nil now is ErrConfig.
func WithClock(now func() time.Time) ClockOption { return clockOption(now) }

// ===========================================================================
// Precedence
// ===========================================================================

// runDefaults binds the agent's identity, Waker and clock to ctx for a run whose context carries
// none: a value the run was given (its context's, or one it inherits from the run that started
// it) takes precedence over the agent's. It is out of line so the loop's frame does not grow.
//
//go:noinline
func (a *Agent) runDefaults(ctx context.Context) context.Context {
	if a.identity != nil {
		if _, set := IdentityFrom(ctx); !set {
			ctx = ContextWithIdentity(ctx, *a.identity)
		}
	}
	if a.waker != nil && wakerFrom(ctx) == nil {
		ctx = ContextWithWaker(ctx, a.waker)
	}
	if a.clock != nil {
		if _, set := ctx.Value(clockKey{}).(func() time.Time); !set {
			ctx = ContextWithClock(ctx, a.clock)
		}
	}
	return ctx
}

// cloneSampling returns a copy of s that shares no pointer or slice with it.
func cloneSampling(s Sampling) Sampling {
	c := Sampling{Stop: slices.Clone(s.Stop)}
	if s.Temperature != nil {
		v := *s.Temperature
		c.Temperature = &v
	}
	if s.TopP != nil {
		v := *s.TopP
		c.TopP = &v
	}
	if s.MaxTokens != nil {
		v := *s.MaxTokens
		c.MaxTokens = &v
	}
	if s.Seed != nil {
		v := *s.Seed
		c.Seed = &v
	}
	return c
}
