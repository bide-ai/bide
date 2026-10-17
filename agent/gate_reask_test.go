package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tally names of an identity-bound gate and its re-asks (docs/design/approver-identity.md): the
// terminal tally is approval-tally:<id>:ident, a plain-name tally on such a gate is superseded, and an
// exec hold of generation g supersedes the generation-g tally so the next count is generation g+1.

func tallyJSON(t *testing.T, tl ApprovalTally) []byte {
	t.Helper()
	b, err := json.Marshal(tl)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func passedTally() ApprovalTally {
	return ApprovalTally{Need: 1, Approved: 1, Approvers: []string{"alice", "bob"}, ApprovedBy: []string{"alice"}, DecisionFormat: DecisionFormat}
}

func deniedTally() ApprovalTally {
	return ApprovalTally{Need: 2, Denied: 1, Approvers: []string{"alice", "bob"}, DeniedBy: []string{"alice"}, DecisionFormat: DecisionFormat}
}

func valueRec(t *testing.T, name string, v any) Record {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return Record{Kind: StepValue, Name: name, Result: b}
}

func holdFor(t *testing.T, call string, g int) Record {
	return valueRec(t, ApprovalExecHoldStep(call, g), ExecHold{Format: ApprovalExecHoldFormat, ToolUseID: call, Generation: g, Reason: ReasonRevoked, Detail: "d", ReasonCode: -1})
}

func namesOf(recs ...Record) map[string]Record {
	m := map[string]Record{}
	for _, r := range recs {
		m[r.Name] = r
	}
	return m
}

func TestIdentChain(t *testing.T) {
	g0 := valueRec(t, ApprovalIdentTallyStep("c1", 0), passedTally())
	g1 := valueRec(t, ApprovalIdentTallyStep("c1", 1), passedTally())
	g2 := valueRec(t, ApprovalIdentTallyStep("c1", 2), deniedTally())
	for _, tc := range []struct {
		name    string
		recs    []Record
		present bool
		decided bool
		gen     int
		prev    string
		outcome string
	}{
		{"nothing", nil, false, false, 0, "", ""},
		{"gen 0 tally", []Record{g0}, true, true, 0, "", g0.Name},
		{"a hold supersedes it", []Record{g0, holdFor(t, "c1", 0)}, true, false, 1, g0.Name, ""},
		{"gen 1 decided", []Record{g0, holdFor(t, "c1", 0), g1}, true, true, 1, g0.Name, g1.Name},
		{"held again", []Record{g0, holdFor(t, "c1", 0), g1, holdFor(t, "c1", 1)}, true, false, 2, g1.Name, ""},
		{"a denial at gen 2 is final", []Record{g0, holdFor(t, "c1", 0), g1, holdFor(t, "c1", 1), g2}, true, true, 2, g1.Name, g2.Name},
		{"a hold on a denial is ignored", []Record{valueRec(t, ApprovalIdentTallyStep("c1", 0), deniedTally()), holdFor(t, "c1", 0)}, true, true, 0, "", ApprovalIdentTallyStep("c1", 0)},
		{"a hold without its tally is ignored", []Record{holdFor(t, "c1", 0)}, false, false, 0, "", ""},
		{"a tally past a missing generation is ignored", []Record{g0, g2}, true, true, 0, "", g0.Name},
		{"a hold of another call is ignored", []Record{g0, holdFor(t, "c2", 0)}, true, true, 0, "", g0.Name},
		{"a hold of the wrong generation is ignored", []Record{g0, holdFor(t, "c1", 1)}, true, true, 0, "", g0.Name},
		{"a hold in another format is ignored", []Record{g0, valueRec(t, ApprovalExecHoldStep("c1", 0), ExecHold{Format: "bide.approval-exec-hold.v1", ToolUseID: "c1", Generation: 0})}, true, true, 0, "", g0.Name},
		{"a hold naming another call is ignored", []Record{g0, valueRec(t, ApprovalExecHoldStep("c1", 0), ExecHold{Format: ApprovalExecHoldFormat, ToolUseID: "c2", Generation: 0})}, true, true, 0, "", g0.Name},
		{"a hold naming another generation is ignored", []Record{g0, valueRec(t, ApprovalExecHoldStep("c1", 0), ExecHold{Format: ApprovalExecHoldFormat, ToolUseID: "c1", Generation: 1})}, true, true, 0, "", g0.Name},
		{"a hold that does not read as one is ignored", []Record{g0, {Kind: StepValue, Name: ApprovalExecHoldStep("c1", 0), Result: json.RawMessage(`{"format":"x"}`)}}, true, true, 0, "", g0.Name},
	} {
		st := identChain(namesOf(tc.recs...), "c1")
		if st.present != tc.present || st.decided != tc.decided || st.gen != tc.gen || st.prev != tc.prev || st.outcome.Name != tc.outcome {
			t.Errorf("%s: %+v", tc.name, st)
		}
		if got := identGen(tc.recs, "c1"); got != tc.gen {
			t.Errorf("%s: identGen %d", tc.name, got)
		}
	}
}

// RecordedTally reads the identity chain first: the newest unsuperseded tally is the outcome, a
// plain-name tally beside it is not, and over a hold there is no outcome yet.
func TestRecordedTally_IdentityChain(t *testing.T) {
	plain := valueRec(t, ApprovalTallyStep("c1"), passedTally())
	g0 := valueRec(t, ApprovalIdentTallyStep("c1", 0), passedTally())
	g1 := valueRec(t, ApprovalIdentTallyStep("c1", 1), passedTally())
	if r, ok := RecordedTally(namesOf(plain), "c1"); !ok || r.Name != plain.Name {
		t.Fatalf("plain only: %v %v", r.Name, ok)
	}
	if r, ok := RecordedTally(namesOf(plain, g0), "c1"); !ok || r.Name != g0.Name {
		t.Fatalf("plain and ident: %v %v", r.Name, ok)
	}
	if r, ok := RecordedTally(namesOf(plain, g0, holdFor(t, "c1", 0)), "c1"); ok {
		t.Fatalf("held: the outcome is %v", r.Name)
	}
	if r, ok := RecordedTally(namesOf(plain, g0, holdFor(t, "c1", 0), g1), "c1"); !ok || r.Name != g1.Name {
		t.Fatalf("re-counted: %v %v", r.Name, ok)
	}
}

func TestSupersededTallies_IdentityChain(t *testing.T) {
	g0 := valueRec(t, ApprovalIdentTallyStep("c1", 0), func() ApprovalTally { x := passedTally(); x.Supersedes = ApprovalTallyStep("c1"); return x }())
	g1t := passedTally()
	g1t.Supersedes = g0.Name
	g1 := valueRec(t, ApprovalIdentTallyStep("c1", 1), g1t)
	g2t := passedTally()
	g2t.Supersedes = g1.Name
	g2 := valueRec(t, ApprovalIdentTallyStep("c1", 2), g2t)
	by := namesOf(valueRec(t, ApprovalTallyStep("c1"), passedTally()), g0, g1, g2)
	got := SupersededTallies(by, g2.Name)
	want := []string{ApprovalTallyStep("c1"), g0.Name, g1.Name}
	if !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

// ---- the live gate ----

// boundGate is a gate of two certificate-bound approvers, alice and bob, where a test chooses what the
// verifier finds for each at the time of the check.
type boundGate struct {
	t       *testing.T
	store   *Journal
	pol     *ApprovalPolicy
	alice   *fakeCertVerifier
	bob     *fakeCertVerifier
	charged int
	first   bool
}

func newBoundGate(t *testing.T) *boundGate {
	t.Helper()
	ok := func(id string, k byte) *fakeCertVerifier {
		return &fakeCertVerifier{id: id, check: func(CertDecision) CertCheck {
			return CertCheck{Identity: id, KeyHash: [32]byte{k}, Tier: "final", Basis: "b"}
		}}
	}
	g := &boundGate{t: t, store: memJournal(), pol: &ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}}, alice: ok("alice", 1), bob: ok("bob", 2), first: true}
	return g
}

