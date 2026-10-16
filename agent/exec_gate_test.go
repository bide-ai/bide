package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go/ast"
	"go/token"
	"iter"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The pre-execution check of a gate with certificate-bound approvers (preExecute): every path that can run
// the tool passes it. fakeCertVerifier answers it with execCheck (else with check).

var (
	okCheck      = func(CertDecision) CertCheck { return CertCheck{Identity: "x", KeyHash: [32]byte{1}, Tier: "final"} }
	staleCheck   = func(CertDecision) CertCheck { return CertCheck{Reason: ReasonStatusStale, Detail: "d"} }
	revokedCheck = func(CertDecision) CertCheck {
		return CertCheck{Reason: ReasonRevoked, Detail: "d", Anchored: time.Date(2026, 10, 13, 12, 14, 13, 0, time.UTC),
			Revocation: &CertRevocation{Serial: "42", E: "2026-10-13T12:00:00Z", ReasonCode: 1, CRL: "ab12"}}
	}
)

// revokeAt makes approver v's certificate revoked from now on, for the count and for the check before
// execution alike (the status a gate holds is one thing).
func revokeAt(v *fakeCertVerifier) {
	v.check, v.execCheck = revokedCheck, revokedCheck
}

func (g *boundGate) holdRecord(gen int) (ExecHold, bool) {
	g.t.Helper()
	r, ok := hasStep(g.t, g.store, "r1", ApprovalExecHoldStep("c1", gen))
	if !ok {
		return ExecHold{}, false
	}
	var h ExecHold
	if err := json.Unmarshal(r.Result, &h); err != nil {
		g.t.Fatal(err)
	}
	return h, true
}

// With every counted approval holding, the call runs and nothing is held.
func TestPreExecute_RunsWhenTheApprovalsHold(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	g.decide("alice")
	calls := 0
	g.alice.execCheck = func(cd CertDecision) CertCheck { calls++; return okCheck(cd) }
	if _, err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
	if calls != 1 {
		t.Fatalf("the check before execution ran %d times", calls)
	}
	if _, held := g.holdRecord(0); held || g.has(ApprovalIdentTallyStep("c1", 1)) {
		t.Fatal("a hold or a second generation was recorded")
	}
	// It saw the decision in its place in the journal, for the call as the model made it.
	if g.alice.execSeen == nil || g.alice.execSeen.Subject.ToolName != "charge" || g.alice.execSeen.Index < 0 || g.alice.execSeen.Record.Approver() != "alice" {
		t.Fatalf("%+v", g.alice.execSeen)
	}
}

// A counted approval whose certificate is revoked between the count and the call: the call does not run, an
// exec hold of generation 0 is written with the evidence, the gate asks again, and approvers who decide
// again run it under generation 1.
func TestPreExecute_RevokedApprovalHoldsAndReasks(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	g.decide("alice")
	g.alice.execCheck = func(cd CertDecision) CertCheck { revokeAt(g.alice); return revokedCheck(cd) }
	_, err := g.run()
	pend := wantReason(t, err, PendingExecHoldReapproval)
	if g.charged != 0 || pend.Quorum == nil || pend.Quorum.Approved != 0 {
		t.Fatalf("charged %d, quorum %+v", g.charged, pend.Quorum)
	}
	h, ok := g.holdRecord(0)
	if !ok || h.Format != ApprovalExecHoldFormat || h.ToolUseID != "c1" || h.Generation != 0 || h.Reason != ReasonRevoked ||
		!slices.Equal(h.Approvers, []string{"alice"}) || h.Serial != "42" || h.E != "2026-10-13T12:00:00Z" || h.ReasonCode != 1 || h.CRLHash != "ab12" ||
		h.AnchoredAt != "2026-10-13T12:14:13Z" || h.CheckedAt == "" || h.Detail == "" {
		t.Fatalf("%+v ok %v", h, ok)
	}
	// A phase note says the call was held (the count of generation 1 then waits for decisions).
	held := false
	for _, r := range pauseNotes(mustHistory(t, g.store, "r1"), "c1") {
		var w ApprovalWait
		if json.Unmarshal(r.Result, &w) == nil && w.Pause == PauseExecHold && w.PauseDetail != "" {
			held = true
		}
	}
	if !held {
		t.Fatal("no exec-hold phase note")
	}
	// The re-ask is announced, once, under the next generation.
	if !g.has(ApprovalIdentReaskStep("c1", 1)) {
		t.Fatal("no re-ask note")
	}
	g.decide("bob")
	if _, err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
	if !g.has(ApprovalIdentTallyStep("c1", 1)) || !g.has(ApprovalIdentityStep("c1", 1)) {
		t.Fatal("generation 1 is not recorded")
	}
}

