package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/internal/journaltest"
)

// value returns a step fn that records v.
func value(v string) func(context.Context) (agent.Record, error) {
	return func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(v)}, nil
	}
}

// entries returns runID's entries in Load order.
func entries(t *testing.T, s agent.Store, runID string) []agent.Entry {
	t.Helper()
	var out []agent.Entry
	for e, err := range s.Load(context.Background(), runID, -1) {
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		out = append(out, e)
	}
	return out
}

// isHeader reports whether e is a journal header naming format.
func isHeader(e agent.Entry, format string) bool {
	var h struct {
		Name   string         `json:"name"`
		Kind   agent.StepKind `json:"kind"`
		Format string         `json:"format"`
	}
	return e.Name == "@journal" && json.Unmarshal(e.Data, &h) == nil && h.Name == "@journal" && h.Kind == agent.StepHeader && h.Format == format
}

// Header (a): a Journal's first write to a run puts the header first, naming JournalFormat.
func headerFirst(t *testing.T, s agent.Store) {
	ctx := context.Background()
	j := journal(t, s)
	id := runID(t)
	if f, err := j.Format(ctx, id); err != nil || f != "" {
		t.Fatalf("Format of an empty run = %q, %v; want \"\"", f, err)
	}
	if _, err := journaltest.Do(ctx, j, id, "a", value(`1`)); err != nil {
		t.Fatal(err)
	}
	es := entries(t, s, id)
	if len(es) != 2 || !isHeader(es[0], agent.JournalFormat) || es[1].Name != "a" {
		t.Fatalf("the run's entries are %v, want the header naming %s, then a", names(es), agent.JournalFormat)
	}
	if f, err := j.Format(ctx, id); err != nil || f != agent.JournalFormat {
		t.Fatalf("Format = %q, %v; want %s", f, err, agent.JournalFormat)
	}
	hist, err := j.History(ctx, id)
	if err != nil || len(hist) != 2 || hist[0].Kind != agent.StepHeader || hist[0].Format != agent.JournalFormat {
		t.Fatalf("History = %v, %v; want the header record first", hist, err)
	}
}

func names(es []agent.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Name
	}
	return out
}