func (g *boundGate) run() (Message, error) {
	g.t.Helper()
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	m, err := mofnRun(g.store, "r1", g.first, g.pol, vf, &g.charged)
	g.first = false
	return m, err
}

func (g *boundGate) decide(approver string) {
	g.t.Helper()
	writeRaw(g.t, g.store, "r1", "approval:c1:"+approver+":00", certRec("approval:c1:"+approver+":00", approver))
}

// holdGenerationZero pauses the call, has alice decide, records the gate's generation-0 count without
// running the tool, then records an exec hold of generation 0 (what the pre-execution check writes)
// and makes alice's decision fail the verifier (revoked since).
func (g *boundGate) holdGenerationZero() {
	g.t.Helper()
	_, _ = g.run()
	g.decide("alice")
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	if _, final, err := tallyGate(context.Background(), g.store, "r1", subjectOf(g.t, g.store, "r1", "c1"), g.pol, vf, ""); err != nil || !final {
		g.t.Fatalf("%v %v", final, err)
	}
	writeRaw(g.t, g.store, "r1", ApprovalExecHoldStep("c1", 0), holdFor(g.t, "c1", 0))
	g.alice.check = func(CertDecision) CertCheck { return CertCheck{Reason: ReasonRevoked, Detail: "d"} }
}

func (g *boundGate) has(name string) bool {
	_, ok := hasStep(g.t, g.store, "r1", name)
	return ok
}

func (g *boundGate) tally(name string) ApprovalTally {
	g.t.Helper()
	r, ok := hasStep(g.t, g.store, "r1", name)
	if !ok {
		g.t.Fatalf("no %s", name)
	}
	var tl ApprovalTally
	if err := json.Unmarshal(r.Result, &tl); err != nil {
		g.t.Fatal(err)
	}
	return tl
}

func wantReason(t *testing.T, err error, reason string) *ApprovalPending {
	t.Helper()
	var pend *ApprovalPending
	if !errors.As(err, &pend) || pend.Reason != reason {
		t.Fatalf("err = %v, want a pending gate with reason %q", err, reason)
	}
	return pend
}

// The terminal tally of a bound gate is recorded under the :ident name, with its identity record,
// and the plain name is never written.
func TestBoundGate_TallyIsIdentNamed(t *testing.T) {
	g := newBoundGate(t)
	if _, err := g.run(); err == nil {
		t.Fatal("the gate ran with no decision")
	}
	g.decide("alice")
	if _, err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
	if !g.has(ApprovalIdentTallyStep("c1", 0)) || !g.has(ApprovalIdentityStep("c1", 0)) || g.has(ApprovalTallyStep("c1")) {
		t.Fatal("want the :ident tally and identity record and no plain tally")
	}
}

// A plain-name tally that passed is superseded on a bound gate: the call does not run on it, the gate
// asks again, and the new count names what it supersedes.
func TestBoundGate_PlainTallyIsSuperseded(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	writeRaw(t, g.store, "r1", ApprovalTallyStep("c1"), valueRec(t, ApprovalTallyStep("c1"), passedTally()))
	_, err := g.run()
	pend := wantReason(t, err, PendingIdentReapproval)
	if g.charged != 0 || pend.Quorum == nil {
		t.Fatalf("charged %d, quorum %v", g.charged, pend.Quorum)
	}
	g.decide("bob")
	if _, err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
	if got := g.tally(ApprovalIdentTallyStep("c1", 0)).Supersedes; got != ApprovalTallyStep("c1") {
		t.Fatalf("Supersedes %q", got)
	}
	// The re-ask note is the ask of its own, and the journal proves it.
	recs, _ := g.store.History(context.Background(), "r1")
	by := valuesByName(recs)
	n, ok := by[ApprovalIdentReaskStep("c1", 0)]
	var w ApprovalWait
	if !ok || json.Unmarshal(n.Result, &w) != nil || w.Reask != PendingIdentReapproval || w.Supersedes != ApprovalTallyStep("c1") || !ValidReaskNote(by, "c1", n.Name, w) {
		t.Fatalf("note %v %+v", ok, w)
	}
}

// A plain-name denial stays final on a bound gate, as on any gate.
func TestBoundGate_PlainDenialStaysFinal(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	writeRaw(t, g.store, "r1", ApprovalTallyStep("c1"), valueRec(t, ApprovalTallyStep("c1"), deniedTally()))
	g.decide("alice")
	_, _ = g.run()
	if g.charged != 0 {
		t.Fatalf("charged %d after a recorded denial", g.charged)
	}
}

// An exec hold supersedes the tally of its generation: the call does not run, the gate asks again, and
// the next count is the next generation's, recorded under :ident-reask:<n> with its own identity record.
func TestBoundGate_ExecHoldReasks(t *testing.T) {
	g := newBoundGate(t)
	g.holdGenerationZero()
	_, err := g.run()
	pend := wantReason(t, err, PendingExecHoldReapproval)
	if g.charged != 0 || pend.Quorum == nil || pend.Quorum.Approved != 0 {
		t.Fatalf("charged %d, quorum %+v", g.charged, pend.Quorum)
	}
	g.decide("bob")
	if _, err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
	t1 := g.tally(ApprovalIdentTallyStep("c1", 1))
	if t1.Supersedes != ApprovalIdentTallyStep("c1", 0) || !slices.Equal(t1.ApprovedBy, []string{"bob"}) || !g.has(ApprovalIdentityStep("c1", 1)) {
		t.Fatalf("%+v", t1)
	}
}

// A hold written by someone with no tally under it does not re-ask the gate (it is not a hold).
func TestBoundGate_HoldWithoutTallyIsIgnored(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	writeRaw(t, g.store, "r1", ApprovalExecHoldStep("c1", 0), holdFor(t, "c1", 0))
	g.decide("alice")
	if _, err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
	if !g.has(ApprovalIdentTallyStep("c1", 0)) {
		t.Fatal("the count is generation 0")
	}
}

