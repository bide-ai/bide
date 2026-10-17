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
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bide-ai/bide/internal/identitygate"
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
	if err := preExecute(context.Background(), g2.store, "r1", "r1", "c1", g2.pol, vf2, ""); !errors.Is(err, errExecNotApproved) {
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
func (describedVerifier) VerifierInfo() (string, string, string) { return "", "", "" }

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
	if err := preExecute(context.Background(), g.store, "r1", "r1", "c1", g.pol, vf, ""); !errors.Is(err, errExecNotApproved) {
		t.Fatalf("%v", err)
	}
}

// ---- review of E2b (pki-auditor-2) ----

// A journal that says the gate is bound, reaching preExecute under a policy that lost its certificate-bound
// approvers, is refused there too (the gate refuses it before the handler today; this keeps a future path
// from running the call unchecked). A bare gate's journal is not.
func TestPreExecute_BoundJournalUnderABarePolicyIsRefused(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	g.decide("alice")
	g.countOnly()
	bare := fakeVerifiers("alice", "bob")
	err := preExecute(context.Background(), g.store, "r1", "r1", "c1", g.pol, bare, "")
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("%v", err)
	}
	// Through the handler, too: the run fails and the tool does not run.
	var charged int
	if _, err := mofnRun(g.store, "r1", false, g.pol, bare, &charged); !errors.Is(err, ErrConfig) || charged != 0 {
		t.Fatalf("%v charged %d", err, charged)
	}
	// A bare gate's journal: nothing to refuse.
	store := memJournal()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"alice"}}
	_, _ = mofnRun(store, "r1", true, pol, fakeVerifiers("alice"), &charged)
	if err := preExecute(context.Background(), store, "r1", "r1", "c1", pol, fakeVerifiers("alice"), ""); err != nil {
		t.Fatalf("a bare gate: %v", err)
	}
}

// A hold after which no seat can pass (both counted decisions fail the recount): the call does not run and the drive records no result; the denial itself is covered by TestPreExecute_DenialIsNotAConfigError (not
// ErrConfig), and the next drive records the denial as the call's result.
func TestPreExecute_ARecountWithNoSeatsLeftRunsNothingAndRecordsNothing(t *testing.T) {
	g := newBoundGate(t)
	g.pol.Need = 2
	_, _ = g.run()
	g.decide("alice")
	g.decide("bob")
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	if _, final, err := tallyGate(context.Background(), g.store, "r1", subjectOf(t, g.store, "r1", "c1"), g.pol, vf, ""); err != nil || !final {
		t.Fatalf("%v %v", final, err)
	}
	// alice is revoked before the call; bob then withdraws by denying (a later decision of his own does not
	// count, so use a denial recorded as his first: simulate with his certificate failing too).
	g.alice.execCheck = func(cd CertDecision) CertCheck { revokeAt(g.alice); return revokedCheck(cd) }
	g.bob.check = func(CertDecision) CertCheck { return CertCheck{Reason: ReasonRevoked} } // the recount drops bob as well
	_, err := g.run()
	if g.charged != 0 {
		t.Fatalf("the call ran (%v)", err)
	}
	// Both seats are gone: the recount cannot pass, and nobody is left to decide it (a pending gate), or it
	// is a denial; either way the call did not run and nothing was recorded as its result.
	if _, ok := hasStep(t, g.store, "r1", ToolResultStep("c1")); ok {
		t.Fatalf("a result was recorded by the drive that held the call (%v)", err)
	}
}

// preExecute on a gate whose outcome is a recorded denial returns errExecNotApproved, not ErrConfig, and
// the loop records the denial as the call's result on the next drive.
func TestPreExecute_DenialIsNotAConfigError(t *testing.T) {
	g := newBoundGate(t)
	g.pol.Need = 2
	_, _ = g.run()
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	name := ApprovalIdentTallyStep("c1", 0)
	writeRaw(t, g.store, "r1", name, valueRec(t, name, deniedTally()))
	err := preExecute(context.Background(), g.store, "r1", "r1", "c1", g.pol, vf, "")
	if !errors.Is(err, errExecNotApproved) || errors.Is(err, ErrConfig) {
		t.Fatalf("%v", err)
	}
	// The pre-pass reads the recorded denial: the call is denied, the tool never runs, the run goes on.
	if _, err := g.run(); err != nil || g.charged != 0 {
		t.Fatalf("the next drive: %v charged %d", err, g.charged)
	}
	if r, ok := hasStep(t, g.store, "r1", ToolResultStep("c1")); !ok || !r.IsError {
		t.Fatalf("the denial was not recorded: %+v %v", r, ok)
	}
}

