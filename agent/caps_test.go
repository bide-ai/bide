package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// moneyTool is a tool whose Spec carries caps and whose calls are recorded.
type moneyTool struct {
	spec ToolSpec
	mu   sync.Mutex
	args []string
}

func (t *moneyTool) Spec() ToolSpec { return t.spec }
func (t *moneyTool) Call(_ context.Context, a json.RawMessage) (json.RawMessage, error) {
	t.mu.Lock()
	t.args = append(t.args, string(a))
	t.mu.Unlock()
	return json.RawMessage(`{"ok":true}`), nil
}
func (t *moneyTool) calls() int { t.mu.Lock(); defer t.mu.Unlock(); return len(t.args) }

const refundSchema = `{"type":"object","properties":{"amount":{"type":"integer"},"currency":{"type":"string"},"note":{"type":"string"}},"required":["amount","currency"]}`

const (
	refuseUSD = `big_refund: refund.amount[minor, currency=currency] > 5 USD -> refuse`
	gateUSD   = `big_refund: refund.amount[minor, currency=currency] > 5 USD -> gate`
)

func newCapTool(caps ...string) *moneyTool {
	return &moneyTool{spec: ToolSpec{Name: "refund", Input: json.RawMessage(refundSchema), Caps: caps}}
}

func capArgs(amount, cur string) string {
	return fmt.Sprintf(`{"amount":%s,"currency":%q}`, amount, cur)
}

// --- registration ---

