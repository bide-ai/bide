package agent

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// Concurrent Insert, Get and Load on one run: one winner per name, every reader sees the winner's
// exact bytes, Load yields a gap-free prefix in Seq order, positions follow real-time order, and
// mutating input or output buffers never reaches the store.
func TestRev138c_MemStoreHammer(t *testing.T) {
	ctx := context.Background()
	for round := 0; round < 20; round++ {
		m := NewMemStore()
		const writers, names, readers = 16, 64, 8
		var winners [names]atomic.Int32
		var winSeq [names]atomic.Int64
		var wg sync.WaitGroup
		var stop atomic.Bool
		var lastDone atomic.Int64 // largest Seq of an Insert that has returned
		lastDone.Store(-1)
		errs := make(chan string, 1024)
		report := func(f string, a ...any) {
			select {
			case errs <- fmt.Sprintf(f, a...):
			default:
			}
		}
		payload := func(w, n int) []byte { return bytes.Repeat([]byte{byte('a' + w)}, 64+n) }
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for n := 0; n < names; n++ {
					in := payload(w, n)
					floor := lastDone.Load()
					e, ins, err := m.Insert(ctx, "r", fmt.Sprint("n", n), in)
					for i := range in {
						in[i] = 'X' // the caller reuses its buffer
					}
					if err != nil {
						report("insert: %v", err)
						return
					}
					if ins {
						winners[n].Add(1)
						winSeq[n].Store(e.Seq)
						if e.Seq <= floor {
							report("A2: seq %d not after completed insert %d", e.Seq, floor)
						}
						for {
							c := lastDone.Load()
							if e.Seq <= c || lastDone.CompareAndSwap(c, e.Seq) {
								break
							}
						}
					}
					if len(e.Data) != 64+n || bytes.IndexByte(e.Data, 'X') >= 0 {
						report("insert returned torn data %q", e.Data)
					}
					for i := 1; i < len(e.Data); i++ {
						if e.Data[i] != e.Data[0] {
							report("insert returned mixed data")
							break
						}
					}
					for i := range e.Data {
						e.Data[i] = 'Y' // the caller scribbles on its result
					}
				}
			}(w)
		}
		for r := 0; r < readers; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stop.Load() {
					var prev int64 = -1
					for e, err := range m.Load(ctx, "r", -1) {
						if err != nil {
							report("load: %v", err)
							return
						}
						if e.Seq != prev+1 {
							report("load gap: %d after %d", e.Seq, prev)
						}
						prev = e.Seq
						if bytes.ContainsAny(e.Data, "XY") || len(e.Data) < 64 {
							report("load torn %q", e.Data)
						}
						g, ok, err := m.Get(ctx, "r", e.Name)
						if err != nil || !ok || g.Seq != e.Seq || !bytes.Equal(g.Data, e.Data) {
							report("get disagrees with load for %s", e.Name)
						}
						e.Data[0] = 'Y'
					}
				}
			}()
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		// wait for writers only, then stop readers
		for {
			all := true
			for n := 0; n < names; n++ {
				if winners[n].Load() == 0 {
					all = false
				}
			}
			if all {
				break
			}
		}
		stop.Store(true)
		<-done
		close(errs)
		for e := range errs {
			t.Fatal(e)
		}
		for n := 0; n < names; n++ {
			if c := winners[n].Load(); c != 1 {
				t.Fatalf("name n%d won %d times", n, c)
			}
			g, ok, _ := m.Get(ctx, "r", fmt.Sprint("n", n))
			if !ok || g.Seq != winSeq[n].Load() || bytes.ContainsAny(g.Data, "XY") {
				t.Fatalf("final get n%d = %+v", n, g)
			}
		}
	}
}