// RecoverLoop re-drives a run paused for stale status: on the first pass after status is current the call
// runs, with the same approvals.
func TestPreExecute_RecoverLoopRunsAfterAStaleWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newBoundGate(t)
		tool := &countingTool{name: "charge", approval: g.pol, calls: &g.charged, safety: Safety{Idempotent: true}}
		a := g.agentFor(tool)
		_, _ = a.Run(context.Background(), "r1", UserText("pay"))
		g.decide("alice")
		var stale atomic.Bool
		stale.Store(true)
		g.alice.execCheck = func(cd CertDecision) CertCheck {
			if stale.Load() {
				return CertCheck{Reason: ReasonStatusStale}
			}
			return okCheck(cd)
		}
		stop := runLoop(t, g.store, ResumeAgent(a), WithRecoverInterval(20*time.Millisecond))
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		if g.charged != 0 {
			t.Fatalf("the call ran on stale status (%d)", g.charged)
		}
		stale.Store(false)
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		_ = stop()
		if g.charged != 1 {
			t.Fatalf("the call ran %d times after status became current", g.charged)
		}
		if _, held := g.holdRecord(0); held {
			t.Fatal("a stale wait wrote a hold")
		}
	})
}

// fakeCertApprover is a certificate-bound verifier that describes its policy, for the gate's record
// writer.
type fakeCertApprover struct{ id string }

func (fakeCertApprover) Alg() Alg                                  { return AlgCertificate }
func (fakeCertApprover) Verify(_, _ []byte) bool                   { return false }
func (f fakeCertApprover) KeyIDs() []string                        { return []string{"ident:" + f.id} }
func (fakeCertApprover) VerifyCertDecision(CertDecision) CertCheck { return CertCheck{} }
func (fakeCertApprover) DescribeIdentity() IdentityPolicyRecord    { return IdentityPolicyRecord{} }
func (fakeCertApprover) PolicyDigest() string                      { return "pd" }
func (fakeCertApprover) RosterDigest() string                      { return "rd" }
func (fakeCertApprover) VerifierInfo() (string, string, string) {
	return "rev", "x509sha1=1", "x509usefallbackroots=0"
}

// The identity record's tally digest is the SHA-256 of the bytes the journal stores for the tally, which
// is not json.Marshal's encoding of it: an approver id may contain '&', '<' and '>' (printable ASCII),
// which the journal does not escape.
func TestRecordIdentity_TallyDigestIsOfTheStoredBytes(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(NewMemStore())
	id := "a&b<c>"
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{id}}
	vf := func(x string) (ApproverVerifier, bool) { return fakeCertApprover{id: x}, x == id }
	tally := ApprovalTally{Need: 1, Approvers: []string{id}, Approved: 1, ApprovedBy: []string{id}, DecisionFormat: DecisionFormat}
	checks := []DecisionCheck{{Step: "approval:c1:x", Approver: id, Approved: true, Counted: true,
		Cert: &CertCheck{Identity: "ident", Chain: []string{"l"}, Tier: "final", Time: "log signer clock: head of size 1", BasisKind: BasisAtOrAfterDecision}}}
	s := ApprovalSubject{RunID: "r", ToolUseID: "c1"}
	if err := recordIdentity(ctx, j, "r", s, pol, vf, 0, ApprovalTallyStep("c1"), tally, checks); err != nil {
		t.Fatal(err)
	}
	stored, err := step(ctx, j, "r", ApprovalTallyStep("c1"), func(context.Context) (ApprovalTally, error) { return tally, nil }, WithSafety(Safety{ReadOnly: true}))
	if err != nil {
		t.Fatal(err)
	}
	_ = stored
	tr, ok, err := j.Get(ctx, "r", ApprovalTallyStep("c1"))
	if err != nil || !ok {
		t.Fatalf("%v %v", err, ok)
	}
	ir, ok, err := j.Get(ctx, "r", ApprovalIdentityStep("c1", 0))
	if err != nil || !ok {
		t.Fatalf("%v %v", err, ok)
	}
	var rec ApprovalIdentity
	if err := json.Unmarshal(ir.Result, &rec); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(tr.Result)
	if rec.TallyDigest != hex.EncodeToString(sum[:]) {
		t.Fatalf("tally_digest %s is not the digest of the stored tally %s", rec.TallyDigest, tr.Result)
	}
	if rec.GoDebug != "x509sha1=1" || rec.BuildGoDebug != "x509usefallbackroots=0" || rec.VerifierRev != "rev" {
		t.Fatalf("verifier info not in the record: %+v", rec)
	}
	if plain, _ := json.Marshal(tally); string(plain) == string(tr.Result) {
		t.Fatal("the test's tally has nothing the two encoders disagree on")
	}
}

// The identity record of a tally is the one whose digest matches the stored tally bytes, under the plain
// name first and then the digest-named one.
func TestIdentityRecordFor(t *testing.T) {
	tally := Record{Kind: StepValue, Name: ApprovalTallyStep("c1"), Result: []byte(`{"need":1}`)}
	sum := sha256.Sum256(tally.Result)
	digest := hex.EncodeToString(sum[:])
	rec := func(name, td string, gen int) Record {
		b, _ := json.Marshal(ApprovalIdentity{ToolUseID: "c1", Generation: gen, TallyDigest: td, Format: ApprovalIdentityFormat})
		return Record{Kind: StepValue, Name: name, Result: b}
	}
	stale := rec(ApprovalIdentityStep("c1", 0), "00", 0)
	right := rec(ApprovalIdentityDigestStep("c1", 0, digest), digest, 0)
	plain := rec(ApprovalIdentityStep("c1", 0), digest, 0)
	for name, tc := range map[string]struct {
		recs []Record
		gen  int
		ok   bool
	}{
		"matching plain name":               {[]Record{plain}, 0, true},
		"stale plain, matching digest name": {[]Record{stale, right}, 0, true},
		"digest name first in the journal":  {[]Record{right, stale}, 0, true},
		"only a stale plain":                {[]Record{stale}, 0, false},
		"none":                              {nil, 0, false},
		"another generation":                {[]Record{plain}, 1, false},
		"a digest name with another digest": {[]Record{rec(ApprovalIdentityDigestStep("c1", 0, digest), "11", 0)}, 0, false},
	} {
		got, ok := IdentityRecordFor(tc.recs, "c1", tc.gen, tally)
		if ok != tc.ok || ok && got.TallyDigest != digest {
			t.Fatalf("%s: ok=%v %+v", name, ok, got)
		}
	}
}