func TestCaps_Registration(t *testing.T) {
	schemaWith := func(props string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":{` + props + `}}`)
	}
	amountInt := `"amount":{"type":"integer"}`
	cur := `"currency":{"type":"string"}`
	for _, tc := range []struct {
		name   string
		spec   ToolSpec
		want   string // "" for accepted, else a substring of the ErrConfig
		modify func(*ToolSpec)
	}{
		{name: "per-call refuse", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt + "," + cur), Caps: []string{refuseUSD}}},
		{name: "string amount, major", spec: ToolSpec{Name: "refund", Input: schemaWith(`"amount":{"type":"string"},` + cur), Caps: []string{`r: refund.amount[major, currency=currency] > 5 USD -> refuse`}}},
		{name: "number amount", spec: ToolSpec{Name: "refund", Input: schemaWith(`"amount":{"type":"number"},` + cur), Caps: []string{`r: refund.amount[major, currency=currency] > 5 USD -> refuse`}}},
		{name: "nullable amount", spec: ToolSpec{Name: "refund", Input: schemaWith(`"amount":{"type":["integer","null"]},` + cur), Caps: []string{refuseUSD}}},
		{name: "nested path", spec: ToolSpec{Name: "refund", Input: schemaWith(`"p":{"type":"object","properties":{"amount":{"type":"integer"},"cur":{"type":"string"}}}`), Caps: []string{`r: refund.p.amount[minor, currency=p.cur] > 5 USD -> refuse`}}},
		{name: "fixed currency, no currency member", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt), Caps: []string{`r: refund.amount[minor] > 5 USD -> refuse`}}},
		{name: "fixed currency but the schema has one", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt + "," + cur), Caps: []string{`r: refund.amount[minor] > 5 USD -> refuse`}}, want: "fixed currency"},
		{name: "no such member", spec: ToolSpec{Name: "refund", Input: schemaWith(`"sum":{"type":"integer"},` + cur), Caps: []string{refuseUSD}}, want: "no member"},
		{name: "boolean amount", spec: ToolSpec{Name: "refund", Input: schemaWith(`"amount":{"type":"boolean"},` + cur), Caps: []string{refuseUSD}}, want: "want integer, number, string"},
		{name: "array amount", spec: ToolSpec{Name: "refund", Input: schemaWith(`"amount":{"type":"array","items":{"type":"integer"}},` + cur), Caps: []string{refuseUSD}}, want: "array"},
		{name: "object amount", spec: ToolSpec{Name: "refund", Input: schemaWith(`"amount":{"type":"object"},` + cur), Caps: []string{refuseUSD}}, want: "object"},
		{name: "untyped amount", spec: ToolSpec{Name: "refund", Input: schemaWith(`"amount":{},` + cur), Caps: []string{refuseUSD}}, want: "no type"},
		{name: "amount behind $ref", spec: ToolSpec{Name: "refund", Input: schemaWith(`"amount":{"$ref":"#/$defs/a"},` + cur), Caps: []string{refuseUSD}}, want: "$ref"},
		{name: "amount behind anyOf", spec: ToolSpec{Name: "refund", Input: schemaWith(`"amount":{"anyOf":[{"type":"integer"}]},` + cur), Caps: []string{refuseUSD}}, want: "anyOf"},
		{name: "currency not a string", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt + `,"currency":{"type":"integer"}`), Caps: []string{refuseUSD}}, want: "currency argument"},
		{name: "currency member missing", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt), Caps: []string{refuseUSD}}, want: "currency argument"},
		{name: "does not parse", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt + "," + cur), Caps: []string{`nonsense`}}, want: "caps"},
		{name: "gate is not yet supported", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt + "," + cur), Caps: []string{gateUSD}}, want: "escalation gate"},
		{name: "cumulative refuse", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt + "," + cur), Caps: []string{`d: refund.amount[minor, currency=currency] sum 5000 USD per day -> refuse`}}},
		{name: "cumulative gate is not yet supported", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt + "," + cur), Caps: []string{`d: refund.amount[minor, currency=currency] sum 5000 USD per day -> gate`}}, want: "escalation gate"},
		{name: "named gate is not yet supported", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt + "," + cur), Caps: []string{`r: refund.amount[minor, currency=currency] > 5 USD -> gate(finance)`}}, want: "not yet supported"},
		{name: "rule does not name the tool", spec: ToolSpec{Name: "other", Input: schemaWith(amountInt + "," + cur), Caps: []string{refuseUSD}}, want: "does not name this tool"},
		{name: "limit over 2^53 minor units", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt + "," + cur), Caps: []string{`r: refund.amount[minor, currency=currency] > 90071992547409.93 USD -> refuse`}}, want: "2^53"},
		{name: "limit at 2^53 minor units", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt + "," + cur), Caps: []string{`r: refund.amount[minor, currency=currency] > 90071992547409.92 USD -> refuse`}}},
		{name: "refuse with the tool's own approval", spec: ToolSpec{Name: "refund", Input: schemaWith(amountInt + "," + cur), Approval: SingleApproval(), Caps: []string{refuseUSD}}},
		{name: "a quoted tool name", spec: ToolSpec{Name: "stripe.refund", Input: schemaWith(amountInt + "," + cur), Caps: []string{`r: "stripe.refund".amount[minor, currency=currency] > 5 USD -> refuse`}}},
	} {
		_, err := New(&scriptModel{}, memJournal(), WithTools(&moneyTool{spec: tc.spec}))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v, want accepted", tc.name, err)
		case tc.want != "" && (!errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want ErrConfig containing %q", tc.name, err, tc.want)
		}
	}
}

// The ToolOptions set the spec, and a tool built by Func is checked against its generated schema.
func TestCaps_FuncToolWithOptions(t *testing.T) {
	type in struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	f := func(opts ...ToolOption) error {
		tool, err := Func("refund", "refund", func(context.Context, in) (string, error) { return "ok", nil }, opts...)
		if err != nil {
			return err
		}
		_, err = New(&scriptModel{}, memJournal(), WithTools(tool))
		return err
	}
	if err := f(WithCaps(refuseUSD)); err != nil {
		t.Fatalf("a Func tool with caps: %v", err)
	}
	for name, opts := range map[string][]ToolOption{
		"no lines":       {WithCaps()},
		"unparseable":    {WithCaps("x")},
		"gate":           {WithCaps(gateUSD)},
		"wrong tool":     {WithCaps(`r: other.amount[minor, currency=currency] > 5 USD -> refuse`)},
		"missing member": {WithCaps(`r: refund.total[minor, currency=currency] > 5 USD -> refuse`)},
	} {
		if err := f(opts...); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v, want ErrConfig", name, err)
		}
	}
}

