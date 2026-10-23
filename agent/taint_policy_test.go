package agent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// PR 9a of the taint build: the policy value, the digest, run:start's record, the sticky status down the
// tree and along a session, and the refusal by an agent with no policy. Nothing is gated yet.

func gatePolicy() taintPolicy {
	return taintPolicy{Gate: &ApprovalPolicy{Need: 2, Approvers: []string{"ops", "risk"}, ExcludePrincipal: true}}
}

func noKeys() Option {
	return WithApproverVerifiers(func(string) (ApproverVerifier, bool) { return nil, false })
}

type counter struct{ n atomic.Int32 }

func (c *counter) tool(name string, opts ...ToolOption) Tool {
	return MustFunc(name, name, func(context.Context, struct{}) (string, error) { c.n.Add(1); return "ok", nil }, opts...)
}

func startOf(t *testing.T, j *Journal, run string) RunStart {
	t.Helper()
	s, ok, err := RecordedStart(context.Background(), j, run)
	if err != nil || !ok {
		t.Fatalf("no run:start for %s: %v %v", run, ok, err)
	}
	return s
}

// refundTool is a money tool named "refund" carrying the cap rule line.
func refundTool(line string) Tool {
	type in struct {
		Amount   int64  `json:"amount"`
		Currency string `json:"currency"`
	}
	return MustFunc("refund", "refund", func(context.Context, in) (string, error) { return "ok", nil }, WithCaps(line))
}

