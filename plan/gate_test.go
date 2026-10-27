package plan

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

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
	if err := runGate(ctx, j, "r1", f.core, nil, f.core.byName["refund"], "node:refund", refund{Order: "order-42", Amount: 120}); err != nil {
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

// A plan flow gate whose recorded tally passed on decisions that predate bide.approval.v3 (no
// decision format) does not run its node on that tally: it asks again (PendingReapproval), and
// once the approver signs under v3 the node runs once.
func TestFlowGate_PreV3PassedTallyReapproval(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	old, _ := json.Marshal(agent.ApprovalTally{Need: 1, Approvers: []string{"ops"}, Approved: 1, ApprovedBy: []string{"ops"}})
	if _, err := journaltest.Put(ctx, j, "r1", agent.ApprovalTallyStep("node:refund"), agent.Record{Kind: agent.StepValue, Result: old}); err != nil {
		t.Fatal(err)
	}
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	if pend.Reason != agent.PendingReapproval || refunds != 0 {
		t.Fatalf("drive on a pre-v3 passed tally: reason %q, refunds %d; want a re-approval pause and no refund", pend.Reason, refunds)
	}
	as["ops"].decide(t, j, pend, true)
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("drive after v3 re-approval = %v, refunds %d; want the node run once", err, refunds)
	}
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("a later drive = %v, refunds %d", err, refunds)
	}
}

// A SingleApproval recorded before the run ever reaches the gate (node keys are predictable)
// lets the node run without any pause and without an approval-request being recorded, so the
// decision was never bound to the input the node ran with.
func TestRev_SingleApprovalBeforeRequest(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, agent.SingleApproval(), nil, &refunds)
	// Approve refuses a decision on a node key with no request yet; written directly (as a
	// journal's writer could), the decision must still not count.
	if err := agent.Approve(ctx, j, "r1", "node:refund", true); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("Approve before the request = %v; want ErrConfig", err)
	}
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Arefund", agent.Record{Kind: agent.StepApproval, ToolUseID: "node:refund", Approved: true}); err != nil {
		t.Fatal(err)
	}
	_, err := f.Run(ctx, j, "r1", "order-42")
	recs, _ := j.History(ctx, "r1")
	var names []string
	for _, r := range recs {
		names = append(names, r.Name)
	}
	t.Logf("Run err=%v refunds=%d records=%v", err, refunds, names)
	if refunds != 0 {
		t.Fatalf("gated node ran %d times on an approval recorded before its request (no subject ever shown)", refunds)
	}
	asPending(t, err)
}

// Resuming a paused run under a flow whose gate was removed or loosened must be refused.
func TestRev_ResumeGateRemoved(t *testing.T) {
	ctx := context.Background()
	_, vf := approvers("ops", "finance", "risk")
	for _, tc := range []struct {
		name string
		pol  *agent.ApprovalPolicy
	}{{"loosened", &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops", "finance", "risk"}}}, {"single", agent.SingleApproval()}} {
		j := agenttest.MemJournal()
		n := 0
		f := refundFlow(t, &agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}, vf, &n)
		_, err := f.Run(ctx, j, "r1", "order-42")
		asPending(t, err)
		g := refundFlow(t, tc.pol, vf, &n)
		_, err = g.Run(ctx, j, "r1", "order-42")
		t.Logf("%s: %v", tc.name, err)
		if err == nil || !strings.Contains(err.Error(), "") {
			t.Fatalf("%s: resumed", tc.name)
		}
	}
}

// signedDecision is a's decision on the subject (runID, key, name, args) as SubmitDecision records it.
func signedDecision(a edApprover, runID, key, name, args string, approved bool) (agent.Decision, agent.Record) {
	s := agent.ApprovalSubject{RunID: runID, ToolUseID: key, ToolName: name, Args: []byte(args)}
	sig := ed25519.Sign(a.priv, agent.ApprovalDecisionBytes(s, a.id, approved, testDecidedAt))
	d := agent.Decision{RunID: runID, ToolUseID: key, ApproverID: a.id, Approved: approved, DecidedAt: testDecidedAt, Alg: "ed25519", Signature: sig}
	return d, agent.Record{Kind: agent.StepApproval, ToolUseID: key, Approved: approved, ApproverSignature: &agent.ApproverSignature{Approver: a.id, ApproverAlg: "ed25519", DecidedAt: agent.FormatTimestamp(testDecidedAt), Signature: sig}}
}

