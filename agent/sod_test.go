package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Separation of duties: an m-of-n policy with ExcludePrincipal never counts a decision by the
// principal the run acts for (its journaled run:start principal, OnBehalfOf).

// sodRun drives one run of a fresh agent with a gated "charge" tool, as mofnRun does, under the
// identity id when it is not empty (a resume passes none: the journaled principal applies).
func sodRun(store *Journal, runID string, first bool, pol *ApprovalPolicy, verifiers ApproverVerifierFor, charged *int, id Identity) (Message, error) {
	turns := [][]Emit{textTurn("done")}
	if first {
		turns = [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("done")}
	}
	charge := &countingTool{name: "charge", approval: pol, calls: charged}
	a, err := New(&scriptModel{turns: turns}, store, WithTools(charge), WithApproverVerifiers(verifiers))
	if err != nil {
		return Message{}, err
	}
	var opts []RunOption
	if !id.Empty() {
		opts = append(opts, WithIdentity(id))
	}
	res, err := a.Run(context.Background(), runID, UserText("pay"), opts...)
	if err != nil {
		return Message{}, err
	}
	return res.Message, nil
}

func sodPolicy(need int) *ApprovalPolicy {
	return &ApprovalPolicy{Need: need, Approvers: abc, ExcludePrincipal: true}
}

// The requester's approval never counts: with alice as the run's principal, alice and bob
// approving leaves a 2-of-3 gate pending, and carol's approval passes it. The tally records the
// exclusion, the requester, and alice as excluded.
func TestSoD_RequesterApprovalNeverCounts(t *testing.T) {
	store := memJournal()
	pol, vf := sodPolicy(2), fakeVerifiers(abc...)
	var charged int
	alice := Identity{Actor: "agent@1", OnBehalfOf: "alice"}

	_, err := sodRun(store, "r1", true, pol, vf, &charged, alice)
	wantPending(t, err, counts{Need: 2, Pending: []string{"bob", "carol"}})

	approveAs(t, store, "r1", "c1", "alice", true)
	approveAs(t, store, "r1", "c1", "bob", true)
	_, err = sodRun(store, "r1", false, pol, vf, &charged, Identity{})
	wantPending(t, err, counts{Need: 2, Approved: 1, ApprovedBy: []string{"bob"}, Pending: []string{"carol"}})
	var pend *ApprovalPending
	if errors.As(err, &pend); pend.Quorum.Requester != "alice" || !pend.Quorum.ExcludePrincipal || !slices.Equal(pend.Quorum.Excluded, []string{"alice"}) {
		t.Fatalf("running tally = %+v, want requester alice excluded", *pend.Quorum)
	}
	if charged != 0 {
		t.Fatalf("charge ran %d times on the requester's approval, want 0", charged)
	}

	approveAs(t, store, "r1", "c1", "carol", true)
	if _, err := sodRun(store, "r1", false, pol, vf, &charged, Identity{}); err != nil {
		t.Fatalf("run at quorum without the requester: %v", err)
	}
	if charged != 1 {
		t.Fatalf("charged = %d, want 1", charged)
	}
	rec, ok := hasStep(t, store, "r1", ApprovalTallyStep("c1"))
	if !ok {
		t.Fatal("no terminal tally")
	}
	tally, err := decodeTally("r1", rec)
	if err != nil {
		t.Fatal(err)
	}
	if !tally.ExcludePrincipal || tally.Requester != "alice" || !slices.Equal(tally.Excluded, []string{"alice"}) ||
		!slices.Equal(tally.ApprovedBy, []string{"bob", "carol"}) {
		t.Fatalf("tally = %+v, want bob and carol counted, alice excluded as the requester", tally)
	}
}