func TestTaintPolicy_Validation(t *testing.T) {
	mk := func(p taintPolicy, extra ...Option) error {
		_, err := New(NewScriptedModel(TextTurn("x")), memJournal(), append([]Option{withTaintPolicy(p)}, extra...)...)
		return err
	}
	for name, p := range map[string]taintPolicy{
		"neither gate nor refuse": {},
		"both gate and refuse":    {Gate: SingleApproval(), Refuse: true},
		"an invalid gate":         {Gate: &ApprovalPolicy{Need: 3, Approvers: []string{"a"}}},
	} {
		if err := mk(p); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v, want ErrConfig", name, err)
		}
	}
	if err := mk(gatePolicy()); !errors.Is(err, ErrConfig) {
		t.Errorf("an m-of-n gate with no approver verifiers = %v, want ErrConfig", err)
	}
	for name, p := range map[string]taintPolicy{"refuse": {Refuse: true}, "single": {Gate: SingleApproval()}} {
		if err := mk(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Two approvers on one key fill one seat: the policy's Gate is refused as a tool's would be.
	shared := WithApproverVerifiers(func(string) (ApproverVerifier, bool) { return sharedKeyVerifier{}, true })
	if err := mk(gatePolicy(), shared); !errors.Is(err, ErrConfig) {
		t.Errorf("a Gate whose approvers share a key = %v, want ErrConfig", err)
	}
	if err := mk(gatePolicy(), noKeys()); err != nil {
		t.Errorf("an m-of-n gate with verifiers: %v", err)
	}
}

func TestTaintDigest_StableAndSensitive(t *testing.T) {
	var c counter
	build := func(p taintPolicy, tools ...Tool) *Agent {
		return mustNew(NewScriptedModel(TextTurn("x")), memJournal(), WithTools(tools...), withTaintPolicy(p), noKeys())
	}
	base := build(gatePolicy(), c.tool("a"), c.tool("b", Untrusted("crm")))
	if d := base.taintDigest(); len(d) != 64 || d != build(gatePolicy(), c.tool("a"), c.tool("b", Untrusted("crm"))).taintDigest() {
		t.Fatalf("digest %q is not a stable SHA-256", d)
	}
	other := gatePolicy()
	other.Gate.Need = 1
	refuse := taintPolicy{Refuse: true}
	require := gatePolicy()
	require.RequireProvenance = true
	for name, a := range map[string]*Agent{
		"another gate":             build(other, c.tool("a"), c.tool("b", Untrusted("crm"))),
		"refuse instead of a gate": build(refuse, c.tool("a"), c.tool("b", Untrusted("crm"))),
		"RequireProvenance":        build(require, c.tool("a"), c.tool("b", Untrusted("crm"))),
		"another source":           build(gatePolicy(), c.tool("a"), c.tool("b", Untrusted("web"))),
		"no declaration":           build(gatePolicy(), c.tool("a"), c.tool("b")),
		"an exemption":             build(gatePolicy(), c.tool("a", TaintExempt("reviewed")), c.tool("b", Untrusted("crm"))),
		"a source through Safety":  build(gatePolicy(), c.tool("a", WithSafety(Safety{Untrusted: "x"})), c.tool("b", Untrusted("crm"))),
	} {
		if a.taintDigest() == base.taintDigest() {
			t.Errorf("%s: the digest did not change", name)
		}
	}
	// two different cap rules are two digests (not only "a cap rule or none")
	capped := func(limit string) string {
		return build(gatePolicy(), refundTool(strings.Replace(refuseUSD, "> 5 USD", "> "+limit+" USD", 1))).taintDigest()
	}
	if capped("500") == capped("501") {
		t.Error("two cap rules with different limits share a digest")
	}
	if d := mustNew(NewScriptedModel(TextTurn("x")), memJournal()).taintDigest(); d != "" {
		t.Errorf("an agent with no policy has digest %q", d)
	}
}

// A run records the digest only when its agent has a policy; a run with none has no ext member at all.
func TestTaintRun_StartRecordsTheDigestOnlyUnderAPolicy(t *testing.T) {
	ctx := context.Background()
	for _, withPolicy := range []bool{false, true} {
		j := memJournal()
		opts := []Option{}
		if withPolicy {
			opts = append(opts, withTaintPolicy(taintPolicy{Refuse: true}))
		}
		a := mustNew(NewScriptedModel(TextTurn("done")), j, opts...)
		if _, err := a.Run(ctx, "r", UserText("go")); err != nil {
			t.Fatal(err)
		}
		raw, _, _ := j.Get(ctx, "r", runStartStep)
		got := startOf(t, j, "r").taintDigest()
		if withPolicy && (got == "" || got != a.taintDigest()) {
			t.Errorf("a run under a policy recorded %q, want %q", got, a.taintDigest())
		}
		if !withPolicy && (got != "" || strings.Contains(string(raw.Result), "ext")) {
			t.Errorf("a run with no policy recorded an ext member: %s", raw.Result)
		}
	}
}

// The no-policy refusal applies to a run that started under a policy, never to one that did not, and
// only to a side effect.
func TestTaintRun_NoPolicyRefusalIsScopedToRunsStartedUnderAPolicy(t *testing.T) {
	ctx := context.Background()
	var c counter
	tools := func() []Tool {
		return []Tool{c.tool("read", WithSafety(Safety{ReadOnly: true}), Untrusted("crm")), c.tool("write")}
	}
	script := func() *ScriptedModel {
		return NewScriptedModel(ToolTurn("c1", "read", `{}`), ToolTurn("c2", "write", `{}`), TextTurn("done"))
	}
	// Started with no policy, then driven by a process that has one: never refused, never gated yet.
	j := memJournal()
	if _, err := mustNew(NewScriptedModel(ToolTurn("c1", "read", `{}`), ErrorTurn(errors.New("down"))), j, WithTools(tools()...)).Run(ctx, "plain", UserText("go")); err == nil {
		t.Fatal("seeding drive did not stop")
	}
	policy := mustNew(script(), j, WithTools(tools()...), withTaintPolicy(taintPolicy{Refuse: true}))
	if _, err := policy.Run(ctx, "plain", UserText("go")); err != nil {
		t.Fatalf("a run started with no policy, driven under one: %v", err)
	}
	if startOf(t, j, "plain").taintDigest() != "" {
		t.Error("a policy process retro-fitted a digest onto a run that started without one")
	}

	// Started under a policy, then driven by a process with none: the read runs, the write is refused
	// with ErrConfig and its tool never called.
	c.n.Store(0)
	j = memJournal()
	if _, err := mustNew(NewScriptedModel(ToolTurn("c1", "read", `{}`), ErrorTurn(errors.New("down"))), j, WithTools(tools()...), withTaintPolicy(taintPolicy{Refuse: true})).Run(ctx, "tainted", UserText("go")); err == nil {
		t.Fatal("seeding drive did not stop")
	}
	_, err := mustNew(script(), j, WithTools(tools()...)).Run(ctx, "tainted", UserText("go"))
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "taint") {
		t.Fatalf("a policy-started run driven with no policy: %v, want an ErrConfig naming taint", err)
	}
	if got := c.n.Load(); got != 1 {
		t.Fatalf("tools ran %d times, want 1 (the read only)", got)
	}
	// A process with the policy drives it on.
	if _, err := mustNew(script(), j, WithTools(tools()...), withTaintPolicy(taintPolicy{Refuse: true})).Run(ctx, "tainted", UserText("go")); err != nil {
		t.Fatalf("the run, driven by an agent with a policy: %v", err)
	}
}