// An m-of-n decision signed for the right input but recorded before the gate's request does not
// count, and SubmitDecision refuses one: the gate counts only decisions that follow its request.
func TestRev_MofNDecisionBeforeRequest(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	const args = `{"order":"order-42","amount":120}`
	d, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", args, true)
	if err := agent.SubmitDecision(ctx, j, d); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("SubmitDecision before the request = %v; want ErrConfig", err)
	}
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Arefund:ops:early", rec); err != nil {
		t.Fatal(err)
	}
	_, err := f.Run(ctx, j, "r1", "order-42")
	pend := asPending(t, err)
	if refunds != 0 || pend.Quorum.Approved != 0 {
		t.Fatalf("refunds %d, quorum %+v; want the early decision not counted", refunds, pend.Quorum)
	}
	// The same decision after the request counts.
	if err := agent.SubmitDecision(ctx, j, d); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("after a decision that follows the request: Run = %v, refunds %d", err, refunds)
	}
}

// In a loop, a decision on a later iteration's key recorded before that iteration's request does
// not count, and SubmitDecision refuses it.
func TestRev_LoopDecisionBeforeIterationRequest(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	pays := 0
	f := loopRefund(t, vf, &pays)
	_, err := f.Run(ctx, j, "r1", 2)
	pend := asPending(t, err)
	d1, rec1 := signedDecision(as["ops"], "r1", "node:iter:1:pay", "pay", "1", true)
	if err := agent.SubmitDecision(ctx, j, d1); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("SubmitDecision for iteration 1 before its request = %v; want ErrConfig", err)
	}
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Aiter%3A1%3Apay:ops:early", rec1); err != nil {
		t.Fatal(err)
	}
	as["ops"].decide(t, j, pend, true) // iteration 0
	_, err = f.Run(ctx, j, "r1", 2)
	pend = asPending(t, err)
	if pend.ToolUseID != "node:iter:1:pay" || pays != 1 || pend.Quorum.Approved != 0 {
		t.Fatalf("paused at %s with %d pays, quorum %+v; want iteration 1 waiting, the early decision not counted", pend.ToolUseID, pays, pend.Quorum)
	}
}

// approval-pending is written on any pause that finds it missing, so a crash between the request
// and the note does not hide the wait from a viewer.
func TestRev_PendingWrittenWhenMissing(t *testing.T) {
	ctx := context.Background()
	_, vf := approvers("ops")
	mem := agent.NewMemStore()
	fs := &failingStore{Store: mem, name: agent.ApprovalPendingStep("node:refund")}
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	if _, err := f.Run(ctx, agenttest.MustJournal(fs), "r1", "order-42"); err == nil {
		t.Fatal("no pause")
	}
	j := agenttest.MustJournal(mem)
	if _, ok, _ := j.Get(ctx, "r1", agent.ApprovalPendingStep("node:refund")); ok {
		t.Fatal("the injected failure did not drop the note")
	}
	asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	if _, ok, _ := j.Get(ctx, "r1", agent.ApprovalPendingStep("node:refund")); !ok {
		t.Fatal("a later pause did not write the missing approval-pending note")
	}
}

// A decision recorded before the request (by a writer the API no longer allows, e.g. a run
// journaled before the fix) must not leave the gate undecidable: after the pause, Approve takes
// effect.
func TestRev201_StaleSingleDecisionBlocksGate(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, agent.SingleApproval(), nil, &refunds)
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Arefund", agent.Record{Kind: agent.StepApproval, ToolUseID: "node:refund", Approved: true}); err != nil {
		t.Fatal(err)
	}
	_, err := f.Run(ctx, j, "r1", "order-42")
	asPending(t, err)
	aerr := agent.Approve(ctx, j, "r1", "node:refund", true)
	_, err = f.Run(ctx, j, "r1", "order-42")
	t.Logf("Approve after pause: %v; Run: %v; refunds=%d", aerr, err, refunds)
	if aerr == nil && refunds == 0 {
		t.Fatalf("Approve returned nil but the gate stays paused forever")
	}
	// The stale record never counts and no longer blocks: Approve records the real decision under
	// the gate's second name, and the node runs on it once.
	if aerr != nil || err != nil || refunds != 1 {
		t.Fatalf("Approve after a stale decision = %v, Run = %v (refunds %d); want the decision recorded and the node run once", aerr, err, refunds)
	}
}

// The same with a denial: the stale approval does not stand in for it.
func TestRev201_StaleSingleDecisionThenDenial(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, agent.SingleApproval(), nil, &refunds)
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Arefund", agent.Record{Kind: agent.StepApproval, ToolUseID: "node:refund", Approved: true}); err != nil {
		t.Fatal(err)
	}
	asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	if err := agent.Approve(ctx, j, "r1", "node:refund", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, j, "r1", "order-42"); !errors.Is(err, ErrApprovalDenied) || refunds != 0 {
		t.Fatalf("Run after a denial over a stale approval = %v (refunds %d); want ErrApprovalDenied", err, refunds)
	}
}