// A verifier that has no status archive cannot take a CRL above the inline limit: the gate refuses to
// count (ErrConfig naming the fix), and writes no tally.
func TestRecordIdentity_OversizeStatusWithoutAnArchiverIsRefused(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(NewMemStore())
	id := "a"
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{id}}
	vf := func(x string) (ApproverVerifier, bool) { return fakeCertApprover{id: x}, x == id }
	tally := ApprovalTally{Need: 1, Approvers: []string{id}, Approved: 1, ApprovedBy: []string{id}}
	checks := []DecisionCheck{{Step: "s", Approver: id, Approved: true, Counted: true,
		Cert: &CertCheck{Identity: "i", Chain: []string{"l"}, Tier: "final", Evidence: []ApprovalStatusBytes{{Issuer: "ab", DER: make([]byte, MaxStatusInline+1)}}}}}
	err := recordIdentity(ctx, j, "r", ApprovalSubject{RunID: "r", ToolUseID: "c1"}, pol, vf, 0, ApprovalTallyStep("c1"), tally, checks)
	if err == nil || !errors.Is(err, ErrConfig) || errors.Is(err, ErrStorage) || !strings.Contains(err.Error(), "StatusArchive") {
		t.Fatalf("%v", err)
	}
	if _, ok, _ := j.Get(ctx, "r", ApprovalIdentityStep("c1", 0)); ok {
		t.Fatal("an identity record was written")
	}
}

// EvaluateGate reads a recorded denial as final, whatever the policy is now, and a recorded result as
// the call having run (or been denied): the cases the loop decides before it looks at the policy.
func TestEvaluateGate_RecordedOutcomesComeBeforeThePolicy(t *testing.T) {
	s := ApprovalSubject{RunID: "r", ToolUseID: "c1", ToolName: "charge"}
	denial, _ := json.Marshal(ApprovalTally{Need: 2, Approvers: []string{"a", "b", "c"}, Denied: 2, DeniedBy: []string{"a", "b"}, DecisionFormat: DecisionFormat})
	pass, _ := json.Marshal(ApprovalTally{Need: 1, Approvers: []string{"a"}, Approved: 1, ApprovedBy: []string{"a"}, DecisionFormat: DecisionFormat})
	tally := func(b []byte) Record { return Record{Name: ApprovalTallyStep("c1"), Kind: StepValue, Result: b} }
	human := func(ok bool) Record {
		return Record{Name: "approval:c1", Kind: StepApproval, ToolUseID: "c1", Approved: ok}
	}
	ran := Record{Name: "tool:c1", Kind: StepToolResult, ToolUseID: "c1", Result: []byte(`"ok"`)}
	denied := Record{Name: "tool:c1", Kind: StepToolResult, ToolUseID: "c1", IsError: true, Result: []byte(`"tool call denied by human"`)}
	mofn := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	for name, tc := range map[string]struct {
		recs   []Record
		pol    *ApprovalPolicy
		status GateStatus
		denied bool
		ran    bool
	}{
		"a recorded denial tally, no policy now":       {[]Record{tally(denial)}, nil, GateDecided, true, false},
		"a recorded denial tally, a one-decision gate": {[]Record{tally(denial)}, SingleApproval(), GateDecided, true, false},
		"a human denial, no policy now":                {[]Record{human(false)}, nil, GateDecided, true, false},
		"a human denial, an m-of-n policy":             {[]Record{human(false)}, mofn, GateDecided, true, false},
		"a human approval, a one-decision gate":        {[]Record{human(true)}, SingleApproval(), GateSingle, false, false},
		"a recorded pass, no policy now":               {[]Record{tally(pass)}, nil, GateUngated, false, false},
		"a result":                                     {[]Record{ran}, mofn, GateDecided, false, true},
		"a denial's result":                            {[]Record{denied}, mofn, GateDecided, true, true},
	} {
		t.Run(name, func(t *testing.T) {
			got := EvaluateGate(tc.recs, s, tc.pol, fakeVerifiers("a", "b", "c"))
			if got.Status != tc.status || got.Denied != tc.denied || got.Ran != tc.ran {
				t.Fatalf("%+v, want status %s denied %v ran %v", got, tc.status, tc.denied, tc.ran)
			}
		})
	}
}