// A bare-key gate is unchanged: its tally keeps the plain name.
func TestBareGate_KeepsPlainTallyName(t *testing.T) {
	store := memJournal()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"alice"}}
	var charged int
	_, _ = mofnRun(store, "r1", true, pol, fakeVerifiers("alice"), &charged)
	approveAs(t, store, "r1", "c1", "alice", true)
	if _, err := mofnRun(store, "r1", false, pol, fakeVerifiers("alice"), &charged); err != nil || charged != 1 {
		t.Fatalf("%v %d", err, charged)
	}
	if _, ok := hasStep(t, store, "r1", ApprovalTallyStep("c1")); !ok {
		t.Fatal("no plain tally")
	}
	if _, ok := hasStep(t, store, "r1", ApprovalIdentTallyStep("c1", 0)); ok {
		t.Fatal("an :ident tally on a bare-key gate")
	}
}

// A plan flow node's gate reads the same chain: a plain-name tally that passed is not its outcome when
// the gate has a certificate-bound approver, and the count is recorded under the identity tally name.
func TestFlowGate_BoundGateSupersedesPlainTally(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	g := newBoundGate(t)
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	args := json.RawMessage(`{"amount":1}`)
	_, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, g.pol, vf)
	var pend *ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("first evaluation = %v", err)
	}
	if _, err := j.do(ctx, "r1", ApprovalTallyStep("node:pay"), func(context.Context) (Record, error) {
		return valueRec(t, ApprovalTallyStep("node:pay"), passedTally()), nil
	}); err != nil {
		t.Fatal(err)
	}
	ok, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, g.pol, vf)
	if ok || !errors.As(err, &pend) || pend.Reason != PendingIdentReapproval {
		t.Fatalf("with a plain tally: %v %v", ok, err)
	}
	rec := certRec("approval:node%3Apay:bob:00", "bob")
	rec.ToolUseID = "node:pay"
	if _, err := j.do(ctx, "r1", rec.Name, func(context.Context) (Record, error) { return rec, nil }); err != nil {
		t.Fatal(err)
	}
	ok, err = flowGate(ctx, j, "r1", "node:pay", "pay", args, g.pol, vf)
	if err != nil || !ok {
		t.Fatalf("after bob decides: %v %v", ok, err)
	}
	if _, found, _ := j.Get(ctx, "r1", ApprovalIdentTallyStep("node:pay", 0)); !found {
		t.Fatal("no identity tally")
	}
}

// A certificate-bound gate whose recorded tally passed without excluding the requester, under a policy
// that now excludes it, cannot be re-asked in place: it refuses with ErrConfig and the call does not run.
func TestBoundGate_SoDChangeAfterCountRefuses(t *testing.T) {
	g := newBoundGate(t)
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	var charged int
	// The run's requester is someone who is no approver, so the exclusion changes nobody's seat.
	_, _ = sodRun(g.store, "r1", true, g.pol, vf, &charged, Identity{OnBehalfOf: "zed"})
	g.decide("alice")
	if _, final, err := tallyGate(context.Background(), g.store, "r1", subjectOf(t, g.store, "r1", "c1"), g.pol, vf, ""); err != nil || !final {
		t.Fatalf("%v %v", final, err)
	}
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}, ExcludePrincipal: true}
	if _, err := sodRun(g.store, "r1", false, pol, vf, &charged, Identity{}); !errors.Is(err, ErrConfig) || charged != 0 {
		t.Fatalf("%v charged %d", err, charged)
	}
}

// A note for generation 0 names a plain-name tally and the reason of that ask; a note for a later
// generation names the held tally of the one before, and only where that generation's hold is there.
func TestIdentReask_NoteValidityByGeneration(t *testing.T) {
	plain := valueRec(t, ApprovalTallyStep("c1"), passedTally())
	g0 := valueRec(t, ApprovalIdentTallyStep("c1", 0), passedTally())
	g1 := valueRec(t, ApprovalIdentTallyStep("c1", 1), passedTally())
	n := func(reask, sup string) ApprovalWait {
		return ApprovalWait{ToolUseID: "c1", Reask: reask, Supersedes: sup}
	}
	// generation 0
	by := namesOf(plain)
	if !ValidReaskNote(by, "c1", ApprovalIdentReaskStep("c1", 0), n(PendingIdentReapproval, plain.Name)) {
		t.Fatal("the genuine generation-0 note")
	}
	if ValidReaskNote(by, "c1", ApprovalIdentReaskStep("c1", 0), n(PendingExecHoldReapproval, plain.Name)) {
		t.Fatal("generation 0 with the hold reason")
	}
	// generation 1: the hold is there
	by = namesOf(g0, holdFor(t, "c1", 0))
	if !ValidReaskNote(by, "c1", ApprovalIdentReaskStep("c1", 1), n(PendingExecHoldReapproval, g0.Name)) {
		t.Fatal("the genuine generation-1 note")
	}
	// the same note with no hold under the tally
	if ValidReaskNote(namesOf(g0), "c1", ApprovalIdentReaskStep("c1", 1), n(PendingExecHoldReapproval, g0.Name)) {
		t.Fatal("generation 1 with no hold")
	}
	// generation 2 must name generation 1's tally, not an older held one
	by = namesOf(g0, holdFor(t, "c1", 0), g1, holdFor(t, "c1", 1))
	if !ValidReaskNote(by, "c1", ApprovalIdentReaskStep("c1", 2), n(PendingExecHoldReapproval, g1.Name)) {
		t.Fatal("the genuine generation-2 note")
	}
	if ValidReaskNote(by, "c1", ApprovalIdentReaskStep("c1", 2), n(PendingExecHoldReapproval, g0.Name)) {
		t.Fatal("generation 2 naming generation 0")
	}
}

// The note of generation 0 (a plain-name tally superseded) is the newest note too.
func TestIdentReask_LatestPauseNoteGenerationZero(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	writeRaw(t, g.store, "r1", ApprovalTallyStep("c1"), valueRec(t, ApprovalTallyStep("c1"), passedTally()))
	_, err := g.run()
	wantReason(t, err, PendingIdentReapproval)
	r, ok := LatestPauseNote(valuesByName(mustHistory(t, g.store, "r1")), "c1")
	if !ok || r.Name != ApprovalIdentReaskStep("c1", 0) {
		t.Fatalf("%v %v", r.Name, ok)
	}
}

// A recorded denial of a certificate-bound gate is final in EvaluateGate whatever the policy is now,
// including no gate at all.
func TestEvaluateGate_BoundDenialIsFinalWithoutPolicy(t *testing.T) {
	recs := []Record{valueRec(t, ApprovalIdentTallyStep("c1", 0), deniedTally())}
	st := EvaluateGate(recs, ApprovalSubject{RunID: "r1", ToolUseID: "c1"}, nil, nil)
	if st.Status != GateDecided || !st.Denied {
		t.Fatalf("%+v", st)
	}
}