// An m-of-n decision before the request is refused (SubmitDecision), as is a decision on a later
// loop iteration's key before that iteration reaches its gate.
func TestRev201_DecisionsBeforeRequestRefused(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	for _, key := range []string{"node:refund", "node:iter:2:refund"} {
		err := agent.SubmitDecision(ctx, j, agent.Decision{RunID: "r1", ToolUseID: key, ApproverID: "ops", Approved: true, DecidedAt: testDecidedAt, Alg: "ed25519", Signature: make([]byte, 64)})
		if !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("SubmitDecision(%s) before the request = %v; want ErrConfig", key, err)
		}
		if err := agent.Approve(ctx, j, "r1", key, true); !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("Approve(%s) before the request = %v; want ErrConfig", key, err)
		}
	}
}

// The m-of-n form: a pre-request decision stored under its canonical name (ed25519 signatures are
// deterministic) takes the name of the approver's identical decision after the pause, so that one
// is not recorded and SubmitDecision says so (ErrConfig), pointing the approver at re-signing at a
// new decided_at, which counts (TestReview_StaleSeatNotLostUnderV3).
func TestRev201_StaleMofNDecisionBlocksGate(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	const args = `{"order":"order-42","amount":120}`
	d, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", args, true)
	if _, err := journaltest.Put(ctx, j, "r1", agent.DecisionStep(d), rec); err != nil {
		t.Fatal(err)
	}
	_, err := f.Run(ctx, j, "r1", "order-42")
	pend := asPending(t, err)
	serr := agent.SubmitDecision(ctx, j, d)
	_, err = f.Run(ctx, j, "r1", "order-42")
	t.Logf("subject args %s; SubmitDecision: %v; Run: %v; refunds=%d", pend.Args, serr, err, refunds)
	if serr == nil && refunds == 0 {
		t.Fatalf("SubmitDecision returned nil but its decision never counts")
	}
	if !errors.Is(serr, agent.ErrConfig) || !strings.Contains(serr.Error(), "sign the decision again at a new decided_at") {
		t.Fatalf("SubmitDecision after a stale decision = %v; want ErrConfig saying to re-sign", serr)
	}
}

// With a stale decision on record for one approver, the gate can still pass with the others.
func TestRev201_StaleSeatOthersCanPass(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops", "finance")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops", "finance"}}, vf, &refunds)
	const args = `{"order":"order-42","amount":120}`
	_, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", args, true)
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Arefund:ops:stale", rec); err != nil {
		t.Fatal(err)
	}
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	as["finance"].decide(t, j, pend, true)
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("Run with finance's approval = %v, refunds %d; want the node run once", err, refunds)
	}
}

// A stale decision under the previous, signature-derived name still takes the name of the
// approver's identical decision after the pause, as before the upgrade: the identical resubmission
// is not recorded and SubmitDecision says so; re-signing at a new decided_at counts.
func TestRev201_LegacyNamedStaleDecisionStillBlocksTheIdenticalOne(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	const args = `{"order":"order-42","amount":120}`
	d, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", args, true)
	if _, err := journaltest.Put(ctx, j, "r1", legacyDecisionStep(d), rec); err != nil {
		t.Fatal(err)
	}
	asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	if err := agent.SubmitDecision(ctx, j, d); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("SubmitDecision of the identical decision = %v; want ErrConfig", err)
	}
}

// legacyDecisionStep is the signature-derived name journals written before the decision step was
// keyed by identity hold their decisions under.
func legacyDecisionStep(d agent.Decision) string {
	h := sha256.New()
	h.Write([]byte{1})
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(d.Alg))))
	h.Write([]byte(d.Alg))
	at := agent.FormatTimestamp(d.DecidedAt)
	h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(at))))
	h.Write([]byte(at))
	h.Write(d.Signature)
	return "approval:" + url.QueryEscape(d.ToolUseID) + ":" + d.ApproverID + ":" + hex.EncodeToString(h.Sum(nil))
}

// In a flow gate a decision journaled before the request is held, but not verified: it cannot count.
func TestRev201_HeldDecisionStepFlagsAPreRequestRecord(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	d, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", `{"order":"order-42","amount":120}`, true)
	if _, err := journaltest.Put(ctx, j, "r1", agent.DecisionStep(d), rec); err != nil {
		t.Fatal(err)
	}
	asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	_, held, verified, err := agent.HeldDecisionStep(ctx, j, d, vf)
	if err != nil || !held || verified {
		t.Fatalf("held %v verified %v %v; want held and not verified", held, verified, err)
	}
}

// Decision times of a flow gate under bide.approval.v3.

