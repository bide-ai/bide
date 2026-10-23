package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// Provenance declarations and the marks they leave (docs/design/untrusted-content-and-amount-caps.md,
// sections 5.1 and 5.4). Nothing here gates a call.

func TestProvenanceDeclarations_RefuseBadNames(t *testing.T) {
	bad := map[string]string{"empty": "", "control character": "crm\nnotes", "zero-width": "crm​notes", "too long": strings.Repeat("a", 129), "invalid UTF-8": "crm\xff"}
	ok := func(context.Context, struct{}) (string, error) { return "ok", nil }
	for name, s := range bad {
		if _, err := agent.Func("t", "t", ok, agent.Untrusted(s)); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("Untrusted(%s): %v, want ErrConfig", name, err)
		}
		if _, err := agent.Func("t", "t", ok, agent.TaintExempt(s)); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("TaintExempt(%s): %v, want ErrConfig", name, err)
		}
		if err := runOpt(agent.WithUntrustedInput(s)); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("WithUntrustedInput(%s): %v, want ErrConfig", name, err)
		}
		if err := runOpt(agent.WithTrustedInput(s)); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("WithTrustedInput(%s): %v, want ErrConfig", name, err)
		}
	}
	for _, f := range []func() error{
		func() error {
			_, err := agent.Func("t", "t", ok, agent.Untrusted("crm-notes"), agent.TaintExempt("reviewed"))
			return err
		},
	} {
		if err := f(); err != nil {
			t.Errorf("valid declarations: %v", err)
		}
	}
	if err := runOpt(agent.WithUntrustedInput("inbound-email")); err != nil {
		t.Errorf("valid WithUntrustedInput: %v", err)
	}
}

// runOpt applies a run option by driving a run that fails at its model, and returns the option's
// own error (ErrConfig) if the drive refuses it before any model call.
func runOpt(o agent.RunOption) error {
	m := agenttest.NewScriptedModel(agenttest.ErrorTurn(errors.New("model down")))
	a := agenttest.MustNew(m, agenttest.MemJournal())
	_, err := a.Run(context.Background(), "r", agent.UserText("go"), o)
	if errors.Is(err, agent.ErrConfig) {
		return err
	}
	return nil
}

func resultRecord(t *testing.T, j *agent.Journal, run, call string) agent.Record {
	t.Helper()
	r, ok, err := j.Get(context.Background(), run, agent.ToolResultStep(call))
	if err != nil || !ok {
		t.Fatalf("no result record for %s: %v %v", call, ok, err)
	}
	return r
}

func untrustedOf(r agent.Record) string {
	if r.Safety == nil {
		return ""
	}
	return r.Safety.Untrusted
}

// Every path that records a call's result records the tool's mark, with no taint policy: the normal
// result, an error result, a denial, a saga's failure record (and the result of the call before it),
// and nothing is marked for a tool that declares nothing.
func TestUntrusted_MarksEveryResultPath(t *testing.T) {
	ctx := context.Background()
	ok := func(context.Context, struct{}) (string, error) { return "ok", nil }
	boom := func(context.Context, struct{}) (string, error) { return "", errors.New("no seats") }

	// A result, with a side-effecting tool that is not gated: nothing refuses it.
	j := agenttest.MemJournal()
	m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "read", `{}`), agenttest.ToolTurn("c2", "plain", `{}`), agenttest.TextTurn("done"))
	a := agenttest.MustNew(m, j, agent.WithTools(agent.MustFunc("read", "read", ok, agent.Untrusted("crm-notes")), agent.MustFunc("plain", "plain", ok)))
	if _, err := a.Run(ctx, "r", agent.UserText("go")); err != nil {
		t.Fatalf("a run that read an Untrusted tool and called a tool: %v (no policy, so nothing may refuse it)", err)
	}
	if got := untrustedOf(resultRecord(t, j, "r", "c1")); got != "crm-notes" {
		t.Errorf("a result of an Untrusted tool records %q, want crm-notes", got)
	}
	plain := resultRecord(t, j, "r", "c2")
	if untrustedOf(plain) != "" {
		t.Errorf("a result of a tool that declares nothing is marked %q", untrustedOf(plain))
	}
	if b, _ := json.Marshal(plain); strings.Contains(string(b), "untrusted") {
		t.Errorf("an unmarked result's JSON mentions untrusted: %s", b)
	}

	// An error result.
	j = agenttest.MemJournal()
	m = agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "read", `{}`), agenttest.TextTurn("done"))
	a = agenttest.MustNew(m, j, agent.WithTools(agent.MustFunc("read", "read", boom, agent.Untrusted("crm-notes"))))
	if _, err := a.Run(ctx, "e", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if r := resultRecord(t, j, "e", "c1"); !r.IsError || untrustedOf(r) != "crm-notes" {
		t.Errorf("an error result: IsError %v, mark %q; want an error result marked crm-notes", r.IsError, untrustedOf(r))
	}

	// A denial.
	j = agenttest.MemJournal()
	gated := agent.MustFunc("read", "read", ok, agent.Untrusted("crm-notes"), agent.WithApproval(agent.SingleApproval()))
	newAgent := func() *agent.Agent {
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "read", `{}`), agenttest.TextTurn("done"))
		return agenttest.MustNew(m, j, agent.WithTools(gated))
	}
	if _, err := newAgent().Run(ctx, "d", agent.UserText("go")); !isApprovalPending(err) {
		t.Fatalf("Run = %v, want ApprovalPending", err)
	}
	if err := agent.Approve(ctx, j, "d", "c1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := newAgent().Run(ctx, "d", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if r := resultRecord(t, j, "d", "c1"); !r.IsError || untrustedOf(r) != "crm-notes" {
		t.Errorf("a denial: IsError %v, mark %q; want a denial marked crm-notes", r.IsError, untrustedOf(r))
	}

	// A saga's failure record.
	j = agenttest.MemJournal()
	m = agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "read", `{}`), agenttest.TextTurn("done"))
	a = agenttest.MustNew(m, j, agent.WithTools(agent.MustFunc("read", "read", boom, agent.Untrusted("crm-notes"))))
	_, _ = a.Run(ctx, "s", agent.UserText("go"), agent.WithSaga())
	recs, err := j.History(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, r := range recs {
		if r.Kind == agent.StepSagaFail {
			seen = true
			if untrustedOf(r) != "crm-notes" {
				t.Errorf("a saga_fail record's mark = %q, want crm-notes", untrustedOf(r))
			}
		}
	}
	if !seen {
		t.Error("the saga recorded no saga_fail")
	}
}