// The classification names the reason, so the gate, the viewer and the audit can report it.
func TestSoD_TallyClassifiesRequester(t *testing.T) {
	store := memJournal()
	pol, vf := sodPolicy(2), fakeVerifiers(abc...)
	var charged int
	_, _ = sodRun(store, "r1", true, pol, vf, &charged, Identity{OnBehalfOf: "alice"})
	approveAs(t, store, "r1", "c1", "alice", true)
	recs, err := store.History(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	_, checks := TallyApprovals(recs, subjectOf(t, store, "r1", "c1"), *pol, vf)
	if len(checks) != 1 || checks[0].Counted || checks[0].Reason != ReasonRequester {
		t.Fatalf("checks = %+v, want alice's decision not counted: %q", checks, ReasonRequester)
	}
	if got, ok := RecordedRequester(recs); !ok || got != "alice" {
		t.Fatalf("RecordedRequester = %q, %v, want alice", got, ok)
	}
}

// The requester is matched to approver ids ignoring ASCII case, as Validate refuses two approvers
// that differ only by case: "Alice" requesting excludes approver "alice".
func TestSoD_RequesterMatchesIgnoringCase(t *testing.T) {
	store := memJournal()
	pol, vf := sodPolicy(2), fakeVerifiers(abc...)
	var charged int
	_, _ = sodRun(store, "r1", true, pol, vf, &charged, Identity{OnBehalfOf: "Alice"})
	approveAs(t, store, "r1", "c1", "alice", true)
	approveAs(t, store, "r1", "c1", "bob", true)
	_, err := sodRun(store, "r1", false, pol, vf, &charged, Identity{})
	wantPending(t, err, counts{Need: 2, Approved: 1, ApprovedBy: []string{"bob"}, Pending: []string{"carol"}})
}

// A requester who is not an approver excludes no one: every approver keeps a seat.
func TestSoD_RequesterNotAnApprover(t *testing.T) {
	store := memJournal()
	pol, vf := sodPolicy(3), fakeVerifiers(abc...)
	var charged int
	_, _ = sodRun(store, "r1", true, pol, vf, &charged, Identity{OnBehalfOf: "desk-7"})
	for _, id := range abc {
		approveAs(t, store, "r1", "c1", id, true)
	}
	if _, err := sodRun(store, "r1", false, pol, vf, &charged, Identity{}); err != nil {
		t.Fatalf("3 of 3 with a requester outside the approvers: %v", err)
	}
	rec, _ := hasStep(t, store, "r1", ApprovalTallyStep("c1"))
	tally, _ := decodeTally("r1", rec)
	if tally.Requester != "desk-7" || len(tally.Excluded) != 0 || tally.Approved != 3 {
		t.Fatalf("tally = %+v, want requester desk-7 and no one excluded", tally)
	}
}

// Fail closed: a policy that excludes the requester refuses a run that records no principal, and
// one whose principal is not printable ASCII (it could only look like an approver id), with
// ErrConfig, and runs nothing.
func TestSoD_NoUsableRequesterIsConfigError(t *testing.T) {
	for name, id := range map[string]Identity{
		"no principal":   {Actor: "agent@1"},
		"authority only": {AuthorityRef: "grant:1"},
		"non-ASCII":      {OnBehalfOf: "аlice"}, // Cyrillic a
		"space":          {OnBehalfOf: "alice smith"},
		"control":        {OnBehalfOf: "alice\n"},
	} {
		t.Run(name, func(t *testing.T) {
			store := memJournal()
			var charged int
			_, err := sodRun(store, "r1", true, sodPolicy(2), fakeVerifiers(abc...), &charged, id)
			if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "requester") {
				t.Fatalf("err = %v, want ErrConfig naming the requester", err)
			}
			if charged != 0 {
				t.Fatal("the tool ran")
			}
			if _, ok := hasStep(t, store, "r1", ApprovalTallyStep("c1")); ok {
				t.Fatal("a tally was recorded")
			}
		})
	}
}

// k must stay reachable without the requester: 3 of 3 with the requester among the approvers is
// refused at the gate (ErrConfig), before any pause, rather than waiting for ever.
func TestSoD_InfeasibleWithoutRequesterIsConfigError(t *testing.T) {
	store := memJournal()
	var charged int
	_, err := sodRun(store, "r1", true, sodPolicy(3), fakeVerifiers(abc...), &charged, Identity{OnBehalfOf: "carol"})
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "carol") {
		t.Fatalf("err = %v, want ErrConfig naming the requester", err)
	}
	var pend *ApprovalPending
	if errors.As(err, &pend) {
		t.Fatal("paused on an unreachable gate")
	}
}