// TestReview_StaleSeatNotLostUnderV3: when an approver's identical decision was journaled before a
// flow gate's request, SubmitDecision (and its godoc, design-mofn-approval.md, plan-flows.md and
// Flows.tla) said "their seat is lost for this gate". Under v3 the decision's journal name hashes
// decided_at, so the approver re-signs at another time, the new record gets a new name, and it
// counts: the seat is not lost. The error must not tell the operator to give up on (or cancel) a
// gate the approver can still pass alone, and the re-signed decision must pass the gate.
func TestReview_StaleSeatNotLostUnderV3(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	const args = `{"order":"order-42","amount":120}`
	d, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", args, true)
	if _, err := journaltest.Put(ctx, j, "r1", agent.DecisionStep(d), rec); err != nil {
		t.Fatal(err)
	}
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	// The identical decision: it cannot be recorded (its name is taken), and must be reported, but
	// not as a lost seat.
	serr := agent.SubmitDecision(ctx, j, d)
	if !errors.Is(serr, agent.ErrConfig) {
		t.Fatalf("SubmitDecision of the identical early decision = %v; want ErrConfig", serr)
	}
	if strings.Contains(serr.Error(), "seat is lost") {
		t.Errorf("SubmitDecision says %q, but under v3 the approver can sign again at another time", serr)
	}
	// ops re-signs the same approval a second later.
	later := d.DecidedAt.Add(time.Second)
	d.DecidedAt = later
	d.Signature = ed25519.Sign(as["ops"].priv, agent.ApprovalDecisionBytes(pend.Subject(), "ops", true, later))
	if err := agent.SubmitDecision(ctx, j, d); err != nil {
		t.Fatalf("re-signed SubmitDecision: %v", err)
	}
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("Run after the re-signed approval = %v, refunds %d; want the node run once", err, refunds)
	}
}

// TestReview_DecisionCheckCountsEarlyRecord: WithDecisionCheck's "already decided" check reads
// every decision on the call, including one journaled before a flow gate's request, which the
// gate never counts. So ops's re-signed approval (which, without the check, is recorded and
// passes the gate, see TestReview_StaleSeatNotLostUnderV3) is refused with ErrAlreadyDecided,
// "the approver's earlier valid decision already counts", when it does not count. The pre-flight
// and the gate disagree about which decisions count.
func TestReview_DecisionCheckCountsEarlyRecord(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	const args = `{"order":"order-42","amount":120}`
	d, rec := signedDecision(as["ops"], "r1", "node:refund", "refund", args, true)
	if _, err := journaltest.Put(ctx, j, "r1", "approval:node%3Arefund:ops:early", rec); err != nil {
		t.Fatal(err)
	}
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	later := d.DecidedAt.Add(time.Second)
	d.DecidedAt = later
	d.Signature = ed25519.Sign(as["ops"].priv, agent.ApprovalDecisionBytes(pend.Subject(), "ops", true, later))
	if err := agent.SubmitDecision(ctx, j, d, agent.WithDecisionCheck(vf)); err != nil {
		t.Fatalf("SubmitDecision with WithDecisionCheck = %v; but the early decision does not count, and the same call without the check passes the gate", err)
	}
}

// TestReview_ConformRejectsRetally (re-attack): a flow run whose gate asked again under v3 records
// its outcome under agent.ApprovalRetallyStep ("approval-tally:<enc>:v3"). Flow.Conform maps an
// "approval-tally:" record to its gate by cutting at the first ':' and looking the rest up as an
// encoded node key, so "<enc>:v3" matches no gate and a correct re-approved run is reported as
// not conforming ("approval record of a gate the run never reached").
func TestReview_ConformRejectsRetally(t *testing.T) {
	ctx := context.Background()
	as, vf := approvers("ops")
	j := agenttest.MemJournal()
	refunds := 0
	f := refundFlow(t, &agent.ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}, vf, &refunds)
	asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	old, _ := json.Marshal(agent.ApprovalTally{Need: 1, Approvers: []string{"ops"}, Approved: 1, ApprovedBy: []string{"ops"}})
	if _, err := journaltest.Put(ctx, j, "r1", agent.ApprovalTallyStep("node:refund"), agent.Record{Kind: agent.StepValue, Result: old}); err != nil {
		t.Fatal(err)
	}
	pend := asPending(t, func() error { _, err := f.Run(ctx, j, "r1", "order-42"); return err }())
	as["ops"].decide(t, j, pend, true)
	if _, err := f.Run(ctx, j, "r1", "order-42"); err != nil || refunds != 1 {
		t.Fatalf("drive after v3 re-approval = %v, refunds %d", err, refunds)
	}
	ok, diffs, err := f.Conform(ctx, j, "r1")
	if err != nil || !ok {
		t.Fatalf("Conform of a re-approved run = %v, %q, %v; want it to conform", ok, diffs, err)
	}
}
