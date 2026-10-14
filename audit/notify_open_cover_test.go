package audit_test

import (
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// With an open trail given, every delivered event that can owe an all-clear is on it: listed by a
// watch record or a checkpoint. A backfill that lists nothing cannot hide an event observed after
// the open trail began; one observed before needs the backfill recorded.
func TestVerifyNotifications_TheOpenTrailCoversEveryDeliveredEvent(t *testing.T) {
	d0, d1, idx := openChain(t) // lists k1 (c1) and k2 (c2) of r1, from t0+1
	k1, k2, k3 := agent.ApprovalKey("r1", "c1", 0), agent.ApprovalKey("r1", "c2", 0), agent.ApprovalKey("r1", "c3", 0)
	for _, tc := range []struct {
		name string
		run  func() *trailBuilder
		want string // "" none
	}{
		{"listed events", func() *trailBuilder {
			return newTrail(t).complete(k1, agent.NotificationApprovalPending, 1).complete(k2, agent.NotificationApprovalPending, 5)
		}, ""},
		{"an event after the open trail began that was never listed", func() *trailBuilder {
			return newTrail(t).complete(k1, agent.NotificationApprovalPending, 1).complete(k3, agent.NotificationApprovalPending, 10)
		}, "never listed"},
		{"an event from before the open trail with its all-clear delivered", func() *trailBuilder {
			return newTrail(t).complete(k3, agent.NotificationApprovalPending, -7200).add(
				agent.NotifyRecord{Kind: agent.NotifyObserved, Key: agent.ResolvedKey(k3), At: at(-7100), EventKind: string(agent.NotificationResolved), EventAt: at(-7100), Reason: "decided"},
				claimed(agent.ResolvedKey(k3), -7099), acked(agent.ResolvedKey(k3), -7098, -7098))
		}, ""},
		{"an event from before the open trail, backfill recorded, never listed", func() *trailBuilder {
			return newTrail(t).complete(k3, agent.NotificationApprovalPending, -7200)
		}, "though the backfill is recorded"},
		{"an event cleared before any delivery needs no listing", func() *trailBuilder {
			return newTrail(t).add(observed(k3, agent.NotificationApprovalPending, 10, 11), cleared(k3, 12, "decided"))
		}, ""},
		{"a run_aborted event is not listed", func() *trailBuilder {
			return newTrail(t).complete(agent.AbortedKey("r1"), agent.NotificationRunAborted, 10)
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep, _ := checkTrails(t, tc.run().export(), d0, d1, idx)
			got := violationsOf(rep)
			if tc.want == "" && got != "" {
				t.Fatalf("flagged: %s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("violations = %q, want one containing %q", got, tc.want)
			}
		})
	}
}

// An event observed before the open trail needs the backfill: with none recorded it is flagged.
func TestVerifyNotifications_AnEventFromBeforeTheOpenTrailNeedsTheBackfill(t *testing.T) {
	k1, k3 := agent.ApprovalKey("r1", "c1", 0), agent.ApprovalKey("r1", "c3", 0)
	day0 := recsTrail(t, day(0), segRec(agent.NotifyCheckpoint, 0, ""), op(agent.NotifyWatch, k1, 1))
	idx := recsTrail(t, agent.OpenIndex(t0.Year()), segRec(agent.NotifySegment, 0, day(0)))
	run := newTrail(t).complete(k3, agent.NotificationApprovalPending, -7200).export()
	rep, _ := checkTrails(t, run, day0, idx)
	if !strings.Contains(violationsOf(rep), "no backfill is recorded") {
		t.Fatalf("violations = %q", violationsOf(rep))
	}
	withMark := recsTrail(t, day(0), segRec(agent.NotifyCheckpoint, 0, ""), op(agent.NotifyWatch, k1, 1), op(agent.NotifyWatch, k3, 2), agent.NotifyRecord{Kind: agent.NotifyBackfilled, At: at(3), BackfillWindow: time.Hour})
	rep, _ = checkTrails(t, run, withMark, idx)
	if v := violationsOf(rep); v != "" {
		t.Fatalf("flagged with the backfill recorded and the event listed: %s", v)
	}
}

// signedRecs is recsTrail with a signed head over it.
func signedRecs(t *testing.T, priv ed25519.PrivateKey, name string, recs ...agent.NotifyRecord) (audit.NotifyTrailExport, audit.SignedTreeHead) {
	t.Helper()
	es := audit.NewMemEventStore()
	tr := audit.NewNotifyTrail(es)
	for i, r := range recs {
		if err := tr.Append(context.Background(), name, i, r); err != nil {
			t.Fatal(err)
		}
	}
	x, err := audit.ExportNotifyTrail(context.Background(), es, name)
	if err != nil {
		t.Fatal(err)
	}
	h, err := audit.SignNotifyTrail(context.Background(), es, name, audit.Ed25519Signer{Priv: priv}, at(86500))
	if err != nil {
		t.Fatal(err)
	}
	return x, h
}

// Anchored (not -unanchored), the newest segment may not be missing: a segment an index or a seal
// names that is not given is a finding (a deleted newest segment), not a crash window; unanchored
// it stays allowed, since nothing there is attested anyway.
func TestVerifyNotifications_AnchoredAMissingSegmentIsAFinding(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	k1, k2 := agent.ApprovalKey("r1", "c1", 0), agent.ApprovalKey("r1", "c2", 0)
	d0, h0 := signedRecs(t, priv, day(0), segRec(agent.NotifyCheckpoint, 0, ""), op(agent.NotifyWatch, k1, 1), op(agent.NotifyWatch, k2, 2), agent.NotifyRecord{Kind: agent.NotifyBackfilled, At: at(4), BackfillWindow: time.Hour}, segRec(agent.NotifySeal, 86400, day(1)))
	d1, h1 := signedRecs(t, priv, day(1), agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: at(86400), Backfilled: true, Backfills: 1, BackfillAt: at(4), BackfillWindow: time.Hour,
		Watching: []agent.Watched{{Key: k1, RunID: "r1", At: at(1)}, {Key: k2, RunID: "r1", At: at(2)}}})
	idx, hi := signedRecs(t, priv, agent.OpenIndex(t0.Year()), segRec(agent.NotifySegment, 0, day(0)), segRec(agent.NotifySegment, 86400, day(1)))
	in := func(xs []audit.NotifyTrailExport, hs []audit.SignedTreeHead) (audit.NotifyReport, error) {
		return audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: xs, Heads: hs, Verifier: audit.Ed25519Verifier{Pub: pub}, Now: at(86600)})
	}
	if rep, err := in([]audit.NotifyTrailExport{d0, d1, idx}, []audit.SignedTreeHead{h0, h1, hi}); err != nil {
		t.Fatalf("all given and signed: %v\n%s", err, violationsOf(rep))
	}
	// The newest segment deleted (its head may still be around, or gone with it).
	for name, hs := range map[string][]audit.SignedTreeHead{"head kept": {h0, h1, hi}, "head gone": {h0, hi}} {
		rep, err := in([]audit.NotifyTrailExport{d0, idx}, hs)
		if err == nil || !strings.Contains(violationsOf(rep), day(1)) {
			t.Errorf("%s: the newest segment missing verifies: %v\n%s", name, err, violationsOf(rep))
		}
	}
	// Unanchored the crash window stays a window.
	if rep, _ := checkTrails(t, d0, idx); violationsOf(rep) != "" {
		t.Fatalf("unanchored: %s", violationsOf(rep))
	}
	_ = d1
}