// When the decisions that still count already pass, the next generation is counted at once and the call
// runs in the same drive, after its own check.
func TestPreExecute_ReplacementThatAlreadyPassesRuns(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	g.decide("alice")
	g.decide("bob")
	g.alice.execCheck = func(cd CertDecision) CertCheck { revokeAt(g.alice); return revokedCheck(cd) }
	bobChecks := 0
	g.bob.execCheck = func(cd CertDecision) CertCheck { bobChecks++; return okCheck(cd) }
	if _, err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
	t1 := g.tally(ApprovalIdentTallyStep("c1", 1))
	if !slices.Equal(t1.ApprovedBy, []string{"bob"}) || t1.Supersedes != ApprovalIdentTallyStep("c1", 0) {
		t.Fatalf("%+v", t1)
	}
	if bobChecks < 2 { // generation 0 (bob held) and generation 1 (the one that ran)
		t.Fatalf("bob was checked %d times", bobChecks)
	}
}

// Status that is stale holds the call without writing anything authoritative: no hold, no new tally; the
// same approvals run it when the check passes again.
func TestPreExecute_StaleStatusWaitsAndRunsOnTheSameTally(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	g.decide("alice")
	g.alice.execCheck = staleCheck
	_, err := g.run()
	pend := wantReason(t, err, PendingIdentityStatus)
	if g.charged != 0 || pend.Quorum == nil || !pend.Quorum.Passed() {
		t.Fatalf("charged %d quorum %+v", g.charged, pend.Quorum)
	}
	if _, held := g.holdRecord(0); held || g.has(ApprovalIdentTallyStep("c1", 1)) {
		t.Fatal("a stale check wrote a hold or a new generation")
	}
	recs := mustHistory(t, g.store, "r1")
	if w, ok := LatestPausePhase(recs, "c1"); !ok || w.Pause != PauseExecHold {
		t.Fatalf("%+v %v", w, ok)
	}
	// A second drive with the same result writes no second note and still does not run.
	n := len(pauseNotes(recs, "c1"))
	_, err = g.run()
	wantReason(t, err, PendingIdentityStatus)
	if got := len(pauseNotes(mustHistory(t, g.store, "r1"), "c1")); got != n {
		t.Fatalf("%d phase notes, then %d", n, got)
	}
	// Status is current again: the call runs on the tally already recorded.
	g.alice.execCheck = nil
	if _, err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
	if g.has(ApprovalIdentTallyStep("c1", 1)) {
		t.Fatal("the run counted again")
	}
}

// A stale-status wait does not notify anyone: nobody is asked to decide.
func TestPreExecute_StaleStatusIsNoApprovalNotification(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	g.decide("alice")
	g.alice.execCheck = staleCheck
	_, err := g.run()
	e := &notifyEngine{cfg: defaultNotifyConfig(), j: g.store}
	if ev, ok := e.eventFromErr("r1", err); ok {
		t.Fatalf("event %+v", ev)
	}
}

