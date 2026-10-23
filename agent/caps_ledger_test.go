package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/capledger"
)

// Cumulative caps end to end: the rules reserve in the cap ledger at the tool handler, after the attempt
// claim and before the call begins.

const sumPerDay = `refund_total: refund.amount[minor, currency=currency] sum 10 USD per day -> refuse`

var capNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func capClockOpt() Option { return WithClock(func() time.Time { return capNow }) }

// sumTool is a refund tool with the given cumulative lines, with an optional member for a key.
func sumTool(safety Safety, lines ...string) *moneyTool {
	schema := `{"type":"object","properties":{"amount":{"type":"integer"},"currency":{"type":"string"},"customer":{"type":"string"}},"required":["amount","currency"]}`
	return &moneyTool{spec: ToolSpec{Name: "refund", Input: json.RawMessage(schema), Caps: lines, Safety: safety}}
}

// runSum runs one refund call of args as run runID of j, with the tool and options.
func runSum(t *testing.T, j *Journal, runID, args string, tool Tool, opts ...Option) (*Result, error) {
	t.Helper()
	a := mustNew(capModel{args}, j, append([]Option{WithTools(tool), capClockOpt()}, opts...)...)
	return a.Run(context.Background(), runID, UserText("go"))
}

// capModel calls refund once (call c1) with args, then answers; its answer depends only on the
// conversation, so a re-drive of the run gets the same turns.
type capModel struct{ args string }

// capModelFor is capModel for a named tool.
type capModelFor struct{ tool, args string }

