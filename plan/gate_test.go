package plan

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// edApprover is a test approver: an ed25519 key pair whose verifier reports the key's identity,
// as the audit verifiers do.
type edApprover struct {
	id   string
	priv ed25519.PrivateKey
}

type edVerifier struct{ pub ed25519.PublicKey }

func (edVerifier) Alg() agent.Alg { return "ed25519" }
func (v edVerifier) Verify(msg, sig []byte) bool {
	return ed25519.Verify(v.pub, msg, sig)
}
func (v edVerifier) KeyIDs() []string {
	sum := sha256.Sum256(v.pub)
	return []string{"ed25519:" + hex.EncodeToString(sum[:])}
}

// approvers returns test approvers for ids and the resolver of their verifiers.
func approvers(ids ...string) (map[string]edApprover, agent.ApproverVerifierFor) {
	as := make(map[string]edApprover, len(ids))
	vs := make(map[string]agent.ApproverVerifier, len(ids))
	for i, id := range ids {
		seed := sha256.Sum256([]byte(fmt.Sprintf("approver %d %s", i, id)))
		priv := ed25519.NewKeyFromSeed(seed[:])
		as[id] = edApprover{id: id, priv: priv}
		vs[id] = edVerifier{pub: priv.Public().(ed25519.PublicKey)}
	}
	return as, func(id string) (agent.ApproverVerifier, bool) { v, ok := vs[id]; return v, ok }
}

// decide records a's signed decision on the paused gate pend.
func (a edApprover) decide(t *testing.T, j *agent.Journal, pend *agent.ApprovalPending, approved bool) {
	t.Helper()
	sig := ed25519.Sign(a.priv, agent.ApprovalDecisionBytes(pend.Subject(), a.id, approved, testDecidedAt))
	if err := agent.SubmitDecision(context.Background(), j, agent.Decision{RunID: pend.RunID, ToolUseID: pend.ToolUseID, ApproverID: a.id, Approved: approved, DecidedAt: testDecidedAt, Alg: "ed25519", Signature: sig}); err != nil {
		t.Fatalf("SubmitDecision(%s): %v", a.id, err)
	}
}

type refund struct {
	Order  string `json:"order"`
	Amount int    `json:"amount"`
}

// refundFlow is lookup -> refund (gated by pol) -> notify. refunds counts refund's runs.
func refundFlow(t *testing.T, pol *agent.ApprovalPolicy, vf agent.ApproverVerifierFor, refunds *int) *Flow[string, string] {
	t.Helper()
	b := New[string, string]("refunds").WithApproverVerifiers(vf)
	lookup := b.Step("lookup", func(_ context.Context, order string) (refund, error) {
		return refund{Order: order, Amount: 120}, nil
	}, ReadOnly())
	pay := b.Step("refund", func(_ context.Context, r refund) (refund, error) {
		*refunds++
		return r, nil
	}, Approval(pol))
	notify := b.Step("notify", func(_ context.Context, r refund) (string, error) {
		return fmt.Sprintf("refunded %d on %s", r.Amount, r.Order), nil
	}, ReadOnly())
	b.Edge(lookup, pay)
	b.Edge(pay, notify)
	f, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return f
}

func asPending(t *testing.T, err error) *agent.ApprovalPending {
	t.Helper()
	var pend *agent.ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("Run = %v; want *agent.ApprovalPending", err)
	}
	return pend
}