// Under-taint status is sticky down the tree: a child of a policy-started parent is under taint though
// its own agent has no policy, and is refused its side effects there; a child of a run with no policy is
// not, and a child agent with a policy under a parent with none is under taint on its own account.
func TestTaintRun_StatusIsStickyDownTheTree(t *testing.T) {
	ctx := context.Background()
	var c counter
	child := func(opts ...Option) (Tool, *Journal) {
		cj := memJournal()
		m := NewScriptedModel(ToolTurn("w1", "write", `{}`), TextTurn("child done"))
		return MustSubAgent("delegate", "delegate", mustNew(m, cj, append([]Option{WithTools(c.tool("write"))}, opts...)...)), cj
	}
	run := func(parentOpts []Option, childOpts ...Option) (parent, childJ *Journal) {
		tool, cj := child(childOpts...)
		j := memJournal()
		m := NewScriptedModel(ToolTurn("d1", "delegate", `{"task":"go"}`), TextTurn("done"))
		_, _ = mustNew(m, j, append([]Option{WithTools(tool)}, parentOpts...)...).Run(ctx, "r", UserText("go"))
		return j, cj
	}
	pol := []Option{withTaintPolicy(taintPolicy{Refuse: true})}
	sub := SubRunID("r", "d1")

	// Parent under a policy, child agent with none: the child's write is refused.
	c.n.Store(0)
	j, cj := run(pol)
	if got := c.n.Load(); got != 0 {
		t.Fatalf("the child's write ran %d times under a policy-started parent", got)
	}
	if d := startOf(t, cj, sub).taintDigest(); d == "" || d != startOf(t, j, "r").taintDigest() {
		t.Errorf("the child recorded digest %q, want the parent's %q", d, startOf(t, j, "r").taintDigest())
	}

	// Parent with no policy, child agent with none: not under taint, the write runs.
	c.n.Store(0)
	j, cj = run(nil)
	if c.n.Load() != 1 {
		t.Fatalf("no policy anywhere: %d writes, want 1", c.n.Load())
	}
	if d := startOf(t, cj, sub).taintDigest(); d != "" {
		t.Errorf("a child of a run with no policy recorded %q", d)
	}

	// Parent with none, child agent with a policy: the child is under taint on its own account.
	c.n.Store(0)
	j, cj = run(nil, pol...)
	if d := startOf(t, cj, sub).taintDigest(); d == "" {
		t.Error("a child agent with a policy did not record a digest")
	}
	if d := startOf(t, j, "r").taintDigest(); d != "" {
		t.Errorf("the parent with no policy recorded %q", d)
	}
}