func (m capModelFor) Stream(ctx context.Context, req Request) (*Stream, error) {
	emits := toolTurn("c1", m.tool, m.args)
	for _, msg := range req.Messages {
		if msg.Role == RoleTool {
			emits = textTurn("done")
		}
	}
	ch := make(chan Emit, len(emits))
	for _, e := range emits {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

func (m capModel) Stream(_ context.Context, req Request) (*Stream, error) {
	emits := toolTurn("c1", "refund", m.args)
	for _, msg := range req.Messages {
		if msg.Role == RoleTool {
			emits = textTurn("done")
		}
	}
	ch := make(chan Emit, len(emits))
	for _, e := range emits {
		ch <- e
	}
	close(ch)
	return NewStream(ch), nil
}

func sumArgs(amount int, customer string) string {
	if customer == "" {
		return fmt.Sprintf(`{"amount":%d,"currency":"USD"}`, amount)
	}
	return fmt.Sprintf(`{"amount":%d,"currency":"USD","customer":%q}`, amount, customer)
}

// ledgerSlots reads the live slots of the (only) ledger run of the store, by the rule and key digest.
func ledgerSlots(t *testing.T, j *Journal, rule, key string) []capledger.Slot {
	t.Helper()
	l := newCapLedger(j, func() time.Time { return capNow }, 0)
	run := l.LedgerRun(capledger.Rule{Namespace: defaultCapNamespace, Name: rule, Currency: "USD", Kind: capledger.Day}, "2026-10-22", capledger.KeyDigest(key))
	live, err := l.Live(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	return live
}

func resultText(t *testing.T, j *Journal, runID string) (string, bool) {
	t.Helper()
	recs, err := j.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Kind == StepToolResult && r.ToolUseID == "c1" {
			var text string
			_ = json.Unmarshal(r.Result, &text)
			return text + string(r.Result), r.IsError
		}
	}
	t.Fatalf("no result in %s", runID)
	return "", false
}

func TestCapsSum_RegistrationChecksTheKey(t *testing.T) {
	schema := func(props string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":{"amount":{"type":"integer"},"currency":{"type":"string"},` + props + `}}`)
	}
	for _, tc := range []struct {
		name, props, line, want string
	}{
		{"key member", `"customer":{"type":"string"}`, `r: refund.amount[minor, currency=currency] sum 10 USD per day by customer -> refuse`, ""},
		{"integer key", `"customer":{"type":"integer"}`, `r: refund.amount[minor, currency=currency] sum 10 USD per day by customer -> refuse`, ""},
		{"principal needs no member", `"x":{"type":"string"}`, `r: refund.amount[minor, currency=currency] sum 10 USD per hour by principal -> refuse`, ""},
		{"per run", `"x":{"type":"string"}`, `r: refund.amount[minor, currency=currency] sum 10 USD per run -> refuse`, ""},
		{"no key member", `"x":{"type":"string"}`, `r: refund.amount[minor, currency=currency] sum 10 USD per day by customer -> refuse`, "no member"},
		{"array key", `"customer":{"type":"array","items":{"type":"string"}}`, `r: refund.amount[minor, currency=currency] sum 10 USD per day by customer -> refuse`, "array"},
		{"boolean key", `"customer":{"type":"boolean"}`, `r: refund.amount[minor, currency=currency] sum 10 USD per day by customer -> refuse`, "want string"},
		{"gate", `"x":{"type":"string"}`, `r: refund.amount[minor, currency=currency] sum 10 USD per day -> gate`, "escalation gate"},
	} {
		_, err := New(&scriptModel{}, memJournal(), WithTools(&moneyTool{spec: ToolSpec{Name: "refund", Input: schema(tc.props), Caps: []string{tc.line}}}))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (!errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want ErrConfig containing %q", tc.name, err, tc.want)
		}
	}
	if _, err := New(&scriptModel{}, memJournal(), WithCapNamespace("")); !errors.Is(err, ErrConfig) {
		t.Errorf("an empty namespace: %v", err)
	}
}

// The window fills across runs of one store: the call that would take it over is refused, the tool not
// called, and a refusal gives no figures.
func TestCapsSum_RefusesTheCallThatOverflowsTheWindow(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{}, sumPerDay)
	for i, amount := range []int{400, 400} {
		if _, err := runSum(t, j, fmt.Sprintf("r%d", i), sumArgs(amount, ""), tool); err != nil {
			t.Fatal(err)
		}
	}
	if tool.calls() != 2 {
		t.Fatalf("%d calls, want 2", tool.calls())
	}
	if _, err := runSum(t, j, "r2", sumArgs(400, ""), tool); err != nil {
		t.Fatal(err)
	}
	if tool.calls() != 2 {
		t.Fatalf("the call over the limit ran the tool")
	}
	text, isErr := resultText(t, j, "r2")
	if !isErr || !strings.Contains(text, "refund_total") || !strings.Contains(text, "limit") {
		t.Fatalf("result = %v %s", isErr, text)
	}
	for _, leak := range []string{"1000", "800", "400", "200"} {
		if strings.Contains(text, leak) {
			t.Fatalf("the refusal text %q gives %q", text, leak)
		}
	}
	// The refusal is at the enforcement point, after the attempt was claimed, and writes no gate-reason (a
	// per-call decision does); a per-call refusal would have been before the claim.
	recs, _ := j.History(context.Background(), "r2")
	claimed := false
	for _, r := range recs {
		claimed = claimed || r.Kind == StepAttempt
	}
	if _, has, _ := getGateReason(context.Background(), j, "r2", "c1"); !claimed || has {
		t.Fatalf("claimed = %v, gate-reason = %v: want the refusal after the claim and no gate-reason", claimed, has)
	}
	// A refused call holds no slot; the window still has 200.
	if live := ledgerSlots(t, j, "refund_total", ""); len(live) != 2 {
		t.Fatalf("%d live slots, want 2", len(live))
	}
	if _, err := runSum(t, j, "r3", sumArgs(200, ""), tool); err != nil || tool.calls() != 3 {
		t.Fatalf("exactly the limit: err = %v, calls = %d, want it run", err, tool.calls())
	}
	// An amount over the whole limit is refused the same way (at the handler, after the claim, no gate-reason),
	// not as a per-call rule at the decision point.
	if _, err := runSum(t, j, "r5", sumArgs(1500, ""), tool); err != nil || tool.calls() != 3 {
		t.Fatalf("an amount over the limit: err = %v, calls = %d", err, tool.calls())
	}
	recs5, _ := j.History(context.Background(), "r5")
	claimed5 := false
	for _, r := range recs5 {
		claimed5 = claimed5 || r.Kind == StepAttempt
	}
	if _, has, _ := getGateReason(context.Background(), j, "r5", "c1"); !claimed5 || has {
		t.Fatalf("over the whole limit: claimed = %v, gate-reason = %v", claimed5, has)
	}
	if _, err := runSum(t, j, "r4", sumArgs(1, ""), tool); err != nil || tool.calls() != 3 {
		t.Fatalf("one over the limit: err = %v, calls = %d, want it refused", err, tool.calls())
	}
}

// probeTool runs probe inside Call, before the wrapped tool does.
type probeTool struct {
	Tool
	probe func()
}

func (p probeTool) Call(ctx context.Context, a json.RawMessage) (json.RawMessage, error) {
	p.probe()
	return p.Tool.Call(ctx, a)
}

// The reservation is made before the tool begins: when Call runs, its slot is in the ledger.
func TestCapsSum_ReservesBeforeTheToolBegins(t *testing.T) {
	j := memJournal()
	var seen []capledger.Slot
	tool := probeTool{Tool: sumTool(Safety{}, sumPerDay), probe: func() { seen = ledgerSlots(t, j, "refund_total", "") }}
	if _, err := runSum(t, j, "r0", sumArgs(10, ""), tool); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Amount != 10 || !seen[0].Marked {
		t.Fatalf("when the tool began the ledger held %+v, want its own marked slot of 10", seen)
	}
}

// The claim comes first: a call whose attempt another driver already claimed halts and reserves nothing.
func TestCapsSum_NoSlotForACallThatLostTheClaim(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{}, sumPerDay)
	if _, _, _, err := claimNextAttempt(context.Background(), j, "r0", toolAttemptStep("c1"), Record{Kind: StepAttempt, ToolUseID: "c1"}); err != nil {
		t.Fatal(err)
	}
	_, err := runSum(t, j, "r0", sumArgs(10, ""), tool)
	if _, halted := errors.AsType[*OutcomeUnknown](err); !halted || tool.calls() != 0 {
		t.Fatalf("err = %v, calls = %d, want a halt without a call", err, tool.calls())
	}
	if live := ledgerSlots(t, j, "refund_total", ""); len(live) != 0 {
		t.Fatalf("the halted call reserved %+v", live)
	}
}

