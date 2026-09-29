// Command bench is a concurrency/scale load harness: it drives N durable agent runs
// concurrently in one process against a stub model (so it measures the FRAMEWORK's
// orchestration, journaling, and concurrency overhead, not LLM latency), and reports
// throughput, latency percentiles, peak goroutines, and memory. Optional -latency simulates
// per model-call I/O wait to show how Go absorbs large concurrent I/O-bound fan-out.
//
// The journal-records figure counts the records the runs actually wrote (read back from the
// store after the timed section) and divides by the elapsed time. The store is the in-memory
// MemStore, so it is not a durable-store write rate.
//
//	go run ./cmd/bench -runs 20000 -concurrency 5000 -latency 50ms
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bide-ai/bide/agent"
)

// stubModel is concurrency-safe and stateless: it decides the turn from the message count,
// so it needs no shared cursor. First turn calls the no-op tool; the next returns final text.
type stubModel struct{ latency time.Duration }

func (m stubModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	if m.latency > 0 {
		time.Sleep(m.latency)
	}
	ch := make(chan agent.Emit, 2)
	if len(req.Messages) <= 1 {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: "c1", Name: "noop", ArgsFragment: []byte(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "done"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

func main() {
	runs := flag.Int("runs", 5000, "total agent runs")
	conc := flag.Int("concurrency", 256, "max concurrent runs")
	latency := flag.Duration("latency", 0, "simulated per model-call latency")
	flag.Parse()
	for _, f := range []struct {
		name string
		v    int
	}{{"runs", *runs}, {"concurrency", *conc}} {
		if f.v < 1 {
			fmt.Fprintf(os.Stderr, "bench: -%s must be at least 1, got %d\n", f.name, f.v)
			flag.Usage()
			os.Exit(2)
		}
	}

	tool := agent.Func("noop", "no-op", agent.Safety{ReadOnly: true},
		func(_ context.Context, _ struct{}) (string, error) { return "ok", nil })
	store := agent.NewMemStore()
	a := agent.New(stubModel{latency: *latency}, store, tool)

	// Peak-goroutine sampler.
	var peak int64
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if g := int64(runtime.NumGoroutine()); g > atomic.LoadInt64(&peak) {
					atomic.StoreInt64(&peak, g)
				}
			}
		}
	}()

	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	lat := make([]time.Duration, *runs)
	var errs int64
	sem := make(chan struct{}, *conc)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < *runs; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			t0 := time.Now()
			if _, err := a.Run(context.Background(), fmt.Sprintf("run-%d", i), "go"); err != nil {
				atomic.AddInt64(&errs, 1)
			}
			lat[i] = time.Since(t0)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(done)

	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	// Count the journal records the runs wrote, outside the timed section.
	var records int
	for i := 0; i < *runs; i++ {
		h, err := store.History(context.Background(), fmt.Sprintf("run-%d", i))
		if err != nil {
			atomic.AddInt64(&errs, 1)
			continue
		}
		records += len(h)
	}

	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pc := func(p float64) time.Duration { return lat[int(float64(len(lat)-1)*p)] }

	fmt.Printf("runs=%d concurrency=%d sim-latency=%s\n", *runs, *conc, *latency)
	fmt.Printf("elapsed=%s throughput=%.0f runs/s (%.0f journal records/s, in-memory store; %d records)\n",
		elapsed.Round(time.Millisecond), float64(*runs)/elapsed.Seconds(), float64(records)/elapsed.Seconds(), records)
	fmt.Printf("run latency: p50=%s p90=%s p99=%s max=%s\n", pc(0.50).Round(time.Microsecond), pc(0.90).Round(time.Microsecond), pc(0.99).Round(time.Microsecond), lat[len(lat)-1].Round(time.Microsecond))
	fmt.Printf("peak goroutines=%d  heap alloc delta=%.1f MB  total alloc=%.1f MB  numGC=%d\n",
		atomic.LoadInt64(&peak), float64(m1.HeapAlloc-m0.HeapAlloc)/1e6, float64(m1.TotalAlloc-m0.TotalAlloc)/1e6, m1.NumGC-m0.NumGC)
	fmt.Printf("errors=%d\n", errs)
}