// Header (b): concurrent first writers through two handles leave exactly one header, and it is
// the run's first entry.
func concurrentFirstWriters(t *testing.T, open func(*testing.T) agent.Store) {
	ctx := context.Background()
	hs := []agent.Store{open(t), open(t)}
	for round := range 20 {
		js := []*agent.Journal{journal(t, hs[0]), journal(t, hs[1])} // fresh: they have checked no run
		id := runID(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		for g := range 16 {
			wg.Go(func() {
				<-start
				if _, err := journaltest.Do(ctx, js[g%2], id, fmt.Sprintf("s%d", g), value(`1`)); err != nil {
					errs <- err
				}
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}
		es := entries(t, hs[1], id)
		headers := 0
		for _, e := range es {
			if e.Name == "@journal" {
				headers++
			}
		}
		if len(es) != 17 || headers != 1 || !isHeader(es[0], agent.JournalFormat) {
			t.Fatalf("round %d: entries %v; want one header, first, then the 16 steps", round, names(es))
		}
	}
}

// rawHeader returns the bytes of a journal header naming format.
func rawHeader(format string) []byte {
	return fmt.Appendf(nil, `{"name":"@journal","kind":"header","format":%q,"salt":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`, format)
}

// rawRecord returns the bytes a journal stores for a value record named name.
func rawRecord(t *testing.T, name string) []byte {
	t.Helper()
	b, err := agent.JournalEntry(name, agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// refused checks that a Journal refuses the run: reads and writes fail with a
// *agent.JournalVersionError naming found, and a write stores nothing and runs nothing.
func refused(t *testing.T, s agent.Store, id, found string) {
	t.Helper()
	ctx := context.Background()
	before := names(entries(t, s, id))
	j := journal(t, s)
	check := func(op string, err error) {
		t.Helper()
		var v *agent.JournalVersionError
		if !errors.As(err, &v) || !errors.Is(err, agent.ErrJournalVersion) || !errors.Is(err, agent.ErrProtocol) || v.Found != found || v.RunID != id {
			t.Fatalf("%s = %v; want a *JournalVersionError for run %s naming %q", op, err, id, found)
		}
	}
	_, err := j.History(ctx, id)
	check("History", err)
	for _, err := range j.Records(ctx, id) {
		check("Records", err)
	}
	_, _, err = j.Get(ctx, id, "a")
	check("Get", err)
	// A name the run does not hold is not "not found": the run is not one this version can read.
	_, _, err = journal(t, s).Get(ctx, id, "no-such-step")
	check("Get of a missing name", err)
	ran := false
	_, err = journaltest.Do(ctx, j, id, "b", func(context.Context) (agent.Record, error) {
		ran = true
		return agent.Record{Kind: agent.StepValue}, nil
	})
	check("Do", err)
	_, err = j.Step(ctx, id, "effect", func(context.Context) (int, error) { ran = true; return 1, nil })
	check("Step", err)
	if ran {
		t.Fatal("a step ran for a run the journal refuses")
	}
	if after := names(entries(t, s, id)); !slices.Equal(after, before) {
		t.Fatalf("the journal wrote to a run it refuses: %v, then %v", before, after)
	}
	// A drive of the run refuses it without writing too.
	ag, err := agent.New(agenttest.NewScriptedModel(agenttest.TextTurn("done")), journal(t, s))
	if err != nil {
		t.Fatal(err)
	}
	_, err = ag.Run(ctx, id, agent.UserText("hi"))
	check("Run", err)
	if after := names(entries(t, s, id)); !slices.Equal(after, before) {
		t.Fatalf("a drive wrote to a run the journal refuses: %v, then %v", before, after)
	}
}

// Header (c): a run whose header names a format this version does not support is refused on
// History, Get and writes: a later format, the 1.0 format (which no pre-release reads), and a
// pre-release tag other than the one dev tag used until 1.0.
func unsupportedFormat(t *testing.T, s agent.Store) {
	ctx := context.Background()
	for _, format := range []string{"bide.journal.v999", "bide.journal.v1", "bide.journal.v1-dev.1"} {
		if format == agent.JournalFormat {
			t.Fatalf("this version writes %q, which must be refused", format)
		}
		id := runID(t)
		if _, _, err := s.Insert(ctx, id, "@journal", rawHeader(format)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Insert(ctx, id, "a", rawRecord(t, "a")); err != nil {
			t.Fatal(err)
		}
		refused(t, s, id, format)
		if f, err := journal(t, s).Format(ctx, id); err != nil || f != format {
			t.Fatalf("Format = %q, %v; want the unsupported format %q reported", f, err, format)
		}
	}
}

// Header (d): a journal with no header first (written before the header existed, or by something
// other than a Journal) is refused.
func headerless(t *testing.T, s agent.Store) {
	ctx := context.Background()
	id := runID(t)
	if _, _, err := s.Insert(ctx, id, "a", rawRecord(t, "a")); err != nil {
		t.Fatal(err)
	}
	refused(t, s, id, "")
	var v *agent.JournalVersionError
	if _, err := journal(t, s).Format(ctx, id); !errors.As(err, &v) {
		t.Fatalf("Format of a headerless run = %v, want a *JournalVersionError", err)
	}
}

// Header (e): a read that races a run's first write never reports its record as headerless: the
// header commits before the record, and a reader checks the header after it reads the record.
func readRacingFirstWrite(t *testing.T, open func(*testing.T) agent.Store) {
	ctx := context.Background()
	w, r := open(t), open(t)
	for round := range 30 {
		id := runID(t)
		done := make(chan error, 1)
		go func() {
			_, err := journaltest.Do(ctx, journal(t, w), id, "a", value(`1`))
			done <- err
		}()
		deadline := time.Now().Add(30 * time.Second)
		for {
			j := journal(t, r) // a fresh journal has checked no run
			_, ok, err := j.Get(ctx, id, "a")
			if err != nil {
				t.Fatalf("round %d: Get racing the first write = %v", round, err)
			}
			if _, err := j.History(ctx, id); err != nil {
				t.Fatalf("round %d: History racing the first write = %v", round, err)
			}
			if ok {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("round %d: the record never became visible", round)
			}
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// Journals in one process over one store share in-flight steps: a step one Journal is running is
// not run again by another.
func sharedFlights(t *testing.T, s agent.Store) {
	ctx := context.Background()
	j1, j2 := journal(t, s), journal(t, s)
	id := runID(t)
	var calls atomic.Int64
	started, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := journaltest.Do(ctx, j1, id, "slow", func(context.Context) (agent.Record, error) {
			calls.Add(1)
			close(started)
			<-release
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
		})
		first <- err
	}()
	<-started
	second := make(chan error, 1)
	go func() {
		rec, err := journaltest.Do(ctx, j2, id, "slow", func(context.Context) (agent.Record, error) {
			calls.Add(1)
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`2`)}, nil
		})
		if err == nil && string(rec.Result) != "1" {
			err = fmt.Errorf("the second Journal got %s, want the first's 1", rec.Result)
		}
		second <- err
	}()
	time.Sleep(50 * time.Millisecond) // the second Journal's Do is waiting on the first's by now
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("the step ran %d times through two Journals over one store, want once", n)
	}
}

// commitThenFail is a store whose Insert of the name fail commits and then reports an error, once
// (a connection lost after the commit), and whose Inserts of names starting with refuse fail
// without committing.
type commitThenFail struct {
	agent.Store
	fail, refuse string
	failed       atomic.Bool
	// lose, when set, is a name prefix whose first Insert commits and then reports an error.
	lose string
	lost atomic.Bool
}

func (c *commitThenFail) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	if c.refuse != "" && strings.HasPrefix(name, c.refuse) {
		return agent.Entry{}, false, errors.New("storetest: store unreachable")
	}
	e, ok, err := c.Store.Insert(ctx, runID, name, data)
	if err == nil && name == c.fail && c.failed.CompareAndSwap(false, true) {
		return agent.Entry{}, false, errors.New("storetest: connection lost after the commit")
	}
	if err == nil && c.lose != "" && strings.HasPrefix(name, c.lose) && c.lost.CompareAndSwap(false, true) {
		return agent.Entry{}, false, errors.New("storetest: connection lost after the commit")
	}
	return e, ok, err
}

// A claim whose insert failed but committed does not halt a re-drive over an effect that never
// ran: the claimant records that its attempt did not start, so the next claim re-attempts it, in
// this process or another, and the effect runs exactly once. When that record cannot be written
// either (or is written and reported failed), a re-drive in the same process through the same
// store writes it again and re-attempts; two re-drives racing never both run the effect.
func ambiguousClaim(t *testing.T, s agent.Store) {
	ctx := context.Background()
	const marker = "attempt:step:pay"
	var fired atomic.Int64
	pay := func(context.Context) (string, error) { fired.Add(1); return "paid", nil }
	redrive := func(t *testing.T, id string, w agent.Store, n int) {
		t.Helper()
		var wg sync.WaitGroup
		var paid atomic.Int64
		for range n {
			wg.Go(func() {
				v, err := journal(t, w).Step(ctx, id, "pay", pay)
				var halt *agent.OutcomeUnknown
				switch {
				case err == nil && v == "paid":
					paid.Add(1)
				case errors.As(err, &halt):
				default:
					t.Errorf("re-drive = %q, %v", v, err)
				}
			})
		}
		wg.Wait()
		if fired.Load() != 1 || paid.Load() == 0 {
			t.Fatalf("after the ambiguous claim, re-drives ran the effect %d times (%d got its value); want once", fired.Load(), paid.Load())
		}
	}

	// The not-started record is written: a re-drive through another store handle (another
	// process) re-attempts.
	id := runID(t)
	w := &commitThenFail{Store: s, fail: marker}
	if _, err := journal(t, w).Step(ctx, id, "pay", pay); err == nil || fired.Load() != 0 {
		t.Fatalf("Step whose claim failed = %v (effect ran %d times); want the error, and no effect", err, fired.Load())
	}
	redrive(t, id, &commitThenFail{Store: s}, 2)

	// The not-started record cannot be written either: a re-drive in the same process, through
	// the same store, writes it again and re-attempts the effect.
	fired.Store(0)
	id = runID(t)
	w = &commitThenFail{Store: s, fail: marker, refuse: "attempt:not-started:"}
	if _, err := journal(t, w).Step(ctx, id, "pay", pay); err == nil || fired.Load() != 0 {
		t.Fatalf("Step whose claim failed = %v (effect ran %d times); want the error, and no effect", err, fired.Load())
	}
	w.refuse = ""
	redrive(t, id, w, 2)
	// The not-started record commits and then reports an error too. The marker is voided: a
	// re-drive in the same process must not run the effect under it (a later loss of its result
	// would then be re-attempted by the next driver, and fire the effect again), and re-attempts it
	// under the next marker instead.
	fired.Store(0)
	id = runID(t)
	w = &commitThenFail{Store: s, fail: marker, lose: "attempt:not-started:"}
	if _, err := journal(t, w).Step(ctx, id, "pay", pay); err == nil || fired.Load() != 0 {
		t.Fatalf("Step whose claim failed = %v (effect ran %d times); want the error, and no effect", err, fired.Load())
	}
	redrive(t, id, w, 2)
	var live []string
	for e, err := range s.Load(ctx, id, -1) {
		if err != nil {
			t.Fatal(err)
		}
		live = append(live, e.Name)
	}
	if !slices.Contains(live, "attempt:retry:1:step:pay") {
		t.Fatalf("the effect ran without a claim of the next attempt; the run holds %v", live)
	}
}

// lister checks the Lister contract: ascending order, the filter's After, Prefix and
// ExcludeHolding, early breaks, and writes inside the loop (leaseLapsed checks LeaseLapsed).
func lister(t *testing.T, s agent.Store) {
	ctx := context.Background()
	l, _ := agent.Capability[agent.Lister](s)
	prefix := runID(t) + "/"
	const n = 1100 // more than two pages of a paged store
	var all, open []string
	for i := range n {
		id := fmt.Sprintf("%s%04d", prefix, i)
		all = append(all, id)
		if _, _, err := s.Insert(ctx, id, "x", []byte("x")); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if _, _, err := s.Insert(ctx, id, "done", []byte("x")); err != nil {
				t.Fatal(err)
			}
		} else {
			open = append(open, id)
		}
	}
	list := func(f agent.RunFilter) []string {
		t.Helper()
		var out []string
		for id, err := range l.Runs(ctx, f) {
			if err != nil {
				t.Fatalf("Runs(%+v): %v", f, err)
			}
			out = append(out, id)
		}
		return out
	}
	if got := list(agent.RunFilter{Prefix: prefix}); !slices.Equal(got, all) {
		t.Fatalf("Runs(Prefix) yields %d runs (sorted: %v), want the %d runs in ascending order", len(got), slices.IsSorted(got), n)
	}
	if got := list(agent.RunFilter{Prefix: prefix, ExcludeHolding: []string{"done", "nothing"}}); !slices.Equal(got, open) {
		t.Fatalf("Runs(ExcludeHolding) yields %d runs, want the %d without a done entry", len(got), len(open))
	}
	if got := list(agent.RunFilter{Prefix: prefix, After: all[599]}); !slices.Equal(got, all[600:]) {
		t.Fatalf("Runs(After) yields %d runs, want the %d after the cursor", len(got), n-600)
	}
	if got := list(agent.RunFilter{Prefix: prefix + "no-such-run"}); len(got) != 0 {
		t.Fatalf("Runs over an unused prefix yields %v", got)
	}
	// Breaking early and writing inside the loop neither leak nor deadlock.
	tctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for range 20 {
		for _, err := range l.Runs(tctx, agent.RunFilter{Prefix: prefix}) {
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	seen := 0
	for id, err := range l.Runs(tctx, agent.RunFilter{Prefix: prefix, ExcludeHolding: []string{"done"}}) {
		if err != nil {
			t.Fatalf("Runs with writes inside the loop: %v", err)
		}
		if !strings.HasPrefix(id, prefix) {
			t.Fatalf("Runs yields %q outside the prefix", id)
		}
		if _, _, err := s.Insert(tctx, id, "done", []byte("x")); err != nil {
			t.Fatalf("Insert inside the Runs loop: %v", err)
		}
		seen++
	}
	if seen < len(open) {
		t.Fatalf("the loop visited %d of %d runs", seen, len(open))
	}
	if got := list(agent.RunFilter{Prefix: prefix, ExcludeHolding: []string{"done"}}); len(got) != 0 {
		t.Fatalf("Runs(ExcludeHolding) still yields %d runs after every run got a done entry", len(got))
	}
}

// leaseLapsed checks RunFilter.LeaseLapsed: it admits exactly the runs the store holds whose lease
// has lapsed, a lapsed lease is one AcquireLease grants to a new holder, and the filter combines
// with Prefix, After and ExcludeHolding over more than one page. A store without Leaser admits no
// run under it.
func leaseLapsed(t *testing.T, s agent.Store) {
	ctx := context.Background()
	l, _ := agent.Capability[agent.Lister](s)
	prefix := runID(t) + "/"
	list := func(f agent.RunFilter) []string {
		t.Helper()
		f.Prefix, f.LeaseLapsed = prefix, true
		var out []string
		for id, err := range l.Runs(ctx, f) {
			if err != nil {
				t.Fatalf("Runs(%+v): %v", f, err)
			}
			out = append(out, id)
		}
		return out
	}
	leaser, ok := agent.Capability[agent.Leaser](s)
	if !ok {
		if _, _, err := s.Insert(ctx, prefix+"r", "x", []byte("x")); err != nil {
			t.Fatal(err)
		}
		if got := list(agent.RunFilter{}); len(got) != 0 {
			t.Fatalf("Runs(LeaseLapsed) over a store without Leaser yields %v, want no run", got)
		}
		return
	}
	acquire := func(id, holder string, ttl time.Duration) {
		t.Helper()
		if ok, err := leaser.AcquireLease(ctx, id, holder, ttl); err != nil || !ok {
			t.Fatalf("AcquireLease(%s, %s) = %v, %v; want granted", id, holder, ok, err)
		}
	}
	// Each run falls in a class by its index: 0 to 3 a lapsed lease, 4 a lapsed lease and a done
	// entry, 5 a live lease, 6 a released lease (even tens) or none (odd tens), 7 a lapsed lease
	// another holder took since. More than a page of runs has a lapsed lease.
	const n = 1100
	var lapsed, lapsedOpen, retaken []string
	for i := range n {
		id := fmt.Sprintf("%s%04d", prefix, i)
		if _, _, err := s.Insert(ctx, id, "x", []byte("x")); err != nil {
			t.Fatal(err)
		}
		switch c := i % 8; c {
		case 0, 1, 2, 3, 4:
			acquire(id, "dead", time.Millisecond)
			lapsed = append(lapsed, id)
			if c == 4 {
				if _, _, err := s.Insert(ctx, id, "done", []byte("x")); err != nil {
					t.Fatal(err)
				}
			} else {
				lapsedOpen = append(lapsedOpen, id)
			}
		case 5:
			acquire(id, "live", time.Hour)
		case 6:
			if i/10%2 == 0 {
				acquire(id, "gone", time.Hour)
				if err := leaser.ReleaseLease(ctx, id, "gone"); err != nil {
					t.Fatal(err)
				}
			}
		case 7:
			acquire(id, "dead", time.Millisecond)
			retaken = append(retaken, id)
		}
	}
	// A lapsed lease on a run the store does not hold (no entry) is not a run to list.
	acquire(prefix+"orphan", "dead", time.Millisecond)

	// Wait for the millisecond leases to lapse on the store's clock.
	want := slices.Sorted(slices.Values(append(slices.Clone(lapsed), retaken...)))
	deadline := time.Now().Add(30 * time.Second)
	for got := list(agent.RunFilter{}); !slices.Equal(got, want); got = list(agent.RunFilter{}) {
		if time.Now().After(deadline) {
			t.Fatalf("Runs(LeaseLapsed) yields %d runs 30s after their millisecond leases, want the %d with a lapsed lease (and no other)", len(got), len(want))
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, id := range retaken {
		acquire(id, "new", time.Hour) // a lapsed lease is granted to a new holder, and is live again
	}
	if got := list(agent.RunFilter{}); !slices.Equal(got, lapsed) {
		t.Fatalf("Runs(LeaseLapsed) yields %d runs, want the %d whose lease lapsed and was not taken since", len(got), len(lapsed))
	}
	if got := list(agent.RunFilter{ExcludeHolding: []string{"done"}}); !slices.Equal(got, lapsedOpen) {
		t.Fatalf("Runs(LeaseLapsed, ExcludeHolding) yields %d runs, want the %d lapsed ones without a done entry", len(got), len(lapsedOpen))
	}
	cursor := lapsedOpen[len(lapsedOpen)/2]
	if got := list(agent.RunFilter{ExcludeHolding: []string{"done"}, After: cursor}); !slices.Equal(got, lapsedOpen[len(lapsedOpen)/2+1:]) {
		t.Fatalf("Runs(LeaseLapsed, After) yields %d runs, want the %d after the cursor", len(got), len(lapsedOpen)-len(lapsedOpen)/2-1)
	}
	// What the filter admits is what AcquireLease grants: a new holder takes a listed run, after
	// which it is no longer lapsed.
	acquire(lapsedOpen[0], "taker", time.Hour)
	if got := list(agent.RunFilter{ExcludeHolding: []string{"done"}}); !slices.Equal(got, lapsedOpen[1:]) {
		t.Fatalf("Runs(LeaseLapsed) yields %d runs after a new holder took one, want %d", len(got), len(lapsedOpen)-1)
	}
}

// reapLeases checks Leaser.ReapLeases: it deletes the lapsed leases of runs that hold an entry
// named in ended or no entry at all, and of runs whose ID holds a '>' (a session's or a
// sub-agent's, which no recovery pass drives), and keeps live leases and the lapsed leases of
// unfinished runs.
func reapLeases(t *testing.T, s agent.Store) {
	ctx := context.Background()
	l, _ := agent.Capability[agent.Lister](s)
	leaser, _ := agent.Capability[agent.Leaser](s)
	prefix := runID(t) + "/"
	end := prefix + "end" // an end marker no other test's run holds
	insert := func(id, name string) {
		t.Helper()
		if _, _, err := s.Insert(ctx, id, name, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	acquire := func(id, holder string, ttl time.Duration) {
		t.Helper()
		if ok, err := leaser.AcquireLease(ctx, id, holder, ttl); err != nil || !ok {
			t.Fatalf("AcquireLease(%s, %s) = %v, %v; want granted", id, holder, ok, err)
		}
	}
	lapsed := func() []string {
		t.Helper()
		var out []string
		for id, err := range l.Runs(ctx, agent.RunFilter{Prefix: prefix, LeaseLapsed: true}) {
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}
	reap := func(ended []string) int {
		t.Helper()
		n, err := leaser.ReapLeases(ctx, ended)
		if err != nil {
			t.Fatalf("ReapLeases(%v): %v", ended, err)
		}
		return n
	}
	finished, open, orphan := prefix+"finished", prefix+"open", prefix+"orphan"
	finishedLive, orphanLive := prefix+"finished-live", prefix+"orphan-live"
	// A session's turn run and a sub-agent's run: unfinished, but no recovery pass takes them over
	// (the session or the root run resumes them), so a lapsed lease on them is never taken by one.
	turn, sub := prefix+"s>@turn/0", prefix+"t>call"
	turnLive := prefix + "u>@turn/0"
	insert(turn, "x")
	insert(sub, "x")
	insert(turnLive, "x")
	acquire(turn, "dead", time.Millisecond)
	acquire(sub, "dead", time.Millisecond)
	acquire(turnLive, "live", time.Hour)
	insert(finished, "x")
	insert(finished, end)
	insert(open, "x")
	insert(finishedLive, "x")
	insert(finishedLive, end)
	acquire(finished, "dead", time.Millisecond)
	acquire(open, "dead", time.Millisecond)
	acquire(orphan, "dead", time.Millisecond)
	acquire(finishedLive, "live", time.Hour)
	acquire(orphanLive, "live", time.Hour)
	want := []string{finished, open, turn, sub}
	deadline := time.Now().Add(30 * time.Second)
	for got := lapsed(); !slices.Equal(got, want); got = lapsed() {
		if time.Now().After(deadline) {
			t.Fatalf("Runs(LeaseLapsed) yields %v 30s after their millisecond leases, want %v", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// With no end markers, the lease on a run the store does not hold goes, and so do those on the
	// session's and the sub-agent's runs (other tests may have left such leases too, so the count
	// is a lower bound).
	if n := reap(nil); n < 3 {
		t.Fatalf("ReapLeases(nil) deleted %d leases, want at least the lapsed ones on a run with no entry, a session's run and a sub-agent's", n)
	}
	want = []string{finished, open}
	if got := lapsed(); !slices.Equal(got, want) {
		t.Fatalf("after ReapLeases(nil), Runs(LeaseLapsed) yields %v, want %v: it deleted a lease on a run that has entries", got, want)
	}
	// Leases of other tests sharing the store may lapse meanwhile and go too: counts are lower
	// bounds, and the listing shows which of this test's leases went.
	if n := reap([]string{"nothing", end}); n < 1 {
		t.Fatalf("ReapLeases(end) deleted %d leases, want at least the lapsed lease on a finished run", n)
	}
	if got := lapsed(); !slices.Equal(got, []string{open}) {
		t.Fatalf("after ReapLeases(end), Runs(LeaseLapsed) yields %v, want only the unfinished run %s", got, open)
	}
	reap([]string{end})
	if got := lapsed(); !slices.Equal(got, []string{open}) {
		t.Fatalf("after a second ReapLeases(end), Runs(LeaseLapsed) yields %v, want only the unfinished run %s", got, open)
	}
	// Live leases are kept, finished run or not: their holders still renew them.
	for _, id := range []string{finishedLive, orphanLive, turnLive} {
		if ok, err := leaser.RenewLease(ctx, id, "live", time.Hour); err != nil || !ok {
			t.Fatalf("RenewLease(%s) after ReapLeases = %v, %v; want the live lease kept", id, ok, err)
		}
	}
	// The kept lapsed lease is still one a new holder takes.
	acquire(open, "new", time.Hour)
}

// CheckWrapper checks a store wrapper against the rules for wrappers (see agent.Store): its
// mapping of run IDs and names to stored entries does not depend on the context (A1), and it may
// expose the capabilities of the store it wraps through Unwrap only if it passes run IDs and names
// through unchanged. wrap wraps the store it is given; CheckWrapper wraps a MemStore, which
// implements Lister and Leaser.
//
// ctxs are at least two contexts the wrapper will be called with, and they must differ in the
// values the wrapper reads from a context (two requests of different tenants, say, carrying the
// wrapper's own context keys): an entry written under the first must read back the same under each
// of the others. CheckWrapper cannot guess a wrapper's context keys, so with fewer than two
// contexts it fails the test rather than check nothing.
func CheckWrapper(t testing.TB, wrap func(agent.Store) agent.Store, ctxs ...context.Context) {
	t.Helper()
	checkWrapper(t, wrap, ctxs...)
}

// reporter is the part of testing.TB CheckWrapper uses.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

func checkWrapper(t reporter, wrap func(agent.Store) agent.Store, ctxs ...context.Context) {
	t.Helper()
	if len(ctxs) < 2 {
		t.Fatalf("CheckWrapper needs at least two contexts that differ in the values the wrapper reads, got %d", len(ctxs))
	}
	ctx := ctxs[0]
	inner := agent.NewMemStore()
	w := wrap(inner)
	id := fmt.Sprintf("checkwrapper-%d-%d", time.Now().UnixNano(), runIDs.Add(1))
	if _, _, err := w.Insert(ctx, id, "k", []byte("v")); err != nil {
		t.Fatalf("Insert through the wrapper: %v", err)
	}
	for i, c := range ctxs[1:] {
		e, ok, err := w.Get(c, id, "k")
		if err != nil || !ok || string(e.Data) != "v" {
			t.Errorf("an entry written through %T under one context reads back as %q, %v, %v under context %d: a store's mapping of run IDs and names must not depend on the context (A1)", w, e.Data, ok, err, i+1)
		}
		n := 0
		for e, err := range w.Load(c, id, -1) {
			if err != nil || e.Name != "k" {
				t.Errorf("Load through %T under context %d yields %q, %v", w, i+1, e.Name, err)
			}
			n++
		}
		if n != 1 {
			t.Errorf("Load through %T under context %d yields %d entries, want the 1 written under another context (A1)", w, i+1, n)
		}
	}
	e, same, err := inner.Get(context.Background(), id, "k")
	if err != nil {
		t.Fatalf("%v", err)
	}
	same = same && string(e.Data) == "v"
	if _, unwraps := w.(interface{ Unwrap() agent.Store }); unwraps && !same {
		t.Errorf("%T rewrites run IDs or names but implements Unwrap, so Capability exposes the wrapped store's Lister and Leaser under the wrong keys; implement each capability on the wrapper instead", w)
	}
	if l, ok := agent.Capability[agent.Lister](w); ok {
		found := false
		for got, err := range l.Runs(ctx, agent.RunFilter{Prefix: id}) {
			if err != nil {
				t.Fatalf("Runs through the wrapper: %v", err)
			}
			found = found || got == id
		}
		if !found {
			t.Errorf("the Lister reached through %T does not list run %q, which was written through it", w, id)
		}
	}
}
