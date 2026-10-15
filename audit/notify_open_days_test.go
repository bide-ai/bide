package audit

import (
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

func odAt(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }

func odSeg(day int) string { return agent.OpenSegment(odAt(day, 0)) }

// odTrails is a valid chain of three daily segments with one event listed from day 13 on.
func odTrails() map[string][]agent.NotifyRecord {
	w := agent.Watched{Key: "k1", RunID: "r1", At: odAt(13, 1)}
	return map[string][]agent.NotifyRecord{
		agent.OpenIndex(2026): {
			{Kind: agent.NotifySegment, Next: odSeg(13), At: odAt(13, 0)},
			{Kind: agent.NotifySegment, Next: odSeg(14), At: odAt(14, 0)},
			{Kind: agent.NotifySegment, Next: odSeg(15), At: odAt(15, 0)},
		},
		odSeg(13): {
			{Kind: agent.NotifyCheckpoint, At: odAt(13, 0)},
			{Kind: agent.NotifyWatch, Key: "k1", RunID: "r1", At: odAt(13, 1)},
			{Kind: agent.NotifySeal, Next: odSeg(14), At: odAt(14, 0)},
		},
		odSeg(14): {
			{Kind: agent.NotifyCheckpoint, At: odAt(14, 0), Watching: []agent.Watched{w}},
			{Kind: agent.NotifySeal, Next: odSeg(15), At: odAt(15, 0)},
		},
		odSeg(15): {
			{Kind: agent.NotifyCheckpoint, At: odAt(15, 0), Watching: []agent.Watched{w}},
		},
	}
}

func problems(m map[string][]string) string {
	var b strings.Builder
	for k, v := range m {
		for _, s := range v {
			b.WriteString(k + ": " + s + "\n")
		}
	}
	return b.String()
}

func TestOpenDays_AValidChainHasNoProblems(t *testing.T) {
	if p := checkOpenGroup(odTrails(), time.Minute, false); len(p) != 0 {
		t.Fatal(problems(p))
	}
}

// A record is dated inside its segment's UTC day (within the clock skew); one months away is not
// from this notifier.
func TestOpenDays_ARecordOutsideItsSegmentsDayIsFlagged(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string][]agent.NotifyRecord)
	}{
		{"a watch months late", func(tr map[string][]agent.NotifyRecord) { tr[odSeg(13)][1].At = odAt(13, 1).AddDate(0, 3, 0) }},
		{"a checkpoint a day early", func(tr map[string][]agent.NotifyRecord) { tr[odSeg(14)][0].At = odAt(13, 12) }},
		{"a checkpoint a day late", func(tr map[string][]agent.NotifyRecord) {
			tr[odSeg(15)][0].At = odAt(16, 12)
		}},
		{"an index record dated before its segment's day", func(tr map[string][]agent.NotifyRecord) {
			tr[agent.OpenIndex(2026)][2].At = odAt(14, 12)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := odTrails()
			tc.edit(tr)
			if p := checkOpenGroup(tr, time.Minute, false); len(p) == 0 {
				t.Fatal("verifies")
			}
		})
	}
}

// Skew is tolerated at the day's edges, not beyond.
func TestOpenDays_TheDayEdgesAllowTheClockSkew(t *testing.T) {
	tr := odTrails()
	tr[odSeg(14)][0].At = odAt(14, 0).Add(-30 * time.Second)
	if p := checkOpenGroup(tr, time.Minute, false); len(p) != 0 {
		t.Fatalf("30s before the day with a minute of skew: %s", problems(p))
	}
	tr[odSeg(14)][0].At = odAt(14, 0).Add(-2 * time.Minute)
	if p := checkOpenGroup(tr, time.Minute, false); len(p) == 0 {
		t.Fatal("two minutes before the day with a minute of skew verifies")
	}
}

// A seal's time is the roller's, in the day it rolls into.
func TestOpenDays_ASealIsDatedInTheDayItSealsInto(t *testing.T) {
	tr := odTrails()
	tr[odSeg(13)][2].At = odAt(13, 23) // before the next segment's day began
	if p := checkOpenGroup(tr, time.Minute, false); len(p) == 0 {
		t.Fatal("a seal dated before the day it rolls into verifies")
	}
	tr = odTrails()
	tr[odSeg(13)][2].At = odAt(20, 0)
	if p := checkOpenGroup(tr, time.Minute, false); len(p) == 0 {
		t.Fatal("a seal dated days after the day it rolls into verifies")
	}
}

// A seal skipping days is allowed only over days with no segment (an idle gap) and no index entry;
// jumping over a segment that exists (or that the index lists) hides it.
func TestOpenDays_ASealMayNotSkipASegmentThatExists(t *testing.T) {
	tr := odTrails()
	tr[odSeg(13)][2].Next = odSeg(15)
	tr[odSeg(13)][2].At = odAt(15, 0)
	if p := checkOpenGroup(tr, time.Minute, false); len(p) == 0 {
		t.Fatal("a seal from day 13 to 15 over a segment for day 14 verifies")
	}
	// With day 14 absent from the trails and the index, the gap is an idle one.
	tr = odTrails()
	delete(tr, odSeg(14))
	tr[agent.OpenIndex(2026)] = []agent.NotifyRecord{tr[agent.OpenIndex(2026)][0], tr[agent.OpenIndex(2026)][2]}
	tr[odSeg(13)][2].Next = odSeg(15)
	tr[odSeg(13)][2].At = odAt(15, 0)
	if p := checkOpenGroup(tr, time.Minute, false); len(p) != 0 {
		t.Fatalf("an idle gap: %s", problems(p))
	}
	// But the index still naming day 14 says it was created.
	tr[agent.OpenIndex(2026)] = odTrails()[agent.OpenIndex(2026)]
	if p := checkOpenGroup(tr, time.Minute, false); len(p) == 0 {
		t.Fatal("a seal that skips a segment the index lists verifies")
	}
}

// A yearly index names segments of its own year.
func TestOpenDays_AnIndexNamesSegmentsOfItsYear(t *testing.T) {
	tr := odTrails()
	tr[agent.OpenIndex(2025)] = []agent.NotifyRecord{{Kind: agent.NotifySegment, Next: odSeg(12), At: odAt(12, 0)}}
	if p := checkOpenGroup(tr, time.Minute, false); len(p) == 0 {
		t.Fatal("the index of 2025 names a segment of 2026 and verifies")
	}
}

// clock_at is the writer's own clock reading, kept when the record's time was moved later to fit
// its segment: it must be earlier than the time, and only the open trail's records carry it.
func TestOpenDays_ClockAtIsEarlierThanAtAndOnlyOnOpenRecords(t *testing.T) {
	tr := odTrails()
	tr[odSeg(14)][1].ClockAt = odAt(14, 0).Add(-time.Hour) // a seal moved forward: fine
	if p := checkOpenGroup(tr, time.Minute, false); len(p) != 0 {
		t.Fatalf("a clock_at before at: %s", problems(p))
	}
	tr[odSeg(14)][1].ClockAt = tr[odSeg(14)][1].At.Add(time.Hour)
	if p := checkOpenGroup(tr, time.Minute, false); len(p) == 0 {
		t.Fatal("a clock_at after at verifies: a record cannot be moved earlier")
	}
}