// The deadline check and the notifier read an expiry at every tally name of the gate, plain or not.
func TestGateTallyRecords(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	names := []string{ApprovalTallyStep("c1"), ApprovalRetallyStep("c1"), ApprovalSoDRetallyStep("c1"), ApprovalIdentTallyStep("c1", 0), ApprovalIdentTallyStep("c1", 1), ApprovalIdentTallyStep("c1", 3)}
	for _, n := range names {
		writeRaw(t, j, "r1", n, valueRec(t, n, passedTally()))
	}
	got, err := gateTallyRecords(func(name string) (Record, bool, error) { return j.Get(ctx, "r1", name) }, "c1")
	if err != nil {
		t.Fatal(err)
	}
	var gotNames []string
	for _, r := range got {
		gotNames = append(gotNames, r.Name)
	}
	// Generation 3 is past a gap: the chain ends at the first absent generation.
	if want := names[:5]; !slices.Equal(gotNames, want) {
		t.Fatalf("%v, want %v", gotNames, want)
	}
}

// ---- a bound gate is never counted with bare keys ----

// bareRun resumes the gate under a policy whose resolver knows only bare keys for both approvers.
func (g *boundGate) bareRun() error {
	g.t.Helper()
	_, err := mofnRun(g.store, "r1", false, g.pol, fakeVerifiers("alice", "bob"), &g.charged)
	return err
}

// The journal says the gate is certificate-bound after its first pause, after its first count and
// after a hold; in each case a policy that lost the certificate-bound approvers is refused, and the
// call does not run.
func TestBoundGate_DowngradeIsRefused(t *testing.T) {
	// after the pause note
	g := newBoundGate(t)
	_, _ = g.run()
	approveAs(t, g.store, "r1", "c1", "alice", true) // a bare-key decision on a bare resolver
	if err := g.bareRun(); !errors.Is(err, ErrConfig) || g.charged != 0 {
		t.Fatalf("after the note: %v charged %d", err, g.charged)
	}
	// after the first count (no pause note is needed: the identity tally says it)
	g = newBoundGate(t)
	_, _ = g.run()
	g.decide("alice")
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	if _, final, err := tallyGate(context.Background(), g.store, "r1", subjectOf(t, g.store, "r1", "c1"), g.pol, vf, ""); err != nil || !final {
		t.Fatalf("%v %v", final, err)
	}
	if err := g.bareRun(); !errors.Is(err, ErrConfig) || g.charged != 0 {
		t.Fatalf("after the tally: %v charged %d", err, g.charged)
	}
	// after a hold: the generation-1 count must not be made with bare keys
	g = newBoundGate(t)
	g.holdGenerationZero()
	approveAs(t, g.store, "r1", "c1", "bob", true)
	if err := g.bareRun(); !errors.Is(err, ErrConfig) || g.charged != 0 {
		t.Fatalf("after the hold: %v charged %d", err, g.charged)
	}
	if g.has(ApprovalIdentTallyStep("c1", 1)) || g.has(ApprovalTallyStep("c1")) {
		t.Fatal("a tally was recorded by the downgraded policy")
	}
}

// A policy with other approver kinds than the gate was asked under (here bob became bare) is refused too.
func TestBoundGate_ChangedKindsAreRefused(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	vf := func(id string) (ApproverVerifier, bool) {
		if id == "alice" {
			return g.alice, true
		}
		return fakeVerifier{id: id}, true
	}
	if _, err := mofnRun(g.store, "r1", false, g.pol, vf, &g.charged); !errors.Is(err, ErrConfig) || g.charged != 0 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
}

// A bare-key gate is not affected by the check: no note says bound.
func TestBareGate_IsNotRefused(t *testing.T) {
	store := memJournal()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"alice"}}
	var charged int
	_, _ = mofnRun(store, "r1", true, pol, fakeVerifiers("alice"), &charged)
	if bound, _ := journalBound(mustHistory(t, store, "r1"), "c1"); bound {
		t.Fatal("a bare-key gate reads as bound")
	}
}

// A flow gate never takes a plain-name pass as its outcome, and never the pass of a stale replica over an
// identity tally: with a policy that has no certificate approver (a downgrade) it is refused, not run.
func TestFlowGate_PlainPassOverBoundJournalIsNotTaken(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(*Journal, string){
		"ident tally held": func(j *Journal, key string) {
			writeRaw(t, j, "r1", ApprovalIdentTallyStep(key, 0), valueRec(t, ApprovalIdentTallyStep(key, 0), passedTally()))
			writeRaw(t, j, "r1", ApprovalExecHoldStep(key, 0), holdFor(t, key, 0))
		},
		"ident denial": func(j *Journal, key string) {
			writeRaw(t, j, "r1", ApprovalIdentTallyStep(key, 0), valueRec(t, ApprovalIdentTallyStep(key, 0), deniedTally()))
		},
		"bound note": func(j *Journal, key string) {}, // the first note, written below by a bound deployment
	} {
		j := memJournal()
		g := newBoundGate(t)
		args := json.RawMessage(`{"amount":1}`)
		bare := fakeVerifiers("alice", "bob")
		// The first pause is made by a deployment with the certificate approvers: its note says so.
		bound := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
		if name == "bound note" {
			_, _ = flowGate(ctx, j, "r1", "node:pay", "pay", args, g.pol, bound)
		} else {
			_, _ = flowGate(ctx, j, "r1", "node:pay", "pay", args, g.pol, bare)
		}
		setup(j, "node:pay")
		writeRaw(t, j, "r1", ApprovalTallyStep("node:pay"), valueRec(t, ApprovalTallyStep("node:pay"), passedTally()))
		ok, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, g.pol, bare)
		if name == "ident denial" {
			// an identity denial is the outcome whatever the policy says: the node does not run
			if ok {
				t.Errorf("%s: the node ran", name)
			}
			continue
		}
		if ok || !errors.Is(err, ErrConfig) {
			t.Errorf("%s: ok %v err %v", name, ok, err)
		}
	}
}

// A long chain of generations is listed whole.
func TestSupersededTallies_LongChainIsWhole(t *testing.T) {
	var recs []Record
	for g := 0; g < 100; g++ {
		tl := passedTally()
		if g > 0 {
			tl.Supersedes = ApprovalIdentTallyStep("c1", g-1)
		}
		recs = append(recs, valueRec(t, ApprovalIdentTallyStep("c1", g), tl))
	}
	if got := SupersededTallies(namesOf(recs...), ApprovalIdentTallyStep("c1", 99)); len(got) != 99 || got[0] != ApprovalIdentTallyStep("c1", 0) {
		t.Fatalf("%d names", len(got))
	}
	// A journal that loops ends the walk.
	a, b := passedTally(), passedTally()
	a.Supersedes, b.Supersedes = "y", "x"
	loop := namesOf(valueRec(t, "x", a), valueRec(t, "y", b))
	if got := SupersededTallies(loop, "x"); len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

// The read paths report the separation-of-duties dead end by its sentinel.
func TestEvaluateGate_BoundNeedsNewRun(t *testing.T) {
	g := newBoundGate(t)
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	var charged int
	_, _ = sodRun(g.store, "r1", true, g.pol, vf, &charged, Identity{OnBehalfOf: "zed"})
	g.decide("alice")
	if _, final, err := tallyGate(context.Background(), g.store, "r1", subjectOf(t, g.store, "r1", "c1"), g.pol, vf, ""); err != nil || !final {
		t.Fatalf("%v %v", final, err)
	}
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"alice", "bob"}, ExcludePrincipal: true}
	st := EvaluateGate(mustHistory(t, g.store, "r1"), subjectOf(t, g.store, "r1", "c1"), pol, vf)
	if st.Status != GateMisconfigured || !errors.Is(st.Err, ErrGateNeedsNewRun) || !errors.Is(st.Err, ErrConfig) {
		t.Fatalf("%+v", st)
	}
}