// A decision that no longer verifies for a reason that cannot mend (another approver now shares its key)
// is held like a revocation; one that cannot be proven right now waits.
func TestPreExecute_ReasonsThatHoldAndReasonsThatWait(t *testing.T) {
	for _, tc := range []struct {
		reason string
		hold   bool
	}{
		{ReasonRevoked, true}, {ReasonSharedKey, true}, {ReasonBadSig, true}, {ReasonCertChain, true},
		{ReasonStatusUnknown, false}, {ReasonStatusStale, false}, {ReasonNoCoveringHead, false}, {ReasonHeadTimes, false},
	} {
		g := newBoundGate(t)
		_, _ = g.run()
		g.decide("alice")
		bad := func(CertDecision) CertCheck { return CertCheck{Reason: tc.reason} }
		g.alice.execCheck = func(CertDecision) CertCheck {
			if tc.hold { // the status a gate holds is one thing: the count fails the same way from now on
				g.alice.check = bad
			}
			return bad(CertDecision{})
		}
		_, err := g.run()
		want := PendingIdentityStatus
		if tc.hold {
			want = PendingExecHoldReapproval
		}
		var pend *ApprovalPending
		if !errors.As(err, &pend) || pend.Reason != want || g.charged != 0 {
			t.Fatalf("%q: %v charged %d", tc.reason, err, g.charged)
		}
		if _, held := g.holdRecord(0); held != tc.hold {
			t.Fatalf("%q: hold written %v", tc.reason, held)
		}
		if tc.hold {
			// The hold names the reason of the failure.
			if h, _ := g.holdRecord(0); h.Reason != tc.reason {
				t.Fatalf("%q: hold reason %q", tc.reason, h.Reason)
			}
		}
	}
}

// A verifier that cannot do the check holds every call it authorized (fail closed).
func TestPreExecute_VerifierWithoutTheCheckWaits(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	g.decide("alice")
	vf := func(id string) (ApproverVerifier, bool) {
		switch id {
		case "alice":
			return noExecVerifier{g.alice}, true
		case "bob":
			return g.bob, true
		}
		return nil, false
	}
	_, err := mofnRun(g.store, "r1", false, g.pol, vf, &g.charged)
	wantReason(t, err, PendingIdentityStatus)
	if g.charged != 0 {
		t.Fatal("the call ran")
	}
}

// noExecVerifier hides VerifyCertExec.
type noExecVerifier struct{ f *fakeCertVerifier }

func (n noExecVerifier) Alg() Alg                { return n.f.Alg() }
func (n noExecVerifier) Verify(m, s []byte) bool { return n.f.Verify(m, s) }
func (n noExecVerifier) KeyIDs() []string        { return n.f.KeyIDs() }
func (n noExecVerifier) VerifyCertDecision(cd CertDecision) CertCheck {
	return n.f.VerifyCertDecision(cd)
}

// A crash between the hold and the pause leaves the hold: the next drive counts the next generation.
func TestPreExecute_ACrashAfterTheHoldIsFinishedByTheNextDrive(t *testing.T) {
	g := newBoundGate(t)
	g.holdGenerationZero() // the hold is journaled; alice fails the count now
	g.decide("bob")
	if _, err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
	if !g.has(ApprovalIdentTallyStep("c1", 1)) {
		t.Fatal("no generation 1")
	}
}

// Two drives that hold the same generation write one hold; the second finds it.
func TestPreExecute_TwoDrivesWriteOneHold(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	g.decide("alice")
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	s := subjectOf(t, g.store, "r1", "c1")
	if _, final, err := tallyGate(context.Background(), g.store, "r1", s, g.pol, vf, ""); err != nil || !final {
		t.Fatalf("%v %v", final, err)
	}
	v := ExecVerdict{Outcome: ExecReask, Checked: 1, Failed: []ExecFailure{{Approver: "alice", Reason: ReasonRevoked}}}
	for range 2 {
		if err := writeExecHold(context.Background(), g.store, "r1", s, g.pol, vf, 0, v); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, r := range mustHistory(t, g.store, "r1") {
		if r.Name == ApprovalExecHoldStep("c1", 0) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d holds", n)
	}
}

// A gate with no certificate-bound approver is untouched: no history read for the check, no records.
func TestPreExecute_BareGateIsUntouched(t *testing.T) {
	store := memJournal()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"alice"}}
	var charged int
	_, _ = mofnRun(store, "r1", true, pol, fakeVerifiers("alice"), &charged)
	approveAs(t, store, "r1", "c1", "alice", true)
	if _, err := mofnRun(store, "r1", false, pol, fakeVerifiers("alice"), &charged); err != nil || charged != 1 {
		t.Fatalf("%v %d", err, charged)
	}
	for _, r := range mustHistory(t, store, "r1") {
		if len(r.Name) > 17 && r.Name[:17] == "approval-exec-hol" {
			t.Fatalf("a hold on a bare gate: %s", r.Name)
		}
	}
	if err := preExecute(context.Background(), store, "r1", "r1", "c1", pol, fakeVerifiers("alice"), ""); err != nil {
		t.Fatal(err)
	}
	if err := preExecute(context.Background(), store, "r1", "r1", "c1", nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := preExecute(context.Background(), store, "r1", "r1", "c1", SingleApproval(), nil, ""); err != nil {
		t.Fatal(err)
	}
}

