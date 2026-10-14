package audit

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// The open trail lists the events whose all-clear may still be owed, so a reader need not scan every
// run's trail. It is a chain of daily segments (agent.OpenSegment) joined by seals, and a yearly
// index (agent.OpenIndex) of the segments in creation order. The functions here check that
// structure across the segments and indexes given together.

type openKind int

const (
	openNone openKind = iota // a run's trail (or the digest trail)
	openSegment
	openIndex
)

// openTrailKind says which part of the open trail name is, if any.
func openTrailKind(name string) openKind {
	rest, ok := strings.CutPrefix(name, agent.NotifyOpenTrail+":")
	switch {
	case !ok:
		return openNone
	case strings.HasPrefix(rest, "index:"):
		return openIndex
	default:
		return openSegment
	}
}

// validSegmentName reports whether name is agent.OpenSegment of some UTC day.
func validSegmentName(name string) bool {
	if openTrailKind(name) != openSegment {
		return false
	}
	t, err := time.Parse("2006-01-02", strings.TrimPrefix(name, agent.NotifyOpenTrail+":"))
	return err == nil && agent.OpenSegment(t) == name
}

// segmentDay is the UTC day a segment is named for.
func segmentDay(name string) time.Time {
	t, _ := time.Parse("2006-01-02", strings.TrimPrefix(name, agent.NotifyOpenTrail+":"))
	return t
}

// inDay reports whether t is in the UTC day starting at start, give or take skew (the writers' clocks
// differ).
func inDay(t, start time.Time, skew time.Duration) bool {
	return !t.Before(start.Add(-skew)) && t.Before(start.Add(24*time.Hour+skew))
}