// Tools that share a rule must carry the same line, and a rule name means one rule.
func TestCaps_SharedRuleMustBeIdentical(t *testing.T) {
	mk := func(name, line string) *moneyTool {
		return &moneyTool{spec: ToolSpec{Name: name, Input: json.RawMessage(refundSchema), Caps: []string{line}}}
	}
	same := `r: refund|refund_large.amount[minor, currency=currency] > 500 USD -> refuse`
	if _, err := New(&scriptModel{}, memJournal(), WithTools(mk("refund", same), mk("refund_large", same))); err != nil {
		t.Fatalf("one rule on two tools: %v", err)
	}
	if _, err := New(&scriptModel{}, memJournal(), WithTools(mk("refund", same), &moneyTool{spec: ToolSpec{Name: "refund_large", Input: json.RawMessage(refundSchema)}})); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "registered without it") {
		t.Fatalf("a named tool with no caps: %v, want ErrConfig", err)
	}
	if _, err := New(&scriptModel{}, memJournal(), WithTools(mk("refund", same))); err != nil {
		t.Fatalf("a rule naming a tool the agent does not have: %v", err)
	}
	other := `r: refund|refund_large.amount[minor, currency=currency] > 900 USD -> refuse`
	if _, err := New(&scriptModel{}, memJournal(), WithTools(mk("refund", same), mk("refund_large", other))); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("one rule name with two texts: %v, want ErrConfig", err)
	}
}

// The model is never sent the limits.
func TestCaps_RequestToolsCarryNoCaps(t *testing.T) {
	var seen []ToolSpec
	m := &scriptModel{turns: [][]Emit{textTurn("done")}}
	spy := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			seen = append(seen, call.Request.Tools...)
			return next(ctx, call)
		}
	}
	a := mustNew(m, memJournal(), WithTools(newCapTool(refuseUSD)), WithMiddleware(spy))
	if _, err := a.Run(context.Background(), "r", UserText("go")); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Name != "refund" || len(seen[0].Caps) != 0 {
		t.Fatalf("the model's request carried %+v", seen)
	}
	// And With keeps the rules.
	b, err := a.With()
	if err != nil {
		t.Fatal(err)
	}
	if b.caps["refund"] == nil {
		t.Fatal("With dropped the compiled caps")
	}
}

// --- evaluation ---

func TestCaps_Evaluate(t *testing.T) {
	tc, err := compileCaps(*mustSpec(refuseUSD))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		args string
		want []string // the why of each hit
	}{
		{"under", capArgs("499", "USD"), nil},
		{"at the limit", capArgs("500", "USD"), nil},
		{"zero", capArgs("0", "USD"), nil},
		{"over by one", capArgs("501", "USD"), []string{"over"}},
		{"over, huge", capArgs("9007199254740993", "USD"), []string{"over"}},
		{"beyond int64", capArgs("99999999999999999999", "USD"), []string{"not_assessable"}},
		{"negative", capArgs("-1", "USD"), []string{"not_assessable"}},
		{"fraction on minor units", capArgs("5.5", "USD"), []string{"not_assessable"}},
		{"exponent form", capArgs("5e2", "USD"), []string{"not_assessable"}},
		{"leading plus", `{"amount":+5,"currency":"USD"}`, []string{"not_assessable"}},
		{"amount is a string of digits", `{"amount":"501","currency":"USD"}`, []string{"over"}},
		{"amount is text", `{"amount":"lots","currency":"USD"}`, []string{"not_assessable"}},
		{"amount is null", `{"amount":null,"currency":"USD"}`, []string{"not_assessable"}},
		{"amount is a bool", `{"amount":true,"currency":"USD"}`, []string{"not_assessable"}},
		{"amount absent", `{"currency":"USD"}`, []string{"not_assessable"}},
		{"empty arguments", ``, []string{"not_assessable"}},
		{"not an object", `[1]`, []string{"not_assessable"}},
		{"duplicate amount", `{"amount":1,"amount":900,"currency":"USD"}`, []string{"not_assessable"}},
		{"duplicate elsewhere", `{"amount":1,"currency":"USD","note":"a","note":"b"}`, []string{"not_assessable"}},
		{"case variant", `{"amount":1,"Amount":900,"currency":"USD"}`, []string{"not_assessable"}},
		{"unknown currency", capArgs("1", "EUR"), []string{"currency"}},
		{"lower-case currency", capArgs("1", "usd"), []string{"currency"}},
		{"padded currency", capArgs("1", "USD "), []string{"currency"}},
		{"currency absent", `{"amount":1}`, []string{"not_assessable"}},
		{"currency not a string", `{"amount":1,"currency":840}`, []string{"not_assessable"}},
		{"currency null", `{"amount":1,"currency":null}`, []string{"not_assessable"}},
	} {
		var got []string
		for _, h := range tc.evaluate([]byte(c.args)) {
			got = append(got, string(h.Why))
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %s: hits %v, want %v", c.name, c.args, got, c.want)
		}
	}
}

