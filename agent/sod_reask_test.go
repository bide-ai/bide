package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
)

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
	if decided, err := e.gateDecided(ctx, "r1", "c1", PendingSoDReapproval); err != nil || decided {
		t.Fatalf("the re-ask reads as decided (%v); the recorded tally it asks over does not end it", err)
	}
	if decided, _ := e.gateDecided(ctx, "r1", "c1", ""); !decided {
		t.Fatal("the first ask is not ended by its recorded tally")
	}
	approveAs(t, store, "r1", "c1", "carol", true)
	var charged int
	if _, err := sodRun(store, "r1", false, sodPolicy(2), fakeVerifiers(abc...), &charged, Identity{}); err != nil {
		t.Fatal(err)
	}
	if decided, _ := e.gateDecided(ctx, "r1", "c1", PendingSoDReapproval); !decided {
		t.Fatal("the re-ask is not ended by its re-tally")
	}
	if evs := e.pendingEvents("r1", mustHistory(t, store, "r1")); len(evs) != 0 {
		t.Fatalf("events after the gate passed = %+v", evs)
	}
}