// GateReask is the rule the gate applies and the viewers read.
func TestGateReask(t *testing.T) {
	passed := ApprovalTally{Need: 1, Approved: 1, ApprovedBy: []string{"a"}, DecisionFormat: DecisionFormat}
	for name, tc := range map[string]struct {
		t    ApprovalTally
		sod  bool
		want string
	}{
		"a current pass":                         {passed, false, ""},
		"a pass before the signed format":        {ApprovalTally{Need: 1, Approved: 1, ApprovedBy: []string{"a"}}, false, PendingReapproval},
		"a pass without excluding the requester": {passed, true, PendingSoDReapproval},
		"a pass that excluded the requester":     {ApprovalTally{Need: 1, Approved: 1, ApprovedBy: []string{"a"}, ExcludePrincipal: true, DecisionFormat: DecisionFormat}, true, ""},
		"a denial":                               {ApprovalTally{Need: 1, Denied: 1, DeniedBy: []string{"a"}, DecisionFormat: DecisionFormat}, true, ""},
	} {
		if got := GateReask(tc.t, tc.sod); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// A plan flow node's gate is read as the flow's own gate reads it (flowGate): only the decisions after
// its request count, and the node's result is the key's own record. EvaluateGateAfter, given the
// request's step, agrees with flowGate on every one of these, and EvaluateGate, which counts the whole
// journal, would not (the decision recorded before the request).
func TestEvaluateGateAfter_AgreesWithFlowGate(t *testing.T) {
	ctx := context.Background()
	vf := fakeVerifiers("a", "b")
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}
	args := json.RawMessage(`{"amount":7}`)
	subj := ApprovalSubject{RunID: "r1", ToolUseID: "node:pay", ToolName: "pay", Args: args}
	decide := func(t *testing.T, j *Journal, id string, v bool) {
		t.Helper()
		if err := decideAs(ctx, j, "r1", "node:pay", id, v, fakeSign(id, ApprovalDecisionBytes(subj, id, v, testDecidedAt))); err != nil {
			t.Fatal(err)
		}
	}
	type result int
	const (
		pauses result = iota
		passes
		denies
	)
	for name, tc := range map[string]struct {
		setup func(t *testing.T, j *Journal)
		want  result
	}{
		"just paused":   {func(t *testing.T, j *Journal) {}, pauses},
		"one decision":  {func(t *testing.T, j *Journal) { decide(t, j, "a", true) }, pauses},
		"quorum":        {func(t *testing.T, j *Journal) { decide(t, j, "a", true); decide(t, j, "b", true) }, passes},
		"quorum denied": {func(t *testing.T, j *Journal) { decide(t, j, "a", false); decide(t, j, "b", true) }, denies},
		"both decisions are signed before the request": {func(t *testing.T, j *Journal) {}, pauses},
	} {
		t.Run(name, func(t *testing.T) {
			j := memJournal()
			if name == "both decisions are signed before the request" {
				// Decisions recorded under their names before the gate's request: they cannot count.
				for _, id := range []string{"a", "b"} {
					sig := fakeSign(id, ApprovalDecisionBytes(subj, id, true, testDecidedAt))
					writeRaw(t, j, "r1", approvalDecisionStep("node:pay", id, true, fakeAlg, FormatTimestamp(testDecidedAt), sig),
						Record{Kind: StepApproval, ToolUseID: "node:pay", Approved: true, ApproverSignature: &ApproverSignature{Approver: id, ApproverAlg: fakeAlg, Signature: sig, DecidedAt: FormatTimestamp(testDecidedAt)}})
				}
			}
			var pend *ApprovalPending
			if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, pol, vf); !errors.As(err, &pend) {
				t.Fatalf("first evaluation = %v; want the pause", err)
			}
			tc.setup(t, j)
			recs, _ := j.History(ctx, "r1")
			st := EvaluateGateAfter(recs, subj, pol, vf, ApprovalRequestStep("node:pay"))
			whole := EvaluateGate(recs, subj, pol, vf)
			approved, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, pol, vf)
			got := passes
			switch {
			case errors.As(err, &pend):
				got = pauses
			case err != nil:
				t.Fatalf("flowGate: %v", err)
			case !approved:
				got = denies
			}
			if got != tc.want {
				t.Fatalf("flowGate did %d, want %d", got, tc.want)
			}
			switch got {
			case pauses:
				if st.Status != GateWaiting || st.Tally.Passed() || st.Tally.Unreachable() {
					t.Fatalf("flowGate pauses, EvaluateGateAfter says %+v", st)
				}
			case passes:
				if st.Status != GateWaiting || !st.Tally.Passed() {
					t.Fatalf("flowGate passes, EvaluateGateAfter says %+v", st)
				}
			case denies:
				if st.Status != GateWaiting || !st.Tally.Unreachable() {
					t.Fatalf("flowGate denies, EvaluateGateAfter says %+v", st)
				}
			}
			// After the gate acts, its recorded outcome is read the same way, and the node's result is the
			// key's own record.
			recs, _ = j.History(ctx, "r1")
			if got != pauses {
				if st := EvaluateGateAfter(recs, subj, pol, vf, ApprovalRequestStep("node:pay")); st.Status != GateDecided || !st.Recorded {
					t.Fatalf("after the gate recorded its outcome: %+v", st)
				}
			}
			if name == "both decisions are signed before the request" && whole.Tally.Approved != 2 {
				t.Fatalf("setup: the whole-journal count should see both decisions, got %+v", whole.Tally)
			}
			writeRaw(t, j, "r1", "node:pay", Record{Kind: StepValue, Result: []byte(`"done"`)})
			recs, _ = j.History(ctx, "r1")
			if st := EvaluateGateAfter(recs, subj, pol, vf, ApprovalRequestStep("node:pay")); st.Status != GateDecided || !st.Ran {
				t.Fatalf("a node whose result is recorded: %+v", st)
			}
		})
	}
}