func mustSpec(caps ...string) *ToolSpec { s := newCapTool(caps...).spec; return &s }

// Rules in two currencies on one argument: the call's currency picks the rule, and an uncovered
// currency gets the strictest action among them.
func TestCaps_CurrencyPicksTheRule(t *testing.T) {
	spec := mustSpec(
		`usd: refund.amount[minor, currency=currency] > 5 USD -> refuse`,
		`jpy: refund.amount[minor, currency=currency] > 50000 JPY -> refuse`,
	)
	tc, err := compileCaps(*spec)
	if err != nil {
		t.Fatal(err)
	}
	hit := func(args string) []capHit { return tc.evaluate([]byte(args)) }
	if h := hit(capArgs("500", "USD")); len(h) != 0 {
		t.Fatalf("500 USD: %+v", h)
	}
	if h := hit(capArgs("501", "USD")); len(h) != 1 || h[0].Rule != "usd" || h[0].Action.Kind != 2 || *h[0].Amount != 501 {
		t.Fatalf("501 USD: %+v", h)
	}
	if h := hit(capArgs("50001", "JPY")); len(h) != 1 || h[0].Rule != "jpy" {
		t.Fatalf("50001 JPY: %+v", h)
	}
	// 100 EUR: no rule covers it; the strictest action among the argument's rules is refuse.
	if h := hit(capArgs("100", "EUR")); len(h) != 1 || h[0].Why != capCurrency || h[0].Rule != "usd" || h[0].Action.Kind != 2 || h[0].Currency != "EUR" {
		t.Fatalf("100 EUR: %+v", h)
	}
}

// A [major] amount is a decimal with the currency's exponent, and the limit's exponent comes from the
// ISO table (JPY has none, KWD has three).
func TestCaps_MajorUnitsAndExponents(t *testing.T) {
	spec := &ToolSpec{Name: "refund", Input: json.RawMessage(`{"type":"object","properties":{"amount":{"type":"string"},"currency":{"type":"string"}}}`),
		Caps: []string{`usd: refund.amount[major, currency=currency] > 500.50 USD -> refuse`, `jpy: refund.amount[major, currency=currency] > 500 JPY -> refuse`, `kwd: refund.amount[major, currency=currency] > 5.125 KWD -> refuse`}}
	tc, err := compileCaps(*spec)
	if err != nil {
		t.Fatal(err)
	}
	why := func(amount, cur string) string {
		h := tc.evaluate([]byte(fmt.Sprintf(`{"amount":%q,"currency":%q}`, amount, cur)))
		if len(h) == 0 {
			return ""
		}
		return string(h[0].Why)
	}
	for _, c := range []struct{ amt, cur, want string }{
		{"500.50", "USD", ""}, {"500.51", "USD", "over"}, {"500.505", "USD", "not_assessable"},
		{"500", "JPY", ""}, {"501", "JPY", "over"}, {"500.5", "JPY", "not_assessable"},
		{"5.125", "KWD", ""}, {"5.126", "KWD", "over"}, {"5.1259", "KWD", "not_assessable"},
	} {
		if got := why(c.amt, c.cur); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.amt, c.cur, got, c.want)
		}
	}
}

