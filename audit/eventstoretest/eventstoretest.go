// Package eventstoretest checks that an audit.EventStore keeps the contract the event trail builds
// on: contiguous, append-only positions per run, an idempotent re-append of the same leaf, a
// refusal (audit.ErrSeqTaken) of a different leaf at a stored position, and exactly one winner
// among writers racing for one position. Every EventStore in this repository runs it, and a new
// backend should too:
//
//	func TestEventStore(t *testing.T) {
//	    db := openTestDatabase(t)
//	    eventstoretest.Run(t, func(t *testing.T) audit.EventStore { return myEventStoreOn(db) })
//	}
//
// open may be called several times in one test; every handle it returns must reach the same data
// (a new connection to the same database, or the same in-memory store), since the suite checks
// what one handle's writes look like through another and races appends across handles. Run IDs
// are unique per call, so the suite can run repeatedly against a persistent backend.
package eventstoretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
)

var runCounter atomic.Int64

// runID returns a run ID no other call of this process, and no earlier process, used.
func runID(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("evstore-%d-%d", time.Now().UnixNano(), runCounter.Add(1))
}

// racers is how many writers race for one position.
const racers = 8

// Run runs the whole suite against the stores open returns.
func Run(t *testing.T, open func(t *testing.T) audit.EventStore) {
	t.Run("UnknownRunLoadsNil", func(t *testing.T) { unknownRun(t, open(t)) })
	t.Run("AppendLoadInOrder", func(t *testing.T) { appendLoad(t, open(t)) })
	t.Run("ByteFidelity", func(t *testing.T) { byteFidelity(t, open(t)) })
	t.Run("CopiesInAndOut", func(t *testing.T) { copies(t, open(t)) })
	t.Run("SameLeafReappendIsNoop", func(t *testing.T) { sameLeaf(t, open(t)) })
	t.Run("DifferentLeafIsErrSeqTaken", func(t *testing.T) { differentLeaf(t, open(t)) })
	t.Run("GapAndNegativeRefused", func(t *testing.T) { gap(t, open(t)) })
	t.Run("RunsAreIndependent", func(t *testing.T) { independentRuns(t, open(t)) })
	t.Run("ReadYourWritesAcrossHandles", func(t *testing.T) { acrossHandles(t, open) })
	t.Run("RaceForOnePositionOneWinner", func(t *testing.T) { raceDifferent(t, open) })
	t.Run("RaceSameLeafAllSucceed", func(t *testing.T) { raceSame(t, open) })
	t.Run("ConcurrentWritersOfOneTrail", func(t *testing.T) { concurrentTrail(t, open) })
}

func leafN(i int) []byte { return []byte(fmt.Sprintf("leaf-%d", i)) }

// appendN appends leaves 0..n-1 to run.
func appendN(t *testing.T, s audit.EventStore, run string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := s.Append(context.Background(), run, i, leafN(i)); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
}