// A gated node pauses before it runs, as a gated tool call does, and runs once k signed approvals
// are recorded. The pause names the node by its key, and its subject binds the node's input.
func TestGate_PausesUntilQuorum(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops", "finance", "risk")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}, vf, &refunds)

	_, err := f.Run(ctx, j, "r1", "order-42")
	pend := asPending(t, err)
	if refunds != 0 {
		t.Fatalf("the gated node ran %d times before any approval", refunds)
	}
	if pend.ToolUseID != "node:refund" || pend.ToolName != "refund" || pend.RunID != "r1" || pend.RootRunID != "r1" {
		t.Fatalf("pause = %+v; want node:refund of run r1", pend)
	}
	if !agentSameJSON(pend.Args, `{"order":"order-42","amount":120}`) {
		t.Fatalf("pause args = %s; want the node's input", pend.Args)
	}
	if pend.Quorum == nil || pend.Quorum.Need != 2 || pend.Quorum.Approved != 0 {
		t.Fatalf("pause quorum = %+v; want 0 of 2", pend.Quorum)
	}

	as["ops"].decide(t, j, pend, true)
	_, err = f.Run(ctx, j, "r1", "order-42")
	pend = asPending(t, err)
	if refunds != 0 || pend.Quorum.Approved != 1 {
		t.Fatalf("after one approval: refunds = %d, quorum %+v; want still paused at 1 of 2", refunds, pend.Quorum)
	}

	as["risk"].decide(t, j, pend, true)
	out, err := f.Run(ctx, j, "r1", "order-42")
	if err != nil || out != "refunded 120 on order-42" || refunds != 1 {
		t.Fatalf("after two approvals: Run = %q, %v, refunds %d; want the flow to finish with one refund", out, err, refunds)
	}

	// The records come in the gate's order: request, decisions, tally, then the node.
	recs, err := j.History(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	at := map[string]int{}
	for i, r := range recs {
		at[r.Name] = i
	}
	req, tally, node := at[agent.ApprovalRequestStep("node:refund")], at[agent.ApprovalTallyStep("node:refund")], at["node:refund"]
	if !(req > 0 && req < tally && tally < node) {
		t.Fatalf("journal order request %d, tally %d, node %d; want request < tally < node", req, tally, node)
	}
	if _, ok := at[agent.ApprovalPendingStep("node:refund")]; !ok {
		t.Fatalf("no %s record; want the pause noted for a viewer", agent.ApprovalPendingStep("node:refund"))
	}
}

func agentSameJSON(a json.RawMessage, b string) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal([]byte(b), &y) == nil && fmt.Sprint(x) == fmt.Sprint(y)
}

// A decision signed for another input, by an approver outside the policy, or with another key does
// not count: the gate verifies every decision against the node's recorded input.
func TestGate_InvalidDecisionsDoNotCount(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops", "finance")
	outsider, _ := approvers("mallory")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops", "finance"}}, vf, &refunds)
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())

	other := *pend
	other.Args = json.RawMessage(`{"order":"order-42","amount":99999}`)
	as["ops"].decide(t, j, &other, true) // signed for other arguments
	outsider["mallory"].decide(t, j, pend, true)
	forged := edApprover{id: "finance", priv: outsider["mallory"].priv} // finance's id, mallory's key
	forged.decide(t, j, pend, true)

	_, err := f.Run(ctx, j, "r1", "order-42")
	pend = asPending(t, err)
	if refunds != 0 || pend.Quorum.Approved != 0 || len(pend.Quorum.Records) != 3 {
		t.Fatalf("refunds %d, quorum %+v; want no approval counted out of 3 records read", refunds, pend.Quorum)
	}
}

// Enough valid denials make the quorum unreachable: the gate records a denying tally, the node
// never runs, and every later drive returns ErrApprovalDenied from the recorded tally.
func TestGate_DenyStopsTheRun(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops", "finance", "risk")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}, vf, &refunds)
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	as["ops"].decide(t, j, pend, false)
	as["finance"].decide(t, j, pend, false)
	for drive := 0; drive < 2; drive++ {
		_, err := f.Run(ctx, j, "r1", "order-42")
		if !errors.Is(err, ErrApprovalDenied) || !strings.Contains(err.Error(), `"refund"`) {
			t.Fatalf("drive %d: Run = %v; want ErrApprovalDenied naming the step", drive, err)
		}
	}
	// A late approval does not reopen the gate: the terminal tally is final.
	as["risk"].decide(t, j, pend, true)
	if _, err := f.Run(ctx, j, "r1", "order-42"); !errors.Is(err, ErrApprovalDenied) || refunds != 0 {
		t.Fatalf("after a late approval: Run = %v, refunds %d; want still denied, never run", err, refunds)
	}
}