// --- the engine ---

func capRun(t *testing.T, tool *moneyTool, args string, opts ...Option) (*Result, error, *Journal) {
	t.Helper()
	j := memJournal()
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "refund", args), textTurn("done")}}
	a := mustNew(m, j, append([]Option{WithTools(tool)}, opts...)...)
	res, err := a.Run(context.Background(), "r", UserText("go"))
	return res, err, j
}

func capResultOf(t *testing.T, j *Journal, id string) Record {
	t.Helper()
	recs, err := j.History(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Kind == StepToolResult && r.ToolUseID == id {
			return r
		}
	}
	t.Fatalf("no tool result for %s", id)
	return Record{}
}

func TestCaps_RefuseAtTheDecisionPoint(t *testing.T) {
	tool := newCapTool(refuseUSD)
	res, err, j := capRun(t, tool, capArgs("501", "USD"))
	if err != nil || res.Message.Text() != "done" {
		t.Fatalf("Run = %v, %v", res, err)
	}
	if tool.calls() != 0 {
		t.Fatalf("the refused call ran the tool %d times", tool.calls())
	}
	r := capResultOf(t, j, "c1")
	var text string
	_ = json.Unmarshal(r.Result, &text)
	if !r.IsError || !strings.Contains(text, "big_refund") || !strings.Contains(text, "limit") {
		t.Fatalf("result = %v %s", r.IsError, r.Result)
	}
	for _, leak := range []string{"500", "501", "USD", "50000"} {
		if strings.Contains(text, leak) {
			t.Fatalf("the refusal text %q gives %q", text, leak)
		}
	}
	// The figures are in the journal, in the gate-reason record, and no attempt was claimed.
	rec, ok, err := getGateReason(context.Background(), j, "r", "c1")
	if err != nil || !ok || len(rec.Reasons) != 1 {
		t.Fatalf("gate-reason = %+v %v %v", rec, ok, err)
	}
	g := rec.Reasons[0]
	if g.Kind != gateReasonCap || g.Rule != "big_refund" || g.Action != "refuse" || g.Why != "over" || g.Currency != "USD" || g.Amount == nil || *g.Amount != 501 || g.Limit != 500 {
		t.Fatalf("reason = %+v", g)
	}
	recs, _ := j.History(context.Background(), "r")
	for _, r := range recs {
		if r.Kind == StepAttempt {
			t.Fatalf("a refused call claimed an attempt: %+v", r)
		}
	}
}

func TestCaps_UnderTheLimitRunsAndWritesNoReason(t *testing.T) {
	tool := newCapTool(refuseUSD)
	_, err, j := capRun(t, tool, capArgs("500", "USD"))
	if err != nil || tool.calls() != 1 {
		t.Fatalf("err = %v, calls = %d", err, tool.calls())
	}
	if _, ok, _ := getGateReason(context.Background(), j, "r", "c1"); ok {
		t.Fatal("a call no rule fired for has a gate-reason record")
	}
}

func TestCaps_NotAssessableGetsTheRulesAction(t *testing.T) {
	for _, args := range []string{`{"currency":"USD"}`, `{"amount":-5,"currency":"USD"}`, `{"amount":1,"amount":2,"currency":"USD"}`, capArgs("1", "EUR")} {
		tool := newCapTool(refuseUSD)
		_, err, j := capRun(t, tool, args)
		if err != nil || tool.calls() != 0 {
			t.Errorf("%s: err = %v, tool ran %d times", args, err, tool.calls())
		}
		if r := capResultOf(t, j, "c1"); !r.IsError {
			t.Errorf("%s: not refused", args)
		}
	}
}

// --- the enforcement point (C7) ---