// wantLeaves fails unless s holds exactly want for run, in order.
func wantLeaves(t *testing.T, s audit.EventStore, run string, want [][]byte) {
	t.Helper()
	got, err := s.Load(context.Background(), run)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("Load returned %d leaves, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("leaf %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func leaves(n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = leafN(i)
	}
	return out
}

func unknownRun(t *testing.T, s audit.EventStore) {
	got, err := s.Load(context.Background(), runID(t))
	if err != nil {
		t.Fatalf("Load of an unknown run: %v", err)
	}
	if got != nil {
		t.Fatalf("Load of an unknown run = %v, want nil", got)
	}
}

func appendLoad(t *testing.T, s audit.EventStore) {
	run := runID(t)
	// More than one page of any backend that reads the trail in pages.
	const n = 600
	appendN(t, s, run, n)
	wantLeaves(t, s, run, leaves(n))
}

func byteFidelity(t *testing.T, s audit.EventStore) {
	run := runID(t)
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	big := bytes.Repeat([]byte{0, 0xff, 'a', 0x80}, 1<<18) // 1 MiB
	want := [][]byte{all, {0}, {0, 0, 0}, []byte("\xc3\x28 not utf-8"), big, []byte(`{"a":1}` + "\n")}
	for i, l := range want {
		if err := s.Append(context.Background(), run, i, l); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	wantLeaves(t, s, run, want)
}

func copies(t *testing.T, s audit.EventStore) {
	ctx, run := context.Background(), runID(t)
	in := []byte("original")
	if err := s.Append(ctx, run, 0, in); err != nil {
		t.Fatal(err)
	}
	copy(in, "MUTATED!") // the store must not have kept the caller's slice
	got, err := s.Load(ctx, run)
	if err != nil || len(got) != 1 {
		t.Fatalf("Load = %v, %v", got, err)
	}
	if !bytes.Equal(got[0], []byte("original")) {
		t.Fatalf("a later write to the appended slice reached the store: %q", got[0])
	}
	copy(got[0], "CHANGED!") // nor may the slice Load returned alias the store
	wantLeaves(t, s, run, [][]byte{[]byte("original")})
}

func sameLeaf(t *testing.T, s audit.EventStore) {
	ctx, run := context.Background(), runID(t)
	appendN(t, s, run, 3)
	for _, seq := range []int{0, 1, 2} { // a stored position, tail or not
		if err := s.Append(ctx, run, seq, leafN(seq)); err != nil {
			t.Fatalf("re-appending the same leaf at %d: %v", seq, err)
		}
	}
	wantLeaves(t, s, run, leaves(3))
	if err := s.Append(ctx, run, 3, leafN(3)); err != nil { // the trail still extends
		t.Fatalf("Append(3) after re-appends: %v", err)
	}
	wantLeaves(t, s, run, leaves(4))
}

func differentLeaf(t *testing.T, s audit.EventStore) {
	ctx, run := context.Background(), runID(t)
	appendN(t, s, run, 3)
	for _, seq := range []int{0, 1, 2} {
		err := s.Append(ctx, run, seq, []byte("forked"))
		if !errors.Is(err, audit.ErrSeqTaken) {
			t.Fatalf("a different leaf at stored position %d: err = %v, want ErrSeqTaken", seq, err)
		}
	}
	// A leaf that extends the stored one is a different leaf.
	if err := s.Append(ctx, run, 0, append(leafN(0), 'x')); !errors.Is(err, audit.ErrSeqTaken) {
		t.Fatalf("a longer leaf at a stored position: err = %v, want ErrSeqTaken", err)
	}
	wantLeaves(t, s, run, leaves(3)) // the refusal changed nothing
}

func gap(t *testing.T, s audit.EventStore) {
	ctx, run := context.Background(), runID(t)
	for _, seq := range []int{1, 5} { // beyond the next position of an empty run
		err := s.Append(ctx, run, seq, []byte("x"))
		if err == nil || errors.Is(err, audit.ErrSeqTaken) {
			t.Fatalf("Append(%d) on an empty run: err = %v, want a gap error that is not ErrSeqTaken", seq, err)
		}
	}
	if got, err := s.Load(ctx, run); err != nil || got != nil {
		t.Fatalf("a refused gap stored something: %v, %v", got, err)
	}
	appendN(t, s, run, 2)
	if err := s.Append(ctx, run, 3, []byte("x")); err == nil || errors.Is(err, audit.ErrSeqTaken) {
		t.Fatalf("Append(3) with 2 stored: err = %v, want a gap error that is not ErrSeqTaken", err)
	}
	if err := s.Append(ctx, run, -1, []byte("x")); err == nil || errors.Is(err, audit.ErrSeqTaken) {
		t.Fatalf("Append(-1): err = %v, want an error that is not ErrSeqTaken", err)
	}
	wantLeaves(t, s, run, leaves(2))
	if err := s.Append(ctx, run, 2, leafN(2)); err != nil { // the trail was not wedged by the refusals
		t.Fatalf("Append(2) after the refusals: %v", err)
	}
}

func independentRuns(t *testing.T, s audit.EventStore) {
	ctx, a, b := context.Background(), runID(t), runID(t)
	if err := s.Append(ctx, a, 0, []byte("a0")); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, b, 0, []byte("b0")); err != nil { // the same seq of another run is free
		t.Fatalf("seq 0 of a second run: %v", err)
	}
	if err := s.Append(ctx, b, 1, []byte("b1")); err != nil {
		t.Fatal(err)
	}
	wantLeaves(t, s, a, [][]byte{[]byte("a0")})
	wantLeaves(t, s, b, [][]byte{[]byte("b0"), []byte("b1")})
}

func acrossHandles(t *testing.T, open func(t *testing.T) audit.EventStore) {
	ctx, run := context.Background(), runID(t)
	w, r := open(t), open(t)
	for i := 0; i < 5; i++ {
		if err := w.Append(ctx, run, i, leafN(i)); err != nil {
			t.Fatal(err)
		}
		wantLeaves(t, r, run, leaves(i+1)) // committed before Append returned, so visible at once
	}
	// A handle that did not write sees the stored leaf as taken, and the same one as a no-op.
	if err := r.Append(ctx, run, 2, leafN(2)); err != nil {
		t.Fatalf("same leaf through another handle: %v", err)
	}
	if err := r.Append(ctx, run, 2, []byte("other")); !errors.Is(err, audit.ErrSeqTaken) {
		t.Fatalf("different leaf through another handle: err = %v, want ErrSeqTaken", err)
	}
}

// race starts racers goroutines, each on a handle of its own, and releases them together; leaf(i)
// is what goroutine i appends at seq of run. It returns their errors by goroutine.
func race(t *testing.T, open func(t *testing.T) audit.EventStore, run string, seq int, leaf func(i int) []byte) []error {
	t.Helper()
	handles := make([]audit.EventStore, racers)
	for i := range handles {
		handles[i] = open(t)
	}
	errs := make([]error, racers)
	var ready, wg sync.WaitGroup
	start := make(chan struct{})
	ready.Add(racers)
	wg.Add(racers)
	for i := range handles {
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			errs[i] = handles[i].Append(context.Background(), run, seq, leaf(i))
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	return errs
}

func raceDifferent(t *testing.T, open func(t *testing.T) audit.EventStore) {
	const rounds = 10
	run := runID(t)
	check := open(t)
	for seq := 0; seq < rounds; seq++ {
		errs := race(t, open, run, seq, func(i int) []byte { return []byte(fmt.Sprintf("seq%d-writer%d", seq, i)) })
		winner := -1
		for i, err := range errs {
			switch {
			case err == nil:
				if winner >= 0 {
					t.Fatalf("seq %d: writers %d and %d both won", seq, winner, i)
				}
				winner = i
			case !errors.Is(err, audit.ErrSeqTaken):
				t.Fatalf("seq %d: writer %d lost with %v, want ErrSeqTaken", seq, i, err)
			}
		}
		if winner < 0 {
			t.Fatalf("seq %d: no writer won: %v", seq, errs)
		}
		got, err := check.Load(context.Background(), run)
		if err != nil || len(got) != seq+1 {
			t.Fatalf("seq %d: Load = %d leaves, %v", seq, len(got), err)
		}
		if want := fmt.Sprintf("seq%d-writer%d", seq, winner); string(got[seq]) != want {
			t.Fatalf("seq %d holds %q, want the winner's %q", seq, got[seq], want)
		}
	}
}

func raceSame(t *testing.T, open func(t *testing.T) audit.EventStore) {
	run := runID(t)
	check := open(t)
	for seq := 0; seq < 5; seq++ {
		for i, err := range race(t, open, run, seq, func(int) []byte { return leafN(seq) }) {
			if err != nil {
				t.Fatalf("seq %d: writer %d of the same leaf failed: %v", seq, i, err)
			}
		}
	}
	wantLeaves(t, check, run, leaves(5))
}

// concurrentTrail has several writers mirror one whole trail at once, as PersistJournal does from
// several processes: each appends 0..n-1 in order, so each is sometimes the winner of a position
// and sometimes a no-op re-append of it.
func concurrentTrail(t *testing.T, open func(t *testing.T) audit.EventStore) {
	const n = 40
	run := runID(t)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range errs {
		s := open(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := 0; seq < n; seq++ {
				if err := s.Append(context.Background(), run, seq, leafN(seq)); err != nil {
					errs[i] = fmt.Errorf("seq %d: %w", seq, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	wantLeaves(t, open(t), run, leaves(n))
}