// A SingleApproval gate is decided with agent.Approve, as a gated tool call is.
func TestGate_SingleApproval(t *testing.T) {
	ctx := context.Background()
	for _, approved := range []bool{true, false} {
		j := agenttest.MemJournal()
		refunds := 0
		f := refundFlow(t, agent.SingleApproval(), nil, &refunds)
		pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
		if pend.Quorum != nil {
			t.Fatalf("a single-approval pause carries a quorum: %+v", pend.Quorum)
		}
		if err := agent.Approve(ctx, j, pend.RunID, pend.ToolUseID, approved); err != nil {
			t.Fatal(err)
		}
		_, err := f.Run(ctx, j, "r1", "order-42")
		switch {
		case approved && (err != nil || refunds != 1):
			t.Fatalf("approved: Run = %v, refunds %d; want one refund", err, refunds)
		case !approved && (!errors.Is(err, ErrApprovalDenied) || refunds != 0):
			t.Fatalf("denied: Run = %v, refunds %d; want ErrApprovalDenied and no refund", err, refunds)
		}
	}
}

// failingStore fails the Insert of one journal name, once, as a crash at that point would leave
// the journal.
type failingStore struct {
	agent.Store
	name   string
	failed bool
}

func (s *failingStore) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if name == s.name && !s.failed {
		s.failed = true
		return agent.Entry{}, false, errors.New("crash")
	}
	return s.Store.Insert(ctx, runID, name, data)
}

// A pause survives a crash: the process that paused is gone, decisions recorded meanwhile are in
// the journal, and a new process's drive counts them. A crash after the gate recorded its passing
// tally and before the node recorded its result does not ask the approvers again: the next drive
// reads the tally and runs the node once.
func TestGate_CrashWhilePausedThenResume(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops", "finance")
	mem := agent.NewMemStore()
	fs := &failingStore{Store: mem, name: "node:refund"}
	pol := &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance"}}

	refunds := 0
	first := refundFlow(t, pol, vf, &refunds)
	pend := asPending(t, func() error { _, err := first.Run(ctx, agenttest.MustJournal(fs), "r1", "order-42"); return err }())

	// The first process is gone. The approvers decide while no driver runs.
	j := agenttest.MustJournal(mem)
	as["ops"].decide(t, j, pend, true)
	as["finance"].decide(t, j, pend, true)

	// A new process: its first drive passes the gate, runs the node, and crashes before the node's
	// result is recorded (the node is a side effect, so its claim stays and the drive halts next).
	second := refundFlow(t, pol, vf, &refunds)
	if _, err := second.Run(ctx, agenttest.MustJournal(fs), "r1", "order-42"); err == nil {
		t.Fatal("Run succeeded through the injected crash")
	}
	if _, ok, _ := j.Get(ctx, "r1", agent.ApprovalTallyStep("node:refund")); !ok {
		t.Fatal("no tally recorded before the node ran")
	}
	_, err := second.Run(ctx, j, "r1", "order-42")
	var ou *agent.OutcomeUnknown
	if !errors.As(err, &ou) || refunds != 1 {
		t.Fatalf("after the crash: Run = %v, refunds %d; want the node's halt (its outcome is unknown), one refund", err, refunds)
	}
}

// A gate that passed is not asked again on replay, and a node whose gate passed costs one point
// read of the tally: no History.
func TestGate_ReplayReadsTheTallyOnly(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	cs := agenttest.NewCountingStore(agent.NewMemStore())
	j := agenttest.MustJournal(cs)
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	as["ops"].decide(t, j, pend, true)
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil {
		t.Fatal(err)
	}
	// Drop run:complete's shortcut by asking the gate directly, through a replay of the node.
	cs.Reset()
	if err := runGate(ctx, j, "r1", f.core, f.core.byName["refund"], "node:refund", refund{Order: "order-42", Amount: 120}); err != nil {
		t.Fatalf("runGate on a passed gate = %v", err)
	}
	if c := cs.Counts(); c.Load != 0 || c.Insert != 0 || c.Get != 3 {
		t.Fatalf("a passed gate cost %+v; want the tally and two point reads that tell it is not certificate-bound, no Load", c)
	}
}

