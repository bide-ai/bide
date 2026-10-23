package capledger

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// Deterministic simulation: actors reserve, begin, seal and release against one ledger, and a
// scheduler decides, from a seed, which actor takes its next store round trip. Every actor waits for
// the scheduler before each round trip and every store call is serialized, so one seed is one
// interleaving, whatever the machine does. Processes crash at a seeded round trip (before it, or after
// it took effect with the reply lost) and restart with a fresh Ledger, as a resumed run would. After
// every actor has finished a reconcile pass runs, and the properties of model 17 are checked on what
// the store holds: NoOvershoot, Dense, CumExact, ExactlyOnce, ReleaseOnce, NeverUndercount, and that
// no slot of a sealed call is left counted.

type sched struct {
	mu     sync.Mutex
	rnd    *rand.Rand
	msgs   chan int
	grants []chan struct{}
	live   int
}

func newSched(seed uint64, actors int) *sched {
	s := &sched{rnd: rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15)), msgs: make(chan int, actors*4), live: actors}
	for range actors {
		s.grants = append(s.grants, make(chan struct{}))
	}
	return s
}

func (s *sched) turn(id int) { s.msgs <- id; <-s.grants[id] }
func (s *sched) done(id int) { s.msgs <- -1 - id }

// run grants one pending actor at a time, once every live actor is pending or done.
func (s *sched) run() {
	pending := map[int]bool{}
	for s.live > 0 {
		m := <-s.msgs
		if m >= 0 {
			pending[m] = true
		} else {
			s.live--
		}
		for s.live > 0 && len(pending) == s.live {
			ids := make([]int, 0, len(pending))
			for id := range pending {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			id := ids[s.rnd.IntN(len(ids))]
			delete(pending, id)
			s.grants[id] <- struct{}{}
			m = <-s.msgs // the granted actor ran one round trip; it is pending again or done
			if m >= 0 {
				pending[m] = true
			} else {
				s.live--
			}
		}
	}
}

// simProc is one life of an actor's process: its round trips wait for the scheduler, and the failAt-th
// fails.
type simProc struct {
	inner  Store
	sch    *sched
	id     int
	failAt int
	after  bool
	n      int
	dead   bool
}

func (p *simProc) gate(isInsert bool) (before, after bool) {
	if p.dead {
		return true, false
	}
	p.sch.turn(p.id)
	p.n++
	if p.failAt > 0 && p.n == p.failAt {
		p.dead = true
		if p.after && isInsert {
			return false, true
		}
		return true, false
	}
	return false, false
}

func (p *simProc) Insert(ctx context.Context, run, name string, data []byte) ([]byte, bool, error) {
	before, after := p.gate(true)
	if before {
		return nil, false, errCrash
	}
	b, ins, err := p.inner.Insert(ctx, run, name, data)
	if after {
		return nil, false, errCrash
	}
	return b, ins, err
}

func (p *simProc) Get(ctx context.Context, run, name string) ([]byte, bool, error) {
	if before, _ := p.gate(false); before {
		return nil, false, errCrash
	}
	return p.inner.Get(ctx, run, name)
}

func (p *simProc) Scan(ctx context.Context, run string, after int64) iter.Seq2[Item, error] {
	if before, _ := p.gate(false); before {
		return func(yield func(Item, error) bool) { yield(Item{}, errCrash) }
	}
	return p.inner.Scan(ctx, run, after)
}

func simSeeds(t *testing.T) int {
	if v := os.Getenv("BIDE_CAPLEDGER_SEEDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if testing.Short() {
		return 40
	}
	return 400
}

func TestDST_CapLedgerPropertiesOverManySeeds(t *testing.T) {
	for seed := range uint64(simSeeds(t)) {
		simulate(t, seed)
		if t.Failed() {
			t.Fatalf("seed %d failed (rerun with BIDE_CAPLEDGER_SEEDS=%d)", seed, seed+1)
		}
	}
}

func simulate(t *testing.T, seed uint64) {
	t.Helper()
	plan := rand.New(rand.NewPCG(seed, 1))
	const limit = 6
	nAttempts := 3 + plan.IntN(4)
	type attempt struct {
		id     string
		marked bool
		amount int64
		word   atomic.Int32
	}
	attempts := make([]*attempt, nAttempts)
	for i := range attempts {
		attempts[i] = &attempt{id: fmt.Sprintf("a%d", i), marked: plan.IntN(3) > 0, amount: int64(1 + plan.IntN(3))}
	}
	type actor struct {
		att     *attempt
		role    string // "driver": reserve then begin; "loop": reserve-less seal, release and void; "late": the handler that reserves after
		crashAt []int  // per life: the round trip that fails (0: none)
		after   []bool
	}
	var actors []actor
	for _, a := range attempts {
		for range 1 + plan.IntN(2) { // one or two drivers of the attempt
			actors = append(actors, actor{att: a, role: "driver"})
		}
		if a.marked && plan.IntN(2) == 0 {
			actors = append(actors, actor{att: a, role: "loop"})
		}
	}
	for i := range actors {
		for range 3 {
			c := 0
			if plan.IntN(2) == 0 {
				c = 1 + plan.IntN(10)
			}
			actors[i].crashAt = append(actors[i].crashAt, c)
			actors[i].after = append(actors[i].after, plan.IntN(2) == 0)
		}
	}
	clk := &testClock{t: day1}
	st := newMemStore()
	sch := newSched(seed, len(actors))
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded := map[string]bool{} // attempts some driver was told it holds a slot
	voided := map[string]bool{}    // attempts a loop sealed
	for i, a := range actors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer sch.done(i)
			for life := 0; life < 3; life++ {
				proc := &simProc{inner: st, sch: sch, id: i, failAt: a.crashAt[life], after: a.after[life]}
				l := newTestLedger(proc, clk)
				r := req(a.att.id, a.att.amount, limit)
				r.Marked = a.att.marked
				switch a.role {
				case "driver":
					o, err := l.Reserve(context.Background(), r)
					if errors.Is(err, errCrash) {
						continue // a new process resumes
					}
					if err != nil {
						if !errors.Is(err, ErrOverLimit) {
							t.Errorf("seed %d: %s: %v", seed, a.att.id, err)
						}
						return
					}
					// The call begins: the word moves from none to begun, unless the loop sealed it.
					if a.att.word.CompareAndSwap(wordNone, wordBegun) || a.att.word.Load() == wordBegun {
						mu.Lock()
						succeeded[a.att.id] = true
						mu.Unlock()
					}
					_ = o
					return
				case "loop":
					seal, ok := sealOf(a.att.id, &a.att.word)
					if !ok {
						return // the tool began: nothing is released
					}
					mu.Lock()
					voided[a.att.id] = true
					mu.Unlock()
					if _, err := l.Release(context.Background(), tgt(a.att.id), seal); err != nil {
						if errors.Is(err, errCrash) {
							continue
						}
						t.Errorf("seed %d: release %s: %v", seed, a.att.id, err)
					}
					return
				}
			}
		}()
	}
	sch.run()
	wg.Wait()

	run := New(Config{Store: st, Encode: testEncode}).LedgerRun(dayRule(), "2026-10-22", KeyDigest(""))
	// The reconcile pass releases the slots of sealed (voided) marked attempts that were left.
	l := newTestLedger(st, clk)
	if _, err := l.Reconcile(context.Background(), run, func(_ context.Context, s Slot) (bool, error) { return voided[s.Tool], nil }); err != nil {
		t.Errorf("seed %d: reconcile: %v", seed, err)
	}
	slots := slotsOf(t, st, run)
	checkShape(t, slots, limit)
	live, _ := l.Live(context.Background(), run)
	liveBy := map[string]bool{}
	for _, s := range live {
		liveBy[s.Tool] = true
	}
	for _, a := range attempts {
		if a.word.Load() == wordBegun && !liveBy[a.id] {
			t.Errorf("seed %d: attempt %s began and is not counted (NeverUndercount)", seed, a.id)
		}
		if a.word.Load() == wordSealed && liveBy[a.id] {
			t.Errorf("seed %d: attempt %s was sealed and its slot is still counted after reconcile", seed, a.id)
		}
	}
}
