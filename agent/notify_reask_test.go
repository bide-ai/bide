package agent

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

// Tests of a re-ask (ApprovalReaskStep) in the notifier and the journal readers: the key names the
// ask, routing and escalation see the policy the gate asks under, and a note the journal does not
// prove is not read.

// weakNeedThenStrong pauses r1's call c1 under a 1-of-3 policy, records the tally its one approval
// passes, and drives it again under a 2-of-3 policy that excludes the requester: the gate re-asks, and
// the re-ask note's policy differs from the first note's in what it needs.
func weakNeedThenStrong(t *testing.T, store *Journal) error {
	t.Helper()
	vf := fakeVerifiers(abc...)
	var charged int
	weak := &ApprovalPolicy{Need: 1, Approvers: abc}
	_, _ = sodRun(store, "r1", true, weak, vf, &charged, Identity{OnBehalfOf: "alice"})
	approveAs(t, store, "r1", "c1", "alice", true)
	if _, final, err := tallyGate(context.Background(), store, "r1", subjectOf(t, store, "r1", "c1"), weak, vf, ""); err != nil || !final {
		t.Fatalf("the old deployment's tally: %v %v", final, err)
	}
	_, err := sodRun(store, "r1", false, sodPolicy(2), vf, &charged, Identity{})
	if charged != 0 {
		t.Fatal("the call ran")
	}
	return err
}

func reaskByName(t *testing.T, store *Journal) map[string]Record {
	t.Helper()
	return valuesByName(mustHistory(t, store, "r1"))
}

// The key of a re-ask names which ask it is, whether or not it is addressed to one receiver, and the
// event rebuilt from it asks the re-ask's question.
func TestReask_EventFromKeyKeepsTheReason(t *testing.T) {
	for _, tc := range []struct {
		key, want string
	}{
		{ApprovalReaskKey("r1", "c1", true), PendingSoDReapproval},
		{ApprovalReaskKey("r1", "c1", false), PendingReapproval},
		{ToKey("risk", ApprovalReaskKey("r1", "c1", true)), PendingSoDReapproval},
		{ApprovalKey("r1", "c1", 0), ""},
		{ApprovalKey("r1", "c1", 2), ""},
	} {
		ev, ok := eventFromKey(&NotifyEvent{Key: tc.key, Kind: string(NotificationApprovalPending), RunID: "r1"})
		if !ok || ev.approval == nil || ev.approval.Reask != tc.want || ev.approval.ToolUseID != "c1" {
			t.Fatalf("eventFromKey(%q) = %+v (%+v), %v; want Reask %q", tc.key, ev, ev.approval, ok, tc.want)
		}
	}
	// The same through the operation fields a trail record carries.
	ev, ok := eventFromKey(&NotifyEvent{Key: ApprovalReaskKey("r1", "c1", false), Kind: string(NotificationApprovalPending), RunID: "r1", OpRun: "r1", OpKind: "gate", OpID: "c1"})
	if !ok || ev.approval.Reask != PendingReapproval {
		t.Fatalf("event with operation fields = %+v (%v)", ev.approval, ok)
	}
}

