package agent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
)

// PR 9b of the taint build: the escalation gate of a tainted call, as a second instance on the call under
// the derived subject "<call>#taint".

type tgEnv struct {
	t       *testing.T
	j       *Journal
	reads   *counter
	writes  *counter
	policy  taintPolicy
	opts    []Option
	writeOp []ToolOption
}

func newTG(t *testing.T) *tgEnv {
	return &tgEnv{t: t, j: memJournal(), reads: &counter{}, writes: &counter{}, policy: taintPolicy{Gate: SingleApproval()}}
}

// drive runs (or re-drives) run "r" with the remaining model turns, under the policy unless none is set.
func (e *tgEnv) drive(withPolicy bool, turns ...[]Emit) error {
	e.t.Helper()
	read := e.reads.tool("read", WithSafety(Safety{ReadOnly: true}), Untrusted("crm"))
	write := e.writes.tool("write", e.writeOp...)
	opts := append([]Option{WithTools(read, write)}, e.opts...)
	if withPolicy {
		opts = append(opts, withTaintPolicy(e.policy))
	}
	_, err := mustNew(&scriptModel{turns: turns}, e.j, opts...).Run(context.Background(), "r", UserText("go"))
	return err
}

func pendingOn(t *testing.T, err error, id string) *ApprovalPending {
	t.Helper()
	var pend *ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("err = %v, want *ApprovalPending on %s", err, id)
	}
	if pend.ToolUseID != id || pend.ToolName != "write" {
		t.Fatalf("pending on %s/%s, want write/%s", pend.ToolName, pend.ToolUseID, id)
	}
	return pend
}

func (e *tgEnv) reasonOf(call string) (gateReasonRecord, bool) {
	e.t.Helper()
	r, ok, err := getGateReason(context.Background(), e.j, "r", call)
	if err != nil {
		e.t.Fatal(err)
	}
	return r, ok
}

var (
	readThenWrite = func() [][]Emit {
		return [][]Emit{toolTurn("c1", "read", `{}`), toolTurn("c2", "write", `{}`), textTurn("done")}
	}
)

// A write requested after the run read untrusted content is held for the escalation gate, though its tool
// has no gate of its own; the approval of the escalation instance runs it; the record says why.
func TestTaintGate_AWriteAfterAnUntrustedReadWaitsForTheEscalationGate(t *testing.T) {
	e := newTG(t)
	err := e.drive(true, readThenWrite()...)
	pendingOn(t, err, "c2#taint")
	if e.writes.n.Load() != 0 || e.reads.n.Load() != 1 {
		t.Fatalf("reads %d, writes %d; want 1 and 0", e.reads.n.Load(), e.writes.n.Load())
	}
	rec, ok := e.reasonOf("c2")
	if !ok || len(rec.Reasons) != 1 || rec.Reasons[0].Kind != gateReasonTaint || rec.Reasons[0].Source != "crm" || rec.Gate == nil || rec.PolicyDigest != gateDigest(e.policy.Gate) {
		t.Fatalf("gate-reason = %+v, %v; want one taint reason from crm, the gate and its digest", rec, ok)
	}
	// Still pending on a re-drive by a fresh agent (a resume): nothing ran meanwhile.
	pendingOn(t, e.drive(true, textTurn("done")), "c2#taint")
	if err := Approve(context.Background(), e.j, "r", "c2#taint", true); err != nil {
		t.Fatal(err)
	}
	if err := e.drive(true, textTurn("done")); err != nil {
		t.Fatalf("after the approval: %v", err)
	}
	if e.writes.n.Load() != 1 || e.reads.n.Load() != 1 {
		t.Fatalf("reads %d, writes %d; want 1 and 1", e.reads.n.Load(), e.writes.n.Load())
	}
}