// An identity record alone (a crash after it, before the tally) makes the gate bound.
func TestBoundGate_IdentityRecordMakesItBound(t *testing.T) {
	recs := []Record{valueRec(t, ApprovalIdentityStep("c1", 0), ApprovalIdentity{Format: ApprovalIdentityFormat, ToolUseID: "c1"})}
	if bound, _ := journalBound(recs, "c1"); !bound {
		t.Fatal("an identity record does not make the gate bound")
	}
	if bound, _ := journalBound(recs, "c2"); bound {
		t.Fatal("another call's identity record makes the gate bound")
	}
}

// A plain pass counted under bare keys is not taken by a flow gate whose policy has since become
// certificate-bound (an upgrade): the node does not run on approvals counted without certificates.
func TestFlowGate_PlainPassUnderAnUpgradedPolicyIsSuperseded(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	g := newBoundGate(t)
	args := json.RawMessage(`{"amount":1}`)
	_, _ = flowGate(ctx, j, "r1", "node:pay", "pay", args, g.pol, fakeVerifiers("alice", "bob"))
	writeRaw(t, j, "r1", ApprovalTallyStep("node:pay"), valueRec(t, ApprovalTallyStep("node:pay"), passedTally()))
	bound := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	ok, err := flowGate(ctx, j, "r1", "node:pay", "pay", args, g.pol, bound)
	var pend *ApprovalPending
	if ok || !errors.As(err, &pend) || pend.Reason != PendingIdentReapproval {
		t.Fatalf("ok %v err %v", ok, err)
	}
}

// The notifier and the journal readers over a certificate-bound gate's re-asks: the key names the ask
// (ApprovalIdentReaskKey), the event rebuilt from it keeps the reason and the generation, and the ask
// ends with the identity tally of its generation.

func TestIdentReask_KeysRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		key    string
		reason string
		gen    int
	}{
		{ApprovalIdentReaskKey("r1", "c1", 0), PendingIdentReapproval, 0},
		{ApprovalIdentReaskKey("r1", "c1", 1), PendingExecHoldReapproval, 1},
		{ApprovalIdentReaskKey("r1", "c1", 12), PendingExecHoldReapproval, 12},
		{ToKey("risk", ApprovalIdentReaskKey("r1", "c1", 2)), PendingExecHoldReapproval, 2},
	} {
		ev, ok := eventFromKey(&NotifyEvent{Key: tc.key, Kind: string(NotificationApprovalPending), RunID: "r1"})
		if !ok || ev.approval == nil || ev.approval.Reask != tc.reason || ev.approval.reaskGen != tc.gen || ev.approval.ToolUseID != "c1" {
			t.Fatalf("eventFromKey(%q) = %+v, %v", tc.key, ev.approval, ok)
		}
		if got := reaskKeyFor("r1", "c1", tc.reason, tc.gen); got != ApprovalIdentReaskKey("r1", "c1", tc.gen) {
			t.Fatalf("reaskKeyFor %q", got)
		}
	}
	// A key that is not a canonical generation is not a re-ask.
	for _, bad := range []string{"reask-ident-", "reask-ident--1", "reask-ident-01", "reask-ident-x"} {
		parts := []string{"approval", "r1", "c1", bad}
		if reason, _ := reaskOfKey(parts); reason != "" {
			t.Fatalf("%q is read as %q", bad, reason)
		}
	}
	if ApprovalIdentReaskKey("r1", "c1", 0) == ApprovalIdentReaskKey("r1", "c1", 1) || ApprovalIdentReaskKey("r1", "c1", 1) == ApprovalReaskKey("r1", "c1", true) {
		t.Fatal("keys collide")
	}
}

func TestIdentReask_Notification(t *testing.T) {
	ctx := context.Background()
	g := newBoundGate(t)
	g.holdGenerationZero()
	_, err := g.run()
	wantReason(t, err, PendingExecHoldReapproval)
	e := &notifyEngine{cfg: defaultNotifyConfig(), j: g.store}
	ev, ok := e.eventFromErr("r1", err)
	if !ok || ev.key != ApprovalIdentReaskKey("r1", "c1", 1) || ev.approval.Reask != PendingExecHoldReapproval || ev.approval.reaskGen != 1 {
		t.Fatalf("event from the pause = %+v (%+v)", ev, ev.approval)
	}
	// The notes: the first ask is ended by the generation-0 tally; the re-ask stands.
	evs := e.pendingEvents("r1", mustHistory(t, g.store, "r1"))
	if len(evs) != 1 || evs[0].key != ApprovalIdentReaskKey("r1", "c1", 1) || evs[0].approval.reaskGen != 1 {
		t.Fatalf("events from the notes = %+v", evs)
	}
	if decided, err := e.gateDecided(ctx, "r1", "c1", PendingExecHoldReapproval, 1); err != nil || decided {
		t.Fatalf("the re-ask reads as decided (%v): the held tally does not end it", err)
	}
	if decided, _ := e.gateDecided(ctx, "r1", "c1", "", 0); !decided {
		t.Fatal("the first ask is not ended by its identity tally")
	}
	// The journal's view of the same asks.
	jev := map[string]JournalNotifyEvent{}
	for _, v := range JournalNotifyEvents("r1", mustHistory(t, g.store, "r1")) {
		jev[v.Key] = v
	}
	re := jev[ApprovalIdentReaskKey("r1", "c1", 1)]
	if !re.Current || !slices.Equal(re.Approvers, []string{"alice", "bob"}) || re.At.IsZero() || jev[ApprovalKey("r1", "c1", 0)].Current {
		t.Fatalf("journal events = %+v", jev)
	}
	// Bob decides, the gate counts generation 1 and runs the call: nothing is pending.
	g.decide("bob")
	if _, err := g.run(); err != nil || g.charged != 1 {
		t.Fatalf("%v charged %d", err, g.charged)
	}
	if decided, _ := e.gateDecided(ctx, "r1", "c1", PendingExecHoldReapproval, 1); !decided {
		t.Fatal("the re-ask is not ended by the generation-1 tally")
	}
	if evs := e.pendingEvents("r1", mustHistory(t, g.store, "r1")); len(evs) != 0 {
		t.Fatalf("events after the gate passed = %+v", evs)
	}
	for _, v := range JournalNotifyEvents("r1", mustHistory(t, g.store, "r1")) {
		if v.Current {
			t.Fatalf("%s still current", v.Key)
		}
	}
}

