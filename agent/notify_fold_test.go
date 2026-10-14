package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var notifyT0 = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

func nrec(k NotifyRecordKind, key string, min int) NotifyRecord {
	return NotifyRecord{Kind: k, Key: key, RunID: "r", At: notifyT0.Add(time.Duration(min) * time.Minute)}
}

func observed(key string, min int) NotifyRecord {
	r := nrec(NotifyObserved, key, min)
	r.EventKind = "approval_pending"
	r.EventAt = r.At.Add(-time.Second)
	r.DeadlineAt = r.At.Add(5 * time.Minute)
	return r
}

func claimed(key string, min int, until int) NotifyRecord {
	r := nrec(NotifyClaimed, key, min)
	r.Holder = "p"
	r.Until = notifyT0.Add(time.Duration(until) * time.Minute)
	return r
}

func TestFoldNotify_Lifecycle(t *testing.T) {
	st := FoldNotify([]NotifyRecord{observed("k", 0), claimed("k", 1, 3), nrec(NotifyAcked, "k", 2)})
	e := st.Events["k"]
	if e == nil || e.Open() || e.Acked == nil || e.Rounds != 1 || e.Kind != "approval_pending" || !e.EventAt.Equal(notifyT0.Add(-time.Second)) {
		t.Fatalf("event = %+v", e)
	}
	if !reflect.DeepEqual(st.Keys, []string{"k"}) {
		t.Fatalf("keys = %v", st.Keys)
	}
	open := FoldNotify([]NotifyRecord{observed("k", 0)}).Events["k"]
	if !open.Open() || open.Claim != nil {
		t.Fatalf("a fresh event must be open and unclaimed: %+v", open)
	}
}

func TestFoldNotify_IgnoresRecordsThatDoNotApply(t *testing.T) {
	cases := map[string][]NotifyRecord{
		"claim before observation":     {claimed("k", 0, 1)},
		"second observation":           {observed("k", 0), observed("k", 5)},
		"claim after ack":              {observed("k", 0), nrec(NotifyAcked, "k", 1), claimed("k", 2, 4)},
		"ack after clear":              {observed("k", 0), nrec(NotifyCleared, "k", 1), nrec(NotifyAcked, "k", 2)},
		"clear after ack":              {observed("k", 0), nrec(NotifyAcked, "k", 1), nrec(NotifyCleared, "k", 2)},
		"overdue after ack":            {observed("k", 0), nrec(NotifyAcked, "k", 1), nrec(NotifyOverdue, "k", 9)},
		"failing after clear":          {observed("k", 0), nrec(NotifyCleared, "k", 1), nrec(NotifyFailing, "k", 2)},
		"recovered without an episode": {nrec(NotifyRecovered, "", 1)},
		"unknown kind":                 {observed("k", 0), {Kind: "bogus", Key: "k", At: notifyT0}},
	}
	for name, recs := range cases {
		st := FoldNotify(recs)
		switch name {
		case "claim before observation":
			if len(st.Events) != 0 {
				t.Errorf("%s: an event appeared from a claim alone", name)
			}
		case "second observation":
			if !st.Events["k"].ObservedAt.Equal(notifyT0) || len(st.Keys) != 1 {
				t.Errorf("%s: the second observation replaced the first: %+v", name, st.Events["k"])
			}
		case "claim after ack":
			if e := st.Events["k"]; e.Rounds != 0 || e.Claim != nil || e.Acked == nil {
				t.Errorf("%s: %+v", name, e)
			}
		case "ack after clear":
			if e := st.Events["k"]; e.Acked != nil || e.Cleared == nil {
				t.Errorf("%s: the later terminal record won: %+v", name, e)
			}
		case "clear after ack":
			if e := st.Events["k"]; e.Cleared != nil || e.Acked == nil {
				t.Errorf("%s: the later terminal record won: %+v", name, e)
			}
		case "overdue after ack":
			if st.Events["k"].Overdue != nil {
				t.Errorf("%s: overdue recorded on a closed event", name)
			}
		case "failing after clear":
			if st.Events["k"].Failing != nil {
				t.Errorf("%s: failing recorded on a closed event", name)
			}
		case "recovered without an episode":
			if st.Episodes != 0 {
				t.Errorf("%s: Episodes = %d", name, st.Episodes)
			}
		}
	}
}

func TestFoldNotify_FailingAndOverdueAreRaisedOnce(t *testing.T) {
	f1, f2 := nrec(NotifyFailing, "k", 1), nrec(NotifyFailing, "k", 2)
	f1.Failures, f2.Failures = 8, 9
	o1, o2 := nrec(NotifyOverdue, "k", 6), nrec(NotifyOverdue, "k", 7)
	e := FoldNotify([]NotifyRecord{observed("k", 0), f1, f2, o1, o2}).Events["k"]
	if e.Failing.Failures != 8 || !e.Overdue.At.Equal(notifyT0.Add(6*time.Minute)) {
		t.Fatalf("the first failing and overdue records must stand: %+v %+v", e.Failing, e.Overdue)
	}
}