// A single-approval plan flow gate honours an unsigned decision only if it follows the gate's request
// in the journal (flowGate); EvaluateGateAfter must agree with it, before or after the request.
func TestEvaluateGateAfter_AgreesWithFlowGateOnASingleDecision(t *testing.T) {
	ctx := context.Background()
	args := json.RawMessage(`{"amount":7}`)
	subj := ApprovalSubject{RunID: "r1", ToolUseID: "node:pay", ToolName: "pay", Args: args}
	for name, tc := range map[string]struct {
		before, after bool // a decision is recorded before / after the request
		approved      bool
		want          string // pauses, passes or denies
	}{
		"denial before the request":   {before: true, approved: false, want: "pauses"},
		"approval before the request": {before: true, approved: true, want: "pauses"},
		"denial after the request":    {after: true, approved: false, want: "denies"},
		"approval after the request":  {after: true, approved: true, want: "passes"},
		"no decision":                 {want: "pauses"},
	} {
		t.Run(name, func(t *testing.T) {
			j := memJournal()
			decide := func() {
				writeRaw(t, j, "r1", approvalStep("node:pay"), Record{Kind: StepApproval, ToolUseID: "node:pay", Approved: tc.approved})
			}
			if tc.before {
				decide()
			}
			var pend *ApprovalPending
			approved, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, SingleApproval(), nil)
			if tc.before && !errors.As(err, &pend) {
				t.Fatalf("setup: flowGate with a decision before its request = %v, %v; want the pause", approved, err)
			}
			if tc.after {
				decide()
				approved, err = flowGate(ctx, j, "r1", "node:pay", "pay", args, SingleApproval(), nil)
			}
			got := "passes"
			switch {
			case errors.As(err, &pend):
				got = "pauses"
			case err != nil:
				t.Fatalf("flowGate: %v", err)
			case !approved:
				got = "denies"
			}
			if got != tc.want {
				t.Fatalf("flowGate %s, want %s", got, tc.want)
			}
			recs, _ := j.History(ctx, "r1")
			st := EvaluateGateAfter(recs, subj, SingleApproval(), nil, ApprovalRequestStep("node:pay"))
			switch got {
			case "pauses":
				if st.Status != GateSingle || st.SingleDecided {
					t.Fatalf("flowGate pauses, EvaluateGateAfter says %+v", st)
				}
			case "denies":
				if st.Status != GateDecided || !st.Denied {
					t.Fatalf("flowGate denies, EvaluateGateAfter says %+v", st)
				}
			case "passes":
				if st.Status != GateSingle || !st.SingleDecided || !st.SingleApproved {
					t.Fatalf("flowGate passes, EvaluateGateAfter says %+v", st)
				}
			}
		})
	}
}

// The tests of certificate-bound approvers run with the gate switch on; until the last change of the
// feature turns it on by default, a gate refuses such an approver (TestGateRefusesCertificateBoundApprovers).
func TestMain(m *testing.M) {
	identitygate.Enabled.Store(true)
	os.Exit(m.Run())
}

// Until the last change of the feature turns the switch on, a gate refuses a certificate-bound approver
// at configuration (build and every evaluation): no deployment reaches a half-built gate. The counting
// rule itself (TallyApprovals) is not a gate and is unaffected.
func TestGateRefusesCertificateBoundApprovers(t *testing.T) {
	was := identitygate.Enabled.Load()
	defer identitygate.Enabled.Store(was)
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	v := &fakeCertVerifier{id: "a", check: func(CertDecision) CertCheck { return CertCheck{Identity: "a", KeyHash: [32]byte{1}} }}
	vf := certResolver(map[string]ApproverVerifier{"a": v})
	identitygate.Enabled.Store(false)
	err := pol.ValidateKeys(vf)
	if err == nil || !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "certificate-bound") {
		t.Fatalf("switch off: %v", err)
	}
	if err := gatePolicyError(&pol, vf); !errors.Is(err, ErrConfig) {
		t.Fatalf("gatePolicyError, switch off: %v", err)
	}
	if got, _ := TallyApprovals([]Record{certRec("approval:c1:a:00", "a")}, timesSubject, pol, vf); got.Approved != 0 {
		t.Fatalf("a refused approver counted: %+v", got)
	}
	identitygate.Enabled.Store(true)
	if err := pol.ValidateKeys(vf); err != nil {
		t.Fatalf("switch on: %v", err)
	}
	if got, _ := TallyApprovals([]Record{certRec("approval:c1:a:00", "a")}, timesSubject, pol, vf); got.Approved != 1 {
		t.Fatalf("switch on: %+v", got)
	}
}

