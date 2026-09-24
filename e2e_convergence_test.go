package agent_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	gsm "github.com/blackwell-systems/gsm"
	agent "github.com/dayna/go-agents"
	"github.com/dayna/go-agents/audit"
	"github.com/dayna/go-agents/govern"
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

func rotate(s []string, n int) []string {
	if len(s) == 0 {
		return s
	}
	n %= len(s)
	out := make([]string, 0, len(s))
	return append(append(out, s[n:]...), s[:n]...)
}

// TestE2E_ManyAgentsConvergeAndAreTraceable is the combined proof: N concurrent governed agents,
// each applying the SAME multiset of events in a DIFFERENT order, all (a) converge to the same
// machine-checked normal form and (b) produce a governed action that verifies offline against a
// signed tree head. It exercises scale, convergence (order-independence under real concurrency,
// including a compensating cap), and cryptographic traceability in one test. Use -short to run a
// smaller N.
func TestE2E_ManyAgentsConvergeAndAreTraceable(t *testing.T) {
	n := 5000
	if testing.Short() {
		n = 200
	}
	ctx := context.Background()

	// One built, verified machine shared by all agents (Build is the convergence guarantee).
	r := gsm.NewRegistry("cap")
	a := r.Int("a", 0, 5)
	b := r.Int("b", 0, 5)
	r.DeclInvariant("a_cap", gsm.Le(gsm.V(a), gsm.Lit(3)), gsm.Do(gsm.Set(a, gsm.Lit(3))))
	r.DeclEvent("inc_a", gsm.Do(gsm.Set(a, gsm.Add(gsm.V(a), gsm.Lit(1)))))
	r.DeclEvent("inc_b", gsm.Do(gsm.Set(b, gsm.Add(gsm.V(b), gsm.Lit(1)))))
	m, rep, err := r.Build()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, rep)
	}
	digest, err := r.PolicyDigest()
	if err != nil {
		t.Fatalf("PolicyDigest: %v", err)
	}

	// The event multiset every agent applies: four inc_a (capped at 3) and one inc_b. The normal
	// form is a=3, b=1 regardless of order.
	base := []string{"inc_a", "inc_a", "inc_a", "inc_a", "inc_b"}
	ref := govern.New(m, m.NewState())
	for _, e := range base {
		if _, err := ref.Apply(ctx, e); err != nil {
			t.Fatalf("ref apply: %v", err)
		}
	}
	want := ref.State().Digest()
	if ref.State().GetInt(a) != 3 || ref.State().GetInt(b) != 1 {
		t.Fatalf("unexpected normal form: a=%d b=%d", ref.State().GetInt(a), ref.State().GetInt(b))
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	store := agent.NewMemStore()

	digests := make([]string, n)
	var runErrs, auditErrs int64
	var wg sync.WaitGroup
	sem := make(chan struct{}, 512)
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()

			gov := govern.New(m, m.NewState()) // this agent's own governed state
			tools := []agent.Tool{
				govern.AttestedEventTool(gov, "inc_a", "increment a (capped)", "inc_a", digest, agent.Safety{}),
				govern.AttestedEventTool(gov, "inc_b", "increment b", "inc_b", digest, agent.Safety{}),
			}
			ag := agent.New(seqModel{events: rotate(base, i)}, store, tools...)
			runID := fmt.Sprintf("run-%d", i)
			if _, err := ag.Run(ctx, runID, "go"); err != nil {
				atomic.AddInt64(&runErrs, 1)
				return
			}
			digests[i] = gov.State().Digest()

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

	if runErrs != 0 {
		t.Fatalf("%d/%d agent runs failed", runErrs, n)
	}
	if auditErrs != 0 {
		t.Fatalf("%d/%d runs failed the traceability check (proof did not verify)", auditErrs, n)
	}
	// Convergence: every agent reached the same machine-checked normal form.
	for i, d := range digests {
		if d != want {
			t.Fatalf("agent %d did not converge: got %s, want %s", i, d, want)
		}
	}
}
