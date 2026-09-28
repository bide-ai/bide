package agent_test

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/govern"
	gsm "github.com/blackwell-systems/gsm"
)

// seqModel drives one agent through a fixed sequence of governed events (one tool call per turn),
// then a final text turn. It is stateless per request (it derives how far along it is from the
// message count) and therefore concurrency-safe; each agent gets its own seqModel with its own
// event order.
type seqModel struct{ events []string }

func (m seqModel) Stream(_ context.Context, req agent.Request) (*agent.Stream, error) {
	round := (len(req.Messages) - 1) / 2 // one user msg, then +2 (assistant tool-call, tool result) per round
	ch := make(chan agent.Emit, 2)
	if round < len(m.events) {
		ch <- agent.Emit{Event: agent.ToolCallDelta{Index: 0, ID: fmt.Sprintf("call-%d", round), Name: m.events[round], ArgsFragment: []byte(`{}`)}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "tool_use"}}
	} else {
		ch <- agent.Emit{Event: agent.TextDelta{Text: "done"}}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// shuffled returns a random permutation of s, seeded by n so each agent gets a different but
// reproducible order. Because the events genuinely conflict on the capped variable, different
// orders exercise the compensation firing at different (random) points.
func shuffled(s []string, n int) []string {
	out := append([]string(nil), s...)
	rng := rand.New(rand.NewPCG(uint64(n)+1, 0x9e3779b97f4a7c15))
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestE2E_ManyAgentsConvergeAndAreTraceable is the combined proof: many concurrent governed
// agents, each applying the SAME multiset of events in a DIFFERENT order, all (a) converge to the
// same machine-checked normal form and (b) produce a governed action that verifies offline against
// a signed tree head. It exercises scale, convergence (order-independence under real concurrency,
// including a compensating cap), and cryptographic traceability in one test, at 5k/10k/20k agents.
// Use -short for a single small scale.
func TestE2E_ManyAgentsConvergeAndAreTraceable(t *testing.T) {
	scales := []int{5000, 10000, 20000}
	if testing.Short() {
		scales = []int{200}
	}
	// The huge tiers are gated by wall time, not the default suite: each run uses its own store
	// that is dropped after its proof verifies, so memory stays bounded by the in-flight set
	// regardless of total N. E2E_HUGE=1 adds 100k; E2E_HUGE=million adds 100k and 1,000,000.
	switch os.Getenv("E2E_HUGE") {
	case "1":
		scales = append(scales, 100000)
	case "million":
		scales = append(scales, 100000, 1000000)
	case "tenmillion":
		scales = append(scales, 10000000)
	}
	ctx := context.Background()

	// One built, verified machine shared by all agents (Build is the convergence guarantee).
	// Two capped counters with DISTINCT additive events (+1 and +2) plus a boolean flag, so the
	// compensating cap fires at genuinely different points depending on the order events arrive,
	// yet every ordering still reaches the same valid normal form. Build succeeding is the proof
	// that these events commute after compensation (WFC + CC); it stays in the commuting regime
	// (a fully non-commuting case, e.g. ship-before-pay, deliberately does not converge under
	// arbitrary order and would be rejected here).
	r := gsm.NewRegistry("cap")
	a := r.Int("a", 0, 9)
	b := r.Int("b", 0, 9)
	flag := r.Bool("flag")
	r.DeclInvariant("a_cap", gsm.Le(gsm.V(a), gsm.Lit(5)), gsm.Do(gsm.Set(a, gsm.Lit(5))))
	r.DeclInvariant("b_cap", gsm.Le(gsm.V(b), gsm.Lit(5)), gsm.Do(gsm.Set(b, gsm.Lit(5))))
	r.DeclEvent("inc_a", gsm.Do(gsm.Set(a, gsm.Add(gsm.V(a), gsm.Lit(1)))))
	r.DeclEvent("add2_a", gsm.Do(gsm.Set(a, gsm.Add(gsm.V(a), gsm.Lit(2)))))
	r.DeclEvent("inc_b", gsm.Do(gsm.Set(b, gsm.Add(gsm.V(b), gsm.Lit(1)))))
	r.DeclEvent("raise_flag", gsm.Do(gsm.Set(flag, gsm.Lit(1))))
	m, rep, err := r.Build()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, rep)
	}
	digest, err := r.PolicyDigest()
	if err != nil {
		t.Fatalf("PolicyDigest: %v", err)
	}

	// The event multiset every agent applies, in a RANDOM per-agent order. On a: +1+2+2+1 = 6
	// against a cap of 5, so in EVERY ordering the a-cap invariant is violated at some step (a
	// transiently reaches 6 or 7) and the compensation repairs it: violations happen, at a random
	// point, in every run. On b: +1+1 = 2. flag: raised. Yet every ordering still reaches the same
	// valid normal form: a=5, b=2, flag=true. That is the claim: random violation-inducing orders,
	// all converging.
	base := []string{"inc_a", "add2_a", "add2_a", "inc_a", "inc_b", "inc_b", "raise_flag"}
	ref := govern.New(m, m.NewState())
	for _, e := range base {
		if _, err := ref.Apply(ctx, e); err != nil {
			t.Fatalf("ref apply: %v", err)
		}
	}
	want := ref.State().Digest()
	if ref.State().GetInt(a) != 5 || ref.State().GetInt(b) != 2 || !ref.State().GetBool(flag) {
		t.Fatalf("unexpected normal form: a=%d b=%d flag=%v", ref.State().GetInt(a), ref.State().GetInt(b), ref.State().GetBool(flag))
	}

	for _, n := range scales {
		n := n
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			runScale(t, ctx, m, base, digest, want, n)
		})
	}
}