// A plain-name tally superseded on a bound gate is the generation-0 re-ask; the two reasons key
// differently from the generations after a hold.
func TestIdentReask_PlainSupersededNotification(t *testing.T) {
	g := newBoundGate(t)
	_, _ = g.run()
	writeRaw(t, g.store, "r1", ApprovalTallyStep("c1"), valueRec(t, ApprovalTallyStep("c1"), passedTally()))
	_, err := g.run()
	wantReason(t, err, PendingIdentReapproval)
	e := &notifyEngine{cfg: defaultNotifyConfig(), j: g.store}
	ev, ok := e.eventFromErr("r1", err)
	if !ok || ev.key != ApprovalIdentReaskKey("r1", "c1", 0) || ev.approval.Reask != PendingIdentReapproval {
		t.Fatalf("%+v", ev)
	}
	if decided, _ := e.gateDecided(context.Background(), "r1", "c1", PendingIdentReapproval, 0); decided {
		t.Fatal("the plain tally it asks over ends the re-ask")
	}
	// A plain-name tally on a certificate-bound gate (a stale replica wrote it) does not end its first ask.
	if decided, _ := e.gateDecided(context.Background(), "r1", "c1", "", 0); decided {
		t.Fatal("a superseded plain tally ends the first ask")
	}
	for _, v := range JournalNotifyEvents("r1", mustHistory(t, g.store, "r1")) {
		if v.Key == ApprovalKey("r1", "c1", 0) && !v.Current {
			t.Fatal("the journal reads the first ask as decided by a superseded plain tally")
		}
	}
}

// Forged or stale notes are not read: a hold-less generation-1 note, a note whose Supersedes does not
// name the held tally, and a generation-0 note over a plain tally that did not pass.
func TestIdentReask_ForgedNotesAreNotRead(t *testing.T) {
	g := newBoundGate(t)
	g.holdGenerationZero()
	by := valuesByName(mustHistory(t, g.store, "r1"))
	good := ApprovalWait{ToolUseID: "c1", Reask: PendingExecHoldReapproval, Supersedes: ApprovalIdentTallyStep("c1", 0)}
	if !ValidReaskNote(by, "c1", ApprovalIdentReaskStep("c1", 1), good) {
		t.Fatal("the genuine note is not valid")
	}
	for name, mutate := range map[string]func(ApprovalWait) (string, ApprovalWait){
		"wrong supersedes": func(w ApprovalWait) (string, ApprovalWait) {
			w.Supersedes = ApprovalTallyStep("c1")
			return ApprovalIdentReaskStep("c1", 1), w
		},
		"wrong reason": func(w ApprovalWait) (string, ApprovalWait) {
			w.Reask = PendingIdentReapproval
			return ApprovalIdentReaskStep("c1", 1), w
		},
		"another call": func(w ApprovalWait) (string, ApprovalWait) {
			w.ToolUseID = "c2"
			return ApprovalIdentReaskStep("c1", 1), w
		},
		"generation with no hold": func(w ApprovalWait) (string, ApprovalWait) {
			w.Supersedes = ApprovalIdentTallyStep("c1", 1)
			return ApprovalIdentReaskStep("c1", 2), w
		},
		"generation 0 with no plain tally": func(w ApprovalWait) (string, ApprovalWait) {
			w.Reask, w.Supersedes = PendingIdentReapproval, ApprovalTallyStep("c1")
			return ApprovalIdentReaskStep("c1", 0), w
		},
		"not a note name": func(w ApprovalWait) (string, ApprovalWait) {
			return ApprovalIdentReaskStep("c1", 1) + "x", w
		},
	} {
		n, w := mutate(good)
		if ValidReaskNote(by, "c1", n, w) {
			t.Errorf("%s: a forged note is valid", name)
		}
	}
	// LatestPauseNote never returns a note that is not proven: the generation-1 note with no hold of
	// generation 0 (remove it) is ignored.
	noHold := map[string]Record{}
	for k, v := range by {
		if k != ApprovalExecHoldStep("c1", 0) {
			noHold[k] = v
		}
	}
	noHold[ApprovalIdentReaskStep("c1", 1)] = valueRec(t, ApprovalIdentReaskStep("c1", 1), good)
	if r, ok := LatestPauseNote(noHold, "c1"); ok && r.Name == ApprovalIdentReaskStep("c1", 1) {
		t.Fatal("LatestPauseNote read an unproven note")
	}
}

// LatestPauseNote returns the newest generation's re-ask note.
func TestIdentReask_LatestPauseNote(t *testing.T) {
	g := newBoundGate(t)
	g.holdGenerationZero()
	_, err := g.run() // pauses, writes the generation-1 note
	wantReason(t, err, PendingExecHoldReapproval)
	by := valuesByName(mustHistory(t, g.store, "r1"))
	r, ok := LatestPauseNote(by, "c1")
	if !ok || r.Name != ApprovalIdentReaskStep("c1", 1) {
		t.Fatalf("%v %v", r.Name, ok)
	}
}

// An escalation of a certificate-bound gate's re-ask is rebuilt from the note of its own generation.
func TestIdentReask_FillDetailUsesTheGenerationsNote(t *testing.T) {
	g := newBoundGate(t)
	g.holdGenerationZero()
	if _, err := g.run(); err == nil {
		t.Fatal("no re-ask")
	}
	e := &notifyEngine{cfg: defaultNotifyConfig(), j: g.store}
	re := notifyEvent{runID: "r1", opRun: "r1", approval: &NotifyApproval{ToolUseID: "c1", Reask: PendingExecHoldReapproval, reaskGen: 1}}
	e.fillDetail(context.Background(), &re)
	if re.approval.ToolName != "charge" || re.approval.Need != 1 || re.approval.Of != 2 {
		t.Fatalf("detail = %+v", re.approval)
	}
	// The note of another generation is not this ask's.
	other := notifyEvent{runID: "r1", opRun: "r1", approval: &NotifyApproval{ToolUseID: "c1", Reask: PendingExecHoldReapproval, reaskGen: 2}}
	e.fillDetail(context.Background(), &other)
	if other.approval.ToolName != "" {
		t.Fatalf("a generation with no note was filled: %+v", other.approval)
	}
}

// A recorded expiry of a certificate-bound gate is final in the deadline check and in the notifier's
// all-clear, whichever generation recorded it.
func TestIdentReask_ExpiryIsReadFromTheChain(t *testing.T) {
	ctx := context.Background()
	g := newBoundGate(t)
	g.holdGenerationZero()
	if _, err := g.run(); err == nil {
		t.Fatal("no re-ask")
	}
	deadlineAt := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	dl := ApprovalDeadline{ToolUseID: "c1", RequestedAt: FormatTimestamp(deadlineAt.Add(-time.Hour)), ExpiresAt: FormatTimestamp(deadlineAt), TimeoutMS: time.Hour.Milliseconds()}
	writeRaw(t, g.store, "r1", ApprovalDeadlineStep("c1"), valueRec(t, ApprovalDeadlineStep("c1"), dl))
	early := func() time.Time { return deadlineAt.Add(-time.Minute) }
	if err := checkNotExpired(ctx, g.store, "op", "r1", "c1", early); err != nil {
		t.Fatalf("no expiry is recorded yet: %v", err)
	}
	exp := passedTally()
	exp.Expired, exp.Approved = true, 0
	exp.ApprovedBy = nil
	exp.Supersedes = ApprovalIdentTallyStep("c1", 0)
	writeRaw(t, g.store, "r1", ApprovalIdentTallyStep("c1", 1), valueRec(t, ApprovalIdentTallyStep("c1", 1), exp))
	if err := checkNotExpired(ctx, g.store, "op", "r1", "c1", early); !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("a recorded expiry at generation 1 is not read: %v", err)
	}
	e := &notifyEngine{cfg: defaultNotifyConfig(), j: g.store}
	if !e.gateExpired(ctx, "r1", "c1") {
		t.Fatal("the notifier does not read the expiry of generation 1")
	}
}