func TestTaintGate_ADenialIsFinalAndTheModelReadsIt(t *testing.T) {
	e := newTG(t)
	pendingOn(t, e.drive(true, readThenWrite()...), "c2#taint")
	if err := Approve(context.Background(), e.j, "r", "c2#taint", false); err != nil {
		t.Fatal(err)
	}
	if err := e.drive(true, textTurn("done")); err != nil {
		t.Fatal(err)
	}
	r, _, _ := e.j.Get(context.Background(), "r", ToolResultStep("c2"))
	if !r.IsError || !strings.Contains(string(r.Result), "denied") || e.writes.n.Load() != 0 {
		t.Fatalf("result %s (error %v), writes %d; want a denial and no write", r.Result, r.IsError, e.writes.n.Load())
	}
}

// What taint does not gate: a read-only tool, a call requested in the same turn as the read, a run that
// started with no policy, and a tool the registered spec exempts.
func TestTaintGate_WhatIsNotGated(t *testing.T) {
	t.Run("a read-only tool", func(t *testing.T) {
		e := newTG(t)
		if err := e.drive(true, toolTurn("c1", "read", `{}`), toolTurn("c2", "read", `{}`), textTurn("done")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the same turn as the read", func(t *testing.T) {
		e := newTG(t)
		two := []Emit{
			{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "read", ArgsFragment: []byte(`{}`)}},
			{Event: ToolCallDelta{Index: 1, ID: "c2", Name: "write", ArgsFragment: []byte(`{}`)}},
			{Event: Finish{Reason: "tool_use"}},
		}
		if err := e.drive(true, two, textTurn("done")); err != nil {
			t.Fatalf("a write beside the read: %v", err)
		}
		if e.writes.n.Load() != 1 {
			t.Fatalf("writes = %d, want 1", e.writes.n.Load())
		}
	})
	t.Run("a run started with no policy", func(t *testing.T) {
		e := newTG(t)
		if err := e.drive(false, toolTurn("c1", "read", `{}`), ErrorTurnEmit()); err == nil {
			t.Fatal("seeding drive did not stop")
		}
		if err := e.drive(true, toolTurn("c2", "write", `{}`), textTurn("done")); err != nil {
			t.Fatalf("a run started with no policy, driven under one: %v (never retro-gated)", err)
		}
		if e.writes.n.Load() != 1 {
			t.Fatal("the write did not run")
		}
	})
	t.Run("an exempt tool", func(t *testing.T) {
		e := newTG(t)
		e.writeOp = []ToolOption{TaintExempt("reviewed, writes to a sandbox")}
		if err := e.drive(true, readThenWrite()...); err != nil {
			t.Fatalf("an exempt write: %v", err)
		}
		rec, ok := e.reasonOf("c2")
		if !ok || rec.Exempt != "reviewed, writes to a sandbox" {
			t.Fatalf("gate-reason = %+v, %v; want the exemption recorded", rec, ok)
		}
	})
}

// ErrorTurnEmit is a model turn that fails, for a seeding drive that stops.
func ErrorTurnEmit() []Emit { return errTurn(errors.New("model down")) }

// A tool with its own gate and the escalation gate: both are asked, the tool's own first, and a decision
// for one instance never counts for the other.
func TestTaintGate_BothGatesAreEnforced(t *testing.T) {
	e := newTG(t)
	e.writeOp = []ToolOption{WithApproval(SingleApproval())}
	ctx := context.Background()
	// The escalation instance is decided first: the tool's own gate is still asked.
	pendingOn(t, e.drive(true, readThenWrite()...), "c2")
	if err := Approve(ctx, e.j, "r", "c2#taint", true); err != nil {
		t.Fatal(err)
	}
	pendingOn(t, e.drive(true, textTurn("done")), "c2")
	if e.writes.n.Load() != 0 {
		t.Fatal("the write ran on the escalation approval alone")
	}
	if err := Approve(ctx, e.j, "r", "c2", true); err != nil {
		t.Fatal(err)
	}
	if err := e.drive(true, textTurn("done")); err != nil {
		t.Fatal(err)
	}
	if e.writes.n.Load() != 1 {
		t.Fatalf("writes = %d, want 1 once both gates passed", e.writes.n.Load())
	}

	// And the other order: the own gate passes, the escalation instance is asked next, a denial of it wins.
	e = newTG(t)
	e.writeOp = []ToolOption{WithApproval(SingleApproval())}
	pendingOn(t, e.drive(true, readThenWrite()...), "c2")
	if err := Approve(ctx, e.j, "r", "c2", true); err != nil {
		t.Fatal(err)
	}
	pendingOn(t, e.drive(true, textTurn("done")), "c2#taint")
	if err := Approve(ctx, e.j, "r", "c2#taint", false); err != nil {
		t.Fatal(err)
	}
	if err := e.drive(true, textTurn("done")); err != nil {
		t.Fatal(err)
	}
	if e.writes.n.Load() != 0 {
		t.Fatal("the write ran though the escalation gate denied it")
	}
}

