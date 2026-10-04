package storetest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// handles is the number of handles the concurrency checks write through, and writers the number
// of goroutines that race on each name.
const (
	handles = 3
	writers = 64
)

// A1: concurrent Inserts of one name through several handles have exactly one winner, and every
// caller and every later reader sees the winner's bytes.
func uniqueNames(t *testing.T, open func(*testing.T) agent.Store) {
	ctx := context.Background()
	hs := make([]agent.Store, handles)
	for i := range hs {
		hs[i] = open(t)
	}
	id := runID(t)
	for round := range 8 {
		name := fmt.Sprintf("n%d", round)
		type result struct {
			e        agent.Entry
			inserted bool
			err      error
			data     []byte
		}
		res := make([]result, writers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for g := range writers {
			wg.Go(func() {
				data := fmt.Appendf(nil, "writer %d of round %d", g, round)
				<-start
				e, ok, err := hs[g%handles].Insert(ctx, id, name, data)
				res[g] = result{e, ok, err, data}
			})
		}
		close(start)
		wg.Wait()
		var winner *result
		for g := range res {
			r := &res[g]
			if r.err != nil {
				t.Fatalf("round %d: Insert through handle %d: %v", round, g%handles, r.err)
			}
			if r.inserted {
				if winner != nil {
					t.Fatalf("round %d: two Inserts of %q both report they stored it", round, name)
				}
				winner = r
			}
		}
		if winner == nil {
			t.Fatalf("round %d: no Insert of %q reports it stored it", round, name)
		}
		for g, r := range res {
			if !bytes.Equal(r.e.Data, winner.data) || r.e.Seq != winner.e.Seq || r.e.Name != name {
				t.Fatalf("round %d: caller %d got entry %q (seq %d, %q), want the winner's %q (seq %d)", round, g, r.e.Name, r.e.Seq, r.e.Data, winner.data, winner.e.Seq)
			}
		}
		for i, h := range hs {
			e, ok, err := h.Get(ctx, id, name)
			if err != nil || !ok || !bytes.Equal(e.Data, winner.data) || e.Seq != winner.e.Seq {
				t.Fatalf("round %d: Get through handle %d = %q (seq %d), %v, %v; want the winner's bytes", round, i, e.Data, e.Seq, ok, err)
			}
		}
	}
	for i, h := range hs {
		seen := map[string]int{}
		for e, err := range h.Load(ctx, id, -1) {
			if err != nil {
				t.Fatalf("Load through handle %d: %v", i, err)
			}
			seen[e.Name]++
		}
		for name, n := range seen {
			if n != 1 {
				t.Fatalf("Load through handle %d yields %q %d times", i, name, n)
			}
		}
		if len(seen) != 8 {
			t.Fatalf("Load through handle %d yields %d names, want 8", i, len(seen))
		}
	}
}

// A1, as the engine relies on it: a side-effect Step raced by 64 goroutines through Journals on
// three handles runs its effect exactly once. Every other caller gets the recorded value or halts
// on the unknown outcome; none runs the effect a second time.
func atMostOnceEffect(t *testing.T, open func(*testing.T) agent.Store) {
	ctx := context.Background()
	js := make([]*agent.Journal, handles)
	for i := range js {
		js[i] = journal(t, open(t))
	}
	resume := journal(t, open(t))
	for round := range 5 {
		id := runID(t)
		var fired atomic.Int64
		start := make(chan struct{})
		var wg sync.WaitGroup
		var got, halted atomic.Int64
		errs := make(chan error, writers)
		for g := range writers {
			wg.Go(func() {
				<-start
				v, err := js[g%handles].Step(ctx, id, "charge", func(context.Context) (int, error) {
					fired.Add(1)
					return 42, nil
				})
				var halt *agent.OutcomeUnknown
				switch {
				case err == nil && v == 42:
					got.Add(1)
				case errors.As(err, &halt):
					halted.Add(1)
				default:
					errs <- fmt.Errorf("caller %d: Step = %v, %v", g, v, err)
				}
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("round %d: %v", round, err)
		}
		if n := fired.Load(); n != 1 {
			t.Fatalf("round %d: the effect ran %d times under %d racing callers, want exactly once", round, n, writers)
		}
		if got.Load() == 0 {
			t.Fatalf("round %d: no caller got the effect's value", round)
		}
		// A resume reads the recorded value and runs nothing.
		v, err := resume.Step(ctx, id, "charge", func(context.Context) (int, error) {
			fired.Add(1)
			return 0, nil
		})
		if err != nil || v != 42 || fired.Load() != 1 {
			t.Fatalf("round %d: resume = %v, %v (effect ran %d times); want the recorded 42", round, v, err, fired.Load())
		}
	}
}

// A2: readers polling while several writers insert never see an entry before one with a lower
// Seq: every read is a prefix of the run's final order, and Seq strictly increases along it.
func prefixClosed(t *testing.T, open func(*testing.T) agent.Store) {
	ctx := context.Background()
	id := runID(t)
	type seen struct {
		seq  int64
		name string
	}
	read := func(s agent.Store) ([]seen, error) {
		var out []seen
		for e, err := range s.Load(ctx, id, -1) {
			if err != nil {
				return nil, err
			}
			out = append(out, seen{e.Seq, e.Name})
		}
		return out, nil
	}
	const perWriter, writerCount, readerCount = 40, 8, 4
	hs := make([]agent.Store, handles)
	for i := range hs {
		hs[i] = open(t)
	}
	var done atomic.Bool
	var wg sync.WaitGroup
	var observations [][]seen
	var obsMu sync.Mutex
	errs := make(chan error, writerCount+readerCount)
	for r := range readerCount {
		s := hs[r%handles]
		wg.Go(func() {
			for !done.Load() {
				obs, err := read(s)
				if err != nil {
					errs <- fmt.Errorf("reader %d: %w", r, err)
					return
				}
				obsMu.Lock()
				observations = append(observations, obs)
				obsMu.Unlock()
			}
		})
	}
	var ww sync.WaitGroup
	for w := range writerCount {
		s := hs[w%handles]
		ww.Go(func() {
			for i := range perWriter {
				if _, _, err := s.Insert(ctx, id, fmt.Sprintf("w%d-%d", w, i), []byte("x")); err != nil {
					errs <- fmt.Errorf("writer %d: %w", w, err)
					return
				}
			}
		})
	}
	ww.Wait()
	done.Store(true)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	final, err := read(hs[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != perWriter*writerCount {
		t.Fatalf("the run holds %d entries, want %d", len(final), perWriter*writerCount)
	}
	for i := 1; i < len(final); i++ {
		if final[i].seq <= final[i-1].seq {
			t.Fatalf("Seq does not increase along Load order: %d then %d", final[i-1].seq, final[i].seq)
		}
	}
	for n, obs := range observations {
		if len(obs) > len(final) {
			t.Fatalf("read %d saw %d entries, more than the run's final %d", n, len(obs), len(final))
		}
		for i := range obs {
			if obs[i] != final[i] {
				t.Fatalf("read %d is not a prefix of the run: its entry %d is %q (seq %d), the run's is %q (seq %d)", n, i, obs[i].name, obs[i].seq, final[i].name, final[i].seq)
			}
		}
	}
}

// A3: inserting the same bytes again is harmless: nothing new is stored and the entry is the one
// already there.
func retrySameBytes(t *testing.T, s agent.Store) {
	ctx := context.Background()
	id := runID(t)
	e1, ok, err := s.Insert(ctx, id, "a", []byte("payload"))
	if err != nil || !ok {
		t.Fatalf("first Insert = %v, %v", ok, err)
	}
	e2, ok, err := s.Insert(ctx, id, "a", []byte("payload"))
	if err != nil || ok || e2.Seq != e1.Seq || string(e2.Data) != "payload" {
		t.Fatalf("retried Insert = %+v, %v, %v; want the entry already stored", e2, ok, err)
	}
}

// A4: an entry is visible through every handle once its Insert returns, and keeps its bytes and
// position on every later read.
func readYourWrites(t *testing.T, open func(*testing.T) agent.Store) {
	ctx := context.Background()
	w, r := open(t), open(t)
	id := runID(t)
	var inserted []agent.Entry
	for i := range 20 {
		e, _, err := w.Insert(ctx, id, fmt.Sprintf("e%d", i), fmt.Appendf(nil, "v%d", i))
		if err != nil {
			t.Fatal(err)
		}
		inserted = append(inserted, e)
		for _, h := range []agent.Store{w, r} {
			got, ok, err := h.Get(ctx, id, e.Name)
			if err != nil || !ok || !bytes.Equal(got.Data, e.Data) || got.Seq != e.Seq {
				t.Fatalf("Get(%q) right after its Insert = %+v, %v, %v", e.Name, got, ok, err)
			}
		}
	}
	for range 2 {
		var got []agent.Entry
		for e, err := range r.Load(ctx, id, -1) {
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, e)
		}
		if len(got) != len(inserted) {
			t.Fatalf("Load yields %d entries, want %d", len(got), len(inserted))
		}
		for i := range got {
			if got[i].Name != inserted[i].Name || got[i].Seq != inserted[i].Seq || !bytes.Equal(got[i].Data, inserted[i].Data) {
				t.Fatalf("entry %d reads back as %+v, want %+v", i, got[i], inserted[i])
			}
		}
	}
	// after skips exactly the entries at or before it.
	var rest []string
	for e, err := range r.Load(ctx, id, inserted[9].Seq) {
		if err != nil {
			t.Fatal(err)
		}
		rest = append(rest, e.Name)
	}
	if len(rest) != 10 || rest[0] != "e10" {
		t.Fatalf("Load(after the 10th entry's Seq) yields %v, want e10 to e19", rest)
	}
}

// A5: bytes come back exactly as inserted, and neither the slice the caller passed nor the one it
// got back is shared with the store.
func byteFidelity(t *testing.T, s agent.Store) {
	ctx := context.Background()
	id := runID(t)
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	big := make([]byte, 1<<20)
	_, _ = rand.Read(big)
	cases := map[string][]byte{
		"every byte":   all,
		"invalid utf8": []byte("a\xffb\xc3\x00c"),
		"json":         []byte(`{"a":"<b>","n":1.50}`),
		"one mebibyte": big,
	}
	for name, data := range cases {
		in := bytes.Clone(data)
		e, ok, err := s.Insert(ctx, id, name, in)
		if err != nil || !ok || !bytes.Equal(e.Data, data) {
			t.Fatalf("Insert(%s) = %v, %v; returned bytes equal: %v", name, ok, err, bytes.Equal(e.Data, data))
		}
		in[0] ^= 0xff     // the caller reuses its buffer
		e.Data[1] ^= 0xff // and modifies what it got back
		got, ok, err := s.Get(ctx, id, name)
		if err != nil || !ok || !bytes.Equal(got.Data, data) {
			t.Fatalf("Get(%s) after the caller modified its slices = %v, %v; bytes equal: %v", name, ok, err, bytes.Equal(got.Data, data))
		}
	}
	n := 0
	for e, err := range s.Load(ctx, id, -1) {
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(e.Data, cases[e.Name]) {
			t.Fatalf("Load returns other bytes for %s", e.Name)
		}
		n++
	}
	if n != len(cases) {
		t.Fatalf("Load yields %d entries, want %d", n, len(cases))
	}
}

// A6: an entry cannot be changed through the port: a second Insert under its name stores nothing.
func immutable(t *testing.T, s agent.Store) {
	ctx := context.Background()
	id := runID(t)
	if _, _, err := s.Insert(ctx, id, "a", []byte("first")); err != nil {
		t.Fatal(err)
	}
	e, ok, err := s.Insert(ctx, id, "a", []byte("second"))
	if err != nil || ok || string(e.Data) != "first" {
		t.Fatalf("Insert over an existing name = %q, %v, %v; want the first bytes, not stored", e.Data, ok, err)
	}
	if got, _, _ := s.Get(ctx, id, "a"); string(got.Data) != "first" {
		t.Fatalf("Get after an Insert over an existing name = %q, want first", got.Data)
	}
}

// A7: every method honors a cancelled context, and a cancelled Insert leaves its entry absent or
// complete.
func honorsContext(t *testing.T, s agent.Store) {
	id := runID(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.Insert(ctx, id, "a", []byte("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("Insert under a cancelled context = %v, want context.Canceled", err)
	}
	if _, _, err := s.Get(ctx, id, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get under a cancelled context = %v, want context.Canceled", err)
	}
	var loadErr error
	for _, err := range s.Load(ctx, id, -1) {
		loadErr = err
	}
	if !errors.Is(loadErr, context.Canceled) {
		t.Errorf("Load under a cancelled context yields %v, want context.Canceled", loadErr)
	}
	if e, ok, err := s.Get(context.Background(), id, "a"); err != nil || ok && string(e.Data) != "x" {
		t.Errorf("after a cancelled Insert, Get = %q, %v, %v; want the entry absent or complete", e.Data, ok, err)
	}
}

// fill inserts n entries into runID, named e0 to e<n-1>.
func fill(t *testing.T, s agent.Store, runID string, n int) {
	t.Helper()
	for i := range n {
		if _, _, err := s.Insert(context.Background(), runID, fmt.Sprintf("e%d", i), []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
}

// A8: breaking out of Load releases what it held: many early breaks later, the store still serves
// writes and full reads.
func breakEarly(t *testing.T, s agent.Store) {
	id := runID(t)
	const n = 600 // more than two pages of a paged store
	fill(t, s, id, n)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i := range 50 {
		for _, err := range s.Load(ctx, id, -1) {
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		if _, _, err := s.Insert(ctx, id, fmt.Sprintf("after-break-%d", i), []byte("x")); err != nil {
			t.Fatalf("Insert after %d early breaks: %v", i+1, err)
		}
	}
	count := 0
	for _, err := range s.Load(ctx, id, -1) {
		if err != nil {
			t.Fatalf("Load after early breaks: %v", err)
		}
		count++
	}
	if count != n+50 {
		t.Fatalf("Load yields %d entries, want %d", count, n+50)
	}
}

// A8: a caller may write to the run inside its Load loop without deadlocking: the store holds no
// connection, transaction or lock across a yield.
func writeInsideLoad(t *testing.T, s agent.Store) {
	id := runID(t)
	const n = 300
	fill(t, s, id, n)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	nested := 0
	for e, err := range s.Load(ctx, id, -1) {
		if err != nil {
			t.Fatalf("Load with writes inside the loop: %v", err)
		}
		if bytes.HasPrefix([]byte(e.Name), []byte("nested-")) {
			continue
		}
		name := "nested-" + e.Name
		if _, _, err := s.Insert(ctx, id, name, []byte("y")); err != nil {
			t.Fatalf("Insert inside the Load loop: %v", err)
		}
		if _, ok, err := s.Get(ctx, id, name); err != nil || !ok {
			t.Fatalf("Get inside the Load loop = %v, %v", ok, err)
		}
		nested++
	}
	if nested != n {
		t.Fatalf("the loop visited %d of the %d entries it started with", nested, n)
	}
	count := 0
	for _, err := range s.Load(ctx, id, -1) {
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 2*n {
		t.Fatalf("the run holds %d entries, want %d", count, 2*n)
	}
}