func TestCapsSum_KeysAreSeparateWindowsAndNormalised(t *testing.T) {
	j := memJournal()
	line := `per_customer: refund.amount[minor, currency=currency] sum 5 USD per day by customer lower trim -> refuse`
	tool := sumTool(Safety{}, line)
	n := 0
	run := func(amount int, customer string) {
		n++
		if _, err := runSum(t, j, fmt.Sprintf("k%d", n), sumArgs(amount, customer), tool); err != nil {
			t.Fatal(err)
		}
	}
	run(400, "C-17")
	run(400, "C-18") // another customer: its own window
	before := tool.calls()
	run(400, " c-17 ") // the same customer spelled otherwise: normalised, so the same window, over
	if tool.calls() != before {
		t.Fatalf("a spelling of customer C-17 got a window of its own")
	}
	if live := ledgerSlots(t, j, "per_customer", "c-17"); len(live) != 1 || live[0].Amount != 400 {
		t.Fatalf("c-17 holds %+v", live)
	}
	if live := ledgerSlots(t, j, "per_customer", "c-18"); len(live) != 1 {
		t.Fatalf("c-18 holds %+v", live)
	}
}

func TestCapsSum_NotAssessableIsRefusedAndReservesNothing(t *testing.T) {
	line := `per_customer: refund.amount[minor, currency=currency] sum 5 USD per day by customer -> refuse`
	for name, args := range map[string]string{
		"negative amount":      `{"amount":-80,"currency":"USD","customer":"c"}`,
		"fractional minor":     `{"amount":1.5,"currency":"USD","customer":"c"}`,
		"exponent":             `{"amount":1e2,"currency":"USD","customer":"c"}`,
		"missing key":          `{"amount":10,"currency":"USD"}`,
		"null key":             `{"amount":10,"currency":"USD","customer":null}`,
		"empty key":            `{"amount":10,"currency":"USD","customer":""}`,
		"non-ASCII key":        `{"amount":10,"currency":"USD","customer":"Zoë"}`,
		"control byte in key":  `{"amount":10,"currency":"USD","customer":"a\u0001b"}`,
		"currency not covered": `{"amount":10,"currency":"EUR","customer":"c"}`,
		"duplicate member":     `{"amount":10,"amount":20,"currency":"USD","customer":"c"}`,
		"case-variant member":  `{"amount":10,"Amount":20,"currency":"USD","customer":"c"}`,
	} {
		j := memJournal()
		tool := sumTool(Safety{}, line)
		if _, err := runSum(t, j, "r0", args, tool); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if tool.calls() != 0 {
			t.Errorf("%s: the tool ran", name)
		}
		if _, isErr := resultText(t, j, "r0"); !isErr {
			t.Errorf("%s: no refusal recorded", name)
		}
		if live := ledgerSlots(t, j, "per_customer", "c"); len(live) != 0 {
			t.Errorf("%s: reserved %+v", name, live)
		}
	}
}

