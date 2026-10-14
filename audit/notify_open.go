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

// checkOpenGroup returns, by trail name, the ways the open-trail segments and indexes in trails
// (the decoded records of every trail given) are not what a notifier writes. Segments are checked
// for their own shape and as a chain: each sealed segment is continued by a segment whose checkpoint
// is exactly what the sealed one folds to (agent.FoldOpen), and only the first segment starts
// without one. Indexes are checked as the creation order of the segments.
func checkOpenGroup(trails map[string][]agent.NotifyRecord, skew time.Duration) map[string][]string {
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
		for i, r := range recs {
			if r.At.IsZero() {
				add(name, i, recs, "has no time")
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
				}
			default:
				add(name, i, recs, "a %s record does not belong in an open-trail segment", r.Kind)
			}
		}
	}
	// A backfill is done once: count the records that make it, not the checkpoints that carry it.
	own := 0
	for _, name := range segs {
		for _, r := range trails[name] {
			if r.Kind == agent.NotifyBackfilled {
				own++
			}
		}
	}
	if own > 1 {
		add(segs[0], -1, nil, "the backfill is recorded %d times, once is allowed", own)
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

func sameWatched(a, b []agent.Watched) bool {
	return slices.EqualFunc(a, b, func(x, y agent.Watched) bool {
		return x.Key == y.Key && x.RunID == y.RunID && x.At.Equal(y.At)
	})
}
