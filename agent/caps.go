package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/internal/amount"
)

// Per-call amount caps (docs/design/untrusted-content-and-amount-caps.md, sections 6.2 to 6.4). A
// tool declares rule lines (WithCaps, ToolSpec.Caps); the agent checks them against the tool's input
// schema when the tool is registered (compileCaps), and decides every call twice: at the decision
// point of the loop, on the arguments the model sent (capDecide), which refuses before the call is
// claimed, and at the enforcement point of the tool handler, on the arguments the tool will receive
// after any tool middleware (enforceCaps), which refuses again. Only the per-call form ("> N CUR")
// and the action "-> refuse" exist in this version: a cumulative rule ("sum ...") needs the cap
// ledger and a "-> gate" rule needs the escalation gate, and both are refused at registration, so
// nothing that needs either is reachable.

// maxExactMinor is the largest amount a JSON number carries exactly when a tool or its MCP server reads
// it as a double (2^53). A limit above it cannot be compared with such an amount, so it is refused
// at registration.
const maxExactMinor = 1 << 53

// toolCaps is a tool's compiled cap rules.
type toolCaps struct {
	rules  []amount.Rule
	groups []capGroup // the rules by the argument they read, in rule order
}

// capGroup is the rules of one tool that read one argument (and one currency member): a call's
// currency picks the rule among them.
type capGroup struct {
	arg      []string
	currency []string // the currency member; nil when the limits are in a fixed currency
	rules    []int    // indexes into toolCaps.rules
}

// capWhy says why a rule fired.
type capWhy string

const (
	capOver          capWhy = "over"           // the amount is over the limit
	capNotAssessable capWhy = "not_assessable" // the amount, its currency or the arguments cannot be read with certainty
	capCurrency      capWhy = "currency"       // no rule of the argument covers the call's currency
)

// capHit is one rule that fired for a call.
type capHit struct {
	Rule     string
	Action   amount.Action
	Why      capWhy
	Currency string // the rule's currency, or the call's for capCurrency
	Amount   *int64 // minor units; nil when not assessable
	Limit    int64  // the rule's limit in minor units; 0 for capCurrency
}

// reason is the gate-reason record entry for the hit.
func (h capHit) reason() gateReason {
	act := capActionGate
	if h.Action.Kind == amount.Refuse {
		act = capActionRefuse
	}
	return gateReason{Kind: gateReasonCap, Rule: h.Rule, Action: act, Why: string(h.Why), Currency: h.Currency, Amount: h.Amount, Limit: h.Limit}
}

// The actions a cap reason records.
const (
	capActionGate   = "gate"
	capActionRefuse = "refuse"
)