// An approval is for one call: the next write in the run is asked again.
func TestTaintGate_AnApprovalDoesNotClearTaint(t *testing.T) {
	e := newTG(t)
	turns := [][]Emit{toolTurn("c1", "read", `{}`), toolTurn("c2", "write", `{}`), toolTurn("c3", "write", `{}`), textTurn("done")}
	pendingOn(t, e.drive(true, turns...), "c2#taint")
	if err := Approve(context.Background(), e.j, "r", "c2#taint", true); err != nil {
		t.Fatal(err)
	}
	pendingOn(t, e.drive(true, toolTurn("c3", "write", `{}`), textTurn("done")), "c3#taint")
	if e.writes.n.Load() != 1 {
		t.Fatalf("writes = %d, want only the approved one", e.writes.n.Load())
	}
}

func TestTaintGate_RefuseMode(t *testing.T) {
	e := newTG(t)
	e.policy = taintPolicy{Refuse: true}
	if err := e.drive(true, readThenWrite()...); err != nil {
		t.Fatal(err)
	}
	r, _, _ := e.j.Get(context.Background(), "r", ToolResultStep("c2"))
	if !r.IsError || string(r.Result) != taintRefusedResult || e.writes.n.Load() != 0 {
		t.Fatalf("result %s (error %v), writes %d; want the refusal and no write", r.Result, r.IsError, e.writes.n.Load())
	}
	if _, ok := e.reasonOf("c2"); !ok {
		t.Fatal("no gate-reason for a refused call")
	}
}

// The reason record can only add: a planted record with no reasons, or an exemption text, removes
// nothing; a planted taint reason holds a call whose turn read nothing marked; a record naming another
// escalation gate holds the call.
func TestTaintGate_TheReasonRecordOnlyAdds(t *testing.T) {
	ctx := context.Background()
	plant := func(e *tgEnv, rec gateReasonRecord) {
		if _, _, err := putGateReason(ctx, e.j, "r", rec); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("empty and exempt records remove nothing", func(t *testing.T) {
		e := newTG(t)
		// A first drive up to the read, then a record planted for the write before it is evaluated.
		if err := e.drive(true, toolTurn("c1", "read", `{}`), ErrorTurnEmit()); err == nil {
			t.Fatal("seeding drive did not stop")
		}
		plant(e, gateReasonRecord{Format: gateReasonFormat, ToolUseID: "c2", PolicyDigest: gateDigest(e.policy.Gate), Exempt: "planted"})
		pendingOn(t, e.drive(true, toolTurn("c2", "write", `{}`), textTurn("done")), "c2#taint")
	})
	t.Run("a planted taint reason holds a call that read nothing marked", func(t *testing.T) {
		e := newTG(t)
		if err := e.drive(true, ErrorTurnEmit()); err == nil {
			t.Fatal("seeding drive did not stop")
		}
		plant(e, gateReasonRecord{Format: gateReasonFormat, ToolUseID: "c1", PolicyDigest: gateDigest(e.policy.Gate),
			Reasons: []gateReason{{Kind: gateReasonTaint, Source: "planted", Ref: "x", Pos: 1}}})
		pendingOn(t, e.drive(true, toolTurn("c1", "write", `{}`), textTurn("done")), "c1#taint")
		if e.writes.n.Load() != 0 {
			t.Fatal("the write ran")
		}
	})
	t.Run("another escalation gate holds the call", func(t *testing.T) {
		e := newTG(t)
		pendingOn(t, e.drive(true, readThenWrite()...), "c2#taint")
		e.policy = taintPolicy{Gate: &ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}}
		e.opts = []Option{noKeys()}
		err := e.drive(true, textTurn("done"))
		if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "escalation gate") {
			t.Fatalf("a changed escalation gate: %v, want an ErrConfig naming the gate", err)
		}
		if e.writes.n.Load() != 0 {
			t.Fatal("the write ran")
		}
	})
}