func rewriteAmount(to string) ToolMiddleware {
	return func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			call.Use.Args = json.RawMessage(strings.Replace(string(call.Use.Args), `"amount":10`, `"amount":`+to, 1))
			return next(ctx, call)
		}
	}
}

// A tool middleware that raises the amount after the decision point is refused at the enforcement
// point: the tool is not called and the call is recorded as refused.
func TestCaps_EnforcementRefusesARewrittenAmount(t *testing.T) {
	tool := newCapTool(refuseUSD)
	res, err, j := capRun(t, tool, capArgs("10", "USD"), WithToolMiddleware(rewriteAmount("9000")))
	if err != nil || res.Message.Text() != "done" {
		t.Fatalf("Run = %v, %v", res, err)
	}
	if tool.calls() != 0 {
		t.Fatalf("the tool ran with %v", tool.args)
	}
	r := capResultOf(t, j, "c1")
	if !r.IsError || !strings.Contains(string(r.Result), "big_refund") || strings.Contains(string(r.Result), "9000") {
		t.Fatalf("result = %v %s", r.IsError, r.Result)
	}
	// A rewrite within the limit passes, and the tool receives the rewritten arguments.
	tool = newCapTool(refuseUSD)
	if _, err, _ = capRun(t, tool, capArgs("10", "USD"), WithToolMiddleware(rewriteAmount("20"))); err != nil || tool.calls() != 1 || !strings.Contains(tool.args[0], `"amount":20`) {
		t.Fatalf("err = %v, args = %v", err, tool.args)
	}
	// A rewrite of the currency to one no rule covers is refused too.
	tool = newCapTool(refuseUSD)
	rewriteCur := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			call.Use.Args = json.RawMessage(strings.Replace(string(call.Use.Args), `"USD"`, `"EUR"`, 1))
			return next(ctx, call)
		}
	}
	if _, err, _ = capRun(t, tool, capArgs("10", "USD"), WithToolMiddleware(rewriteCur)); err != nil || tool.calls() != 0 {
		t.Fatalf("a rewritten currency: err = %v, tool ran %d times", err, tool.calls())
	}
}

// --- the gate-reason record only adds ---

// A planted record cannot lift a refusal, and a reason it holds keeps a call refused though the rules
// would not fire now.
func TestCaps_RecordedReasonsOnlyAdd(t *testing.T) {
	ctx := context.Background()
	// An empty record: nothing is removed, an over-cap call is still refused.
	tool := newCapTool(refuseUSD)
	j := memJournal()
	if _, _, err := putGateReason(ctx, j, "r", gateReasonRecord{Format: gateReasonFormat, ToolUseID: "c1", Exempt: "trust me"}); err != nil {
		t.Fatal(err)
	}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "refund", capArgs("501", "USD")), textTurn("done")}}
	if _, err := mustNew(m, j, WithTools(tool)).Run(ctx, "r", UserText("go")); err != nil || tool.calls() != 0 {
		t.Fatalf("an empty planted record: err = %v, tool ran %d times", err, tool.calls())
	}
	// A recorded refuse reason refuses a call that is under the limit now.
	tool = newCapTool(refuseUSD)
	j = memJournal()
	if _, _, err := putGateReason(ctx, j, "r", gateReasonRecord{Format: gateReasonFormat, ToolUseID: "c1", Reasons: []gateReason{{Kind: gateReasonCap, Rule: "big_refund", Action: "refuse", Why: "over", Currency: "USD", Limit: 500}}}); err != nil {
		t.Fatal(err)
	}
	m = &scriptModel{turns: [][]Emit{toolTurn("c1", "refund", capArgs("10", "USD")), textTurn("done")}}
	if _, err := mustNew(m, j, WithTools(tool)).Run(ctx, "r", UserText("go")); err != nil || tool.calls() != 0 {
		t.Fatalf("a recorded refuse: err = %v, tool ran %d times", err, tool.calls())
	}
	// A recorded reason whose action this version cannot honour (a gate has no gate to hold it) refuses
	// too, the strictest thing the call can get.
	tool = newCapTool(refuseUSD)
	j = memJournal()
	if _, _, err := putGateReason(ctx, j, "r", gateReasonRecord{Format: gateReasonFormat, ToolUseID: "c1", Reasons: []gateReason{{Kind: gateReasonCap, Rule: "big_refund", Action: "gate", Why: "over", Currency: "USD", Limit: 500}}}); err != nil {
		t.Fatal(err)
	}
	m = &scriptModel{turns: [][]Emit{toolTurn("c1", "refund", capArgs("10", "USD")), textTurn("done")}}
	if _, err := mustNew(m, j, WithTools(tool)).Run(ctx, "r", UserText("go")); err != nil || tool.calls() != 0 {
		t.Fatalf("a recorded gate: err = %v, tool ran %d times", err, tool.calls())
	}
	if r := capResultOf(t, j, "c1"); !r.IsError {
		t.Fatal("a recorded gate reason did not refuse the call")
	}
	// A record of another format or call is a storage error, not a pass.
	tool = newCapTool(refuseUSD)
	j = memJournal()
	if _, _, err := j.putNew(ctx, "r", gateReasonStep("c1"), Record{Kind: StepValue, Result: json.RawMessage(`{"format":"x"}`)}); err != nil {
		t.Fatal(err)
	}
	m = &scriptModel{turns: [][]Emit{toolTurn("c1", "refund", capArgs("10", "USD")), textTurn("done")}}
	if _, err := mustNew(m, j, WithTools(tool)).Run(ctx, "r", UserText("go")); !errors.Is(err, ErrStorage) || tool.calls() != 0 {
		t.Fatalf("a malformed record: err = %v, tool ran %d times; want ErrStorage", err, tool.calls())
	}
}