// The re-ask of a held generation is ended by the identity tally of its generation, even before the
// call runs.
func TestIdentReask_EndedByTheGenerationsTally(t *testing.T) {
	ctx := context.Background()
	g := newBoundGate(t)
	g.holdGenerationZero()
	g.decide("bob")
	vf := certResolver(map[string]ApproverVerifier{"alice": g.alice, "bob": g.bob})
	if _, final, err := tallyGate(ctx, g.store, "r1", subjectOf(t, g.store, "r1", "c1"), g.pol, vf, ""); err != nil || !final {
		t.Fatalf("%v %v", final, err)
	}
	e := &notifyEngine{cfg: defaultNotifyConfig(), j: g.store}
	if decided, _ := e.gateDecided(ctx, "r1", "c1", PendingExecHoldReapproval, 1); !decided {
		t.Fatal("the generation-1 tally does not end the re-ask")
	}
	if decided, _ := e.gateDecided(ctx, "r1", "c1", PendingExecHoldReapproval, 2); decided {
		t.Fatal("the generation-1 tally ends the generation-2 re-ask")
	}
}

// Approver ids are printable ASCII (0x21 to 0x7E): letters, digits and punctuation, no space, no
// control character, nothing outside ASCII. An id outside that set could look like another one (a
// Cyrillic "о" for a Latin "o", a fullwidth spelling, a combining accent) and let one approver fill
// two seats, so it is refused with ErrConfig, naming the id.
func TestApprovalPolicy_RefusesIDsOutsidePrintableASCII(t *testing.T) {
	for name, id := range map[string]string{
		"homoglyph (Cyrillic o)": "оps", // "оps", which reads as "ops"
		"fullwidth":              "ａｌｉｃｅ",
		"accent (NFC)":           "café",
		"accent (NFD)":           "café",
		"sharp s":                "straße",
		"space":                  "al ice",
		"leading space":          " ops",
		"tab":                    "ops\t",
		"newline":                "ops\n",
		"DEL":                    "ops\x7f",
		"NUL":                    "ops\x00",
		"invalid UTF-8":          "bob\xff",
		"zero-width space":       "ops​",
	} {
		approvers := []string{"ops", "finance", id}
		err := ApprovalPolicy{Need: 1, Approvers: approvers}.Validate()
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: Validate(%q) = %v, want ErrConfig", name, approvers, err)
			continue
		}
		if !strings.Contains(err.Error(), "printable ASCII") {
			t.Errorf("%s: error %q does not say ids are printable ASCII", name, err)
		}
	}
}

// The homoglyph case on its own: "ops" in Latin letters and "оps" with a Cyrillic "о" are never
// both accepted, though no case folding or Unicode normalization equates them.
func TestApprovalPolicy_RefusesHomoglyph(t *testing.T) {
	err := ApprovalPolicy{Need: 2, Approvers: []string{"ops", "оps"}}.Validate()
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("Validate = %v, want ErrConfig", err)
	}
}

// Two ids that differ only by ASCII case read as one approver, who could fill two seats: refused
// with ErrConfig, naming both.
func TestApprovalPolicy_RefusesIDsThatDifferOnlyByCase(t *testing.T) {
	for _, pair := range [][2]string{
		{"alice", "Alice"},
		{"bob", "BOB"},
		{"Ops@Example.com", "ops@example.com"},
		{"carol", "CAROL"},
	} {
		approvers := []string{pair[0], "dave", pair[1]}
		err := ApprovalPolicy{Need: 1, Approvers: approvers}.Validate()
		if !errors.Is(err, ErrConfig) {
			t.Errorf("Validate(%q) = %v, want ErrConfig", approvers, err)
			continue
		}
		if !strings.Contains(err.Error(), pair[0]) || !strings.Contains(err.Error(), pair[1]) || !strings.Contains(err.Error(), "case") {
			t.Errorf("error %q does not name both %q and %q as differing only by case", err, pair[0], pair[1])
		}
	}
}

// An empty id is refused.
func TestApprovalPolicy_RefusesEmptyID(t *testing.T) {
	if err := (ApprovalPolicy{Need: 1, Approvers: []string{"alice", ""}}).Validate(); !errors.Is(err, ErrConfig) {
		t.Fatalf("Validate = %v, want ErrConfig", err)
	}
}

// Ids in printable ASCII that differ other than by case pass, emails and URIs included.
func TestApprovalPolicy_DistinctASCIIIDsPass(t *testing.T) {
	for _, approvers := range [][]string{
		{"alice", "bob"},
		{"alice", "alice2"},
		{"alice", "al1ce"},
		{"ops@example.com", "finance@example.com", "risk.team+oncall@example.co.uk"},
		{"spiffe://example.org/approver/ops", "urn:bide:approver:finance"},
		{"!", "~", "a_b-c.d"},
	} {
		if err := (ApprovalPolicy{Need: 1, Approvers: approvers}).Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", approvers, err)
		}
	}
}

// An id listed twice is reported as a repeat, not as two spellings of one approver.
func TestApprovalPolicy_ExactRepeatIsReportedAsTwice(t *testing.T) {
	err := ApprovalPolicy{Need: 1, Approvers: []string{"alice", "alice"}}.Validate()
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("Validate = %v, want ErrConfig saying the id is listed twice", err)
	}
}

// weakThenStrong drives r1 to a pause under a policy that does not exclude the requester (alice),
// records alice's and bob's approvals and the passing tally the old deployment's drive wrote before
// it crashed (the call never ran), and then drives it under the policy that now excludes alice.
func weakThenStrong(t *testing.T, store *Journal) error {
	t.Helper()
	vf := fakeVerifiers(abc...)
	var charged int
	weak := &ApprovalPolicy{Need: 2, Approvers: abc}
	_, _ = sodRun(store, "r1", true, weak, vf, &charged, Identity{OnBehalfOf: "alice"})
	approveAs(t, store, "r1", "c1", "alice", true)
	approveAs(t, store, "r1", "c1", "bob", true)
	if _, final, err := tallyGate(context.Background(), store, "r1", subjectOf(t, store, "r1", "c1"), weak, vf, ""); err != nil || !final {
		t.Fatalf("the old deployment's tally: %v %v", final, err)
	}
	_, err := sodRun(store, "r1", false, sodPolicy(2), vf, &charged, Identity{})
	if charged != 0 {
		t.Fatal("the call ran")
	}
	return err
}