// A re-ask is routed by the approvers of the policy it asks under, to a key of its own per receiver,
// and the catch-up scan of those keys leaves it standing.
func TestReask_RoutedByApproversAndNotClearedByTheScan(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	err := weakNeedThenStrong(t, store)
	clk := resolvedClock()
	def, risk, fin := &fakeSink{}, &fakeSink{}, &fakeSink{}
	trail := NewMemNotifyTrail()
	e := testEngine(t, store, def, trail, clk, "p1",
		WithNotifyReceiver("risk", risk), WithNotifyReceiver("finance", fin),
		WithNotifyApproverRoute("alice", "risk"), WithNotifyApproverRoute("bob", "finance"), WithNotifyApproverRoute("carol", "finance"))
	ev, ok := e.eventFromErr("r1", err)
	if !ok || ev.approval.Reask != PendingSoDReapproval {
		t.Fatalf("no re-ask event from %v", err)
	}
	if err := e.roundRouted(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if risk.count() != 1 || fin.count() != 1 || def.count() != 0 {
		t.Fatalf("deliveries: risk=%d finance=%d default=%d", risk.count(), fin.count(), def.count())
	}
	for _, s := range []*fakeSink{risk, fin} {
		a := s.calls[0].Approval
		if a == nil || a.Reask != PendingSoDReapproval || a.Need != 2 {
			t.Fatalf("notification approval = %+v", a)
		}
	}
	clk.advance(1)
	scanAndFlush(t, e, "r1")
	if got := resolvedCalls(risk); len(got) != 0 || len(resolvedCalls(fin)) != 0 {
		t.Fatalf("an all-clear was sent for a re-ask the gate still waits on")
	}
	// Decided by its re-tally: now each receiver is told.
	approveAs(t, store, "r1", "c1", "carol", true)
	var charged int
	if _, err := sodRun(store, "r1", false, sodPolicy(2), fakeVerifiers(abc...), &charged, Identity{}); err == nil {
		t.Fatal("one approval passed a 2-of-3 gate")
	}
	approveAs(t, store, "r1", "c1", "bob", true)
	if _, err := sodRun(store, "r1", false, sodPolicy(2), fakeVerifiers(abc...), &charged, Identity{}); err != nil {
		t.Fatal(err)
	}
	clk.advance(1)
	scanAndFlush(t, e, "r1")
	if len(resolvedCalls(risk)) != 1 || len(resolvedCalls(fin)) != 1 {
		t.Fatalf("all-clears: risk=%d finance=%d; want one each", len(resolvedCalls(risk)), len(resolvedCalls(fin)))
	}
}

// An escalation of a re-ask is rebuilt from the trail and shows the policy the gate asks under, not
// the first ask's.
func TestReask_FillDetailUsesTheReaskNote(t *testing.T) {
	store := memJournal()
	if err := weakNeedThenStrong(t, store); err == nil {
		t.Fatal("no re-ask")
	}
	e := &notifyEngine{cfg: defaultNotifyConfig(), j: store}
	first := notifyEvent{runID: "r1", opRun: "r1", approval: &NotifyApproval{ToolUseID: "c1"}}
	e.fillDetail(context.Background(), &first)
	if first.approval.Need != 1 {
		t.Fatalf("first ask need = %d, want 1", first.approval.Need)
	}
	re := notifyEvent{runID: "r1", opRun: "r1", approval: &NotifyApproval{ToolUseID: "c1", Reask: PendingSoDReapproval}}
	e.fillDetail(context.Background(), &re)
	if re.approval.Need != 2 || re.approval.ToolName != "charge" {
		t.Fatalf("re-ask detail = %+v, want need 2", re.approval)
	}
}

// The journal's events include the re-ask under its own key, current while the gate waits, and no
// second copy of the first ask.
func TestReask_JournalNotifyEvents(t *testing.T) {
	store := memJournal()
	if err := weakThenStrong(t, store); err == nil {
		t.Fatal("no re-ask")
	}
	evs := JournalNotifyEvents("r1", mustHistory(t, store, "r1"))
	got := map[string]JournalNotifyEvent{}
	for _, ev := range evs {
		if _, dup := got[ev.Key]; dup {
			t.Fatalf("event %s twice", ev.Key)
		}
		got[ev.Key] = ev
	}
	first, re := got[ApprovalKey("r1", "c1", 0)], got[ApprovalReaskKey("r1", "c1", true)]
	if len(got) != 2 || first.Current || !re.Current || !slices.Equal(re.Approvers, abc) || re.At.IsZero() {
		t.Fatalf("events = %+v", got)
	}
	approveAs(t, store, "r1", "c1", "carol", true)
	var charged int
	if _, err := sodRun(store, "r1", false, sodPolicy(2), fakeVerifiers(abc...), &charged, Identity{}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range JournalNotifyEvents("r1", mustHistory(t, store, "r1")) {
		if ev.Current {
			t.Fatalf("%s still current after the gate passed", ev.Key)
		}
		if ev.Key == ApprovalReaskKey("r1", "c1", true) && ev.ResolvedAt.IsZero() {
			t.Fatalf("the re-ask has no resolved time, though carol's decision resolved it")
		}
	}
}

// A journal writer without approver keys can insert a note under a re-ask note's name before the
// gate does. No reader treats it as the gate's ask unless a recorded tally the gate would ask again
// over is there and the note names it.
func TestReask_ForgedNotesAreNotRead(t *testing.T) {
	forge := func(sod bool, mutate func(*ApprovalWait)) *Journal {
		store := memJournal()
		vf := fakeVerifiers(abc...)
		var charged int
		_, _ = sodRun(store, "r1", true, sodPolicy(2), vf, &charged, Identity{OnBehalfOf: "alice"})
		w := ApprovalWait{ToolUseID: "c1", ToolName: "charge", Need: 1, Approvers: []string{"mallory"}, RecordedAt: "2026-10-13T09:00:00.000Z",
			ExcludePrincipal: sod, Reask: PendingReapproval, Supersedes: ApprovalTallyStep("c1")}
		if sod {
			w.Reask = PendingSoDReapproval
		}
		if mutate != nil {
			mutate(&w)
		}
		b, _ := json.Marshal(w)
		writeRaw(t, store, "r1", ApprovalReaskStep("c1", sod), Record{Kind: StepValue, Result: b})
		return store
	}
	tally := func(t *testing.T, store *Journal, tl ApprovalTally) {
		t.Helper()
		b, _ := json.Marshal(tl)
		writeRaw(t, store, "r1", ApprovalTallyStep("c1"), Record{Kind: StepValue, Result: b})
	}
	current := ApprovalTally{Need: 2, Approvers: abc, Approved: 2, ApprovedBy: []string{"bob", "carol"}, ExcludePrincipal: true, Requesters: []string{"alice"}, DecisionFormat: DecisionFormat}
	cases := map[string]func(t *testing.T) *Journal{
		"sod note for another call": func(t *testing.T) *Journal {
			s := forge(true, func(w *ApprovalWait) { w.ToolUseID = "c2" })
			weak := current
			weak.ExcludePrincipal = false
			tally(t, s, weak)
			return s
		},
		"sod note with no reason and no tally": func(t *testing.T) *Journal { return forge(true, func(w *ApprovalWait) { w.Reask = "" }) },
		"sod note with no reason": func(t *testing.T) *Journal {
			s := forge(true, func(w *ApprovalWait) { w.Reask = "" })
			weak := current
			weak.ExcludePrincipal = false
			tally(t, s, weak)
			return s
		},
		"sod note with no tally":    func(t *testing.T) *Journal { return forge(true, nil) },
		"format note with no tally": func(t *testing.T) *Journal { return forge(false, nil) },
		"sod note over a tally that excluded the requester": func(t *testing.T) *Journal {
			s := forge(true, nil)
			tally(t, s, current)
			return s
		},
		"format note over a tally in the current format": func(t *testing.T) *Journal {
			s := forge(false, nil)
			tally(t, s, current)
			return s
		},
		"sod note naming no tally": func(t *testing.T) *Journal {
			s := forge(true, func(w *ApprovalWait) { w.Supersedes = "approval-tally:other" })
			weak := current
			weak.ExcludePrincipal = false
			tally(t, s, weak)
			return s
		},
		"sod note without the exclusion": func(t *testing.T) *Journal {
			s := forge(true, func(w *ApprovalWait) { w.ExcludePrincipal = false })
			weak := current
			weak.ExcludePrincipal = false
			tally(t, s, weak)
			return s
		},
		"note with the other reason": func(t *testing.T) *Journal {
			s := forge(true, func(w *ApprovalWait) { w.Reask = PendingReapproval })
			weak := current
			weak.ExcludePrincipal = false
			tally(t, s, weak)
			return s
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			store := mk(t)
			recs := mustHistory(t, store, "r1")
			byName := valuesByName(recs)
			r, ok := LatestPauseNote(byName, "c1")
			if !ok || r.Name != ApprovalPendingStep("c1") {
				t.Fatalf("LatestPauseNote = %q (%v), want the first note", r.Name, ok)
			}
			e := &notifyEngine{cfg: defaultNotifyConfig(), j: store}
			evs := e.pendingEvents("r1", recs)
			// At most the first ask (a recorded tally decides it): never a second event for the note.
			if len(evs) > 1 || len(evs) == 1 && (evs[0].key != ApprovalKey("r1", "c1", 0) || evs[0].approval.Reask != "") {
				t.Fatalf("pending events = %+v; want at most the first ask", evs)
			}
			// fillDetail of a re-ask event does not take the forged note's policy.
			re := notifyEvent{runID: "r1", opRun: "r1", approval: &NotifyApproval{ToolUseID: "c1", Reask: PendingSoDReapproval}}
			e.fillDetail(context.Background(), &re)
			if re.approval.ToolName != "" || re.approval.Need != 0 {
				t.Fatalf("fillDetail took a forged note: %+v", re.approval)
			}
			for _, ev := range JournalNotifyEvents("r1", recs) {
				if ev.Key != ApprovalKey("r1", "c1", 0) {
					t.Fatalf("journal event %s from a forged note", ev.Key)
				}
			}
		})
	}
}

// The note the gate wrote is read: the control of the forged cases above.
func TestReask_GenuineNotesAreRead(t *testing.T) {
	store := memJournal()
	if err := weakThenStrong(t, store); err == nil {
		t.Fatal("no re-ask")
	}
	byName := reaskByName(t, store)
	r, ok := LatestPauseNote(byName, "c1")
	if !ok || r.Name != ApprovalReaskStep("c1", true) {
		t.Fatalf("LatestPauseNote = %q (%v), want the sod note", r.Name, ok)
	}
}

// ValidReaskNote follows the chain of tallies the gate follows: a separation-of-duties re-ask over a
// tally that predates the format is valid only over the format re-tally, once that is recorded.
func TestReask_ValidNoteFollowsTheChainOfTallies(t *testing.T) {
	rec := func(v any) Record { b, _ := json.Marshal(v); return Record{Kind: StepValue, Result: b} }
	old := ApprovalTally{Need: 2, Approvers: abc, Approved: 2, ApprovedBy: []string{"bob", "carol"}}
	retally := ApprovalTally{Need: 2, Approvers: abc, Approved: 2, ApprovedBy: []string{"bob", "carol"}, DecisionFormat: DecisionFormat, Supersedes: ApprovalTallyStep("c1")}
	byName := map[string]Record{ApprovalTallyStep("c1"): rec(old), ApprovalRetallyStep("c1"): rec(retally)}
	sodNote := func(over string) ApprovalWait {
		return ApprovalWait{ToolUseID: "c1", ExcludePrincipal: true, Reask: PendingSoDReapproval, Supersedes: over}
	}
	if !ValidReaskNote(byName, "c1", ApprovalReaskStep("c1", true), sodNote(ApprovalRetallyStep("c1"))) {
		t.Fatal("a sod note over the format re-tally is not valid")
	}
	if ValidReaskNote(byName, "c1", ApprovalReaskStep("c1", true), sodNote(ApprovalTallyStep("c1"))) {
		t.Fatal("a sod note over the tally the format re-tally replaced is valid")
	}
	delete(byName, ApprovalRetallyStep("c1"))
	if ValidReaskNote(byName, "c1", ApprovalReaskStep("c1", true), sodNote(ApprovalTallyStep("c1"))) {
		t.Fatal("a sod note is valid before the gate has asked for the format")
	}
	v3 := ApprovalWait{ToolUseID: "c1", Reask: PendingReapproval, Supersedes: ApprovalTallyStep("c1")}
	if !ValidReaskNote(byName, "c1", ApprovalReaskStep("c1", false), v3) {
		t.Fatal("a format note over a tally that predates the format is not valid")
	}
}
