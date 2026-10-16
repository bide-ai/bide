package agent

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

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