// A zero amount counts nothing: the tool runs and no ledger is written.
func TestCapsSum_ZeroIsNeverReserved(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{}, sumPerDay)
	if _, err := runSum(t, j, "r0", sumArgs(0, ""), tool); err != nil || tool.calls() != 1 {
		t.Fatalf("err = %v, calls = %d", err, tool.calls())
	}
	if live := ledgerSlots(t, j, "refund_total", ""); len(live) != 0 {
		t.Fatalf("a zero amount reserved %+v", live)
	}
	runs := 0
	for id, err := range j.Store().(Lister).Runs(context.Background(), RunFilter{Prefix: capRunPrefix}) {
		if err != nil {
			t.Fatal(err)
		}
		_ = id
		runs++
	}
	if runs != 0 {
		t.Fatalf("%d ledger runs for a zero amount", runs)
	}
}

func TestCapsSum_ByPrincipalAndPerRun(t *testing.T) {
	line := `p: refund.amount[minor, currency=currency] sum 5 USD per hour by principal -> refuse`
	j := memJournal()
	tool := sumTool(Safety{}, line)
	id := WithIdentity(Identity{Actor: "a", OnBehalfOf: "desk-7"})
	if _, err := runSum(t, j, "r0", sumArgs(400, ""), tool, id); err != nil || tool.calls() != 1 {
		t.Fatalf("err = %v calls %d", err, tool.calls())
	}
	if _, err := runSum(t, j, "r1", sumArgs(400, ""), tool, id); err != nil || tool.calls() != 1 {
		t.Fatalf("the same principal over its window: err = %v calls %d", err, tool.calls())
	}
	// No principal: not assessable.
	if _, err := runSum(t, j, "r2", sumArgs(1, ""), tool); err != nil || tool.calls() != 1 {
		t.Fatalf("no principal: err = %v calls %d, want refused", err, tool.calls())
	}
	// Per run: a window per run tree, so two runs do not share it.
	perRun := `q: refund.amount[minor, currency=currency] sum 5 USD per run -> refuse`
	j2 := memJournal()
	tool2 := sumTool(Safety{}, perRun)
	for _, rid := range []string{"a", "b"} {
		if _, err := runSum(t, j2, rid, sumArgs(400, ""), tool2); err != nil {
			t.Fatal(err)
		}
	}
	if tool2.calls() != 2 {
		t.Fatalf("per run: %d calls, want one in each run", tool2.calls())
	}
}

