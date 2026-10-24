package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The marks a session carries from turn to turn (docs/design/untrusted-content-and-amount-caps.md,
// section 5.5). A session keeps the questions and answers of its turns, so the answer of a turn that ended
// tainted is how hostile content reaches the next turn in the assistant's own voice. A turn that ended
// with marks records them in the session journal, "taint/turn/<n>", and every later turn starts with them
// as inherited marks. Taint is sticky: each record holds the union of all the marks so far, and the
// highest n is the session's.

const sessionTaintTurnPrefix = "taint/turn/"

func sessionTaintTurnStep(n int) string { return sessionTaintTurnPrefix + strconv.Itoa(n) }

// sessionTaintRecord is what a taint/turn/<n> record holds.
type sessionTaintRecord struct {
	Marks []inheritedMark `json:"marks"`
}

// latestSessionTaint is the highest-numbered taint/turn/<n> record of the session journal recs, its n and
// its marks; n is -1 when there is none. The number is parsed: taint/turn/10 is above taint/turn/9
// whatever order the records are listed in. A record whose name or content does not read is an error: the
// session's taint cannot be known, and a turn is not started on a guess.
func latestSessionTaint(recs []Record) (int, []inheritedMark, error) {
	hi, marks := -1, []inheritedMark(nil)
	for _, r := range recs {
		if r.Kind != StepValue || !strings.HasPrefix(r.Name, sessionTaintTurnPrefix) {
			continue
		}
		suffix := strings.TrimPrefix(r.Name, sessionTaintTurnPrefix)
		n, err := strconv.Atoi(suffix)
		if err != nil || n < 0 || strconv.Itoa(n) != suffix {
			return 0, nil, fmt.Errorf("session record %q is not a taint record: %w", r.Name, ErrProtocol)
		}
		var rec sessionTaintRecord
		if err := json.Unmarshal(r.Result, &rec); err != nil {
			return 0, nil, fmt.Errorf("session record %q does not read: %w (%w)", r.Name, err, ErrProtocol)
		}
		if n > hi {
			hi, marks = n, rec.Marks
		}
	}
	return hi, marks, nil
}

// sessionMarks is the marks the session's turns start with: those of its highest taint record.
func (s *Session) sessionMarks(ctx context.Context) ([]inheritedMark, error) {
	recs, err := s.agent.store.History(ctx, sessionJournalID(s.id))
	if err != nil {
		return nil, fmt.Errorf("session %s: load the session journal: %w (%w)", s.id, err, ErrStorage)
	}
	_, marks, err := latestSessionTaint(recs)
	if err != nil {
		return nil, fmt.Errorf("session %s: %w", s.id, err)
	}
	return marks, nil
}

// sameSources reports whether every source of a is in b.
func sourcesWithin(a, b []inheritedMark) bool {
	for _, m := range a {
		if !slices.ContainsFunc(b, func(o inheritedMark) bool { return o.Source == m.Source }) {
			return false
		}
	}
	return true
}

// recordTurnTaint records the marks the completed turn run runID ended with, as the union with the
// session's so far, at the first free number at or above the number of turns the run was seeded with and
// above the highest record. Nothing is written for a turn that ended with no marks (no turn before it
// had any either, or it would have inherited them), and nothing when the highest record already holds
// every source: a retry writes the same.
func (s *Session) recordTurnTaint(ctx context.Context, runID string) error {
	own, under, err := s.agent.endedMarks(ctx, runID)
	if err != nil || len(own) == 0 || !under {
		return err // a turn of a session that is not under taint is not retro-marked when a policy comes
	}
	j := sessionJournalID(s.id)
	var from turnFrom
	if rec, ok, err := s.agent.store.Get(ctx, j, sessionFromStep(runID)); err != nil {
		return fmt.Errorf("session %s: read %s: %w (%w)", s.id, sessionFromStep(runID), err, ErrStorage)
	} else if ok {
		if err := json.Unmarshal(rec.Result, &from); err != nil {
			return fmt.Errorf("session %s: decode %s: %w (%w)", s.id, sessionFromStep(runID), err, ErrProtocol)
		}
	}
	for range maxSessionTaintTries {
		recs, err := s.agent.store.History(ctx, j)
		if err != nil {
			return fmt.Errorf("session %s: load the session journal: %w (%w)", s.id, err, ErrStorage)
		}
		hi, have, err := latestSessionTaint(recs)
		if err != nil {
			return fmt.Errorf("session %s: %w", s.id, err)
		}
		if sourcesWithin(own, have) {
			return nil
		}
		union := slices.Clone(have)
		for _, m := range own {
			if !slices.ContainsFunc(union, func(o inheritedMark) bool { return o.Source == m.Source }) {
				union = append(union, m)
			}
		}
		slices.SortFunc(union, func(a, b inheritedMark) int { return strings.Compare(a.Source, b.Source) })
		b, err := marshalJournal(sessionTaintRecord{Marks: union})
		if err != nil {
			return fmt.Errorf("session %s: encode a taint record: %w (%w)", s.id, err, ErrConfig)
		}
		n := max(from.Turns, hi+1)
		got, inserted, err := s.agent.store.putNew(ctx, j, sessionTaintTurnStep(n), Record{Kind: StepValue, Result: b})
		if err != nil {
			return fmt.Errorf("session %s: record %s: %w", s.id, sessionTaintTurnStep(n), err)
		}
		if inserted {
			return nil
		}
		_ = got // another turn took the number: read the journal again and try above it
	}
	return fmt.Errorf("session %s: no free taint record number after %d tries: %w", s.id, maxSessionTaintTries, ErrStorage)
}

// maxSessionTaintTries bounds the turns that race to record at once; each try finds the number a
// concurrent turn took, so a handful is far more than needed.
const maxSessionTaintTries = 64