// Truncation at the old end: the first segment and its index record deleted, the next segment's
// checkpoint emptied to match. Each trail's signed head still commits to what it had, so the
// deletions are found (a first segment with an empty checkpoint is not accepted on its word).
func TestVerifyNotifications_AnchoredTheOldestSegmentDeletedIsFound(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	k1 := agent.ApprovalKey("r1", "c1", 0)
	_, h0 := signedRecs(t, priv, day(0), segRec(agent.NotifyCheckpoint, 0, ""), op(agent.NotifyWatch, k1, 1), segRec(agent.NotifySeal, 86400, day(1)))
	_, h1 := signedRecs(t, priv, day(1), agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: at(86400), Watching: []agent.Watched{{Key: k1, RunID: "r1", At: at(1)}}})
	_, hi := signedRecs(t, priv, agent.OpenIndex(t0.Year()), segRec(agent.NotifySegment, 0, day(0)), segRec(agent.NotifySegment, 86400, day(1)))
	// What the attacker leaves: day 1 with an empty checkpoint, and an index without day 0.
	d1cut, _ := signedRecs(t, priv, day(1), segRec(agent.NotifyCheckpoint, 86400, ""))
	idxcut, _ := signedRecs(t, priv, agent.OpenIndex(t0.Year()), segRec(agent.NotifySegment, 86400, day(1)))
	rep, err := audit.VerifyNotifications(audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{d1cut, idxcut},
		Heads: []audit.SignedTreeHead{h0, h1, hi}, Verifier: audit.Ed25519Verifier{Pub: pub}, Now: at(86600)})
	if err == nil {
		t.Fatalf("a deleted oldest segment verifies:\n%s", violationsOf(rep))
	}
	v := violationsOf(rep)
	for _, want := range []string{day(0), day(1), agent.OpenIndex(t0.Year())} {
		if !strings.Contains(v, want) {
			t.Errorf("no finding names %s:\n%s", want, v)
		}
	}
}