// loopRefund is a bounded loop whose body node pay is gated: seed -> pay -> check, check loops
// back to pay while N > 0, else exits to done.
func loopRefund(t *testing.T, vf agent.ApproverVerifierFor, pays *int) *Flow[int, int] {
	t.Helper()
	b := New[int, int]("loop-refunds").WithApproverVerifiers(vf)
	seed := b.Step("seed", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly())
	pay := b.Step("pay", func(_ context.Context, n int) (int, error) { *pays++; return n - 1, nil },
		Approval(&agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}))
	check := b.Step("check", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly())
	done := b.Step("done", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly())
	b.Edge(seed, pay)
	b.Edge(pay, check)
	b.Switch(check, LoopBack(5, func(n int) bool { return n > 0 }, pay).Named("again"), Else(done))
	f, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return f
}

// A gate inside a loop body is per iteration: each iteration's run of the node is its own subject
// (its key and input), so an approval of iteration 0 does not let iteration 1 run.
func TestGate_PerIterationInALoop(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	pays := 0
	f := loopRefund(t, vf, &pays)
	for iter := 0; iter < 2; iter++ {
		_, err := f.Run(ctx, j, "r1", 2)
		pend := asPending(t, err)
		want := fmt.Sprintf("node:iter:%d:pay", iter)
		if pend.ToolUseID != want || pays != iter {
			t.Fatalf("iteration %d: paused at %s with %d runs; want %s with %d", iter, pend.ToolUseID, pays, want, iter)
		}
		if !agentSameJSON(pend.Args, fmt.Sprint(2-iter)) {
			t.Fatalf("iteration %d: args %s; want the iteration's input %d", iter, pend.Args, 2-iter)
		}
		as["ops"].decide(t, j, pend, true)
	}
	out, err := f.Run(ctx, j, "r1", 2)
	if err != nil || out != 0 || pays != 2 {
		t.Fatalf("Run = %d, %v, pays %d; want 0 after two approved iterations", out, err, pays)
	}
}

// A gated node on a Switch arm the run does not take never pauses.
func TestGate_UntakenArmDoesNotPause(t *testing.T) {
	ctx := context.Background()
	_, vf := approvers("ops")
	b := New[int, string]("route").WithApproverVerifiers(vf)
	in := b.Step("in", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly())
	big := b.Step("big", func(_ context.Context, n int) (string, error) { return "big", nil },
		Approval(&agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}))
	small := b.Step("small", func(_ context.Context, n int) (string, error) { return "small", nil }, ReadOnly())
	b.Switch(in, When(func(n int) bool { return n > 100 }, big).Named("large"), Else(small))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := f.Run(ctx, agenttest.MemJournal(), "r1", 5); err != nil || out != "small" {
		t.Fatalf("Run(5) = %q, %v; want small with no pause", out, err)
	}
	if _, err := f.Run(ctx, agenttest.MemJournal(), "r2", 500); err == nil {
		t.Fatal("Run(500) did not pause at big's gate")
	} else {
		asPending(t, err)
	}
}

// Build refuses a gate it could not enforce, and an option that would replace a tool's own gate.
func TestGate_BuildRefusals(t *testing.T) {
	_, vf := approvers("ops")
	step := func(_ context.Context, n int) (int, error) { return n, nil }
	cases := map[string]func() error{
		"m-of-n without verifiers": func() error {
			b := New[int, int]("f")
			b.Step("s", step, Approval(&agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}))
			_, err := b.Build()
			return err
		},
		"ill-formed policy": func() error {
			b := New[int, int]("f").WithApproverVerifiers(vf)
			b.Step("s", step, Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops"}}))
			_, err := b.Build()
			return err
		},
		"nil policy": func() error {
			b := New[int, int]("f")
			b.Step("s", step, Approval(nil))
			_, err := b.Build()
			return err
		},
		"another gate on a gated tool": func() error {
			tool := agent.MustFunc("refund", "issue a refund", step, agent.WithApproval(agent.SingleApproval()))
			b := New[int, int]("f").WithApproverVerifiers(vf)
			b.Tool[int, int]("s", tool, Approval(&agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}))
			_, err := b.Build()
			return err
		},
	}
	for name, build := range cases {
		if err := build(); err == nil {
			t.Errorf("%s: Build succeeded; want an error", name)
		}
	}
	// Restating a tool's own gate is allowed.
	tool := agent.MustFunc("refund", "issue a refund", step, agent.WithApproval(agent.SingleApproval()))
	b := New[int, int]("f")
	b.Tool[int, int]("s", tool, Approval(agent.SingleApproval()))
	if _, err := b.Build(); err != nil {
		t.Errorf("restating the tool's gate: Build = %v", err)
	}
}