// The escalation instance of a call resolves to the call, at every site that keys on the ID.
func TestFindToolCall_ResolvesTheEscalationSubject(t *testing.T) {
	recs := []Record{{Kind: StepModel, Message: &Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "write", Args: []byte(`{"a":1}`)}}}}}
	i, call, ok := FindToolCall(recs, "c1#taint")
	if !ok || i != 0 || call.ID != "c1#taint" || call.Name != "write" || string(call.Args) != `{"a":1}` {
		t.Fatalf("FindToolCall(c1#taint) = %d %+v %v", i, call, ok)
	}
	if _, _, ok := FindToolCall(recs, "c2#taint"); ok {
		t.Error("an escalation subject of an unknown call resolved")
	}
	// A plan node named foo#taint is a node: its subject is not mapped to a node foo.
	req := valueRec(t, ApprovalRequestStep("node:foo"), ApprovalRequest{ToolUseID: "node:foo", ToolName: "foo", Args: []byte(`{}`)})
	nodeRecs := []Record{req}
	if _, _, ok := FindToolCall(nodeRecs, "node:foo"); !ok {
		t.Fatal("the node itself does not resolve")
	}
	if _, _, ok := FindToolCall(nodeRecs, "node:foo#taint"); ok {
		t.Error("a plan node named foo#taint resolved as the escalation instance of a node foo")
	}
	if _, call, ok := FindToolCall(recs, "c1"); !ok || call.ID != "c1" {
		t.Errorf("the call itself = %+v %v", call, ok)
	}
}

// A compensation (a saga's rollback re-run) is never gated by taint.
func TestTaintGate_CompensationIsNotGated(t *testing.T) {
	e := newTG(t)
	a := mustNew(&scriptModel{turns: [][]Emit{textTurn("x")}}, e.j, WithTools(e.writes.tool("write")), withTaintPolicy(e.policy))
	spec := a.specs["write"]
	ctx := withUnderTaint(context.Background(), "d")
	marks := []taintMark{{Source: "crm", Ref: "tool:read/c1", Pos: 2}}
	if esc, err := a.escalate(ctx, "r", ToolUse{ID: "c2", Name: "write"}, spec, marks); err != nil || esc.gate == nil {
		t.Fatalf("escalate = %+v, %v; want the gate", esc, err)
	}
	if esc, err := a.escalate(withCompensating(ctx), "r", ToolUse{ID: "c3", Name: "write"}, spec, marks); err != nil || esc.gate != nil || esc.refuse {
		t.Fatalf("a compensation: %+v, %v; want none", esc, err)
	}
}