// A call decided on a resume decides the same: the refusal is recorded once and a second drive of the
// run replays it.
func TestCaps_RefusalReplays(t *testing.T) {
	ctx := context.Background()
	tool := newCapTool(refuseUSD)
	j := memJournal()
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "refund", capArgs("501", "USD")), textTurn("done")}}
	a := mustNew(m, j, WithTools(tool))
	for range 2 {
		if res, err := a.Run(ctx, "r", UserText("go")); err != nil || res.Message.Text() != "done" || tool.calls() != 0 {
			t.Fatalf("Run = %v %v, tool ran %d times", res, err, tool.calls())
		}
	}
}

// The enforcement point's refusal is the call's recorded result: the tool was not called, and a second
// drive of the run finds the call done and neither halts nor runs the tool.
func TestCaps_EnforcementRefusalIsFinal(t *testing.T) {
	ctx := context.Background()
	tool := newCapTool(refuseUSD) // a side effect: its claim is written before the enforcement point
	j := memJournal()
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "refund", capArgs("10", "USD")), textTurn("done")}}
	a := mustNew(m, j, WithTools(tool), WithToolMiddleware(rewriteAmount("9000")))
	for i := range 2 {
		res, err := a.Run(ctx, "r", UserText("go"))
		if err != nil || res.Message.Text() != "done" || tool.calls() != 0 {
			t.Fatalf("drive %d: %v %v, tool ran %d times", i, res, err, tool.calls())
		}
	}
	if r := capResultOf(t, j, "c1"); !r.IsError {
		t.Fatal("the refusal is not a recorded error result")
	}
}

// A compensation, or a rollback's re-run of a call that already ran, is not cap-checked: the handler
// reaches the tool for an over-limit call under the compensating context, and refuses it otherwise.
func TestCaps_CompensatingContextIsNotChecked(t *testing.T) {
	ctx := context.Background()
	tool := newCapTool(refuseUSD)
	tool.spec.Safety = Safety{Idempotent: true}
	a := mustNew(&scriptModel{}, memJournal(), WithTools(tool))
	use := ToolUse{ID: "c1", Name: "refund", Args: json.RawMessage(capArgs("9000", "USD"))}
	if _, _, err := a.toolHandler("r")(withCompensating(ctx), use); err != nil || tool.calls() != 1 {
		t.Fatalf("compensating: err = %v, tool ran %d times", err, tool.calls())
	}
	if _, _, err := a.toolHandler("r")(ctx, use); err == nil || tool.calls() != 1 {
		t.Fatalf("live: err = %v, tool ran %d times; want a refusal and no new call", err, tool.calls())
	}
}