// The digest commits to a node's gate, so a run cannot resume under a flow whose gate was removed
// or changed; an ungated flow's digest is the one it had before gates were enforced.
func TestGate_DigestCommitsToTheGate(t *testing.T) {
	_, vf := approvers("ops", "finance")
	build := func(opts ...NodeOption) string {
		b := New[int, int]("f").WithApproverVerifiers(vf)
		b.Step("s", func(_ context.Context, n int) (int, error) { return n, nil }, opts...)
		f, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return f.Digest()
	}
	none := build()
	single := build(Approval(agent.SingleApproval()))
	one := build(Approval(&agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops", "finance"}}))
	two := build(Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance"}}))
	seen := map[string]string{}
	for name, d := range map[string]string{"none": none, "single": single, "1 of 2": one, "2 of 2": two} {
		if prev, dup := seen[d]; dup {
			t.Fatalf("%s and %s share digest %s", prev, name, d)
		}
		seen[d] = name
	}
	j := agenttest.MemJournal()
	gated, err := func() (*Flow[int, int], error) {
		b := New[int, int]("f").WithApproverVerifiers(vf)
		b.Step("s", func(_ context.Context, n int) (int, error) { return n, nil }, Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance"}}))
		return b.Build()
	}()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gated.Run(context.Background(), j, "r1", 1); err == nil {
		t.Fatal("no pause")
	}
	ungated := New[int, int]("f")
	ungated.Step("s", func(_ context.Context, n int) (int, error) { return n, nil })
	uf, err := ungated.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uf.Run(context.Background(), j, "r1", 1); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("resuming a paused run with the gate removed: Run = %v; want ErrConfig", err)
	}
}

// Conform accepts a gated run's approval records, paused and after the gate passed, and refuses
// gate records on a node the run did not reach.
func TestGate_Conform(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	_, err := f.Run(ctx, j, "r1", "order-42")
	pend := asPending(t, err)
	if ok, diffs, err := f.Conform(ctx, j, "r1"); err != nil || !ok {
		t.Fatalf("paused run: Conform = %v, %v, %v", ok, diffs, err)
	}
	as["ops"].decide(t, j, pend, true)
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil {
		t.Fatal(err)
	}
	if ok, diffs, err := f.Conform(ctx, j, "r1"); err != nil || !ok {
		t.Fatalf("finished run: Conform = %v, %v, %v", ok, diffs, err)
	}

	// A gate record on an arm the run did not take is a divergence.
	_, vf2 := approvers("ops")
	b := New[int, string]("route").WithApproverVerifiers(vf2)
	in := b.Step("in", func(_ context.Context, n int) (int, error) { return n, nil }, ReadOnly())
	big := b.Step("big", func(_ context.Context, n int) (string, error) { return "big", nil },
		Approval(&agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}))
	small := b.Step("small", func(_ context.Context, n int) (string, error) { return "small", nil }, ReadOnly())
	b.Switch(in, When(func(n int) bool { return n > 100 }, big).Named("large"), Else(small))
	rf, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	j2 := agenttest.MemJournal()
	if _, err := rf.Run(ctx, j2, "r2", 5); err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(agent.ApprovalRequest{ToolUseID: "node:big", ToolName: "big", Args: json.RawMessage(`5`)})
	if _, err := journaltest.Put(ctx, j2, "r2", agent.ApprovalRequestStep("node:big"), agent.Record{Kind: agent.StepValue, Result: req}); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := rf.Conform(ctx, j2, "r2"); err != nil || ok {
		t.Fatalf("a gate record off the taken path: Conform = %v, %v; want a divergence", ok, err)
	}
}