// A tally with no identity record cannot be re-checked: the call does not run and the error says why.
func TestPreExecute_MissingIdentityRecordRefuses(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	g.decide("alice")
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	// The gate's tally without the record that belongs to it: written by hand.
	writeRaw(t, g.store, "r1", ApprovalIdentTallyStep("c1", 0), valueRec(t, ApprovalIdentTallyStep("c1", 0), passedTally()))
	err := preExecute(context.Background(), g.store, "r1", "r1", "c1", g.pol, vf, "")
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("%v", err)
	}
	// A recorded denial does not approve a call either.
	g2 := newBoundGate(t)
	_, _ = g2.run()
	writeRaw(t, g2.store, "r1", ApprovalIdentTallyStep("c1", 0), valueRec(t, ApprovalIdentTallyStep("c1", 0), deniedTally()))
	vf2 := certResolver(map[string]ApproverVerifier{"alice": g2.alice, "bob": g2.bob})
	if err := preExecute(context.Background(), g2.store, "r1", "r1", "c1", g2.pol, vf2, ""); !errors.Is(err, ErrConfig) {
		t.Fatalf("%v", err)
	}
}

// A decision record that is gone from the journal is presented empty and holds the call.
func TestPreExecute_VanishedDecisionRecordIsNotAccepted(t *testing.T) {
	ir := ApprovalIdentity{Decisions: []IdentityDecision{{Approver: "alice", Step: "approval:c1:alice:00"}}}
	cds := countedDecisions(nil, ApprovalSubject{ToolUseID: "c1"}, ir)
	if len(cds) != 1 || cds[0].Decision.Index != -1 || cds[0].Decision.Record.Name != "" {
		t.Fatalf("%+v", cds)
	}
	// And a decision of another approver or another call under the same step name is not taken.
	rec := certRec("approval:c1:alice:00", "alice")
	other := certRec("approval:c1:alice:00", "bob")
	if cds := countedDecisions([]Record{other}, ApprovalSubject{ToolUseID: "c1"}, ir); cds[0].Decision.Index != -1 {
		t.Fatal("another approver's record is taken")
	}
	if cds := countedDecisions([]Record{rec}, ApprovalSubject{ToolUseID: "c1"}, ir); cds[0].Decision.Index != 0 || cds[0].Decision.Record.Approver() != "alice" {
		t.Fatalf("%+v", cds[0])
	}
}

// ---- every path to the tool ----

// agentFor builds the gate's agent once, for a test that drives it several ways.
func (g *boundGate) agentFor(tool Tool, opts ...Option) *Agent {
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	opts = append([]Option{WithTools(tool), WithApproverVerifiers(vf)}, opts...)
	return mustNew(&scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}}, g.store, opts...)
}

// A retry (a middleware calling next again) passes the check each time: the first invocation runs the
// tool, a revocation lands, and the retry does not.
func TestPreExecute_RetryIsChecked(t *testing.T) {
	g := newBoundGate(t)
	g.pol.Need = 1
	tool := &countingTool{name: "charge", approval: g.pol, calls: &g.charged, safety: Safety{Idempotent: true}}
	g.alice.execCheck = okCheck
	attempts := 0
	retry := func(next ToolHandler) ToolHandler {
		return func(ctx context.Context, call ToolCall) (json.RawMessage, error) {
			res, err := next(ctx, call)
			if attempts++; attempts == 1 {
				revokeAt(g.alice) // a revocation lands between the invocations
				return next(ctx, call)
			}
			return res, err
		}
	}
	a := g.agentFor(tool, WithToolMiddleware(retry))
	_, _ = a.Run(context.Background(), "r1", UserText("pay"))
	g.decide("alice")
	_, err := a.Run(context.Background(), "r1", UserText("pay"))
	if g.charged != 1 {
		t.Fatalf("the tool ran %d times (err %v); the retry after the revocation must not run it", g.charged, err)
	}
	if _, held := g.holdRecord(0); !held {
		t.Fatal("the retry was not held")
	}
}

