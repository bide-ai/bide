package runview

import (
	"context"
	"iter"
	"net/http"
	"strconv"

	"github.com/bide-ai/bide/agent"
)

// Live mode: the page of a run with no end marker, read from a store, follows the run. Its script
// (assets/live.js) polls GET /run/live?id=<run>&after=<cursor> about once a second; the handler
// answers from one Load of the records after the cursor, and only when there are some does it read
// the run again. The page then fetches itself and swaps in its new content, keeping the scroll
// position. An exported journal is static: its page has no live indicator and no script.

// Follower is a Source whose runs can grow while the viewer reads them: a store, not an export.
// JournalSource is one.
type Follower interface {
	// Grew reports whether runID has a record after the store sequence after (agent.Entry.Seq).
	// It is the read live mode polls, so it is cheap: one Load from after, stopped at its first entry.
	Grew(ctx context.Context, runID string, after int64) (bool, error)
	// HistorySeq is History with each record's store sequence, both read in one pass.
	HistorySeq(ctx context.Context, runID string) ([]agent.Record, []int64, error)
}

// Grew implements Follower.
func (s JournalSource) Grew(ctx context.Context, runID string, after int64) (bool, error) {
	for _, err := range s.J.Store().Load(ctx, runID, after) {
		return err == nil, err
	}
	return false, nil
}

// HistorySeq implements Follower. It reads the run through a Journal over the same store, so each
// record is decoded and checked as History does, and keeps the Seq of each entry the store yields.
func (s JournalSource) HistorySeq(ctx context.Context, runID string) ([]agent.Record, []int64, error) {
	st := &seqStore{Store: s.J.Store()}
	j, err := agent.NewJournal(st)
	if err != nil {
		return nil, nil, err
	}
	recs, err := j.History(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	return recs, st.seqs, nil
}

// seqStore keeps the Seq of every entry Load yields, in order: Journal.History decodes one record
// per entry, so seqs[i] is the sequence of the i-th record.
type seqStore struct {
	agent.Store
	seqs []int64
}

func (s *seqStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) {
		for e, err := range s.Store.Load(ctx, runID, after) {
			if err == nil {
				s.seqs = append(s.seqs, e.Seq)
			}
			if !yield(e, err) {
				return
			}
		}
	}
}

// LiveUpdate is GET /run/live's answer: whether the run has records after the cursor the page
// gave, and, when it has, the timeline rows of those records and the run's state now.
type LiveUpdate struct {
	Changed bool `json:"changed"`
	// Live: the run has no end marker yet, so the page polls again; false once it has one.
	Live bool `json:"live"`
	// Cursor is the store sequence of the run's last record: the after of the next poll.
	Cursor    int64   `json:"cursor"`
	State     string  `json:"state,omitempty"`
	StateText string  `json:"state_text,omitempty"`
	Events    []Event `json:"events,omitempty"` // the rows of the records after the cursor given, only
}

// history reads the run's records, with their store sequences when the source is a Follower.
func (o Options) history(ctx context.Context, runID string) ([]agent.Record, []int64, error) {
	if f, ok := o.Source.(Follower); ok {
		return f.HistorySeq(ctx, runID)
	}
	recs, err := o.Source.History(ctx, runID)
	return recs, nil, err
}

// follow marks v live when it was read with store sequences (seqs) and the run has no end marker.
func follow(v *View, seqs []int64) {
	if len(seqs) == 0 || len(seqs) != len(v.Events) {
		return
	}
	v.Cursor = seqs[len(seqs)-1]
	v.Live = !ended(v.State)
}

// ended reports whether a run in state has its end marker: nothing more is journaled for it.
func ended(state string) bool {
	switch agent.RunState(state) {
	case agent.RunCompleted, agent.RunAborted, agent.RunCancelled, agent.RunEnded:
		return true
	}
	return false
}

// serveLive answers GET /run/live?id=<run>&after=<cursor> (see LiveUpdate). It is 404 for a source
// that does not follow runs (exported journals) and for a run with no records.
func (o Options) serveLive(w http.ResponseWriter, r *http.Request) {
	f, ok := o.Source.(Follower)
	if !ok {
		http.Error(w, "this source is static: exported journals do not grow", http.StatusNotFound)
		return
	}
	runID := r.URL.Query().Get("id")
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil {
		http.Error(w, "after: want the cursor the page gave (a store sequence)", http.StatusBadRequest)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	grew, err := f.Grew(r.Context(), runID, after)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !grew {
		// The page was live at after (it polls only then), and nothing was journaled since.
		writeJSON(w, LiveUpdate{Live: true, Cursor: after})
		return
	}
	recs, seqs, err := f.HistorySeq(r.Context(), runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v := o.viewOf(r.Context(), runID, recs, seqs)
	if v == nil {
		http.NotFound(w, r)
		return
	}
	u := LiveUpdate{Changed: true, Live: v.Live, Cursor: v.Cursor, State: v.State, StateText: v.StateText}
	for i, e := range v.Events {
		if seqs[i] > after {
			u.Events = append(u.Events, e)
		}
	}
	writeJSON(w, u)
}
