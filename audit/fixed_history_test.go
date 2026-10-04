package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"sync"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// fixedHistory is a read-only agent.Store whose every run holds exactly the records it holds,
// each as its stored bytes (its Raw, or its journal encoding when it has none), salts included,
// standing in for a journal exported from a store (as bide-audit reads one). Copying records into
// another store through a Journal would give each a fresh salt, and so a different leaf. Records
// that do not begin with a journal header (built by hand, not read from a journal) are preceded by
// one, since a Journal refuses a run without it.
type fixedHistory []agent.Record

// journal returns a Journal over h.
func (h fixedHistory) journal() *agent.Journal { return agenttest.MustJournal(h) }

// fixedHeader is the journal header a fixedHistory puts before records that have none: one
// header, so every load of a fixedHistory reads the same bytes.
var fixedHeader = sync.OnceValue(func() agent.Record {
	ctx := context.Background()
	j := agenttest.MemJournal()
	if _, err := journaltest.Put(ctx, j, "h", "s", agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}); err != nil {
		panic(err)
	}
	recs, err := j.History(ctx, "h")
	if err != nil || len(recs) == 0 || recs[0].Kind != agent.StepHeader {
		panic("fixedHistory: no journal header to copy")
	}
	return recs[0]
})

func (h fixedHistory) entries() ([]agent.Entry, error) {
	recs := []agent.Record(h)
	if len(recs) == 0 || recs[0].Kind != agent.StepHeader {
		recs = append([]agent.Record{fixedHeader()}, recs...)
	}
	out := make([]agent.Entry, len(recs))
	for i, r := range recs {
		b := r.Raw()
		if b == nil {
			var err error
			if b, err = agent.EncodeRecord(r); err != nil {
				return nil, err
			}
		}
		out[i] = agent.Entry{Seq: int64(i + 1), Name: r.Name, Data: b}
	}
	return out, nil
}

func (fixedHistory) Insert(context.Context, string, string, []byte) (agent.Entry, bool, error) {
	return agent.Entry{}, false, errors.New("fixedHistory is read-only")
}

func (h fixedHistory) Get(_ context.Context, _, name string) (agent.Entry, bool, error) {
	es, err := h.entries()
	if err != nil {
		return agent.Entry{}, false, err
	}
	for _, e := range es {
		if e.Name == name {
			return e, true, nil
		}
	}
	return agent.Entry{}, false, nil
}

func (h fixedHistory) Load(_ context.Context, _ string, after int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) {
		es, err := h.entries()
		if err != nil {
			yield(agent.Entry{}, err)
			return
		}
		for _, e := range es {
			if e.Seq > after && !yield(e, nil) {
				return
			}
		}
	}
}