// A flow gate's request is recorded once. A later evaluation for another node name or another
// input (a resume under another flow or input, which plan refuses earlier) is ErrConfig, so no
// decision is ever counted against an input other than the one it was signed for.
func TestFlowGate_RequestIsFixed(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	pol := SingleApproval()
	_, err := flowGate(ctx, j, "r1", "node:pay", "pay", json.RawMessage(`{"amount":1}`), pol, nil)
	var pend *ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("first evaluation = %v; want a pause", err)
	}
	for name, args := range map[string]string{"pay": `{"amount":2}`, "refund": `{"amount":1}`} {
		if _, err := flowGate(ctx, j, "r1", "node:pay", name, json.RawMessage(args), pol, nil); !errors.Is(err, ErrConfig) {
			t.Errorf("evaluation as %s(%s) = %v; want ErrConfig", name, args, err)
		}
	}
	// The same input in another spelling is the same request.
	if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", json.RawMessage(`{ "amount" : 1.0e0 }`), pol, nil); !errors.As(err, &pend) {
		t.Errorf("evaluation with the input respelled = %v; want the pause", err)
	}
}

// The gate runs only on a plan node key, with a policy, and an m-of-n gate only with verifiers.
func TestFlowGate_Refusals(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	mofn := &ApprovalPolicy{Need: 1, Approvers: []string{"ops"}}
	for name, call := range map[string]func() error{
		"not a node key": func() error { _, err := flowGate(ctx, j, "r1", "tool:x", "x", nil, SingleApproval(), nil); return err },
		"no policy":      func() error { _, err := flowGate(ctx, j, "r1", "node:x", "x", nil, nil, nil); return err },
		"no verifiers":   func() error { _, err := flowGate(ctx, j, "r1", "node:x", "x", nil, mofn, nil); return err },
	} {
		if err := call(); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: flowGate = %v; want ErrConfig", name, err)
		}
	}
}

// FindToolCall finds a flow gate's request only on a plan node key, so a request record cannot
// stand in for an agent's tool call.
func TestFindToolCall_FlowRequestOnlyOnNodeKeys(t *testing.T) {
	req := func(id string) Record {
		b, _ := json.Marshal(ApprovalRequest{ToolUseID: id, ToolName: "pay", Args: json.RawMessage(`1`)})
		return Record{Name: ApprovalRequestStep(id), Kind: StepValue, Result: b}
	}
	if _, call, ok := FindToolCall([]Record{req("node:pay")}, "node:pay"); !ok || call.Name != "pay" || string(call.Args) != "1" {
		t.Fatalf("FindToolCall(node:pay) = %+v, %v; want the request", call, ok)
	}
	if _, _, ok := FindToolCall([]Record{req("c1")}, "c1"); ok {
		t.Fatal("FindToolCall found a request record for a tool-use id")
	}
	other := req("node:pay")
	other.Name = ApprovalRequestStep("node:other")
	if _, _, ok := FindToolCall([]Record{other}, "node:pay"); ok {
		t.Fatal("FindToolCall found a request recorded under another node's name")
	}
}

// A flow node's single-approval gate passes only on a decision recorded after its request: Approve
// refuses one before the gate paused, then a decision after it passes the gate (and a denial
// denies it), and the recorded outcome is final on every later evaluation.
func TestFlowGate_SingleDecisionAfterRequest(t *testing.T) {
	ctx := context.Background()
	for _, approved := range []bool{true, false} {
		j := memJournal()
		args := json.RawMessage(`{"amount":1}`)
		if err := Approve(ctx, j, "r1", "node:pay", approved); !errors.Is(err, ErrConfig) {
			t.Fatalf("Approve before the request = %v; want ErrConfig", err)
		}
		if r, found, err := j.Get(ctx, "r1", approvalStep("node:pay")); err != nil || found {
			t.Fatalf("a refused Approve recorded a decision: %+v, %v", r, err)
		}
		var pend *ApprovalPending
		if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, SingleApproval(), nil); !errors.As(err, &pend) {
			t.Fatalf("first evaluation = %v; want a pause", err)
		}
		if _, ok, _ := j.Get(ctx, "r1", ApprovalPendingStep("node:pay")); !ok {
			t.Error("the pause recorded no ApprovalWait note")
		}
		if err := Approve(ctx, j, "r1", "node:pay", approved); err != nil {
			t.Fatalf("Approve(%v) after the request = %v", approved, err)
		}
		for i := 0; i < 2; i++ {
			got, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, SingleApproval(), nil)
			if err != nil || got != approved {
				t.Fatalf("evaluation %d after Approve(%v) = %v, %v", i, approved, got, err)
			}
		}
	}
}

// A single decision recorded under the gate's name before its request never counts: the gate stays
// paused. Approve then records the real decision under the gate's second name, which the gate
// honours, so the stale record does not block the gate.
func TestFlowGate_DecisionBeforeRequestCannotCount(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	writeRaw(t, j, "r1", approvalStep("node:pay"), Record{Kind: StepApproval, ToolUseID: "node:pay", Approved: true})
	var pend *ApprovalPending
	if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", json.RawMessage(`1`), SingleApproval(), nil); !errors.As(err, &pend) {
		t.Fatalf("gate with a pre-request decision = %v; want the pause", err)
	}
	if err := Approve(ctx, j, "r1", "node:pay", false); err != nil {
		t.Fatalf("Approve over a pre-request decision = %v; want it recorded under the second name", err)
	}
	if _, ok, _ := j.Get(ctx, "r1", approvalAfterRequestStep("node:pay")); !ok {
		t.Fatal("no decision under the second name")
	}
	if got, err := flowGate(ctx, j, "r1", "node:pay", "pay", json.RawMessage(`1`), SingleApproval(), nil); err != nil || got {
		t.Fatalf("gate after Approve(false) = %v, %v; want the denial, not the stale approval", got, err)
	}
}