// compileCaps parses and checks the cap lines of spec against its input schema, and returns the
// compiled rules, or nil for a tool with none. Every failure wraps ErrConfig. The checks, each of which
// fails closed:
//
//   - every line parses (internal/amount); a cumulative rule ("sum ...") and a rule that gates
//     ("-> gate", or "-> gate(name)") are "not yet supported";
//   - the rule names the tool;
//   - the amount member exists in the input schema, through object members only, and is an integer,
//     number or string (an array or object is not supported: a cap cannot see a bulk tool's amounts);
//   - the currency member exists and is a string; a rule with a fixed currency is refused on a tool
//     whose schema has a currency-named member, so the model cannot choose another currency;
//   - a limit above 2^53 minor units is refused (a client that reads numbers as doubles cannot compare
//     it exactly, and no realistic limit is lost).
//
// The checks are not exhaustive: the schema cannot say whether a tool carries amounts elsewhere in its
// arguments (a payments array), which a cap does not see (KNOWN-LIMITATIONS).
func compileCaps(spec ToolSpec) (*toolCaps, error) {
	if len(spec.Caps) == 0 {
		return nil, nil
	}
	fail := func(format string, a ...any) error {
		return fmt.Errorf("agent: tool %q: caps: %s: %w", spec.Name, fmt.Sprintf(format, a...), ErrConfig)
	}
	rules, err := amount.ParseRules(spec.Caps...)
	if err != nil {
		return nil, fail("%v", err)
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(spec.Input, &schema); err != nil || schema == nil {
		return nil, fail("the tool has no input schema to check the rules against")
	}
	tc := &toolCaps{rules: rules}
	for i, r := range rules {
		if r.Kind != amount.PerCall {
			return nil, fail("rule %q is cumulative (sum ... per %s), which is not yet supported", r.Name, r.Window)
		}
		if r.Action.Kind == amount.Gate {
			return nil, fail("rule %q gates (-> gate), which is not yet supported: it arrives with the escalation gate (write -> refuse)", r.Name)
		}
		if !slices.Contains(r.Tools, spec.Name) {
			return nil, fail("rule %q does not name this tool (it names %s)", r.Name, strings.Join(r.Tools, ", "))
		}
		if r.Limit > maxExactMinor {
			return nil, fail("rule %q: the limit is over 2^53 minor units, which a client that reads numbers as doubles cannot compare exactly", r.Name)
		}
		if err := checkMember(spec.Input, r.Arg, "an amount", "integer", "number", "string"); err != nil {
			return nil, fail("rule %q: argument %s: %v", r.Name, strings.Join(r.Arg, "."), err)
		}
		if r.CurrencyArg != nil {
			if err := checkMember(spec.Input, r.CurrencyArg, "a currency", "string"); err != nil {
				return nil, fail("rule %q: currency argument %s: %v", r.Name, strings.Join(r.CurrencyArg, "."), err)
			}
		} else if name, ok := currencyMember(schema); ok {
			return nil, fail("rule %q has a fixed currency, and the tool's schema has the member %q, so the model could choose another currency (write currency=%s)", r.Name, name, name)
		}
		placed := false
		for gi := range tc.groups {
			g := &tc.groups[gi]
			if slices.Equal(g.arg, r.Arg) && slices.Equal(g.currency, r.CurrencyArg) {
				g.rules = append(g.rules, i)
				placed = true
				break
			}
		}
		if !placed {
			tc.groups = append(tc.groups, capGroup{arg: r.Arg, currency: r.CurrencyArg, rules: []int{i}})
		}
	}
	return tc, nil
}

// currencyMember returns the name of a top-level member of the schema that names a currency.
func currencyMember(schema map[string]json.RawMessage) (string, bool) {
	var props map[string]json.RawMessage
	if json.Unmarshal(schema["properties"], &props) != nil {
		return "", false
	}
	for _, name := range slices.Sorted(mapsKeys(props)) {
		if strings.Contains(strings.ToLower(name), "currency") {
			return name, true
		}
	}
	return "", false
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// checkMember checks that path names a member of the object schema, through object members only, and
// that its type is one of types (a "null" alongside is allowed).
func checkMember(schema json.RawMessage, path []string, what string, types ...string) error {
	cur := schema
	for _, name := range path {
		var s struct {
			Type       json.RawMessage            `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Ref        *string                    `json:"$ref"`
			AnyOf      json.RawMessage            `json:"anyOf"`
			OneOf      json.RawMessage            `json:"oneOf"`
			AllOf      json.RawMessage            `json:"allOf"`
		}
		if err := json.Unmarshal(cur, &s); err != nil {
			return errors.New("the schema is not a JSON object")
		}
		if s.Ref != nil || s.AnyOf != nil || s.OneOf != nil || s.AllOf != nil {
			return errors.New("the schema reaches it through $ref, anyOf, oneOf or allOf, which cannot be checked")
		}
		next, ok := s.Properties[name]
		if !ok {
			return fmt.Errorf("the schema has no member %q", cutName(name))
		}
		cur = next
	}
	var m struct {
		Type  json.RawMessage `json:"type"`
		Ref   *string         `json:"$ref"`
		AnyOf json.RawMessage `json:"anyOf"`
		OneOf json.RawMessage `json:"oneOf"`
		AllOf json.RawMessage `json:"allOf"`
	}
	if err := json.Unmarshal(cur, &m); err != nil {
		return errors.New("the member's schema is not a JSON object")
	}
	if m.Ref != nil || m.AnyOf != nil || m.OneOf != nil || m.AllOf != nil {
		return errors.New("the member's type is given through $ref, anyOf, oneOf or allOf, which cannot be checked")
	}
	var got []string
	var one string
	if json.Unmarshal(m.Type, &one) == nil && one != "" {
		got = []string{one}
	} else if json.Unmarshal(m.Type, &got) != nil || len(got) == 0 {
		return errors.New("the member has no type")
	}
	for _, t := range got {
		if t != "null" && !slices.Contains(types, t) {
			if t == "array" || t == "object" {
				return fmt.Errorf("the member is %s, which is not supported: a cap reads %s from an object member only", t, what)
			}
			return fmt.Errorf("the member is %s, want %s", t, strings.Join(types, ", "))
		}
	}
	if len(got) == 1 && got[0] == "null" {
		return errors.New("the member is only null")
	}
	return nil
}

// evaluate decides the rules for one call's arguments. A rule fires when the amount is over the limit or
// cannot be assessed (an absent, negative, unparseable or non-numeric amount, or arguments that are not
// strict JSON), and every rule of an argument fires, with the strictest action among them, when the
// call's currency is not the currency of any rule of that argument (compared byte for byte: "usd" is
// not "USD"). The result is nil when no rule fires.
func (tc *toolCaps) evaluate(args []byte) []capHit {
	var hits []capHit
	for _, g := range tc.groups {
		matched := g.rules
		if g.currency != nil {
			cur, err := callCurrency(args, g.currency)
			if err != nil {
				hits = append(hits, tc.strictest(g, capNotAssessable, ""))
				continue
			}
			matched = nil
			for _, i := range g.rules {
				if tc.rules[i].Currency == cur {
					matched = append(matched, i)
				}
			}
			if len(matched) == 0 {
				hits = append(hits, tc.strictest(g, capCurrency, cur))
				continue
			}
		}
		raw, lerr := amount.Lookup(args, g.arg)
		for _, i := range matched {
			r := tc.rules[i]
			if lerr != nil {
				hits = append(hits, capHit{Rule: r.Name, Action: r.Action, Why: capNotAssessable, Currency: r.Currency, Limit: r.Limit})
				continue
			}
			v, err := amount.Assess(raw, r.Unit, r.Exponent)
			switch {
			case err != nil:
				hits = append(hits, capHit{Rule: r.Name, Action: r.Action, Why: capNotAssessable, Currency: r.Currency, Limit: r.Limit})
			case v > r.Limit:
				hits = append(hits, capHit{Rule: r.Name, Action: r.Action, Why: capOver, Currency: r.Currency, Amount: &v, Limit: r.Limit})
			}
		}
	}
	return hits
}

// strictest is the hit for the group g when the call's currency cannot be matched: the group's
// strictest action (refuse when any of its rules refuses, else gate), under the name of the first
// rule that has that action.
func (tc *toolCaps) strictest(g capGroup, why capWhy, cur string) capHit {
	pick := tc.rules[g.rules[0]]
	for _, i := range g.rules {
		if tc.rules[i].Action.Kind == amount.Refuse {
			pick = tc.rules[i]
			break
		}
	}
	return capHit{Rule: pick.Name, Action: pick.Action, Why: why, Currency: cur}
}

// callCurrency reads the call's currency member as a string, strictly.
func callCurrency(args []byte, path []string) (string, error) {
	raw, err := amount.Lookup(args, path)
	if err != nil {
		return "", err
	}
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil { // a JSON string: not null, which Unmarshal reads as ""
		return "", amount.ErrType
	}
	return s, nil
}

// capDecide is the decision point: it evaluates the tool's rules on the arguments the model sent,
// records the cap reasons the first time a call has any (gate-reason:<call>, insert-once), and returns
// the name of the rule that refuses the call, "" when none does. The reasons enforced are the union of
// those recomputed now and those recorded, so a record can add a requirement and never remove one
// (unionReasons): a recorded cap reason refuses the call whatever the rules would decide now, and a
// reason whose action this version cannot honour (a gate, which has no gate to hold it) refuses too,
// the strictest thing it can do. A tool with no caps is not decided.
func (a *Agent) capDecide(ctx context.Context, runID string, tu ToolUse) (string, error) {
	tc := a.caps[tu.Name]
	if tc == nil {
		return "", nil
	}
	var now []gateReason
	for _, h := range tc.evaluate(tu.Args) {
		now = append(now, h.reason())
	}
	var recorded []gateReason
	if len(now) > 0 {
		rec, _, err := putGateReason(ctx, a.store, runID, gateReasonRecord{Format: gateReasonFormat, ToolUseID: tu.ID, Reasons: slices.Clone(now), RecordedAt: clockFrom(ctx)().UTC().Format(time.RFC3339Nano)})
		if err != nil {
			return "", err
		}
		recorded = rec.Reasons
	} else {
		rec, ok, err := getGateReason(ctx, a.store, runID, tu.ID)
		if err != nil {
			return "", err
		}
		if ok {
			recorded = rec.Reasons
		}
	}
	for _, r := range unionReasons(now, recorded) {
		if r.Kind == gateReasonCap {
			return r.Rule, nil
		}
	}
	return "", nil
}

// refusalText is the result the model reads for a call a cap refused. It names the rule and says a
// limit was exceeded, and gives no figure: a steered model should not be told how to stay just under
// the limit. The figures are in the gate-reason record.
func refusalText(rule string) string {
	return fmt.Sprintf("tool call refused: it exceeds a configured amount limit (rule %q)", cutName(rule))
}

// errCapRefused is a call the enforcement point refused: the tool was not called.
type errCapRefused struct{ rule string }

func (e *errCapRefused) Error() string { return refusalText(e.rule) }

// enforceCaps is the enforcement point, in the tool handler after preExecute and the kill-switch
// check and before the call begins. It evaluates the rules again on the arguments the tool will
// receive, which a tool middleware may have rewritten since the decision point, and refuses the call
// when any rule fires: a rule that fires here is new (the middleware changed the arguments), since one
// the decision point fired never reaches the handler.
func (a *Agent) enforceCaps(tu ToolUse) error {
	tc := a.caps[tu.Name]
	if tc == nil {
		return nil
	}
	if hits := tc.evaluate(tu.Args); len(hits) > 0 {
		return &errCapRefused{rule: hits[0].Rule}
	}
	return nil
}

// refuseCapped records the refusal of a call by a cap rule at the decision point: an error result the
// model reads (refusalText, no figures), the tool never called and no attempt claimed, and returns its
// tool-result message.
func (a *Agent) refuseCapped(ctx context.Context, runID string, tu ToolUse, spec *ToolSpec, rule string) (*Message, error) {
	text, err := marshalJournal(refusalText(rule))
	if err != nil {
		return nil, fmt.Errorf("encode the refusal of call %s: %w (%w)", tu.ID, err, ErrStorage)
	}
	rec, err := a.store.put(ctx, runID, ToolResultStep(tu.ID), Record{Kind: StepToolResult, ToolUseID: tu.ID, IsError: true, Result: text, Safety: recordedSafety(*spec), Approval: effectiveGates(spec, gateInputs{}).Snapshot()})
	if err != nil {
		return nil, err
	}
	return &Message{Role: RoleTool, Parts: []Part{ToolResult{ToolUseID: tu.ID, Result: rec.Result, IsError: rec.IsError}}}, nil
}

// checkCapRules is the check across tools, run once the agent's tools are registered: a rule name is
// one rule (every tool that carries it carries the same one), and a registered tool a rule names
// carries the rule, so naming a tool in a rule cannot leave that tool uncapped. A tool the agent does
// not have is not checked (another agent may register it).
func (a *Agent) checkCapRules() error {
	byName := map[string]amount.Rule{}
	for _, tool := range slices.Sorted(mapsKeys(a.caps)) {
		for _, r := range a.caps[tool].rules {
			if prev, ok := byName[r.Name]; ok {
				if !reflect.DeepEqual(prev, r) {
					return fmt.Errorf("agent: cap rule %q is carried by two tools with different lines (tool %q differs): a rule is one rule, give every tool it names the same line: %w", cutName(r.Name), cutName(tool), ErrConfig)
				}
				continue
			}
			byName[r.Name] = r
		}
	}
	for _, name := range slices.Sorted(mapsKeys(byName)) {
		r := byName[name]
		for _, tool := range r.Tools {
			if _, registered := a.specs[tool]; !registered {
				continue
			}
			if !slices.ContainsFunc(a.caps[tool].rulesOrNil(), func(x amount.Rule) bool { return x.Name == name }) {
				return fmt.Errorf("agent: cap rule %q names tool %q, which is registered without it: give that tool the same line (WithCaps): %w", cutName(name), cutName(tool), ErrConfig)
			}
		}
	}
	return nil
}

func (tc *toolCaps) rulesOrNil() []amount.Rule {
	if tc == nil {
		return nil
	}
	return tc.rules
}