// A source given through WithSafety is a declaration, never ignored: it is recorded as the Untrusted
// option's is; the same source twice is one declaration; two sources are ErrConfig.
func TestUntrusted_SafetyFieldIsADeclaration(t *testing.T) {
	ctx := context.Background()
	ok := func(context.Context, struct{}) (string, error) { return "ok", nil }
	run := func(opts ...agent.ToolOption) (*agent.Journal, error) {
		j := agenttest.MemJournal()
		m := agenttest.NewScriptedModel(agenttest.ToolTurn("c1", "t", `{}`), agenttest.TextTurn("done"))
		a, err := agent.New(m, j, agent.WithTools(agent.MustFunc("t", "t", ok, opts...)))
		if err != nil {
			return nil, err
		}
		_, err = a.Run(ctx, "r", agent.UserText("go"))
		return j, err
	}
	j, err := run(agent.WithSafety(agent.Safety{Idempotent: true, Untrusted: "inbox"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := untrustedOf(resultRecord(t, j, "r", "c1")); got != "inbox" {
		t.Fatalf("a source given through WithSafety was recorded as %q, want inbox", got)
	}
	for name, opts := range map[string][]agent.ToolOption{
		"the option then WithSafety, same source": {agent.Untrusted("inbox"), agent.WithSafety(agent.Safety{Untrusted: "inbox"})},
		"WithSafety then the option, same source": {agent.WithSafety(agent.Safety{Untrusted: "inbox"}), agent.Untrusted("inbox")},
	} {
		j, err := run(opts...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := untrustedOf(resultRecord(t, j, "r", "c1")); got != "inbox" {
			t.Errorf("%s: recorded %q, want inbox", name, got)
		}
	}
	if _, err := run(agent.Untrusted("inbox"), agent.WithSafety(agent.Safety{Untrusted: "web"})); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("two different sources = %v, want ErrConfig", err)
	}
	if _, err := run(agent.WithSafety(agent.Safety{Untrusted: "bad\nname"})); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("an invalid name through Safety = %v, want ErrConfig", err)
	}
}

// A tool that wraps an Untrusted tool and hides the declaration is refused, as one that hides its gate.
type hidingTool struct{ agent.Tool }

func (h hidingTool) Spec() agent.ToolSpec {
	s := h.Tool.Spec()
	s.Untrusted = ""
	return s
}

// hidingSafetyTool hides a declaration made through Safety, too.
type hidingSafetyTool struct{ agent.Tool }

func (h hidingSafetyTool) Spec() agent.ToolSpec {
	s := h.Tool.Spec()
	s.Untrusted, s.Safety.Untrusted = "", ""
	return s
}

func TestUntrusted_DecoratorCannotHideASafetyDeclaration(t *testing.T) {
	ok := func(context.Context, struct{}) (string, error) { return "ok", nil }
	inner := agent.MustFunc("t", "t", ok, agent.WithSafety(agent.Safety{Untrusted: "crm-notes"}))
	_, err := agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("x")), agenttest.MemJournal(), agent.WithTools(hidingSafetyTool{inner}))
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("New with a decorator that hides a Safety declaration = %v, want ErrConfig", err)
	}
}