func runScale(t *testing.T, ctx context.Context, m *gsm.Machine, base []string, digest, want string, n int) {
	pub, priv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}

	concurrency := n
	if concurrency > 2048 {
		concurrency = 2048
	}

	var runErrs, convErrs, auditErrs, peakG int64
	sampleDone := make(chan struct{})
	go func() {
		tk := time.NewTicker(3 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-sampleDone:
				return
			case <-tk.C:
				if g := int64(runtime.NumGoroutine()); g > atomic.LoadInt64(&peakG) {
					atomic.StoreInt64(&peakG, g)
				}
			}
		}
	}()

	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			gov := govern.New(m, m.NewState()) // this agent's own governed state
			tools := []agent.Tool{
				govern.AttestedEventTool(gov, "inc_a", "a += 1 (capped)", "inc_a", digest, agent.Safety{}),
				govern.AttestedEventTool(gov, "add2_a", "a += 2 (capped)", "add2_a", digest, agent.Safety{}),
				govern.AttestedEventTool(gov, "inc_b", "b += 1 (capped)", "inc_b", digest, agent.Safety{}),
				govern.AttestedEventTool(gov, "raise_flag", "raise flag", "raise_flag", digest, agent.Safety{}),
			}
			// Each agent gets its own store, dropped when this goroutine returns (the journal
			// would go to a durable store in production). Memory stays bounded by the in-flight
			// set, not total N, so this isolates runtime scaling from store capacity.
			store := agent.NewMemStore()
			ag := agent.New(seqModel{events: shuffled(base, i)}, store, tools...)
			runID := "run"
			if _, err := ag.Run(ctx, runID, "go"); err != nil {
				atomic.AddInt64(&runErrs, 1)
				return
			}
			// Convergence: this agent reached the same machine-checked normal form. Checked inline
			// (not accumulated) so memory stays bounded even at ten million agents.
			if gov.State().Digest() != want {
				atomic.AddInt64(&convErrs, 1)
				return
			}

			// Traceability: sign a tree head over this run's journal and prove one governed action.
			th, err := audit.NewTreeHead(ctx, store, runID, 1)
			if err != nil {
				atomic.AddInt64(&auditErrs, 1)
				return
			}
			sth := audit.SignTreeHead(th, priv)
			bundle, err := audit.ProveToolCall(ctx, store, runID, "call-0", sth)
			if err != nil {
				atomic.AddInt64(&auditErrs, 1)
				return
			}
			if ok, err := bundle.Verify(pub); err != nil || !ok {
				atomic.AddInt64(&auditErrs, 1)
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(sampleDone)

	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	if runErrs != 0 {
		t.Fatalf("%d/%d agent runs failed", runErrs, n)
	}
	if convErrs != 0 {
		t.Fatalf("%d/%d agents did not converge to the normal form", convErrs, n)
	}
	if auditErrs != 0 {
		t.Fatalf("%d/%d runs failed the traceability check (proof did not verify)", auditErrs, n)
	}
	t.Logf("N=%d: converged + traceable in %s (%.0f agents/s), peak goroutines=%d, heap in use=%.0f MB, total alloc=%.0f MB",
		n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds(),
		atomic.LoadInt64(&peakG), float64(m1.HeapAlloc)/1e6, float64(m1.TotalAlloc-m0.TotalAlloc)/1e6)
}
