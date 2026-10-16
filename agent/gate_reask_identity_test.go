package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
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
	n := func(reask, sup string) ApprovalWait { return ApprovalWait{ToolUseID: "c1", Reask: reask, Supersedes: sup} }
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
