// Package agent is a durability-first, Go-idiomatic toolkit for building AI agents.
//
// The agent loop journals every step to a Journal over a Store via named-step memoization
// and resumes after a crash — reusing recorded steps, re-running
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
	"regexp"
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
// Build an Agent with New and options, and derive a differently configured one with With,
// which copies it: an agent built that way is never changed, so it is safe for concurrent
// Run/Stream/Session calls and for concurrent With calls.
type Agent struct {
	model        Model
	tools        map[string]Tool
	specs        map[string]*ToolSpec // each tool's spec, read once when it was registered; never changed
	specList     []ToolSpec           // specs sorted by name, as model requests are sent them
	store        *Journal
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
	responseFormat *ResponseFormat  // native structured-output constraint (see OutputNative)
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

// addTool registers t, reading its spec once: every decision about its calls reads this copy. It
// refuses, each with ErrConfig, a nil tool, a name the agent already has, a wrapper the agent
// cannot honor (checkWrapper), an approval policy no option would build (a SingleApproval whose
// fields were changed), a name the agent's model's declared tool-name rule (ToolRules) does not
// match, the name RunTyped reserves, an input schema that is not a JSON object, and a method of
// the old Tool method set that disagrees with the tool's Spec (checkOldMethods). A refused tool
// is not registered.
func (a *Agent) addTool(t Tool) error {
	if isNil(t) {
		return fmt.Errorf("agent: nil tool: %w", ErrConfig)
	}
	s := specOf(t)
	if err := checkWrapper(t, s); err != nil {
		return err
	}
	if err := checkOldMethods(t, s); err != nil {
		return err
	}
	if s.Approval != nil {
		if err := checkApproval(s.Approval); err != nil {
			return fmt.Errorf("agent: tool %q: %w", s.Name, err)
		}
	}
	switch _, taken := a.specs[s.Name]; {
	case taken:
		// The model calls a tool by name, so one of the two could never be called, and which one
		// a call reached would depend on the order the host listed them in: a host that adds tools
		// from a runtime source such as an MCP server after its own would send the model's call,
		// arguments and all, to the server.
		return fmt.Errorf("agent: two tools are named %q: %w", s.Name, ErrConfig)
	case !a.modelAcceptsName(s.Name):
		return fmt.Errorf("agent: tool name %q does not match %s, the tool-name rule the agent's model declares (ToolRules): %w",
			s.Name, a.toolNameRule(), ErrConfig)
	case s.Name == finalAnswerTool:
		return fmt.Errorf("agent: tool name %q is reserved for RunTyped's answer: %w", s.Name, ErrConfig)
	}
	if err := checkInputSchema(s); err != nil {
		return err
	}
	a.tools[s.Name], a.specs[s.Name] = t, &s
	return nil
}

// toolNameRule is the tool-name pattern the agent's model declares (ToolRules), or nil.
func (a *Agent) toolNameRule() *regexp.Regexp {
	if r, ok := toolRulesOf(a.model); ok {
		return r.ToolNameRule()
	}
	return nil
}

// modelAcceptsName reports whether name matches the tool-name rule the agent's model declares, if
// it declares one.
func (a *Agent) modelAcceptsName(name string) bool {
	rule := a.toolNameRule()
	return rule == nil || rule.MatchString(name)
}

// checkRequiredChoice refuses tool choice "required" on an agent that offers the model no tool,
// when its model declares that a "required" request needs one (ToolRules): no request it sends
// could be met. An agent counts the tools it supplies itself, RunTyped's answer tool included,
// so New leaves an agent with no tools of its own to its runs: RunTyped may supply one. Run
// checks before it opens the journal.
func (a *Agent) checkRequiredChoice() error {
	if tc := a.toolChoice; tc == nil || tc.Mode != "required" || len(a.specs) > 0 {
		return nil
	}
	if r, ok := toolRulesOf(a.model); ok && r.RequiresToolsForRequired() {
		return fmt.Errorf("agent: tool choice \"required\" with no tools to call, which the agent's model refuses: %w", ErrConfig)
	}
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
