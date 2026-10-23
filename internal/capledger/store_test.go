package capledger

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"
)

// memStore is a Store in memory: one winner per name, entries in commit order. ops counts the store
// round trips, and the wrappers below fail a chosen one.
type memStore struct {
	mu   sync.Mutex
	runs map[string][]Item
	seq  int64
}

func newMemStore() *memStore { return &memStore{runs: map[string][]Item{}} }

func (m *memStore) Insert(_ context.Context, run, name string, data []byte) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range m.runs[run] {
		if it.Name == name {
			return it.Data, false, nil
		}
	}
	m.seq++
	m.runs[run] = append(m.runs[run], Item{Seq: m.seq, Name: name, Data: append([]byte(nil), data...)})
	return data, true, nil
}

func (m *memStore) Get(_ context.Context, run, name string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, it := range m.runs[run] {
		if it.Name == name {
			return it.Data, true, nil
		}
	}
	return nil, false, nil
}

func (m *memStore) Scan(_ context.Context, run string, after int64) iter.Seq2[Item, error] {
	m.mu.Lock()
	var items []Item
	for _, it := range m.runs[run] {
		if it.Seq > after {
			items = append(items, it)
		}
	}
	m.mu.Unlock()
	return func(yield func(Item, error) bool) {
		for _, it := range items {
			if !yield(it, nil) {
				return
			}
		}
	}
}

// names lists the entry names of a run, in commit order.
func (m *memStore) names(run string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, it := range m.runs[run] {
		out = append(out, it.Name)
	}
	return out
}

// total is the number of entries in every run.
func (m *memStore) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.runs {
		n += len(r)
	}
	return n
}

var errCrash = errors.New("crash")

// faultStore fails the failAt-th store round trip (1-based; 0 never): before the operation takes effect,
// or, for an Insert with after set, after it did (the reply is lost). Every later round trip fails too:
// the process is dead.
type faultStore struct {
	inner  Store
	mu     sync.Mutex
	n      int
	failAt int
	after  bool
	dead   bool
}

func (f *faultStore) step(isInsert bool) (failBefore, failAfter bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dead {
		return true, false
	}
	f.n++
	if f.failAt > 0 && f.n == f.failAt {
		f.dead = true
		if f.after && isInsert {
			return false, true
		}
		return true, false
	}
	return false, false
}

func (f *faultStore) Insert(ctx context.Context, run, name string, data []byte) ([]byte, bool, error) {
	before, after := f.step(true)
	if before {
		return nil, false, errCrash
	}
	b, ins, err := f.inner.Insert(ctx, run, name, data)
	if after {
		return nil, false, errCrash
	}
	return b, ins, err
}

func (f *faultStore) Get(ctx context.Context, run, name string) ([]byte, bool, error) {
	if before, _ := f.step(false); before {
		return nil, false, errCrash
	}
	return f.inner.Get(ctx, run, name)
}

func (f *faultStore) Scan(ctx context.Context, run string, after int64) iter.Seq2[Item, error] {
	if before, _ := f.step(false); before {
		return func(yield func(Item, error) bool) { yield(Item{}, errCrash) }
	}
	return f.inner.Scan(ctx, run, after)
}

// failStore fails every round trip: a store that is down.
type failStore struct{}

func (failStore) Insert(context.Context, string, string, []byte) ([]byte, bool, error) {
	return nil, false, errCrash
}
func (failStore) Get(context.Context, string, string) ([]byte, bool, error) { return nil, false, errCrash }
func (failStore) Scan(context.Context, string, int64) iter.Seq2[Item, error] {
	return func(yield func(Item, error) bool) { yield(Item{}, errCrash) }
}

var testEncode = strings.NewReplacer("%", "%25", "/", "%2F", ":", "%3A", ">", "%3E", "@", "%40", "#", "%23").Replace

// testClock is a clock a test moves.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) Set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func newTestLedger(st Store, c *testClock) *Ledger {
	return New(Config{Store: st, Encode: testEncode, Clock: c.Now})
}

var day1 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func dayRule() Rule { return Rule{Namespace: "prod", Name: "refund_total", Currency: "USD", Kind: Day} }

func req(attempt string, amount, limit int64) Request {
	return Request{Rule: dayRule(), KeyDigest: KeyDigest(""), RunID: "run-" + attempt, RootRunID: "root", Attempt: attempt,
		Marked: true, Amount: amount, Limit: limit}
}

func mustReserve(t *testing.T, l *Ledger, r Request) Outcome {
	t.Helper()
	o, err := l.Reserve(context.Background(), r)
	if err != nil {
		t.Fatalf("Reserve(%s): %v", r.Attempt, err)
	}
	return o
}

// slotsOf decodes a ledger run from the store, in slot order.
func slotsOf(t *testing.T, st Store, run string) []Slot {
	t.Helper()
	var out []Slot
	err := New(Config{Store: st, Encode: testEncode, Clock: time.Now}).read(context.Background(), run, func(v *view) error {
		out = append([]Slot(nil), v.slots...)
		return nil
	})
	if err != nil {
		t.Fatalf("read %s: %v", run, err)
	}
	return out
}

// checkShape asserts what every ledger must be, whatever happened to it: no gap (Dense), each cum_after
// the one before plus or minus the slot's amount (CumExact), at most one live slot per attempt
// (ExactlyOnce), at most one release per slot (ReleaseOnce), and the live slots without a waiver within
// limit (NoOvershoot).
func checkShape(t *testing.T, slots []Slot, limit int64) {
	t.Helper()
	var cum int64
	released := map[int]int{}
	live := map[string]int{}
	for i, s := range slots {
		if s.N != i {
			t.Fatalf("slot %d is numbered %d (a gap)", i, s.N)
		}
		if s.Kind == kindReserve {
			cum += s.Amount
		} else {
			cum -= s.Amount
			released[s.Ref]++
			if released[s.Ref] > 1 {
				t.Fatalf("slot %d released twice", s.Ref)
			}
		}
		if s.CumAfter != cum {
			t.Fatalf("slot %d: cum_after %d, want %d", i, s.CumAfter, cum)
		}
	}
	var sum, over int64
	for _, s := range slots {
		if s.Kind == kindReserve && released[s.N] == 0 {
			live[s.Attempt]++
			if live[s.Attempt] > 1 {
				t.Fatalf("attempt %s holds two live slots", s.Attempt)
			}
			sum += s.Amount
			if s.Over {
				over += s.Amount
			}
		}
	}
	if sum-over > limit {
		t.Fatalf("live slots hold %d of which %d waived, limit %d", sum, over, limit)
	}
}