// countOnly records the gate's count of the decisions so far (generation 0 when nothing is held) without
// running the tool: the state of a drive that was lost between the tally and the call.
func (g *boundGate) countOnly() {
	g.t.Helper()
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	if _, final, err := tallyGate(context.Background(), g.store, "r1", subjectOf(g.t, g.store, "r1", "c1"), g.pol, vf, ""); err != nil || !final {
		g.t.Fatalf("%v %v", final, err)
	}
}

// A crash recovery (agent.Recover) and a RecoverLoop pass drive the same handler, so a revocation that
// landed while the run was down stops the call there too.
func TestPreExecute_RecoveryIsChecked(t *testing.T) {
	g := newBoundGate(t)
	tool := &countingTool{name: "charge", approval: g.pol, calls: &g.charged, safety: Safety{Idempotent: true}}
	a := g.agentFor(tool)
	_, _ = a.Run(context.Background(), "r1", UserText("pay"))
	g.decide("alice")
	g.countOnly() // the drive that would have run the call is lost after the tally
	// While the run is down alice's certificate is revoked.
	g.alice.execCheck = func(cd CertDecision) CertCheck { revokeAt(g.alice); return revokedCheck(cd) }
	n, err := Recover(context.Background(), g.store, ResumeAgent(a))
	if err != nil && !IsPause(err) {
		t.Logf("Recover: %v (%d)", err, n)
	}
	if g.charged != 0 {
		t.Fatalf("Recover ran the call %d times after the revocation", g.charged)
	}
	if _, held := g.holdRecord(0); !held {
		t.Fatalf("no hold after Recover (n=%d err=%v)", n, err)
	}
}

func TestPreExecute_RecoverLoopIsChecked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newBoundGate(t)
		tool := &countingTool{name: "charge", approval: g.pol, calls: &g.charged, safety: Safety{Idempotent: true}}
		a := g.agentFor(tool)
		_, _ = a.Run(context.Background(), "r1", UserText("pay"))
		g.decide("alice")
		g.countOnly()
		g.alice.execCheck = func(cd CertDecision) CertCheck { revokeAt(g.alice); return revokedCheck(cd) }
		stop := runLoop(t, g.store, ResumeAgent(a), WithRecoverInterval(20*time.Millisecond))
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		_ = stop()
		if g.charged != 0 {
			t.Fatalf("RecoverLoop ran the call %d times after the revocation", g.charged)
		}
		if _, held := g.holdRecord(0); !held {
			t.Fatal("no hold after RecoverLoop")
		}
	})
}

// A plan flow node's gate passes the same function. (plan has its own test over a flow; here the hook.)
func TestPreExecute_FlowHookRunsTheSameCheck(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	g := newBoundGate(t)
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	args := json.RawMessage(`{"amount":1}`)
	_, _ = flowGate(ctx, j, "r1", "node:pay", "pay", args, g.pol, vf)
	rec := certRec("approval:node%3Apay:alice:00", "alice")
	rec.ToolUseID = "node:pay"
	if _, err := j.do(ctx, "r1", rec.Name, func(context.Context) (Record, error) { return rec, nil }); err != nil {
		t.Fatal(err)
	}
	if ok, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, g.pol, vf); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if err := preExecute(ctx, j, "r1", "r1", "node:pay", g.pol, vf, ApprovalRequestStep("node:pay")); err != nil {
		t.Fatalf("holding approvals: %v", err)
	}
	g.alice.execCheck = func(cd CertDecision) CertCheck { revokeAt(g.alice); return revokedCheck(cd) }
	err := preExecute(ctx, j, "r1", "r1", "node:pay", g.pol, vf, ApprovalRequestStep("node:pay"))
	var pend *ApprovalPending
	if !errors.As(err, &pend) || pend.Reason != PendingExecHoldReapproval {
		t.Fatalf("%v", err)
	}
	if _, ok, _ := j.Get(ctx, "r1", ApprovalExecHoldStep("node:pay", 0)); !ok {
		t.Fatal("no hold")
	}
}