// Two tools share one rule, so one window.
func TestCapsSum_RuleSharedByTwoToolsIsOneWindow(t *testing.T) {
	line := `shared: refund|refund2.amount[minor, currency=currency] sum 5 USD per day -> refuse`
	j := memJournal()
	mk := func(name string) *moneyTool {
		s := sumTool(Safety{}, line)
		s.spec.Name = name
		return s
	}
	t1, t2 := mk("refund"), mk("refund2")
	run := func(rid, tool, args string) {
		a := mustNew(capModelFor{tool, args}, j, WithTools(t1, t2), capClockOpt())
		if _, err := a.Run(context.Background(), rid, UserText("go")); err != nil {
			t.Fatal(err)
		}
	}
	run("a", "refund", sumArgs(300, ""))
	run("b", "refund2", sumArgs(300, ""))
	if t1.calls() != 1 || t2.calls() != 0 {
		t.Fatalf("calls %d and %d, want the second tool refused by the first's reservation", t1.calls(), t2.calls())
	}
}

// A retry-safe tool has no marker: its attempt is the tool-use ID, so a middleware that calls next twice
// reserves once, and the slot is unmarked.
func TestCapsSum_IdempotentToolReservesOncePerToolUseID(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{Idempotent: true}, sumPerDay)
	twice := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			if _, err := next(ctx, call); err != nil {
				return nil, err
			}
			return next(ctx, call)
		}
	}
	if _, err := runSum(t, j, "r0", sumArgs(300, ""), tool, WithToolMiddleware(twice)); err != nil {
		t.Fatal(err)
	}
	live := ledgerSlots(t, j, "refund_total", "")
	if len(live) != 1 || live[0].Marked || live[0].Amount != 300 {
		t.Fatalf("live = %+v, want one unmarked slot of 300", live)
	}
}

// A marked tool whose middleware calls next twice: the second invocation is refused (it already ran) and
// reserves nothing more.
func TestCapsSum_SecondInvocationOfAMarkedCallReservesNothingMore(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{}, sumPerDay)
	twice := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			r, err := next(ctx, call)
			if err != nil {
				return r, err
			}
			next(ctx, call) // refused: already ran
			return r, nil
		}
	}
	if _, err := runSum(t, j, "r0", sumArgs(300, ""), tool, WithToolMiddleware(twice)); err != nil {
		t.Fatal(err)
	}
	if tool.calls() != 1 {
		t.Fatalf("%d calls", tool.calls())
	}
	live := ledgerSlots(t, j, "refund_total", "")
	if len(live) != 1 || !live[0].Marked || live[0].Amount != 300 {
		t.Fatalf("live = %+v, want one marked slot", live)
	}
}

// A rewritten amount is what is reserved (the arguments the tool receives).
func TestCapsSum_ReservesTheRewrittenAmount(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{}, sumPerDay)
	rewrite := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			call.Use.Args = json.RawMessage(sumArgs(900, ""))
			return next(ctx, call)
		}
	}
	if _, err := runSum(t, j, "r0", sumArgs(10, ""), tool, WithToolMiddleware(rewrite)); err != nil {
		t.Fatal(err)
	}
	if live := ledgerSlots(t, j, "refund_total", ""); len(live) != 1 || live[0].Amount != 900 {
		t.Fatalf("live = %+v, want 900", live)
	}
}

// Two cumulative rules on one call hold a slot in each ledger.
func TestCapsSum_TwoRulesHoldASlotEach(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{}, sumPerDay, `refund_hour: refund.amount[minor, currency=currency] sum 1 USD per hour -> refuse`)
	if _, err := runSum(t, j, "r0", sumArgs(50, ""), tool); err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"refund_total"} {
		if live := ledgerSlots(t, j, rule, ""); len(live) != 1 {
			t.Fatalf("%s: %+v", rule, live)
		}
	}
	l := newCapLedger(j, func() time.Time { return capNow }, 0)
	hr := l.LedgerRun(capledger.Rule{Namespace: defaultCapNamespace, Name: "refund_hour", Currency: "USD", Kind: capledger.Hour}, "2026-10-22T12", capledger.KeyDigest(""))
	if live, err := l.Live(context.Background(), hr); err != nil || len(live) != 1 {
		t.Fatalf("hour ledger: %+v %v", live, err)
	}
}