func TestVerifyNotifications_AClockAtOnARunTrailRecordIsAViolation(t *testing.T) {
	k1 := agent.ApprovalKey("r1", "c1", 0)
	b := newTrail(t)
	obs := observed(k1, agent.NotificationApprovalPending, 1, 2)
	obs.ClockAt = at(1)
	b.add(obs, claimed(k1, 3), acked(k1, 4, 4))
	rep, _ := checkTrails(t, b.export())
	if !strings.Contains(violationsOf(rep), "clock_at") {
		t.Fatalf("violations = %q", violationsOf(rep))
	}
}

// The two backfill passes of a rolling upgrade are carried across segments, and verify; the
// second no earlier than the window the first recorded.
func TestVerifyNotifications_TwoBackfillPassesAreCarriedAcrossASegment(t *testing.T) {
	k1 := agent.ApprovalKey("r1", "c1", 0)
	const window = 3 * time.Hour
	mark := func(sec int, w time.Duration) agent.NotifyRecord {
		return agent.NotifyRecord{Kind: agent.NotifyBackfilled, At: at(sec), BackfillWindow: w}
	}
	build := func(second agent.NotifyRecord, firstW time.Duration) []audit.NotifyTrailExport {
		day0 := recsTrail(t, day(0), segRec(agent.NotifyCheckpoint, 0, ""), op(agent.NotifyWatch, k1, 1), mark(2, firstW), second, segRec(agent.NotifySeal, 86400, day(1)))
		day1 := recsTrail(t, day(1), agent.NotifyRecord{Kind: agent.NotifyCheckpoint, At: at(86400), Backfilled: true, Backfills: 2, BackfillAt: second.At, BackfillWindow: firstW,
			Watching: []agent.Watched{{Key: k1, RunID: "r1", At: at(1)}}})
		idx := recsTrail(t, agent.OpenIndex(t0.Year()), segRec(agent.NotifySegment, 0, day(0)), segRec(agent.NotifySegment, 86400, day(1)))
		return []audit.NotifyTrailExport{day0, day1, idx}
	}
	for _, tc := range []struct {
		name   string
		second agent.NotifyRecord
		firstW time.Duration
		want   string
	}{
		{"a second pass after the window", mark(2+int(window/time.Second)+1, window), window, ""},
		{"a second pass at the window", mark(2+int(window/time.Second), window), window, ""},
		{"a second pass inside the window", mark(2+int(window/time.Second)-3600, window), window, "inside the window"},
		{"a first pass that records no window", mark(14*3600, 0), 0, "records no window"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep, _ := checkTrails(t, build(tc.second, tc.firstW)...)
			v := violationsOf(rep)
			if tc.want == "" && v != "" {
				t.Fatalf("flagged: %s", v)
			}
			if tc.want != "" && !strings.Contains(v, tc.want) {
				t.Fatalf("violations = %q, want %q", v, tc.want)
			}
		})
	}
}