// ---- the structure: nothing reaches the tool without the check ----

// In toolHandler's base handler the tool is called in exactly two places (callCounted and t.Call), both
// after preExecute and after beginCall's counterpart in source order; and no other function of the package
// calls a registered tool's Call method. A path that bypassed preExecute would have to call the tool from
// somewhere else, and this test finds it.
func TestPreExecute_IsOnTheOnlyPathToTheTool(t *testing.T) {
	pkgs := parseAgentPackage(t)
	var pre, begin, counted, direct token.Pos
	callSites := map[string][]string{}
	for file, f := range pkgs {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if fun.Sel.Name == "Call" && len(call.Args) == 2 {
						// t.Call(ctx, args): a tool's Call.
						callSites[file] = append(callSites[file], fn.Name.Name)
						if file == "toolexec.go" && fn.Name.Name == "toolHandler" {
							direct = call.Pos()
						}
					}
				case *ast.Ident:
					if file == "toolexec.go" && fn.Name.Name == "toolHandler" {
						switch fun.Name {
						case "preExecute":
							pre = call.Pos()
						case "beginCall":
							begin = call.Pos()
						case "callCounted":
							counted = call.Pos()
						}
					}
				}
				return true
			})
		}
	}
	if pre == token.NoPos || begin == token.NoPos || counted == token.NoPos || direct == token.NoPos {
		t.Fatalf("the base handler's calls were not found: preExecute %v beginCall %v callCounted %v Call %v", pre, begin, counted, direct)
	}
	if !(pre < begin && begin < counted && begin < direct) {
		t.Fatalf("preExecute (%d) must come before beginCall (%d) and the tool's Call (%d, %d)", pre, begin, counted, direct)
	}
	for file, fns := range callSites {
		for _, fn := range fns {
			if file == "toolexec.go" && (fn == "toolHandler" || fn == "callCounted") {
				continue
			}
			// Other files may define a Call method of their own tools (their own receivers); they must not call
			// a tool registered with the agent. The only tool.Call sites outside the handler are listed here.
			if allowedToolCallSites[file+":"+fn] {
				continue
			}
			t.Errorf("%s: %s calls a Call method with two arguments; if it runs a registered tool it bypasses preExecute", file, fn)
		}
	}
}

// allowedToolCallSites are functions of package agent that call a Call(ctx, args) method on something that
// is not a tool registered with an Agent (a model, an adapter of the package's own).
var allowedToolCallSites = map[string]bool{}

// ---- errors before the call is begun record nothing ----

// loadFailStore fails Load once, after the identity tally of generation 0 is inserted: the History read
// that preExecute makes immediately before the call.
type loadFailStore struct {
	Store
	afterTally string
	armed      atomic.Bool
	failed     atomic.Bool
}

func (s *loadFailStore) Unwrap() Store { return s.Store }
func (s *loadFailStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	e, ok, err := s.Store.Insert(ctx, runID, name, data)
	if err == nil && name == s.afterTally {
		s.armed.Store(true)
	}
	return e, ok, err
}
func (s *loadFailStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	if s.armed.Load() && s.failed.CompareAndSwap(false, true) {
		return func(yield func(Entry, error) bool) { yield(Entry{}, errors.New("store blip")) }
	}
	return s.Store.Load(ctx, runID, after)
}

// A store fault in the check before the call is not the tool's result: the run fails, nothing is recorded as
// the call's result, the tool is not called, and a re-drive runs it.
func TestPreExecute_AStoreFaultRecordsNothing(t *testing.T) {
	g := newBoundGate(t)
	st := &loadFailStore{Store: NewMemStore(), afterTally: ApprovalIdentTallyStep("c1", 0)}
	g.store = mustJournal(st)
	_, _ = g.run()
	g.decide("alice")
	_, err := g.run()
	if !errors.Is(err, ErrStorage) || g.charged != 0 {
		t.Fatalf("the faulty drive: %v charged %d", err, g.charged)
	}
	if _, ok := hasStep(t, g.store, "r1", ToolResultStep("c1")); ok {
		t.Fatal("the store fault was recorded as the tool's result")
	}
	if _, err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("the re-drive: %v charged %d", err, g.charged)
	}
}