// Another namespace is another ledger.
func TestCapsSum_NamespacesAreSeparateLedgers(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{}, sumPerDay)
	if _, err := runSum(t, j, "r0", sumArgs(900, ""), tool); err != nil {
		t.Fatal(err)
	}
	if _, err := runSum(t, j, "r1", sumArgs(900, ""), tool, WithCapNamespace("other")); err != nil || tool.calls() != 2 {
		t.Fatalf("another namespace: err = %v calls %d, want it to have its own window", err, tool.calls())
	}
}

// A store that cannot write the ledger means the tool is not run: the run fails, nothing is recorded, a
// re-drive reaches the call again (fail closed).
func TestCapsSum_FailsClosedOnAStoreError(t *testing.T) {
	mem := NewMemStore()
	tool := sumTool(Safety{}, sumPerDay)
	fs := &failSlotStore{Store: mem}
	j := newJournal(fs)
	_, err := runSum(t, j, "r0", sumArgs(10, ""), tool)
	if err == nil || tool.calls() != 0 {
		t.Fatalf("err = %v, calls = %d, want a failed run and no call", err, tool.calls())
	}
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("err = %v, want ErrStorage", err)
	}
	recs, _ := newJournal(mem).History(context.Background(), "r0")
	for _, r := range recs {
		if r.Kind == StepToolResult {
			t.Fatalf("a result was recorded: %+v", r)
		}
	}
	// The store is back: a re-drive reserves and runs the call.
	fs.fail = false
	if _, err := runSum(t, newJournal(mem), "r0", sumArgs(10, ""), tool); err != nil || tool.calls() != 1 {
		t.Fatalf("re-drive: err = %v, calls = %d", err, tool.calls())
	}
}

// failSlotStore fails every insert of a ledger slot while fail is set.
type failSlotStore struct {
	Store
	fail bool
	once bool
}

func (f *failSlotStore) Insert(ctx context.Context, run, name string, data []byte) (Entry, bool, error) {
	if !f.once {
		f.fail, f.once = true, true
	}
	if f.fail && strings.HasPrefix(name, "slot:") {
		return Entry{}, false, errors.New("store down")
	}
	return f.Store.Insert(ctx, run, name, data)
}

// Crash at every write of a run with a cumulative rule: the tool fires at most once, the attempt holds at
// most one live slot, and whenever the tool fired the window counts it.
func TestCapsSum_CrashAtEveryWrite(t *testing.T) {
	for crashAt := 1; crashAt <= 14; crashAt++ {
		t.Run(fmt.Sprintf("crash-%d", crashAt), func(t *testing.T) {
			mem := newJournal(NewMemStore())
			tool := sumTool(Safety{}, sumPerDay)
			a := mustNew(capModel{sumArgs(300, "")}, crashJournal(mem, crashAt), WithTools(tool), capClockOpt())
			_, _ = a.Run(context.Background(), "r0", UserText("go"))
			a2 := mustNew(capModel{sumArgs(300, "")}, mem, WithTools(tool), capClockOpt())
			_, _ = a2.Run(context.Background(), "r0", UserText("go")) // the resumed run: may halt for the unknown outcome
			live := ledgerSlots(t, mem, "refund_total", "")
			if len(live) > 1 {
				t.Fatalf("live slots %+v", live)
			}
			if tool.calls() > 1 {
				t.Fatalf("the tool fired %d times", tool.calls())
			}
			if tool.calls() == 1 && len(live) != 1 {
				t.Fatalf("the tool fired and the window does not count it: %+v", live)
			}
		})
	}
}