func TestUntrusted_DecoratorCannotHideTheDeclaration(t *testing.T) {
	ok := func(context.Context, struct{}) (string, error) { return "ok", nil }
	inner := agent.MustFunc("t", "t", ok, agent.Untrusted("crm-notes"))
	_, err := agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("x")), agenttest.MemJournal(), agent.WithTools(hidingTool{inner}))
	if !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("New with a decorator that hides Untrusted = %v, want ErrConfig", err)
	}
}

func TestRunInput_ProvenanceIsJournaledAndHeldTo(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	failing := func() *agent.Agent {
		return agenttest.MustNew(agenttest.NewScriptedModel(agenttest.ErrorTurn(errors.New("model down"))), j)
	}
	if _, err := failing().Run(ctx, "r", agent.UserText("go"), agent.WithUntrustedInput("inbound-email")); err == nil {
		t.Fatal("seeding drive did not stop")
	}
	start, ok, err := agent.RecordedStart(ctx, j, "r")
	if err != nil || !ok || start.Settings.UntrustedInput == nil || *start.Settings.UntrustedInput != "inbound-email" || start.Settings.TrustedInput != nil {
		t.Fatalf("RecordedStart settings = %+v, %v, %v; want untrusted input inbound-email", start.Settings, ok, err)
	}
	for name, o := range map[string]agent.RunOption{"another source": agent.WithUntrustedInput("web"), "a trusted declaration": agent.WithTrustedInput("ours")} {
		if _, err := failing().Run(ctx, "r", agent.UserText("go"), o); !errors.Is(err, agent.ErrConfig) {
			t.Errorf("a drive with %s = %v, want ErrConfig", name, err)
		}
	}
	// A drive that passes none, and one that passes the same, run under the journaled one.
	for _, opts := range [][]agent.RunOption{nil, {agent.WithUntrustedInput("inbound-email")}} {
		m := agenttest.NewScriptedModel(agenttest.TextTurn("done"))
		if _, err := agenttest.MustNew(m, j).Run(ctx, "r", agent.UserText("go"), opts...); err != nil {
			t.Errorf("a drive under the journaled provenance: %v", err)
		}
	}
	// Both declarations at once.
	if _, err := failing().Run(ctx, "both", agent.UserText("go"), agent.WithUntrustedInput("a"), agent.WithTrustedInput("b")); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("both declarations = %v, want ErrConfig", err)
	}
	// A run that states nothing records nothing, and its run:start bytes carry no new member.
	j2 := agenttest.MemJournal()
	m := agenttest.NewScriptedModel(agenttest.TextTurn("done"))
	if _, err := agenttest.MustNew(m, j2).Run(ctx, "plain", agent.UserText("go")); err != nil {
		t.Fatal(err)
	}
	r, _, _ := j2.Get(ctx, "plain", "run:start")
	if strings.Contains(string(r.Result), "untrusted") || strings.Contains(string(r.Result), "trusted_input") {
		t.Errorf("a run with no provenance recorded a member: %s", r.Result)
	}
}

func TestRetrievalUntrusted_MarkIsRecordedWithTheStep(t *testing.T) {
	ctx := context.Background()
	for name, opts := range map[string][]agent.RetrievalOption{"declared": {agent.RetrievalUntrusted("kb")}, "not declared": nil} {
		j := agenttest.MemJournal()
		a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("done")), j, agent.WithRetrieval(staticRetriever{{Text: "doc"}}, 1, opts...))
		if _, err := a.Run(ctx, "r", agent.UserText("go")); err != nil {
			t.Fatal(err)
		}
		r, ok, err := j.Get(ctx, "r", "@retrieval/0")
		if err != nil || !ok {
			t.Fatalf("%s: no retrieval record: %v %v", name, ok, err)
		}
		has := strings.Contains(string(r.Result), `"untrusted":"kb"`)
		if has != (name == "declared") {
			t.Errorf("%s: retrieval record %s", name, r.Result)
		}
	}
	if _, err := agent.New(agenttest.NewScriptedModel(), agenttest.MemJournal(), agent.WithRetrieval(staticRetriever{}, 1, agent.RetrievalUntrusted(""))); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("RetrievalUntrusted(\"\") = %v, want ErrConfig", err)
	}
}