// A session is under taint from the first turn an agent with a policy drives, on every handle and agent
// after it; turns before that are not retro-gated; a session that never met a policy never is.
func TestTaintSession_StatusIsStickyAlongTheSession(t *testing.T) {
	ctx := context.Background()
	var c counter
	j := memJournal()
	agentFor := func(opts ...Option) *Session {
		s, err := mustNew(writeThenAnswer{}, j, append([]Option{WithTools(c.tool("write"))}, opts...)...).Session(ctx, "conv")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	taintStatus := func() string {
		rec, ok, err := j.Get(ctx, sessionJournalID("conv"), sessionTaintStep)
		if err != nil || !ok {
			return ""
		}
		return string(rec.Result)
	}
	// Turn 1 on an agent with no policy: not under taint, the write runs, no marker.
	if _, err := agentFor().Send(ctx, UserText("one")); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if c.n.Load() != 1 || taintStatus() != "" {
		t.Fatalf("turn 1: %d writes, marker %q; want 1 write and no marker", c.n.Load(), taintStatus())
	}
	// Turn 2 on an agent with a policy: under taint from here; the marker is written.
	if _, err := agentFor(withTaintPolicy(taintPolicy{Refuse: true})).Send(ctx, UserText("two")); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if taintStatus() == "" {
		t.Fatal("turn 2 under a policy wrote no session marker")
	}
	if turn2 := startOf(t, j, sessionTurnRunID("conv", 1)); turn2.taintDigest() == "" {
		t.Error("turn 2's run recorded no digest")
	}
	// Turn 3 on an agent with no policy: under taint by the session, so its write is refused.
	n := c.n.Load()
	if _, err := agentFor().Send(ctx, UserText("three")); !errors.Is(err, ErrConfig) {
		t.Fatalf("turn 3 on a no-policy agent = %v, want ErrConfig", err)
	}
	if c.n.Load() != n {
		t.Errorf("turn 3's write ran under a session that is under taint")
	}
	if turn3 := startOf(t, j, sessionTurnRunID("conv", 2)); turn3.taintDigest() == "" {
		t.Error("turn 3's run did not inherit the session's status")
	}
}

// writeThenAnswer calls the tool "write" unless the last message is a tool result, then answers: one
// write per turn however long the session transcript is (a ScriptedModel picks its turn by the number of
// assistant messages, which a session's transcript grows).
type writeThenAnswer struct{}

func (writeThenAnswer) Stream(_ context.Context, req Request) (*Stream, error) {
	ch := make(chan Emit, 2)
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == RoleTool {
		ch <- Emit{Event: TextDelta{Text: "answer"}}
		ch <- Emit{Event: Finish{Reason: "stop"}}
	} else {
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "w", Name: "write", ArgsFragment: []byte(`{}`)}}
		ch <- Emit{Event: Finish{Reason: "tool_use"}}
	}
	close(ch)
	return NewStream(ch), nil
}

// taintRefusal refuses only a side effect, and not a compensation, of a run under taint that this agent
// cannot enforce.
func TestTaintRefusal_Scope(t *testing.T) {
	write, read := &ToolSpec{Name: "w"}, &ToolSpec{Name: "r", Safety: Safety{ReadOnly: true}}
	idem := &ToolSpec{Name: "i", Safety: Safety{Idempotent: true}}
	none := mustNew(NewScriptedModel(TextTurn("x")), memJournal())
	pol := mustNew(NewScriptedModel(TextTurn("x")), memJournal(), withTaintPolicy(taintPolicy{Refuse: true}))
	under := withUnderTaint(context.Background(), "abc")
	comp := withCompensating(under)
	for _, c := range []struct {
		name string
		a    *Agent
		ctx  context.Context
		spec *ToolSpec
		want bool
	}{
		{"a write", none, under, write, true},
		{"an idempotent write", none, under, idem, true},
		{"a read", none, under, read, false},
		{"a compensation", none, comp, write, false},
		{"a run not under taint", none, context.Background(), write, false},
		{"an agent with a policy", pol, under, write, false},
	} {
		if got := c.a.taintRefusal(c.ctx, c.spec) != nil; got != c.want {
			t.Errorf("%s: refused %v, want %v", c.name, got, c.want)
		}
	}
	if !errors.Is(none.taintRefusal(under, write), ErrConfig) {
		t.Error("the refusal is not ErrConfig")
	}
}