// An m-of-n flow gate counts signed decisions after its request, records a terminal tally, and the
// tally is final: the node passes on quorum, and a denial that makes quorum unreachable denies it.
func TestFlowGate_MofnQuorumAndDenial(t *testing.T) {
	ctx := context.Background()
	vf := fakeVerifiers("a", "b")
	pol := &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}
	args := json.RawMessage(`{"amount":7}`)
	subj := ApprovalSubject{RunID: "r1", ToolUseID: "node:pay", ToolName: "pay", Args: args}
	run := func(t *testing.T, votes map[string]bool) (*Journal, bool, error) {
		j := memJournal()
		var pend *ApprovalPending
		if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, pol, vf); !errors.As(err, &pend) || pend.Quorum == nil {
			t.Fatalf("first evaluation = %v; want a quorum pause", err)
		}
		for _, id := range []string{"a", "b"} {
			v, ok := votes[id]
			if !ok {
				continue
			}
			if err := decideAs(ctx, j, "r1", "node:pay", id, v, fakeSign(id, ApprovalDecisionBytes(subj, id, v, testDecidedAt))); err != nil {
				t.Fatalf("decision by %s: %v", id, err)
			}
		}
		ok, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, pol, vf)
		return j, ok, err
	}

	t.Run("one of two is still pending", func(t *testing.T) {
		_, _, err := run(t, map[string]bool{"a": true})
		var pend *ApprovalPending
		if !errors.As(err, &pend) || pend.Quorum.Approved != 1 {
			t.Fatalf("err = %v; want a pause with one approval counted", err)
		}
	})
	t.Run("quorum passes and the tally is final", func(t *testing.T) {
		j, ok, err := run(t, map[string]bool{"a": true, "b": true})
		if err != nil || !ok {
			t.Fatalf("gate = %v, %v; want passed", ok, err)
		}
		if r, found, err := j.Get(ctx, "r1", ApprovalTallyStep("node:pay")); err != nil || !found || r.Kind != StepValue {
			t.Fatalf("no terminal tally recorded: %+v, %v, %v", r, found, err)
		}
		if ok, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, pol, vf); err != nil || !ok {
			t.Fatalf("replay = %v, %v; want the recorded pass", ok, err)
		}
	})
	t.Run("a denial that makes quorum unreachable denies", func(t *testing.T) {
		j, ok, err := run(t, map[string]bool{"a": false})
		if err != nil || ok {
			t.Fatalf("gate = %v, %v; want denied", ok, err)
		}
		if ok, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, pol, vf); err != nil || ok {
			t.Fatalf("replay = %v, %v; want the recorded denial", ok, err)
		}
	})
}

// A store that cannot be read or written while a gate evaluates is a storage error (ErrStorage on
// a read), never a pass, a denial or a silent skip.
func TestFlowGate_StoreFaults(t *testing.T) {
	ctx := context.Background()
	args := json.RawMessage(`1`)
	vf := fakeVerifiers("a")
	mofn := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	cases := []struct {
		name    string
		sub     string
		pol     *ApprovalPolicy
		wantSto bool
	}{
		{"single outcome read", "approval:", SingleApproval(), true},
		{"m-of-n tally read", "approval-tally:", mofn, true},
		{"request write", "approval-request:", SingleApproval(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var st Store = NewMemStore()
			if c.name == "request write" {
				st = covInsertFail{st, c.sub}
			} else {
				st = covGetFail{st, c.sub}
			}
			j := mustJournal(st)
			_, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, c.pol, vf)
			if err == nil || errors.Is(err, ErrConfig) {
				t.Fatalf("flowGate = %v; want a store error", err)
			}
			var pend *ApprovalPending
			if errors.As(err, &pend) {
				t.Fatalf("flowGate paused on a store fault: %v", err)
			}
			if c.wantSto && !errors.Is(err, ErrStorage) {
				t.Fatalf("flowGate = %v; want ErrStorage", err)
			}
		})
	}
	t.Run("request read in Approve", func(t *testing.T) {
		j := mustJournal(covGetFail{NewMemStore(), "approval-request:"})
		if err := Approve(ctx, j, "r1", "node:pay", true); !errors.Is(err, ErrStorage) {
			t.Fatalf("Approve = %v; want ErrStorage", err)
		}
	})
	t.Run("start read in Approve", func(t *testing.T) {
		j := mustJournal(covGetFail{NewMemStore(), runStartStep})
		if err := Approve(ctx, j, "r1", "node:pay", true); err == nil {
			t.Fatal("Approve with an unreadable run:start succeeded")
		}
	})
}