func TestNotifyEvent_ClaimLiveAndOverdueBoundaries(t *testing.T) {
	e := FoldNotify([]NotifyRecord{observed("k", 0), claimed("k", 1, 3)}).Events["k"]
	if !e.ClaimLive(notifyT0.Add(3*time.Minute - time.Nanosecond)) {
		t.Error("a claim is live until its Until")
	}
	if e.ClaimLive(notifyT0.Add(3 * time.Minute)) {
		t.Error("a claim lapses at Until")
	}
	// observed at minute 0, deadline minute 5
	if e.OverdueDue(notifyT0.Add(5 * time.Minute)) {
		t.Error("not overdue at the deadline itself")
	}
	if !e.OverdueDue(notifyT0.Add(5*time.Minute + time.Nanosecond)) {
		t.Error("overdue just past the deadline")
	}
	e2 := FoldNotify([]NotifyRecord{observed("k", 0), nrec(NotifyOverdue, "k", 6)}).Events["k"]
	if e2.OverdueDue(notifyT0.Add(time.Hour)) {
		t.Error("overdue is raised once")
	}
	e3 := FoldNotify([]NotifyRecord{observed("k", 0), nrec(NotifyAcked, "k", 1)}).Events["k"]
	if e3.OverdueDue(notifyT0.Add(time.Hour)) || e3.ClaimLive(notifyT0) {
		t.Error("a closed event is neither overdue nor claimed")
	}
	if got := FoldNotify([]NotifyRecord{observed("k", 0), claimed("k", 1, 3), claimed("k", 4, 6)}).Events["k"]; got.Rounds != 2 || !got.Claim.Until.Equal(notifyT0.Add(6*time.Minute)) {
		t.Errorf("the latest claim must win: %+v", got)
	}
}

func TestFoldNotify_RecoveryEpisodes(t *testing.T) {
	since := func(min int) NotifyRecord { return nrec(NotifyFailingSince, "", min) }
	st := FoldNotify([]NotifyRecord{since(0), since(5)})
	if st.FailingSince == nil || !st.FailingSince.At.Equal(notifyT0) {
		t.Fatalf("the first failing_since opens the episode: %+v", st.FailingSince)
	}
	d := 15 * time.Minute
	if st.RecoveryFailingDue(notifyT0.Add(d), d) {
		t.Error("not due at exactly D")
	}
	if !st.RecoveryFailingDue(notifyT0.Add(d+time.Nanosecond), d) {
		t.Error("due once past D")
	}
	st = FoldNotify([]NotifyRecord{since(0), nrec(NotifyRecovered, "", 3), since(10)})
	if st.Episodes != 1 || st.FailingSince == nil || !st.FailingSince.At.Equal(notifyT0.Add(10*time.Minute)) {
		t.Fatalf("a second episode after recovery: episodes=%d since=%+v", st.Episodes, st.FailingSince)
	}
	if (&NotifyState{}).RecoveryFailingDue(notifyT0, 0) {
		t.Error("no open episode is never due")
	}
}

func TestFoldNotify_IsPure(t *testing.T) {
	recs := []NotifyRecord{observed("a", 0), observed("b", 1), claimed("a", 2, 4), nrec(NotifyAcked, "b", 3)}
	before := append([]NotifyRecord(nil), recs...)
	a, b := FoldNotify(recs), FoldNotify(recs)
	if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(recs, before) {
		t.Fatal("fold is not deterministic or mutated its input")
	}
	if !reflect.DeepEqual(a.Keys, []string{"a", "b"}) {
		t.Fatalf("keys = %v", a.Keys)
	}
}

func TestNotifyKeys(t *testing.T) {
	if HaltedKey("a:b", OpTool, "c", 1) == HaltedKey("a", OpTool, "b:c", 1) {
		t.Error("ids with ':' must not collide")
	}
	if ApprovalKey("r", "c", 0) == ApprovalKey("r", "c", 1) {
		t.Error("a reminder is a different event")
	}
	if got := HaltedKey("run 1", OpStep, "pay", 2); got != "halted:run%201:step:pay:2" {
		t.Errorf("halted key = %q", got)
	}
	if RecoveryKey("r", 0) == RecoveryKey("r", 1) {
		t.Error("episodes are different events")
	}
	id := NotifyID("k")
	if len(id) != 64 || id != NotifyID("k") || id == NotifyID("k2") {
		t.Errorf("NotifyID = %q", id)
	}
	if NotifyTrailName("r") != "notify:r" || !strings.HasPrefix(NotifyDigestTrail, "notify:") {
		t.Error("trail names")
	}
}

func TestNotifyRecord_JSONOmitsUnsetAndNormalizesZones(t *testing.T) {
	r := NotifyRecord{Kind: NotifyObserved, Key: "k", At: notifyT0.In(time.FixedZone("x", 7200))}
	b, _ := json.Marshal(r.Normalized())
	for _, absent := range []string{"event_at", "deadline_at", "until", "first_sent_at", "holder", "auth"} {
		if strings.Contains(string(b), `"`+absent+`"`) {
			t.Errorf("unset field %s is in %s", absent, b)
		}
	}
	if !strings.Contains(string(b), `"at":"2026-10-13T09:00:00Z"`) {
		t.Errorf("time not normalized to UTC: %s", b)
	}
	if !SameNotifyRecord(r, NotifyRecord{Kind: NotifyObserved, Key: "k", At: notifyT0}) {
		t.Error("one instant in two zones must be the same record")
	}
	if SameNotifyRecord(r, NotifyRecord{Kind: NotifyObserved, Key: "k2", At: notifyT0}) {
		t.Error("different keys must differ")
	}
}

// An event observed with no deadline (a resolved event) is never overdue, however late it is.
func TestFoldNotify_AnEventWithNoDeadlineIsNeverOverdue(t *testing.T) {
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	st := FoldNotify([]NotifyRecord{
		{Kind: NotifyObserved, Key: "resolved:k", RunID: "r", At: at, EventKind: string(NotificationResolved), EventAt: at, Reason: "decided"},
	})
	e := st.Events["resolved:k"]
	if e.Reason != "decided" {
		t.Fatalf("reason = %q, want the observed record's", e.Reason)
	}
	if e.OverdueDue(at.Add(1000 * time.Hour)) {
		t.Fatal("an event with no deadline was overdue")
	}
}