// The escalation gate may be certificate-bound: its counted approvals are re-checked immediately before
// the call runs (preExecute), as the tool's own gate's are, and a revoked one holds the call.
func TestTaintGate_ACertificateBoundEscalationGateIsReCheckedBeforeTheCallRuns(t *testing.T) {
	ok := func(id string, k byte) *fakeCertVerifier {
		return &fakeCertVerifier{id: id, check: func(CertDecision) CertCheck {
			return CertCheck{Identity: id, KeyHash: [32]byte{k}, Tier: "final", Basis: "b"}
		}}
	}
	alice, bob := ok("alice", 1), ok("bob", 2)
	e := newTG(t)
	e.policy = taintPolicy{Gate: &ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}}
	e.opts = []Option{WithApproverVerifiers(certResolver(map[string]ApproverVerifier{"alice": alice, "bob": bob}))}
	pendingOn(t, e.drive(true, readThenWrite()...), "c2#taint")
	dec := certRec("approval:"+encodeID("c2#taint")+":alice:00", "alice")
	dec.ToolUseID = "c2#taint"
	writeRaw(t, e.j, "r", dec.Name, dec)
	alice.execCheck = func(cd CertDecision) CertCheck { revokeAt(alice); return revokedCheck(cd) }
	err := e.drive(true, textTurn("done"))
	var pend *ApprovalPending
	if !errors.As(err, &pend) || pend.Reason != PendingExecHoldReapproval || pend.ToolUseID != "c2#taint" {
		t.Fatalf("err = %v, want the escalation instance held for re-approval", err)
	}
	if e.writes.n.Load() != 0 {
		t.Fatal("the write ran on a revoked approval")
	}
	if _, held := hasStep(t, e.j, "r", ApprovalExecHoldStep("c2#taint", 0)); !held {
		t.Fatal("no exec hold was written for the escalation instance")
	}
}

// A signed m-of-n escalation gate: decisions sign the derived subject, so a decision signed for the call
// does not count on the instance and the reverse.
func TestTaintGate_SignedDecisionsAreForOneInstance(t *testing.T) {
	e := newTG(t)
	e.policy = taintPolicy{Gate: &ApprovalPolicy{Need: 2, Approvers: []string{"ops", "risk"}}}
	e.opts = []Option{WithApproverVerifiers(fakeVerifiers("ops", "risk", "own"))}
	e.writeOp = []ToolOption{WithApproval(&ApprovalPolicy{Need: 1, Approvers: []string{"own"}})}
	pendingOn(t, e.drive(true, readThenWrite()...), "c2")
	approveAs(t, e.j, "r", "c2", "own", true)
	// ops and risk sign the escalation instance; "own" signs it too, which the gate does not list.
	err := e.drive(true, textTurn("done"))
	var pend *ApprovalPending
	if !errors.As(err, &pend) || pend.ToolUseID != "c2#taint" || pend.Quorum == nil || pend.Quorum.Approved != 0 {
		t.Fatalf("err = %v, want the escalation instance with no approvals counted", err)
	}
	approveAs(t, e.j, "r", "c2#taint", "ops", true)
	err = e.drive(true, textTurn("done"))
	if !errors.As(err, &pend) || pend.Quorum.Approved != 1 {
		t.Fatalf("err = %v, want one of two approvals", err)
	}
	approveAs(t, e.j, "r", "c2#taint", "risk", true)
	if err := e.drive(true, textTurn("done")); err != nil {
		t.Fatalf("both approvals: %v", err)
	}
	if e.writes.n.Load() != 1 {
		t.Fatalf("writes = %d, want 1", e.writes.n.Load())
	}
	// A decision signed for the call's own subject was never accepted on the instance's: replay it.
	own := subjectOf(t, e.j, "r", "c2")
	inst := subjectOf(t, e.j, "r", "c2#taint")
	if string(ApprovalDecisionBytes(own, "ops", true, testDecidedAt)) == string(ApprovalDecisionBytes(inst, "ops", true, testDecidedAt)) {
		t.Fatal("the signed bytes of the call and of its escalation instance are equal")
	}
}

// nthInsertFails fails, uncommitted, the n-th Insert it sees (1-based) and none other.
type nthInsertFails struct {
	Store
	n    int
	seen *int
}

func (f nthInsertFails) Unwrap() Store { return f.Store }
func (f nthInsertFails) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	*f.seen++
	if *f.seen == f.n {
		return Entry{}, false, errCovBlip
	}
	return f.Store.Insert(ctx, runID, name, data)
}