// effectiveGates keeps the seam for a gate a later feature adds: with an input it stands in for a tool
// with no gate of its own, and never replaces one the tool has. Nothing sets it yet.
func TestEffectiveGates_CapGateSeam(t *testing.T) {
	cg := SingleApproval()
	if got := effectiveGates(&ToolSpec{}, gateInputs{capGate: cg}).Own(); got != cg {
		t.Fatalf("a tool with no gate: %v, want the input's", got)
	}
	own := SingleApproval()
	if got := effectiveGates(&ToolSpec{Approval: own}, gateInputs{capGate: cg}).Own(); got != own {
		t.Fatalf("a tool with its own gate: %v, want its own", got)
	}
	if got := effectiveGates(&ToolSpec{}, gateInputs{}).Own(); got != nil {
		t.Fatalf("no input, no gate: %v", got)
	}
}

// With copies the compiled caps: adding a capped tool to the copy leaves the original's alone.
func TestCaps_WithDoesNotShareTheCapTable(t *testing.T) {
	a := mustNew(&scriptModel{}, memJournal(), WithTools(newCapTool(refuseUSD)))
	other := &moneyTool{spec: ToolSpec{Name: "pay", Input: json.RawMessage(refundSchema), Caps: []string{`p: pay.amount[minor, currency=currency] > 5 USD -> refuse`}}}
	b, err := a.With(WithTools(other))
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := a.caps["pay"]; leaked || b.caps["pay"] == nil || b.caps["refund"] == nil {
		t.Fatalf("original caps %v, copy caps %v", a.caps, b.caps)
	}
}

// A schema reached through $ref on the way to a nested member cannot be checked and is refused.
func TestCaps_RefOnTheWayToAMemberIsRefused(t *testing.T) {
	in := `{"type":"object","properties":{"p":{"$ref":"#/$defs/p"}}}`
	spec := ToolSpec{Name: "refund", Input: json.RawMessage(in), Caps: []string{`r: refund.p.amount[minor, currency=p.cur] > 5 USD -> refuse`}}
	if _, err := compileCaps(spec); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "$ref") {
		t.Fatalf("err = %v, want ErrConfig naming $ref", err)
	}
}

// capDeco embeds a capped tool and declares its own Spec, as a decorator that renames the tool does:
// built afresh, the spec drops the inner tool's caps, and the 9999 USD call below would run through a
// 5 USD refuse rule. Registration refuses it, as it refuses a decorator that hides an approval gate.
type capDeco struct {
	*moneyTool
	keep []string
}

func (d capDeco) Spec() ToolSpec {
	s := d.moneyTool.spec
	s.Caps = d.keep
	return s
}

func TestCaps_EmbeddingDecoratorCannotDropCaps(t *testing.T) {
	line := `big_refund: refund.amount[minor, currency=currency] > 5 USD -> refuse`
	inner := func() *moneyTool { return newCapTool(line) }
	for name, keep := range map[string][]string{
		"none":      nil,
		"different": {`big_refund: refund.amount[minor, currency=currency] > 99999 USD -> refuse`},
	} {
		if _, err := New(&scriptModel{}, memJournal(), WithTools(capDeco{inner(), keep})); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "amount caps") {
			t.Errorf("%s: err = %v, want ErrConfig naming the caps", name, err)
		}
	}
	// A decorator that keeps the rules is accepted, and its call is capped.
	d := capDeco{inner(), []string{line}}
	a, err := New(&scriptModel{turns: [][]Emit{toolTurn("c1", "refund", capArgs("9999", "USD")), textTurn("done")}}, memJournal(), WithTools(d))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "r", UserText("go")); err != nil || d.calls() != 0 {
		t.Fatalf("err = %v, tool ran %d times", err, d.calls())
	}
}