// A misconfiguration in the check (a tally with no identity record to re-check) is not the tool's result
// either: the run fails with ErrConfig and the call is not recorded as failed.
func TestPreExecute_AMisconfigurationRecordsNothing(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	g.decide("alice")
	writeRaw(t, g.store, "r1", ApprovalIdentTallyStep("c1", 0), valueRec(t, ApprovalIdentTallyStep("c1", 0), passedTally()))
	_, err := g.run()
	if !errors.Is(err, ErrConfig) || g.charged != 0 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
	if _, ok := hasStep(t, g.store, "r1", ToolResultStep("c1")); ok {
		t.Fatal("the misconfiguration was recorded as the tool's result")
	}
}

// ---- the pieces ----

// identityRecordFor takes exactly the record that belongs to the tally: its generation, tally name, digest,
// format and call must all agree.
func TestIdentityRecordFor_MatchesEveryField(t *testing.T) {
	tally := valueRec(t, ApprovalIdentTallyStep("c1", 2), passedTally())
	sum := sha256.Sum256(tally.Result)
	good := ApprovalIdentity{Format: ApprovalIdentityFormat, ToolUseID: "c1", Generation: 2, Tally: tally.Name, TallyDigest: hex.EncodeToString(sum[:])}
	rec := func(name string, ir ApprovalIdentity) []Record { return []Record{valueRec(t, name, ir)} }
	if _, ok := identityRecordFor(rec(ApprovalIdentityStep("c1", 2), good), "c1", 2, tally); !ok {
		t.Fatal("the matching record")
	}
	// The digest-named variant (a crash left another record under the plain name) is found too.
	other := good
	other.TallyDigest = "00"
	both := append(rec(ApprovalIdentityStep("c1", 2), other), valueRec(t, ApprovalIdentityDigestStep("c1", 2, good.TallyDigest), good))
	if _, ok := identityRecordFor(both, "c1", 2, tally); !ok {
		t.Fatal("the digest-named record")
	}
	for name, mod := range map[string]func(*ApprovalIdentity){
		"generation": func(x *ApprovalIdentity) { x.Generation = 1 },
		"tally name": func(x *ApprovalIdentity) { x.Tally = ApprovalIdentTallyStep("c1", 1) },
		"digest":     func(x *ApprovalIdentity) { x.TallyDigest = "ab" },
		"format":     func(x *ApprovalIdentity) { x.Format = "bide.approval-identity.v1" },
		"call":       func(x *ApprovalIdentity) { x.ToolUseID = "c2" },
	} {
		bad := good
		mod(&bad)
		if _, ok := identityRecordFor(rec(ApprovalIdentityStep("c1", 2), bad), "c1", 2, tally); ok {
			t.Errorf("a record with another %s is taken", name)
		}
	}
	// A record under another name (a different generation's) is not looked at.
	if _, ok := identityRecordFor(rec(ApprovalIdentityStep("c1", 3), good), "c1", 2, tally); ok {
		t.Error("a record named for another generation is taken")
	}
}

// A decision is found by its step name, kind and approver; a record that only shares a name is not one.
func TestCountedDecisions_MatchEveryField(t *testing.T) {
	ir := ApprovalIdentity{Decisions: []IdentityDecision{{Approver: "alice", Step: "approval:c1:alice:00"}}}
	s := ApprovalSubject{ToolUseID: "c1"}
	notDecision := Record{Name: "approval:c1:alice:00", Kind: StepValue, ToolUseID: "c1"}
	otherCall := certRec("approval:c1:alice:00", "alice")
	otherCall.ToolUseID = "c2"
	otherName := certRec("approval:c1:alice:01", "alice")
	for name, r := range map[string]Record{"a value record": notDecision, "another call": otherCall, "another name": otherName, "another approver": certRec("approval:c1:alice:00", "bob")} {
		if cds := countedDecisions([]Record{r}, s, ir); cds[0].Decision.Index != -1 {
			t.Errorf("%s is taken", name)
		}
	}
}