// checkOpenGroup returns, by trail name, the ways the open-trail segments and indexes in trails
// (the decoded records of every trail given) are not what a notifier writes. Segments are checked
// for their own shape and as a chain: each sealed segment is continued by a segment whose checkpoint
// is exactly what the sealed one folds to (agent.FoldOpen), and only the first segment starts
// without one. Indexes are checked as the creation order of the segments.
func checkOpenGroup(trails map[string][]agent.NotifyRecord, skew time.Duration, anchored bool) map[string][]string {
	out := map[string][]string{}
	add := func(name string, i int, recs []agent.NotifyRecord, format string, args ...any) {
		where := "trail"
		if i >= 0 {
			where = fmt.Sprintf("record %d (%s)", i, recs[i].Kind)
		}
		out[name] = append(out[name], where+": "+fmt.Sprintf(format, args...))
	}
	var segs, idxs []string
	for name := range trails {
		switch openTrailKind(name) {
		case openSegment:
			segs = append(segs, name)
		case openIndex:
			idxs = append(idxs, name)
		}
	}
	slices.Sort(segs)
	slices.Sort(idxs)
	if len(segs) == 0 && len(idxs) == 0 {
		return out
	}

	for _, name := range segs {
		recs := trails[name]
		if !validSegmentName(name) {
			add(name, -1, recs, "%q is not the name of an open-trail segment (a UTC day)", name)
		}
		listed := map[string]bool{}
		day := segmentDay(name)
		for i, r := range recs {
			if !r.ClockAt.IsZero() && !r.ClockAt.Before(r.At) {
				add(name, i, recs, "keeps the writer's clock reading %s, which is not earlier than its time %s: a time is only moved later", r.ClockAt.UTC().Format(time.RFC3339), r.At.UTC().Format(time.RFC3339))
			}
			if r.At.IsZero() {
				add(name, i, recs, "has no time")
			} else if validSegmentName(name) && r.Kind != agent.NotifySeal && !inDay(r.At, day, skew) {
				add(name, i, recs, "is dated %s, outside the segment's day (%s) by more than %s", r.At.UTC().Format(time.RFC3339), day.Format("2006-01-02"), skew)
			}
			if i > 0 && r.At.Before(recs[i-1].At.Add(-skew)) {
				add(name, i, recs, "is dated more than %s before record %d: the segment is out of order, or a clock is wrong", skew, i-1)
			}
			switch r.Kind {
			case agent.NotifyCheckpoint:
				if i != 0 {
					add(name, i, recs, "a checkpoint is only the first record of a segment")
					continue
				}
				for _, w := range r.Watching {
					if w.Key == "" || w.RunID == "" {
						add(name, i, recs, "lists an event with no key or run")
					}
					if listed[w.Key] {
						add(name, i, recs, "lists event %q twice", w.Key)
					}
					listed[w.Key] = true
				}
			case agent.NotifyWatch, agent.NotifyUnwatch:
				if r.Key == "" || r.RunID == "" {
					add(name, i, recs, "names no event key or run")
					continue
				}
				if r.Kind == agent.NotifyWatch {
					listed[r.Key] = true
				} else if !listed[r.Key] {
					add(name, i, recs, "unwatches event %q, which is not listed", r.Key)
				} else {
					delete(listed, r.Key)
				}
			case agent.NotifyBackfilled:
			case agent.NotifySeal:
				if i != len(recs)-1 {
					add(name, i, recs, "a record follows the seal")
				}
				if !validSegmentName(r.Next) || r.Next <= name {
					add(name, i, recs, "seals into %q, which is not a later segment", r.Next)
				} else {
					if !r.At.IsZero() && !inDay(r.At, segmentDay(r.Next), skew) {
						add(name, i, recs, "is dated %s, outside the day of the segment it seals into (%s) by more than %s: the roller writes the seal on the day it rolls into", r.At.UTC().Format(time.RFC3339), segmentDay(r.Next).Format("2006-01-02"), skew)
					}
					// Days may be skipped (nobody ran on them) but not over a segment an index lists
					// (checked below); one that exists and no index lists is a broken chain.
				}
			default:
				add(name, i, recs, "a %s record does not belong in an open-trail segment", r.Kind)
			}
		}
	}
	// The backfill passes in order across the segments: the first records the window, the second is
	// no sooner than the first plus that window, and the checkpoints carry both.
	var passes int
	var firstAt time.Time
	var window time.Duration
	for _, name := range segs {
		for i, r := range trails[name] {
			switch r.Kind {
			case agent.NotifyCheckpoint:
				if i == 0 && r.Backfills > passes {
					passes = r.Backfills
				}
				if i == 0 && window == 0 {
					window = r.BackfillWindow
				}
			case agent.NotifyBackfilled:
				if passes == 0 {
					if r.BackfillWindow <= 0 {
						add(name, i, trails[name], "the first backfill pass records no window: the second pass cannot be held to one")
					}
					window, firstAt = r.BackfillWindow, r.At
				} else if !firstAt.IsZero() && r.At.Before(firstAt.Add(window).Add(-skew)) {
					add(name, i, trails[name], "the second backfill pass is dated %s, inside the window (%s) the first (%s) recorded", r.At.UTC().Format(time.RFC3339), window, firstAt.UTC().Format(time.RFC3339))
				}
				passes++
			}
		}
	}
	// A backfill is done at most twice: count the records that make it, not the checkpoints that carry it.
	own := 0
	for _, name := range segs {
		for _, r := range trails[name] {
			if r.Kind == agent.NotifyBackfilled {
				own++
			}
		}
	}
	if own > 2 {
		add(segs[0], -1, nil, "the backfill is recorded %d times, at most two passes are allowed (the second a day after the first)", own)
	}

	// The chain: a sealed segment's successor starts with a checkpoint equal to what it folds to.
	targets := map[string]bool{}
	for _, name := range segs {
		st := agent.FoldOpen(trails[name])
		if st.Sealed == "" {
			continue
		}
		targets[st.Sealed] = true
		next, ok := trails[st.Sealed]
		if !ok || len(next) == 0 {
			continue // created next, and not yet written: a crash between the seal and the checkpoint
		}
		cp := next[0]
		if cp.Kind != agent.NotifyCheckpoint {
			add(st.Sealed, 0, next, "the segment continues %s and does not start with a checkpoint", name)
			continue
		}
		if cp.Backfilled != st.Backfilled {
			add(st.Sealed, 0, next, "backfilled is %v, and %s folds to %v", cp.Backfilled, name, st.Backfilled)
		}
		if cp.BackfillWindow != st.BackfillWindow {
			add(st.Sealed, 0, next, "the checkpoint carries backfill window %s, and %s folds to %s", cp.BackfillWindow, name, st.BackfillWindow)
		}
		if n := max(cp.Backfills, b2i(cp.Backfilled)); n != st.Backfills || !cp.BackfillAt.Equal(st.BackfillAt) {
			add(st.Sealed, 0, next, "the checkpoint carries %d backfill pass(es), last at %s, and %s folds to %d, last at %s", n, cp.BackfillAt.UTC().Format(time.RFC3339), name, st.Backfills, st.BackfillAt.UTC().Format(time.RFC3339))
		}
		if !sameWatched(cp.Watching, st.Watched) {
			add(st.Sealed, 0, next, "the checkpoint does not equal the events %s lists (%d listed, %d in the checkpoint)", name, len(st.Watched), len(cp.Watching))
		}
	}
	first := true
	for _, name := range segs {
		if targets[name] || len(trails[name]) == 0 {
			continue
		}
		if !first {
			add(name, -1, nil, "no segment seals into it, and it is not the first segment: the chain is broken")
		} else if r := trails[name][0]; r.Kind == agent.NotifyCheckpoint && (len(r.Watching) > 0 || r.Backfilled) {
			add(name, 0, trails[name], "the first segment's checkpoint lists events carried from a segment that is not here")
		}
		first = false
	}

	// Anchored, nothing may be missing: a segment named by an index or a seal that is not given is a
	// segment deleted (the newest too: a writer that died before creating it is indistinguishable,
	// and a sweep repairs it, so the audit is run again). Unanchored nothing attests the trails
	// anyway, so the newest segment may be missing, as the crash window allows.
	if anchored {
		named := map[string]string{} // segment -> who names it
		for _, name := range idxs {
			for _, r := range trails[name] {
				if r.Kind == agent.NotifySegment && validSegmentName(r.Next) {
					named[r.Next] = name
				}
			}
		}
		for _, name := range segs {
			if next := agent.FoldOpen(trails[name]).Sealed; next != "" && validSegmentName(next) {
				if _, ok := named[next]; !ok {
					named[next] = name
				}
			}
		}
		for seg, by := range named {
			if _, ok := trails[seg]; !ok {
				add(by, -1, nil, "%s is named by this trail and is not among the trails given: a segment deleted, or one a writer has not created yet (a sweep repairs that: run the audit again)", seg)
			}
		}
	}

	// A seal may not skip a segment an index lists, even one that is missing from the trails given.
	for _, name := range idxs {
		for _, r := range trails[name] {
			if r.Kind != agent.NotifySegment || !validSegmentName(r.Next) {
				continue
			}
			for _, sname := range segs {
				if st := agent.FoldOpen(trails[sname]); st.Sealed != "" && r.Next > sname && r.Next < st.Sealed {
					add(sname, -1, nil, "seals into %s, skipping %s, which an index lists", st.Sealed, r.Next)
				}
			}
		}
	}

	// The indexes: one segment record each, in creation order; each segment named exists, or is the
	// first or the last created.
	var indexed []string
	var lastIndexed string
	for _, name := range idxs {
		recs := trails[name]
		for i, r := range recs {
			if r.At.IsZero() {
				add(name, i, recs, "has no time")
			}
			if !r.ClockAt.IsZero() && !r.ClockAt.Before(r.At) {
				add(name, i, recs, "keeps the writer's clock reading %s, which is not earlier than its time", r.ClockAt.UTC().Format(time.RFC3339))
			}
			if i > 0 && r.At.Before(recs[i-1].At.Add(-skew)) {
				add(name, i, recs, "is dated more than %s before record %d", skew, i-1)
			}
			if r.Kind != agent.NotifySegment {
				add(name, i, recs, "a %s record does not belong in an index", r.Kind)
				continue
			}
			if !validSegmentName(r.Next) {
				add(name, i, recs, "names %q, which is not a segment", r.Next)
				continue
			}
			if want := agent.OpenIndex(segmentDay(r.Next).Year()); want != name {
				add(name, i, recs, "names %s, a segment of the year %d: it belongs in %s", r.Next, segmentDay(r.Next).Year(), want)
			}
			if !r.At.IsZero() && r.At.Before(segmentDay(r.Next).Add(-skew)) {
				add(name, i, recs, "is dated %s, before the day of the segment it names (%s): a segment is indexed when it is created", r.At.UTC().Format(time.RFC3339), r.Next)
			}
			if lastIndexed != "" && r.Next <= lastIndexed {
				add(name, i, recs, "names %q after %q: segments are indexed in creation order", r.Next, lastIndexed)
			}
			lastIndexed = r.Next
			indexed = append(indexed, r.Next)
		}
	}
	if len(idxs) > 0 {
		for n, seg := range indexed {
			if len(trails[seg]) == 0 && n != 0 && n != len(indexed)-1 {
				add(idxs[0], -1, nil, "segment %s is indexed and is missing or empty, and is neither the first nor the last created", seg)
			}
		}
	}
	if len(idxs) > 0 || len(segs) > 0 {
		for n, seg := range segs {
			if len(trails[seg]) > 0 && !slices.Contains(indexed, seg) && n != len(segs)-1 {
				add(seg, -1, nil, "the segment is in no index and is not the newest")
			}
		}
	}
	return out
}