// A store fault at any write of a tainted run, however it lands, never lets the write tool run without
// both instances' approvals, never runs it twice, and the run finishes once re-driven and approved (or
// halts for a reconciler, the existing at-most-once outcome of a fault while a result is recorded).
func TestTaintGate_CrashAtEveryWrite(t *testing.T) {
	ctx := context.Background()
	faulted := 0
	for n := 1; n <= 40; n++ {
		inner := NewMemStore()
		seen := 0
		reads, writes := &counter{}, &counter{}
		mk := func(st Store) *tgEnv {
			return &tgEnv{t: t, j: mustJournal(st), reads: reads, writes: writes, policy: taintPolicy{Gate: SingleApproval()}, writeOp: []ToolOption{WithApproval(SingleApproval())}}
		}
		finished := false
		for step := 0; step < 14 && !finished; step++ {
			st := Store(nthInsertFails{Store: inner, n: n, seen: &seen}) // fails the n-th write of all the drives
			// The model turns not yet recorded: a re-drive replays the recorded ones.
			recs, _ := mustJournal(inner).History(ctx, "r")
			modelTurns := 0
			for _, r := range recs {
				if r.Kind == StepModel {
					modelTurns++
				}
			}
			err := mk(st).drive(true, readThenWrite()[modelTurns:]...)
			if errors.Is(err, errCovBlip) {
				faulted++
			}
			var pend *ApprovalPending
			switch {
			case err == nil:
				finished = true
			case errors.As(err, new(*OutcomeUnknown)):
				// A fault while the write's result was recorded: the existing at-most-once halt. The tool
				// ran once; the run waits for a reconciler, as it does without taint.
				finished = true
			case errors.As(err, &pend):
				if aerr := Approve(ctx, mustJournal(inner), "r", pend.ToolUseID, true); aerr != nil {
					t.Fatalf("n=%d: approve %s: %v", n, pend.ToolUseID, aerr)
				}
			}
			if w := writes.n.Load(); w > 1 {
				t.Fatalf("n=%d: the write ran %d times", n, w)
			}
		}
		if !finished {
			t.Fatalf("n=%d: the run did not finish after re-driving", n)
		}
		if writes.n.Load() != 1 {
			t.Fatalf("n=%d: writes = %d, want 1", n, writes.n.Load())
		}
		recs, _ := mustJournal(inner).History(ctx, "r")
		var sawOwn, sawEsc bool
		for _, r := range recs {
			if r.Kind == StepApproval && r.ToolUseID == "c2" {
				sawOwn = true
			}
			if r.Kind == StepApproval && r.ToolUseID == "c2#taint" {
				sawEsc = true
			}
		}
		if !sawOwn || !sawEsc {
			t.Fatalf("n=%d: the write ran without both approvals recorded (own %v, escalation %v)", n, sawOwn, sawEsc)
		}
	}
	if faulted < 10 {
		t.Fatalf("only %d of 40 injected faults surfaced; the test is not reaching the run's writes", faulted)
	}
}

// tgLoadCounter counts the Loads of a store.
type tgLoadCounter struct {
	Store
	n *int
}

func (l tgLoadCounter) Unwrap() Store { return l.Store }
func (l tgLoadCounter) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	*l.n++
	return l.Store.Load(ctx, runID, after)
}

// F3: the fold is computed once per model turn, not once per call: a turn of three exempt writes (each
// evaluated, none paused) reads the journal no more often than a turn of one.
func TestTaintGate_TheJournalIsReadOncePerTurn(t *testing.T) {
	loads := func(calls int) int {
		n := 0
		e := newTG(t)
		e.j = mustJournal(tgLoadCounter{Store: NewMemStore(), n: &n})
		e.writeOp = []ToolOption{TaintExempt("reviewed")}
		var parts []Emit
		for i := 0; i < calls; i++ {
			parts = append(parts, Emit{Event: ToolCallDelta{Index: i, ID: "w" + string(rune('a'+i)), Name: "write", ArgsFragment: []byte(`{}`)}})
		}
		parts = append(parts, Emit{Event: Finish{Reason: "tool_use"}})
		if err := e.drive(true, toolTurn("c1", "read", `{}`), parts, textTurn("done")); err != nil {
			t.Fatal(err)
		}
		if e.writes.n.Load() != int32(calls) {
			t.Fatalf("writes = %d, want %d", e.writes.n.Load(), calls)
		}
		return n
	}
	one, three := loads(1), loads(3)
	if three > one {
		t.Fatalf("a turn of three tainted writes loaded the journal %d times, a turn of one %d: the fold must be shared by the turn's calls", three, one)
	}
}