// A request record that is not the gate's request (not strict JSON of the right shape, or naming
// another node) is ErrConfig: the gate does not evaluate decisions against it.
func TestFlowGate_RequestRecordMustBeTheGates(t *testing.T) {
	ctx := context.Background()
	for name, body := range map[string]string{
		"another node":    `{"tool_use_id":"node:other","tool_name":"pay","args":1}`,
		"unknown field":   `{"tool_use_id":"node:pay","tool_name":"pay","args":1,"extra":true}`,
		"empty object":    `{}`,
		"scalar not json": `7`,
	} {
		t.Run(name, func(t *testing.T) {
			j := memJournal()
			writeRaw(t, j, "r1", ApprovalRequestStep("node:pay"), Record{Kind: StepValue, Result: json.RawMessage(body)})
			_, err := flowGate(ctx, j, "r1", "node:pay", "pay", json.RawMessage(`1`), SingleApproval(), nil)
			if !errors.Is(err, ErrConfig) {
				t.Fatalf("flowGate = %v; want ErrConfig", err)
			}
		})
	}
}

// noKeyVerifier is a verifier that reports no key identity, which no approver can sit on.
type noKeyVerifier struct{ fakeVerifier }

func (noKeyVerifier) KeyIDs() []string { return nil }

// A policy the gate cannot honor is refused (ErrConfig) before anything is recorded: an invalid
// m-of-n policy, and an approver whose verifier reports no key identity.
func TestFlowGate_PolicyRefusedBeforeRecording(t *testing.T) {
	ctx := context.Background()
	noKey := func(id string) (ApproverVerifier, bool) { return noKeyVerifier{fakeVerifier{id}}, true }
	for name, c := range map[string]struct {
		pol *ApprovalPolicy
		vf  ApproverVerifierFor
	}{
		"need above approvers": {&ApprovalPolicy{Need: 3, Approvers: []string{"a"}}, fakeVerifiers("a")},
		"verifier with no key": {&ApprovalPolicy{Need: 1, Approvers: []string{"a"}}, noKey},
	} {
		t.Run(name, func(t *testing.T) {
			j := memJournal()
			if _, err := flowGate(ctx, j, "r1", "node:pay", "pay", nil, c.pol, c.vf); !errors.Is(err, ErrConfig) {
				t.Fatalf("flowGate = %v; want ErrConfig", err)
			}
			if _, ok, err := j.Get(ctx, "r1", ApprovalRequestStep("node:pay")); err != nil || ok {
				t.Fatalf("a refused policy recorded the gate's request: %v, %v", ok, err)
			}
		})
	}
}

// SubmitDecision on a flow node refuses a decision before the gate paused (no request), and an
// identical m-of-n decision recorded before the request is reported once the gate has paused,
// whereas the approver re-signing at a new decided_at is recorded and counts.
func TestFlowGate_MofnDecisionTiming(t *testing.T) {
	ctx := context.Background()
	vf := fakeVerifiers("a")
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	args := json.RawMessage(`1`)
	subj := ApprovalSubject{RunID: "r1", ToolUseID: "node:pay", ToolName: "pay", Args: args}
	sig := func(at Decision) []byte {
		return fakeSign("a", ApprovalDecisionBytes(subj, "a", true, at.DecidedAt))
	}
	d := Decision{RunID: "r1", ToolUseID: "node:pay", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg}
	d.Signature = sig(d)

	j := memJournal()
	if err := SubmitDecision(ctx, j, d); !errors.Is(err, ErrConfig) {
		t.Fatalf("SubmitDecision before the request = %v; want ErrConfig", err)
	}

	// Plant the identical decision under the name a real submit would use, before the gate pauses.
	j2 := memJournal()
	probe := memJournal()
	if _, err := flowGate(ctx, probe, "r1", "node:pay", "pay", args, pol, vf); err == nil {
		t.Fatal("probe gate did not pause")
	}
	if err := SubmitDecision(ctx, probe, d); err != nil {
		t.Fatalf("SubmitDecision after the request: %v", err)
	}
	recs, _ := probe.History(ctx, "r1")
	var name string
	var rec Record
	for _, r := range recs {
		if IsApprovalDecision(r, "node:pay") {
			name, rec = r.Name, r
		}
	}
	if name == "" {
		t.Fatal("no decision record found after SubmitDecision")
	}
	writeRaw(t, j2, "r1", name, rec)
	var pend *ApprovalPending
	if _, err := flowGate(ctx, j2, "r1", "node:pay", "pay", args, pol, vf); !errors.As(err, &pend) {
		t.Fatalf("gate over a pre-request decision = %v; want the pause", err)
	}
	if err := SubmitDecision(ctx, j2, d); !errors.Is(err, ErrConfig) {
		t.Fatalf("re-submitting the stale decision = %v; want ErrConfig", err)
	}
	d2 := d
	d2.DecidedAt = d.DecidedAt.Add(1e6)
	d2.Signature = sig(d2)
	if err := SubmitDecision(ctx, j2, d2); err != nil {
		t.Fatalf("re-signing at a new decided_at: %v", err)
	}
	if ok, err := flowGate(ctx, j2, "r1", "node:pay", "pay", args, pol, vf); err != nil || !ok {
		t.Fatalf("gate after the re-signed decision = %v, %v; want passed", ok, err)
	}
}