// describedVerifier makes a fake verifier describe its policy, for the exec hold's digest.
type describedVerifier struct{ *fakeCertVerifier }

func (describedVerifier) DescribeIdentity() IdentityPolicyRecord { return IdentityPolicyRecord{} }
func (describedVerifier) PolicyDigest() string                   { return "digest-of-the-policy" }
func (describedVerifier) RosterDigest() string                   { return "roster" }
func (describedVerifier) VerifierInfo() (string, string)         { return "", "" }

// The hold names the failure that cannot mend (a revocation first), carries its evidence, and the policy
// digest; without a revocation the reason code is -1.
func TestWriteExecHold_Content(t *testing.T) {
	ctx := context.Background()
	g := newBoundGate(t)
	_, _ = g.run()
	s := subjectOf(t, g.store, "r1", "c1")
	vf := func(id string) (ApproverVerifier, bool) {
		switch id {
		case "alice":
			return describedVerifier{g.alice}, true
		case "bob":
			return g.bob, true
		}
		return nil, false
	}
	read := func(gen int) ExecHold {
		r, ok := hasStep(t, g.store, "r1", ApprovalExecHoldStep("c1", gen))
		if !ok {
			t.Fatal("no hold")
		}
		var h ExecHold
		if err := json.Unmarshal(r.Result, &h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	rev := &CertRevocation{Serial: "9", E: "2026-10-13T12:00:00Z", ReasonCode: 5, CRL: "cd"}
	// A wait first, then a bad signature, then a revocation: the revocation names the hold.
	v := ExecVerdict{Outcome: ExecReask, Checked: 3, Failed: []ExecFailure{
		{Approver: "a", Reason: ReasonStatusStale}, {Approver: "b", Reason: ReasonBadSig}, {Approver: "c", Reason: ReasonRevoked, Revocation: rev, Anchored: "2026-10-13T12:14:13Z", StatusTime: "s"}}}
	if err := writeExecHold(ctx, g.store, "r1", s, g.pol, vf, 0, v); err != nil {
		t.Fatal(err)
	}
	h := read(0)
	if h.Reason != ReasonRevoked || h.Serial != "9" || h.E != "2026-10-13T12:00:00Z" || h.ReasonCode != 5 || h.CRLHash != "cd" || h.AnchoredAt != "2026-10-13T12:14:13Z" ||
		h.StatusTime != "s" || !slices.Equal(h.Approvers, []string{"a", "b", "c"}) || h.PolicyDigest != "digest-of-the-policy" || h.Generation != 0 {
		t.Fatalf("%+v", h)
	}
	// A wait is never the reason, and without a revocation the reason code is -1.
	v2 := ExecVerdict{Outcome: ExecReask, Checked: 2, Failed: []ExecFailure{{Approver: "a", Reason: ReasonStatusStale}, {Approver: "b", Reason: ReasonBadSig}}}
	if err := writeExecHold(ctx, g.store, "r1", s, g.pol, vf, 1, v2); err != nil {
		t.Fatal(err)
	}
	if h := read(1); h.Reason != ReasonBadSig || h.ReasonCode != -1 || h.Serial != "" || h.Generation != 1 {
		t.Fatalf("%+v", h)
	}
}

// A recorded denial that has an identity record and no decisions to re-check still does not approve the
// call: the check refuses it, whatever the records around it say.
func TestPreExecute_ADeniedTallyWithItsIdentityRecordDoesNotRun(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	name := ApprovalIdentTallyStep("c1", 0)
	writeRaw(t, g.store, "r1", name, valueRec(t, name, deniedTally()))
	tr, _ := hasStep(t, g.store, "r1", name)
	sum := sha256.Sum256(tr.Result)
	ir := ApprovalIdentity{Format: ApprovalIdentityFormat, ToolUseID: "c1", Generation: 0, Tally: name, TallyDigest: hex.EncodeToString(sum[:])}
	writeRaw(t, g.store, "r1", ApprovalIdentityStep("c1", 0), valueRec(t, ApprovalIdentityStep("c1", 0), ir))
	if err := preExecute(context.Background(), g.store, "r1", "r1", "c1", g.pol, vf, ""); !errors.Is(err, ErrConfig) {
		t.Fatalf("%v", err)
	}
}
