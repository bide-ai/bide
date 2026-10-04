package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/internal/strictjson"
	"github.com/bide-ai/bide/schema"
)

// Tool is an action the agent can take. The interface is untyped (json.RawMessage) so
// heterogeneous tools share one type, including runtime MCP tools whose schema is only known at
// runtime and can't be a Go struct. Native tools get compile-time typing via Func (below); that's
// the primary ergonomic.
//
// A tool describes itself with a ToolSpec: its name, what the model is shown, how a call may be
// retried, whether it waits for approval, and how long a call may take. The agent reads the spec
// once, when the tool is registered, and decides every call from that copy.
type Tool interface {
	// Spec describes the tool (see ToolSpec).
	Spec() ToolSpec
	// Call runs the tool on the arguments the model sent, and returns its result as JSON.
	Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

// ToolSpec is everything the agent knows about a tool: what the model is shown (Name,
// Description, Input), how a call may be retried (Safety), whether it waits for a human first
// (Approval), and how long a call may take (Timeout). The agent reads a tool's spec once, when the
// tool is registered, and decides every call from that copy.
type ToolSpec struct {
	// Name is the name the model calls the tool by. It must be unique among an agent's tools.
	Name string
	// Title is a human-readable display name (an MCP tool's title). Models are not shown it.
	Title string
	// Description tells the model what the tool does and when to call it.
	Description string
	// Input is the provider-neutral JSON Schema of the arguments, an object schema. The schema/
	// package emits each provider's dialect from it.
	Input json.RawMessage
	// Output is the JSON Schema of the result, when the tool declares one (an MCP tool's
	// outputSchema). Models are not shown it; it documents the result for hosts and auditors.
	Output json.RawMessage
	// Safety declares how a call may be retried (see Safety). It is journaled with each result.
	Safety Safety
	// Approval, when non-nil, pauses each call for human approval before the tool runs: one
	// decision (SingleApproval) or an m-of-n gate over named approvers. It is journaled with
	// each result.
	Approval *ApprovalPolicy
	// Timeout, when positive, bounds each call: the call runs under a context with this
	// deadline. A result the tool returns is recorded even if the deadline has passed; an error
	// it returns after the deadline has an unknown outcome (see WithTimeout).
	Timeout time.Duration
}

// specOf returns t's spec with a copy of its approval policy, so changing the policy does not
// change the tool.
func specOf(t Tool) ToolSpec {
	s := t.Spec()
	s.Approval = s.Approval.Clone()
	return s
}

// checkOldMethods refuses a tool with a method of the Tool interface's old method set (Name,
// Description, ArgsSchema, Safety) whose value is not its Spec's. The agent reads only Spec, so
// such a method is an override written for the old interface that no longer overrides anything:
// typically a decorator that embeds the tool it decorates, whose Spec is the embedded tool's, and
// whose own Safety() (a side effect it adds) would be ignored, so a resume would run it again.
//
// It looks at the methods a pointer to the tool's value has (a pointer-receiver Safety counts when
// the tool is registered by value: it was written to override), and refuses:
//   - such a method that returns another value than Spec holds (ArgsSchema is compared as JSON, so
//     a reformatted schema agrees);
//   - such a method with another signature (Safety() returning another type);
//   - such a method that an embedded field declares but the tool's method set lacks (two embedded
//     fields declare it, so neither is promoted, and the one meant to override is lost).
//
// A method that agrees with Spec changes nothing and is accepted. A method named like one of them
// that means something else (a display Name() that is not the tool's name) is refused all the
// same: rename it, or make Spec say the same.
func checkOldMethods(t Tool, s ToolSpec) error {
	v := reflect.ValueOf(t)
	pv := v
	if v.Kind() != reflect.Pointer {
		pv = reflect.New(v.Type())
		pv.Elem().Set(v)
	}
	var dead []string
	for _, name := range []string{"Name", "Description", "ArgsSchema", "Safety"} {
		m := pv.MethodByName(name)
		if !m.IsValid() {
			if embeddedDeclares(pv.Type().Elem(), name, 0) {
				dead = append(dead, name+" (declared by more than one embedded field, so not promoted)")
			}
			continue
		}
		if m.Type().NumIn() != 0 || m.Type().NumOut() != 1 {
			dead = append(dead, name+" (another signature)")
			continue
		}
		out := m.Call(nil)[0]
		agree := false
		switch name {
		case "Name", "Description":
			want := s.Name
			if name == "Description" {
				want = s.Description
			}
			agree = out.Kind() == reflect.String && out.String() == want
		case "ArgsSchema":
			b, ok := out.Interface().(json.RawMessage)
			agree = ok && sameJSON(b, s.Input)
		case "Safety":
			got, ok := out.Interface().(Safety)
			agree = ok && got == s.Safety
		}
		if !agree {
			dead = append(dead, name)
		}
	}
	if len(dead) > 0 {
		return fmt.Errorf("agent: tool %q (%T): %s disagrees with its Spec, which is all the agent reads: set the value in Spec (a decorator: s := d.Tool.Spec(), then the fields it overrides), or rename a method that means something else: %w",
			s.Name, t, strings.Join(dead, ", "), ErrConfig)
	}
	return nil
}

// embeddedDeclares reports whether a field embedded in t (a struct), at any depth, has a method
// name. Interface fields (the embedded Tool) are not searched: the Tool interface has none of the
// old methods.
func embeddedDeclares(t reflect.Type, name string, depth int) bool {
	if t.Kind() != reflect.Struct || depth > 8 {
		return false
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.Anonymous || f.Type.Kind() == reflect.Interface {
			continue
		}
		ft := f.Type
		if ft.Kind() != reflect.Pointer {
			ft = reflect.PointerTo(ft)
		}
		if _, ok := ft.MethodByName(name); ok || embeddedDeclares(ft.Elem(), name, depth+1) {
			return true
		}
	}
	return false
}

// Safety declares how a tool call may be retried: when a run resumes after a crash and the call's
// outcome is unknown (it was invoked, but no result was journaled), and when a tool middleware
// retries or caches it. It is plain data: comparable, and journaled with each call's result. The
// zero value is a side effect, which runs at most once per call and halts a resume whose outcome
// is unknown. For a trusted MCP server, its tool annotations (readOnlyHint / idempotentHint) map
// onto it (see mcp.TrustAnnotations). Whether a call waits for a human is not Safety but
// ToolSpec.Approval (see WithApproval).
type Safety struct {
	// ReadOnly: no external side effects, so always safe to re-run.
	ReadOnly bool `json:"read_only,omitempty"`
	// Idempotent: mutates state but is safe to retry, because a repeat is a no-op downstream.
	// The tool keeps that promise itself. NextOnceKey dedupes the same call running again (a
	// resume, a middleware retry); it does not dedupe the model calling the tool again, which
	// is a new call with new keys. A tool whose repeats the model may make (after a recorded
	// error or timeout) derives a business key from its arguments for the downstream to dedupe.
	Idempotent bool `json:"idempotent,omitempty"`
}

// ApprovalPolicy declares a human approval gate. The policy SingleApproval returns is one
// decision, recorded with Approve and pausing as *ApprovalPending; only that value is, whatever
// its fields say, so a policy built by hand is never taken for it. Any other policy is an m-of-n
// gate: Need is k and the eligible approvers are the bounded set (n = len(Approvers)), whose
// signed decisions SubmitDecision records; Validate checks it, and refuses one with no approvers.
type ApprovalPolicy struct {
	Need      int      `json:"need"`                // decisions required to proceed (k), 1 <= Need <= len(Approvers)
	Approvers []string `json:"approvers,omitempty"` // eligible approver ids; the bounded set (n)

	// one marks the policy SingleApproval returned (and its copies): the one-decision gate. It is
	// unexported so no literal can set it; MarshalJSON journals it as {"single":true}. The gate a
	// run enforces is always the registered tool's, never one read back from the journal.
	one bool
}

// SingleApproval returns the one-decision gate: a call pauses as *ApprovalPending until Approve
// records a decision for it. It is the only way to ask for that gate: an ApprovalPolicy literal,
// even {Need: 1} with no approvers, is an m-of-n policy, and one with no approvers is ErrConfig.
func SingleApproval() *ApprovalPolicy { return &ApprovalPolicy{Need: 1, one: true} }

// MarshalJSON encodes the policy as the journal records it: SingleApproval as {"single":true}, the
// bide protocol's wire form of the one-decision gate, so an offline reader sees the gate a call ran
// under without inferring it from a shape; any other policy as {"need":k,"approvers":[...]}.
func (p ApprovalPolicy) MarshalJSON() ([]byte, error) {
	if p.one {
		if p.Need != 1 || len(p.Approvers) != 0 {
			return nil, fmt.Errorf("a SingleApproval policy was changed to Need %d and %d approvers: %w", p.Need, len(p.Approvers), ErrConfig)
		}
		return []byte(`{"single":true}`), nil
	}
	return json.Marshal(approvalWire{Need: p.Need, Approvers: p.Approvers})
}

// UnmarshalJSON decodes what MarshalJSON encodes, strictly, as the bide protocol writes the gate:
// {"single":true}, with no other member, is SingleApproval; any other policy has need (and
// approvers) and no single. "single" with any other value, beside need or approvers, a policy
// with no need, a duplicate or unknown member, or a value of the wrong type is ErrProtocol. It is
// the decoding for a policy being configured or received (a protocol input); the approval inside a
// stored Record is read leniently instead (see DecodeStoredRecord), so a journal a newer version
// wrote stays readable.
func (p *ApprovalPolicy) UnmarshalJSON(b []byte) error {
	var members map[string]json.RawMessage
	if err := strictjson.Unmarshal(b, &members, nil); err != nil {
		return fmt.Errorf("approval policy: %w (%w)", err, ErrProtocol)
	}
	if raw, ok := members["single"]; ok {
		if string(bytes.TrimSpace(raw)) != "true" || len(members) != 1 {
			return fmt.Errorf(`approval policy: "single" must be true and alone, as {"single":true}: %w`, ErrProtocol)
		}
		*p = *SingleApproval()
		return nil
	}
	var w approvalWire // need is required: its tag has no omitempty
	if err := strictjson.Unmarshal(b, &w, &strictjson.Options{Fields: strictjson.SchemaFields}); err != nil {
		return fmt.Errorf("approval policy: %w (%w)", err, ErrProtocol)
	}
	*p = ApprovalPolicy{Need: w.Need, Approvers: w.Approvers}
	return nil
}

// approvalWire is an m-of-n ApprovalPolicy's encoding.
type approvalWire struct {
	Need      int      `json:"need"`
	Approvers []string `json:"approvers,omitempty"`
}

// single reports whether p is the one-decision gate SingleApproval returned.
func (p *ApprovalPolicy) single() bool { return p != nil && p.one }

// Clone returns a copy of p that shares nothing with it (a SingleApproval stays one), or nil
// for nil.
func (p *ApprovalPolicy) Clone() *ApprovalPolicy {
	if p == nil {
		return nil
	}
	return &ApprovalPolicy{Need: p.Need, Approvers: slices.Clone(p.Approvers), one: p.one}
}

// checkApproval reports whether p is a gate the agent can enforce (see Validate).
func checkApproval(p *ApprovalPolicy) error {
	if p == nil {
		return fmt.Errorf("approval policy is nil (give no approval option for an ungated tool): %w", ErrConfig)
	}
	return p.Validate()
}

// retriableOnResume reports whether an unknown-outcome call may be safely re-run.
// Anything else halts the run for confirmation rather than risk a double side effect.
func (s Safety) retriableOnResume() bool { return s.ReadOnly || s.Idempotent }

// RetrySafe reports whether a call to the tool may run more than once for one tool call: it
// is ReadOnly or Idempotent. It is the test the agent applies on resume, and the one tool
// middleware applies before retrying a call (see ToolCall).
func (s Safety) RetrySafe() bool { return s.retriableOnResume() }

// retrySafeWrite reports whether a call is retry-safe and changes state: Idempotent, not ReadOnly.
func (s Safety) retrySafeWrite() bool { return s.Idempotent && !s.ReadOnly }

// ToolOption configures a tool built by Func, CompensatedFunc or SubAgent.
type ToolOption interface{ applyTool(*toolConfig) error }

// toolConfig is what the options of one tool constructor set.
type toolConfig struct {
	spec ToolSpec
	// subRuns resolves the agent that runs one of the tool's programmatic sub-runs (WithSubRuns).
	subRuns func(name string) *Agent
	// set records which options were given, for a constructor that refuses one.
	safetySet, timeoutSet, outputSet bool
}

type toolOption func(*toolConfig) error

func (f toolOption) applyTool(c *toolConfig) error { return f(c) }

// safetyOption implements SafetyOption.
type safetyOption Safety

func (s safetyOption) applyTool(c *toolConfig) error {
	c.spec.Safety, c.safetySet = Safety(s), true
	return nil
}

func (s safetyOption) applyStep(c *stepConfig) error {
	c.safety = Safety(s)
	return nil
}

// WithSafety declares how safe a tool's calls, or a step, are to re-run. For a tool built by Func
// or CompensatedFunc it replaces the Safety argument (which the 1.0 rewrite removes in its
// favour); SubAgent refuses it, since a sub-agent call re-enters its sub-run, whose own calls
// carry their safety. For a Step, a step that is RetrySafe (ReadOnly or Idempotent) re-runs after
// a crash, and any other step halts (the default is a side effect).
func WithSafety(s Safety) SafetyOption { return safetyOption(s) }

// WithApproval gates every call to the tool on human approval before it runs: p is
// SingleApproval() for one decision (Approve), or an m-of-n policy (SubmitDecision, and the
// agent's WithApproverVerifiers). A nil or invalid policy is ErrConfig. The policy is copied.
func WithApproval(p *ApprovalPolicy) ToolOption {
	return toolOption(func(c *toolConfig) error {
		if err := checkApproval(p); err != nil {
			return err
		}
		c.spec.Approval = p.Clone()
		return nil
	})
}

// WithTimeout bounds each call to the tool by d: the call runs under a context with that
// deadline, on top of the run's. It bounds only a tool that honors its context; the agent still
// waits for the call to return. A call that returns a result is recorded even if the deadline has
// passed, since a known outcome is never discarded. A call that returns an error after the
// deadline (judged by the deadline itself) has an unknown outcome, as if it had failed with
// ErrToolOutcomeUnknown: a side effect records nothing, and a resume halts for its outcome, while
// a retry-safe tool records the error for the model, which may call it again as a new call (see
// NextOnceKey), and in a saga is reported in SagaAborted.UnknownOutcome. Only a call that reached
// the tool is judged so: one a tool middleware ended first, even at the deadline, never ran the
// tool and fails as a known error. d must be positive (ErrConfig otherwise).
func WithTimeout(d time.Duration) ToolOption {
	return toolOption(func(c *toolConfig) error {
		if d <= 0 {
			return fmt.Errorf("tool timeout %s is not positive: %w", d, ErrConfig)
		}
		c.spec.Timeout, c.timeoutSet = d, true
		return nil
	})
}

// WithTitle sets the tool's human-readable display name (ToolSpec.Title).
func WithTitle(title string) ToolOption {
	return toolOption(func(c *toolConfig) error { c.spec.Title = title; return nil })
}

// WithOutputSchema declares the JSON Schema of the tool's result (ToolSpec.Output). It must be a
// JSON object (ErrConfig otherwise). The schema is copied.
func WithOutputSchema(schema json.RawMessage) ToolOption {
	return toolOption(func(c *toolConfig) error {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(schema, &obj); err != nil || obj == nil {
			return fmt.Errorf("tool output schema is not a JSON object: %w", ErrConfig)
		}
		c.spec.Output, c.outputSet = bytes.Clone(schema), true
		return nil
	})
}

// WithSubRuns declares the agents that run the tool's programmatic sub-runs: agentFor(name) is the
// agent the tool runs RunInfo.SubRunFor(name) with. A saga's rollback walks those sub-runs as it
// walks a sub-agent's (SubAgent): for each sub-run a call of the tool started, latest first, it
// compensates the sub-run's completed writes with agentFor(name)'s compensators, after the call
// itself. agentFor must return the agent (or one with the same tools) for every name the tool
// starts a sub-run under, also after a restart, since the rollback may run in a later process.
//
// A sub-run in a saga's tree must journal to the run's store, where the rollback reads it: Run
// refuses (ErrConfig) one whose agent journals elsewhere. The call's own compensation runs before
// its sub-runs are walked. A sub-run is linked when it starts, within the call; one that is still
// running when the call returns (held by a goroutine the call left behind) may still be writing
// while the rollback walks it, so a write it makes after the walk passed it is not undone.
//
// Without it, or when agentFor returns nil for a sub-run, the rollback cannot undo the sub-run's
// writes: it reports each one in SagaAborted.Uncompensated (and stops for a human at one whose
// outcome is unknown), as it does for a write with no compensator. An agent it cannot use (one on
// another store, or a panic in agentFor) is reported the same way, and the call is listed too,
// with the reason (see SagaAborted). A nil agentFor is ErrConfig, and SubAgent, whose sub-run is
// its own, refuses the option.
func WithSubRuns(agentFor func(name string) *Agent) ToolOption {
	return toolOption(func(c *toolConfig) error {
		if agentFor == nil {
			return fmt.Errorf("WithSubRuns: nil function: %w", ErrConfig)
		}
		c.subRuns = agentFor
		return nil
	})
}

// applyToolOptions applies opts to c in order, and returns the first error, naming the tool.
func applyToolOptions(c *toolConfig, opts []ToolOption) error {
	for _, o := range opts {
		if o == nil {
			return fmt.Errorf("agent: tool %q: nil option: %w", c.spec.Name, ErrConfig)
		}
		if err := o.applyTool(c); err != nil {
			return fmt.Errorf("agent: tool %q: %w", c.spec.Name, err)
		}
	}
	return nil
}

// Func wraps a typed Go function into a Tool. In is decoded from the args strictly, so the
// tool reads exactly what the model sent: a missing required field (one schema.For lists as
// required), null for a required field whose schema does not admit null, an unknown name or a case variant of a field's name, a duplicate name, data after
// the value, invalid UTF-8, or an escaped lone surrogate is ErrToolArgs, which goes back to the
// model as a tool error to correct. Empty args are the empty object. The return value is
// JSON-encoded. This is the compile-time-typed ergonomic: change In and the
// handler won't compile. The Tool interface itself stays untyped so a map of mixed tools (and
// runtime MCP tools) works.
//
// opts set the rest of the tool's spec: WithSafety (the default is a side effect, Safety{}),
// WithApproval, WithTimeout, WithTitle and WithOutputSchema. WithSubRuns declares the agents the
// tool runs programmatic sub-runs with, for a saga's rollback.
//
// Func returns an error wrapping ErrConfig if schema.For cannot describe In (such a type, a field
// reached through an embedded pointer to an unexported struct, could never be decoded from a
// call's arguments, so the tool would fail every call) or an option is invalid. MustFunc panics
// instead, for a tool built at init.
func Func[In, Out any](name, description string, fn func(context.Context, In) (Out, error), opts ...ToolOption) (Tool, error) {
	t, err := newFuncTool(name, description, fn, opts)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// MustFunc is Func for a tool built at init: it panics with Func's error.
func MustFunc[In, Out any](name, description string, fn func(context.Context, In) (Out, error), opts ...ToolOption) Tool {
	return must(Func(name, description, fn, opts...))
}

// must returns v, and panics with err if it is not nil: the Must constructors' body.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func newFuncTool[In, Out any](name, description string, fn func(context.Context, In) (Out, error), opts []ToolOption) (*funcTool[In, Out], error) {
	// Derive the provider-neutral argument schema from In once, at construction. Adapters
	// dialectize it (schema.OpenAIStrict etc.) at request time.
	argsSchema, err := schema.For[In]()
	if err != nil {
		return nil, fmt.Errorf("agent: Func %q: argument type: %w (%w)", name, err, ErrConfig)
	}
	c := toolConfig{spec: ToolSpec{Name: name, Description: description, Input: argsSchema}}
	if err := applyToolOptions(&c, opts); err != nil {
		return nil, err
	}
	return &funcTool[In, Out]{spec: c.spec, fn: fn, subRuns: c.subRuns}, nil
}

type funcTool[In, Out any] struct {
	spec    ToolSpec
	fn      func(context.Context, In) (Out, error)
	subRuns func(name string) *Agent // WithSubRuns; nil if not given
}

func (t *funcTool[In, Out]) subRunAgent(name string) *Agent {
	if t.subRuns == nil {
		return nil
	}
	return t.subRuns(name)
}

// Spec returns the tool's spec, with a copy of its approval policy.
func (t *funcTool[In, Out]) Spec() ToolSpec {
	s := t.spec
	s.Approval = s.Approval.Clone()
	return s
}

func (t *funcTool[In, Out]) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	var in In
	if err := decodeArgs(args, &in); err != nil {
		return nil, fmt.Errorf("decode args for tool %q: %w (%w)", t.spec.Name, err, ErrToolArgs)
	}
	out, err := t.fn(ctx, in)
	if err != nil {
		return nil, err
	}
	return marshalJournal(out) // not HTML-escaped: the model reads this JSON text as written
}

// argsOptions check tool arguments against the fields schema.For describes: exact names, and
// every field the schema lists as required.
var argsOptions = &strictjson.Options{Fields: strictjson.SchemaFields}

// decodeArgs decodes a tool call's arguments into v (a pointer) strictly, so the value holds
// exactly what the arguments say. It rejects what encoding/json would accept loosely: a missing
// required field (one schema.For lists as required: not a pointer, and no omitempty or omitzero
// in its json tag) or null for one whose schema does not admit null, a name that is not a field (an unknown name, or a case variant of a field's
// name), a duplicate name, data after the value, invalid UTF-8, and an escaped lone surrogate.
// Empty arguments are the empty object. Func, SubAgent, and RunTyped's final_answer decode their
// arguments with it.
func decodeArgs(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	return strictjson.Unmarshal(args, v, argsOptions)
}