func notes(t *testing.T, store *Journal, name string) []Record {
	t.Helper()
	var out []Record
	for _, r := range mustHistory(t, store, "r1") {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

// A gate that asks again because its policy now excludes the requester records a re-ask note with
// the policy it asks under, so a reader with only the journal sees it waiting: the first pause note,
// written under the old policy, stays as it was, and LatestPauseNote returns the re-ask note.
func TestSoDReask_NoteRecordsThePolicyNow(t *testing.T) {
	store := memJournal()
	err := weakThenStrong(t, store)
	var pend *ApprovalPending
	if !errors.As(err, &pend) || pend.Reason != PendingSoDReapproval {
		t.Fatalf("err = %v, want a re-approval pause", err)
	}
	ns := notes(t, store, ApprovalReaskStep("c1", true))
	if len(ns) != 1 {
		t.Fatalf("%d re-ask notes, want 1", len(ns))
	}
	var w ApprovalWait
	if err := json.Unmarshal(ns[0].Result, &w); err != nil {
		t.Fatal(err)
	}
	if !w.ExcludePrincipal || w.Need != 2 || !slices.Equal(w.Approvers, abc) || !slices.Equal(w.Requesters, []string{"alice"}) ||
		w.Reask != PendingSoDReapproval || w.Supersedes != ApprovalTallyStep("c1") || w.ToolName != "charge" || w.RecordedAt == "" {
		t.Fatalf("re-ask note = %+v", w)
	}
	var first ApprovalWait
	_ = json.Unmarshal(notes(t, store, ApprovalPendingStep("c1"))[0].Result, &first)
	if first.ExcludePrincipal || first.Reask != "" {
		t.Fatalf("the first pause note changed: %+v", first)
	}
	byName := map[string]Record{}
	for _, r := range mustHistory(t, store, "r1") {
		byName[r.Name] = r
	}
	if r, ok := LatestPauseNote(byName, "c1"); !ok || r.Name != ApprovalReaskStep("c1", true) {
		t.Fatalf("LatestPauseNote = %q, %v", r.Name, ok)
	}
	// A later re-drive that pauses again keeps the note as written.
	before := string(ns[0].Result)
	var charged int
	_, _ = sodRun(store, "r1", false, sodPolicy(2), fakeVerifiers(abc...), &charged, Identity{})
	if ns := notes(t, store, ApprovalReaskStep("c1", true)); len(ns) != 1 || string(ns[0].Result) != before {
		t.Fatalf("the re-ask note was written again: %d", len(ns))
	}
}

// Two processes that re-ask at once write one note: the first writer wins.
func TestSoDReask_ConcurrentReasksWriteOneNote(t *testing.T) {
	for range 20 {
		store := memJournal()
		vf := fakeVerifiers(abc...)
		var charged int
		weak := &ApprovalPolicy{Need: 2, Approvers: abc}
		_, _ = sodRun(store, "r1", true, weak, vf, &charged, Identity{OnBehalfOf: "alice"})
		approveAs(t, store, "r1", "c1", "alice", true)
		approveAs(t, store, "r1", "c1", "bob", true)
		if _, _, err := tallyGate(context.Background(), store, "r1", subjectOf(t, store, "r1", "c1"), weak, vf, ""); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var n int
				_, errs[i] = sodRun(store, "r1", false, sodPolicy(2), vf, &n, Identity{})
			}()
		}
		wg.Wait()
		for _, err := range errs {
			var pend *ApprovalPending
			if !errors.As(err, &pend) || pend.Reason != PendingSoDReapproval {
				t.Fatalf("a drive returned %v, want the re-approval pause", err)
			}
		}
		if ns := notes(t, store, ApprovalReaskStep("c1", true)); len(ns) != 1 {
			t.Fatalf("%d re-ask notes after two drives re-asked at once, want 1", len(ns))
		}
	}
}

// A re-ask for the format (a tally that passed on decisions predating DecisionFormat) writes its
// own note, apart from the separation-of-duties one.
func TestSoDReask_FormatReaskNote(t *testing.T) {
	store := memJournal()
	vf := fakeVerifiers(abc...)
	var charged int
	pol := &ApprovalPolicy{Need: 2, Approvers: abc}
	_, _ = sodRun(store, "r1", true, pol, vf, &charged, Identity{})
	old, _ := json.Marshal(ApprovalTally{Need: 2, Approvers: abc, Approved: 2, ApprovedBy: []string{"alice", "bob"}})
	writeRaw(t, store, "r1", ApprovalTallyStep("c1"), Record{Kind: StepValue, Result: old})
	_, err := sodRun(store, "r1", false, pol, vf, &charged, Identity{})
	var pend *ApprovalPending
	if !errors.As(err, &pend) || pend.Reason != PendingReapproval {
		t.Fatalf("err = %v, want a format re-approval pause", err)
	}
	ns := notes(t, store, ApprovalReaskStep("c1", false))
	var w ApprovalWait
	if len(ns) != 1 || json.Unmarshal(ns[0].Result, &w) != nil || w.Reask != PendingReapproval || w.Supersedes != ApprovalTallyStep("c1") {
		t.Fatalf("format re-ask notes = %d (%+v)", len(ns), w)
	}
	if len(notes(t, store, ApprovalReaskStep("c1", true))) != 0 {
		t.Fatal("a separation-of-duties note was written for a format re-ask")
	}
}

// A re-ask is a notification of its own: the pause and the re-ask note both carry the reason under
// ApprovalReaskKey, the first ask's note is ended by the recorded tally, and the re-ask is ended
// only by its re-tally or the call's result.
func TestSoDReask_Notification(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	err := weakThenStrong(t, store)
	e := &notifyEngine{cfg: defaultNotifyConfig(), j: store}
	ev, ok := e.eventFromErr("r1", err)
	if !ok || ev.key != ApprovalReaskKey("r1", "c1", true) || ev.approval.Reask != PendingSoDReapproval || !slices.Equal(ev.approval.Excluded, []string{"alice"}) {
		t.Fatalf("event from the re-ask pause = %+v (%+v)", ev, ev.approval)
	}
	evs := e.pendingEvents("r1", mustHistory(t, store, "r1"))
	if len(evs) != 1 || evs[0].key != ApprovalReaskKey("r1", "c1", true) || evs[0].approval.Reask != PendingSoDReapproval {
		t.Fatalf("events from the notes = %+v; want only the re-ask", evs)
	}
	if decided, err := e.gateDecided(ctx, "r1", "c1", PendingSoDReapproval, 0); err != nil || decided {
		t.Fatalf("the re-ask reads as decided (%v); the recorded tally it asks over does not end it", err)
	}
	if decided, _ := e.gateDecided(ctx, "r1", "c1", "", 0); !decided {
		t.Fatal("the first ask is not ended by its recorded tally")
	}
	approveAs(t, store, "r1", "c1", "carol", true)
	var charged int
	if _, err := sodRun(store, "r1", false, sodPolicy(2), fakeVerifiers(abc...), &charged, Identity{}); err != nil {
		t.Fatal(err)
	}
	if decided, _ := e.gateDecided(ctx, "r1", "c1", PendingSoDReapproval, 0); !decided {
		t.Fatal("the re-ask is not ended by its re-tally")
	}
	if evs := e.pendingEvents("r1", mustHistory(t, store, "r1")); len(evs) != 0 {
		t.Fatalf("events after the gate passed = %+v", evs)
	}
}