// openListing is what the open trail lists across the segments given: the event keys ever listed
// (a watch record, or a checkpoint's set), whether a backfill is recorded, when the open trail began,
// and whether there is one at all.
type openListing struct {
	any        bool
	keys       map[string]bool
	backfilled bool
	began      time.Time
}

func listingOf(trails map[string][]agent.NotifyRecord) openListing {
	l := openListing{keys: map[string]bool{}}
	for name, recs := range trails {
		if openTrailKind(name) != openSegment {
			continue
		}
		l.any = true
		for _, r := range recs {
			if !r.At.IsZero() && (l.began.IsZero() || r.At.Before(l.began)) {
				l.began = r.At
			}
			switch r.Kind {
			case agent.NotifyWatch:
				l.keys[r.Key] = true
			case agent.NotifyCheckpoint:
				for _, w := range r.Watching {
					l.keys[w.Key] = true
				}
				l.backfilled = l.backfilled || r.Backfilled
			case agent.NotifyBackfilled:
				l.backfilled = true
			}
		}
	}
	return l
}

// checkOpenCoverage returns, by run trail, the delivered events that can owe an all-clear and are
// not on the open trail, when one is given. The notifier lists an event before it observes it, so
// an event observed after the open trail began that is not listed was dropped from it; one observed
// earlier is on it only through the backfill, which must then be recorded. An event cleared before
// any delivery was attempted owes nothing.
func checkOpenCoverage(trails map[string][]agent.NotifyRecord, skew time.Duration) map[string][]string {
	l := listingOf(trails)
	out := map[string][]string{}
	if !l.any {
		return out
	}
	for name, recs := range trails {
		if openTrailKind(name) != openNone || strings.HasPrefix(name, agent.NotifyTrailName("@")) {
			continue
		}
		st := agent.FoldNotify(recs)
		for _, key := range st.Keys {
			es := st.Events[key]
			switch agent.NotificationKind(es.Kind) {
			case agent.NotificationOutcomeUnknown, agent.NotificationApprovalPending, agent.NotificationRecoveryFailing:
			default:
				continue
			}
			if es.Cleared != nil && es.Rounds == 0 || l.keys[key] {
				continue
			}
			if r := st.Events[agent.ResolvedKey(key)]; r != nil && r.Acked != nil {
				continue // the all-clear was delivered: the backfill, like the engine, owes it nothing
			}
			switch {
			case !es.ObservedAt.Before(l.began.Add(-skew)):
				out[name] = append(out[name], fmt.Sprintf("event %s was observed after the open trail began and was never listed on it: its all-clear could be missed", key))
			case !l.backfilled:
				out[name] = append(out[name], fmt.Sprintf("event %s was observed before the open trail and is not listed on it, and no backfill is recorded", key))
			default:
				out[name] = append(out[name], fmt.Sprintf("event %s was observed before the open trail and is not listed on it, though the backfill is recorded", key))
			}
		}
	}
	return out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sameWatched(a, b []agent.Watched) bool {
	return slices.EqualFunc(a, b, func(x, y agent.Watched) bool {
		return x.Key == y.Key && x.RunID == y.RunID && x.At.Equal(y.At)
	})
}
