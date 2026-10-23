package agent

import (
	"context"
	"iter"
	"time"

	"github.com/bide-ai/bide/internal/capledger"
	"github.com/bide-ai/bide/internal/journalhook"
)

// The cap ledger (internal/capledger, model 17) writes its slots, its index and its cap-intent as
// StepValue records of a Journal: capJournal adapts the journal to the ledger's small Store, so a
// ledger run is an ordinary journal run (a header first, readable by History and Status, skipped by
// every consumer of Runs through IsReservedRun). Nothing in the engine calls it yet: the caps wiring
// (per-call and cumulative caps) will.

// capJournal is a Journal as a capledger.Store.
type capJournal struct{ j *Journal }

// Insert stores data as the value of the step name of run, once (A1): the journal's own insert-once.
func (c capJournal) Insert(ctx context.Context, run, name string, data []byte) ([]byte, bool, error) {
	rec, inserted, err := c.j.putNew(ctx, run, name, Record{Kind: StepValue, Result: data})
	if err != nil {
		return nil, false, err
	}
	return rec.Result, inserted, nil
}

// Get returns the value of the step name of run.
func (c capJournal) Get(ctx context.Context, run, name string) ([]byte, bool, error) {
	rec, ok, err := c.j.Get(ctx, run, name)
	if err != nil || !ok {
		return nil, false, err
	}
	return rec.Result, true, nil
}

// Scan yields the entries of run after position after, in commit order: the store's Load, a prefix of
// the run (A2). The journal header and any entry that is not a value are yielded with no data.
func (c capJournal) Scan(ctx context.Context, run string, after int64) iter.Seq2[capledger.Item, error] {
	return func(yield func(capledger.Item, error) bool) {
		for e, err := range c.j.store.Load(ctx, run, after) {
			if err != nil {
				yield(capledger.Item{}, storageErr("read the cap ledger "+run, err))
				return
			}
			it := capledger.Item{Seq: e.Seq, Name: e.Name}
			if e.Name != headerStep {
				rec, derr := decodeStored(run, e.Name, e.Data)
				if derr != nil {
					yield(capledger.Item{}, derr)
					return
				}
				it.Data = rec.Result
			}
			if !yield(it, nil) {
				return
			}
		}
	}
}

// newCapLedger is a cap ledger over j, reading now from clock. bound is how many windows later than the
// newest recorded one a first intent may be (0 for one).
func newCapLedger(j *Journal, clock func() time.Time, bound int) *capledger.Ledger {
	return capledger.New(capledger.Config{Store: capJournal{j}, Encode: encodeID, Clock: clock, Bound: bound})
}

func init() {
	journalhook.CapLedger = func(j any, clock func() time.Time, bound int) *capledger.Ledger {
		d, ok := j.(*Journal)
		if !ok {
			panic("journalhook.CapLedger: not a *agent.Journal")
		}
		return newCapLedger(d, clock, bound)
	}
}