// The policy's static check: SingleApproval has no approver ids to exclude, so ExcludePrincipal on
// it is ErrConfig; on an m-of-n policy it is valid.
func TestSoD_ValidateSingle(t *testing.T) {
	p := SingleApproval()
	p.ExcludePrincipal = true
	if err := p.Validate(); !errors.Is(err, ErrConfig) {
		t.Fatalf("Validate(single, exclude) = %v, want ErrConfig", err)
	}
	if err := sodPolicy(2).Validate(); err != nil {
		t.Fatalf("Validate(2 of 3, exclude) = %v", err)
	}
}

// The wire form: exclude_principal round-trips, is omitted when false (so an existing policy
// encodes as before), and is refused beside "single".
func TestSoD_PolicyJSON(t *testing.T) {
	b, err := json.Marshal(sodPolicy(2))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"need":2,"approvers":["alice","bob","carol"],"exclude_principal":true}` {
		t.Fatalf("encoding = %s", b)
	}
	var back ApprovalPolicy
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, *sodPolicy(2)) {
		t.Fatalf("decode = %+v, %v", back, err)
	}
	b, _ = json.Marshal(ApprovalPolicy{Need: 1, Approvers: []string{"a"}})
	if string(b) != `{"need":1,"approvers":["a"]}` {
		t.Fatalf("encoding without exclusion = %s", b)
	}
	if err := json.Unmarshal([]byte(`{"single":true,"exclude_principal":true}`), &back); !errors.Is(err, ErrProtocol) {
		t.Fatalf("single with exclude_principal: %v, want ErrProtocol", err)
	}
	if err := json.Unmarshal([]byte(`{"need":1,"approvers":["a"],"exclude_principal":"yes"}`), &back); !errors.Is(err, ErrProtocol) {
		t.Fatalf("exclude_principal of the wrong type: %v, want ErrProtocol", err)
	}
	if c := sodPolicy(2).Clone(); !c.ExcludePrincipal {
		t.Fatal("Clone dropped ExcludePrincipal")
	}
}

// The counting rule fails closed on its own: given records with no run:start (no requester), a
// policy that excludes the requester counts no one, and the tally is unreachable rather than
// passing on whoever signed.
func TestSoD_TallyWithoutStartCountsNoOne(t *testing.T) {
	store := memJournal()
	pol, vf := sodPolicy(1), fakeVerifiers(abc...)
	var charged int
	_, _ = sodRun(store, "r1", true, pol, vf, &charged, Identity{OnBehalfOf: "alice"})
	approveAs(t, store, "r1", "c1", "bob", true)
	recs, _ := store.History(context.Background(), "r1")
	recs = slices.DeleteFunc(recs, func(r Record) bool { return r.Name == runStartStep })
	tally, checks := TallyApprovals(recs, subjectOf(t, store, "r1", "c1"), *pol, vf)
	if tally.Passed() || !tally.Unreachable() || len(checks) != 1 || checks[0].Reason != ReasonNoRequester {
		t.Fatalf("tally = %+v, checks = %+v, want nothing counted (%q)", tally, checks, ReasonNoRequester)
	}
}

// The pause note carries the exclusion, so a viewer can show it before any decision.
func TestSoD_PauseNoteCarriesExclusion(t *testing.T) {
	store := memJournal()
	var charged int
	_, _ = sodRun(store, "r1", true, sodPolicy(2), fakeVerifiers(abc...), &charged, Identity{OnBehalfOf: "alice"})
	rec, ok := hasStep(t, store, "r1", ApprovalPendingStep("c1"))
	if !ok {
		t.Fatal("no pause note")
	}
	var w ApprovalWait
	if err := json.Unmarshal(rec.Result, &w); err != nil || !w.ExcludePrincipal || w.Requester != "alice" {
		t.Fatalf("pause note = %s (%v), want the exclusion and requester alice", rec.Result, err)
	}
}

// A gated call inside a sub-agent excludes the sub-run's principal, which a plain sub-agent
// inherits from its parent run.
func TestSoD_SubAgentInheritsRequester(t *testing.T) {
	store := memJournal()
	pol, vf := sodPolicy(2), fakeVerifiers(abc...)
	var charged int
	charge := &countingTool{name: "charge", approval: pol, calls: &charged}
	childModel := &scriptModel{turns: [][]Emit{toolTurn("c1", "charge", `{}`), textTurn("child done"), textTurn("child done")}}
	child, err := New(childModel, store, WithTools(charge), WithApproverVerifiers(vf))
	if err != nil {
		t.Fatal(err)
	}
	sub := MustSubAgent("pay", "pays", child)
	parent, err := New(&scriptModel{turns: [][]Emit{toolTurn("p1", "pay", `{"task":"go"}`), textTurn("done"), textTurn("done")}}, store, WithTools(sub))
	if err != nil {
		t.Fatal(err)
	}
	_, err = parent.Run(context.Background(), "root", UserText("pay"), WithIdentity(Identity{OnBehalfOf: "alice"}))
	var pend *ApprovalPending
	if !errors.As(err, &pend) {
		t.Fatalf("err = %v, want the sub-run's gate to pause", err)
	}
	if pend.Quorum == nil || pend.Quorum.Requester != "alice" || !slices.Equal(pend.Quorum.Excluded, []string{"alice"}) {
		t.Fatalf("sub-run tally = %+v, want requester alice excluded", pend.Quorum)
	}
}

// A run that an earlier version started records its run:start only when a drive under this version
// first loads it, after the call was requested; its principal is then whatever that drive was given,
// not the requester's. The gate refuses to take a requester recorded after the request.
func TestSoD_RunStartAfterTheRequestIsConfigError(t *testing.T) {
	ctx := context.Background()
	src := memJournal()
	pol, vf := sodPolicy(2), fakeVerifiers(abc...)
	var charged int
	_, _ = sodRun(src, "r1", true, pol, vf, &charged, Identity{OnBehalfOf: "alice"})
	recs, err := src.History(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	legacy := memJournal() // the same run with no run:start, as an earlier version journaled it
	for _, r := range recs {
		if r.Name == runStartStep {
			continue
		}
		if _, err := legacy.put(ctx, "r1", r.Name, r); err != nil {
			t.Fatal(err)
		}
	}
	_, err = sodRun(legacy, "r1", false, pol, vf, &charged, Identity{OnBehalfOf: "mallory"})
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "after") {
		t.Fatalf("err = %v, want ErrConfig: the requester was recorded after the request", err)
	}
	if got, _ := RecordedRequester(mustHistory(t, legacy, "r1")); got != "mallory" {
		t.Fatalf("the late run:start names %q; the test expects the resuming drive's principal", got)
	}
}

func mustHistory(t *testing.T, j *Journal, runID string) []Record {
	t.Helper()
	recs, err := j.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// RecordedRequester reads run:start strictly: a record that names the principal twice, or under a
// case variant encoding/json would also read, names no requester (the gate then refuses the run).
func TestSoD_RecordedRequesterIsStrict(t *testing.T) {
	start := func(js string) []Record {
		return []Record{{Name: runStartStep, Kind: StepValue, Result: json.RawMessage(js)}}
	}
	for js, want := range map[string]string{
		`{"input":"x","principal":{"on_behalf_of":"alice"}}`:                                    "alice",
		`{"input":"x","principal":{"on_behalf_of":"alice"},"later_field":1}`:                    "alice",
		`{"input":"x","principal":{"on_behalf_of":"alice"},"Principal":{"on_behalf_of":"bob"}}`: "",
		`{"input":"x","principal":{"on_behalf_of":"alice","on_behalf_of":"bob"}}`:               "",
		`{"input":"x","principal":{"On_Behalf_Of":"bob"}}`:                                      "",
		`{"input":"x"}`: "",
	} {
		got, ok := RecordedRequester(start(js))
		if got != want || ok != (want != "") {
			t.Errorf("RecordedRequester(%s) = %q, %v; want %q", js, got, ok, want)
		}
	}
	two := append(start(`{"principal":{"on_behalf_of":"alice"}}`), start(`{"principal":{"on_behalf_of":"bob"}}`)...)
	if got, ok := RecordedRequester(two); ok {
		t.Errorf("two disagreeing run:start records: %q", got)
	}
}