// A recorded denial of the escalation instance is final even after the policy is gone: a redeploy with no
// taint policy does not run the call that the escalation gate denied.
func TestTaintGate_ADenialOfTheInstanceSurvivesARedeployWithNoPolicy(t *testing.T) {
	ctx := context.Background()
	t.Run("a one-decision gate", func(t *testing.T) {
		e := newTG(t)
		pendingOn(t, e.drive(true, readThenWrite()...), "c2#taint")
		if err := Approve(ctx, e.j, "r", "c2#taint", false); err != nil {
			t.Fatal(err)
		}
		if err := e.drive(false, textTurn("done")); err != nil { // redeployed: no policy
			t.Fatalf("re-drive with no policy: %v", err)
		}
		r, _, _ := e.j.Get(ctx, "r", ToolResultStep("c2"))
		if !r.IsError || !strings.Contains(string(r.Result), "denied") || e.writes.n.Load() != 0 {
			t.Fatalf("result %s (error %v), writes %d; want the denial to stand", r.Result, r.IsError, e.writes.n.Load())
		}
	})
	t.Run("an m-of-n gate whose terminal tally denied", func(t *testing.T) {
		e := newTG(t)
		pol := &ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
		e.policy = taintPolicy{Gate: pol}
		e.opts = []Option{WithApproverVerifiers(fakeVerifiers("ops"))}
		pendingOn(t, e.drive(true, readThenWrite()...), "c2#taint")
		// The gate's terminal tally of the instance, denied, as a counting drive would have recorded it.
		tally := ApprovalTally{Need: 1, Approvers: []string{"ops"}, Denied: 1, DeniedBy: []string{"ops"}}
		writeRaw(t, e.j, "r", ApprovalTallyStep("c2#taint"), valueRec(t, ApprovalTallyStep("c2#taint"), tally))
		e.opts = nil
		if err := e.drive(false, textTurn("done")); err != nil {
			t.Fatalf("re-drive with no policy: %v", err)
		}
		r, _, _ := e.j.Get(ctx, "r", ToolResultStep("c2"))
		if !r.IsError || e.writes.n.Load() != 0 {
			t.Fatalf("result %s (error %v), writes %d; want the denial to stand", r.Result, r.IsError, e.writes.n.Load())
		}
	})
}

// A denial of the tool's own gate ends the call: the escalation instance is never asked, so it has no
// pause note and no reason record.
func TestTaintGate_AnOwnGateDenialEndsTheCallBeforeTheInstanceIsAsked(t *testing.T) {
	ctx := context.Background()
	e := newTG(t)
	e.writeOp = []ToolOption{WithApproval(SingleApproval())}
	pendingOn(t, e.drive(true, readThenWrite()...), "c2")
	if err := Approve(ctx, e.j, "r", "c2", false); err != nil {
		t.Fatal(err)
	}
	if err := e.drive(true, textTurn("done")); err != nil {
		t.Fatalf("after the own-gate denial: %v", err)
	}
	if e.writes.n.Load() != 0 {
		t.Fatal("the write ran")
	}
	if _, ok := hasStep(t, e.j, "r", ApprovalPendingStep("c2#taint")); ok {
		t.Error("the escalation instance was asked after the own gate denied the call")
	}
	if _, ok := e.reasonOf("c2"); ok {
		t.Error("a gate-reason was written for a call the own gate denied")
	}
}
