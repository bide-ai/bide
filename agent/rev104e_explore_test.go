package agent_test

// rev104e: the claim exploration harnesses (claim_explore_test.go, claim_explore_conc_test.go)
// driven through a Durable wrapper with Unwrap() Durable (audit.AuditedStore's shape), and mixed
// with the plain Journal, so the wrapper path of ClaimAttempt, recordNotStarted and
// retryNotStarted is explored with the same faults and invariants.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/bide-ai/bide/agent"
)

// eWrap is a Durable wrapper that forwards Do and History and unwraps to the Durable beneath.
type eWrap struct{ inner agent.Durable }

func (w *eWrap) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	return w.inner.Do(ctx, runID, name, fn)
}
func (w *eWrap) History(ctx context.Context, runID string) ([]agent.Record, error) {
	return w.inner.History(ctx, runID)
}
func (w *eWrap) Unwrap() agent.Durable { return w.inner }

// eMode picks the Durable of a drive: 0 always the wrapper, 1 wrapper on even drives and the
// Journal on odd ones, 2 the reverse.
func eDurable(j *agent.Journal, mode, drive int) agent.Durable {
	switch mode {
	case 1:
		if drive%2 == 1 {
			return j
		}
	case 2:
		if drive%2 == 0 {
			return j
		}
	}
	return &eWrap{inner: j}
}

func eSubjects(mode int) []hSubject {
	step := func(rs bool) func(ctx context.Context, p *hProc, runID string, drive int) (string, error) {
		return func(ctx context.Context, p *hProc, runID string, drive int) (string, error) {
			j, err := agent.NewJournal(p)
			if err != nil {
				return "", err
			}
			var opts []agent.StepOption
			if rs {
				opts = append(opts, agent.WithSafety(agent.Safety{Idempotent: true}))
			}
			return agent.Step(ctx, eDurable(j, mode, drive), runID, "pay", func(context.Context) (string, error) { return p.fire(drive) }, opts...)
		}
	}
	tool := func(rs bool) func(ctx context.Context, p *hProc, runID string, drive int) (string, error) {
		return func(ctx context.Context, p *hProc, runID string, drive int) (string, error) {
			j, err := agent.NewJournal(p)
			if err != nil {
				return "", err
			}
			charge := agent.Func("charge", "", agent.Safety{Idempotent: rs}, func(context.Context, struct{}) (string, error) { return p.fire(drive) })
			m := agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.TextTurn("done"))
			msg, err := agent.New(m, eDurable(j, mode, drive), charge).SetMaxConcurrency(1).Run(ctx, runID, "hi")
			return msg.Text(), err
		}
	}
	return []hSubject{
		{name: "step-effect", result: "pay", runOnce: step(false)},
		{name: "step-retrysafe", retrySafe: true, result: "pay", runOnce: step(true)},
		{name: "tool-effect", result: "tool:c1", runOnce: tool(false)},
		{name: "tool-retrysafe", retrySafe: true, result: "tool:c1", runOnce: tool(true)},
	}
}

func TestRev104e_ExploreClaimProtocolThroughWrapper(t *testing.T) {
	plans := [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}}
	if !exploreFull() {
		plans = [][2]bool{{false, false}, {true, false}}
	}
	total := 0
	for mode := range 3 {
		for _, sub := range eSubjects(mode) {
			for _, plan := range plans {
				ex := &hExplorer{}
				n := 0
				counts := map[string]int{}
				examples := map[string]string{}
				for {
					viol, h := hRun(sub, plan, ex)
					n++
					for _, v := range viol {
						counts[v.kind]++
						if _, ok := examples[v.kind]; !ok {
							examples[v.kind] = v.detail + "\n      " + strings.Join(h.log, "\n      ")
						}
					}
					if !ex.next() {
						break
					}
				}
				total += n
				keys := make([]string, 0, len(counts))
				for k := range counts {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				t.Logf("mode %d %s plan(%v): %d schedules", mode, sub.name, plan, n)
				for _, k := range keys {
					if allowedViolation(k) {
						t.Logf("   %s (allowed): %d", k, counts[k])
						continue
					}
					t.Errorf("mode %d %s plan %v:  %s: %d   e.g. %s", mode, sub.name, plan, k, counts[k], examples[k])
				}
			}
		}
	}
	t.Logf("TOTAL: %d", total)
}

func eCSubjects(mode int) []cSubject {
	pick := func(ctx context.Context, j *agent.Journal) agent.Durable {
		// mode 0: always the wrapper; 1: driver 1 through the Journal, the others through the
		// wrapper; 2: driver 2 through the Journal. (Drivers are numbered from 1: mode 1 compared
		// with driver 0 before, so it repeated mode 0.)
		if mode > 0 && cDrv(ctx) == mode {
			return j
		}
		return &eWrap{inner: j}
	}
	step := func(rs bool) func(ctx context.Context, p *cProc, runID string) (string, error) {
		return func(ctx context.Context, p *cProc, runID string) (string, error) {
			j, err := agent.NewJournal(p)
			if err != nil {
				return "", err
			}
			var opts []agent.StepOption
			if rs {
				opts = append(opts, agent.WithSafety(agent.Safety{Idempotent: true}))
			}
			return agent.Step(ctx, pick(ctx, j), runID, "pay", p.fire, opts...)
		}
	}
	tool := func(rs bool) func(ctx context.Context, p *cProc, runID string) (string, error) {
		return func(ctx context.Context, p *cProc, runID string) (string, error) {
			j, err := agent.NewJournal(p)
			if err != nil {
				return "", err
			}
			charge := agent.Func("charge", "", agent.Safety{Idempotent: rs}, func(ctx context.Context, _ struct{}) (string, error) { return p.fire(ctx) })
			m := agent.NewScriptedModel(agent.ToolTurn("c1", "charge", `{}`), agent.TextTurn("done"))
			msg, err := agent.New(m, pick(ctx, j), charge).SetMaxConcurrency(1).Run(ctx, runID, "hi")
			return msg.Text(), err
		}
	}
	return []cSubject{
		{name: "step-effect", result: "pay", run: step(false)},
		{name: "tool-effect", result: "tool:c1", run: tool(false)},
	}
}

func TestRev104e_ExploreConcurrentThroughWrapper(t *testing.T) {
	// The scheduler needs a synctest bubble: synctest.Wait is its quiescence detector.
	synctest.Test(t, func(t *testing.T) {
		cFlightHooks(t)
		maxPre := 1
		if exploreFull() {
			maxPre = 2
		}
		total := 0
		for mode := range 3 {
			for _, sub := range eCSubjects(mode) {
				for topo := 0; topo < 2; topo++ {
					for p2 := 0; p2 < 2; p2++ {
						ex := &hExplorer{}
						n := 0
						counts := map[string]int{}
						examples := map[string]string{}
						for {
							viol, h := cRun(sub, topo, p2, ex, maxPre)
							cSigRecord(t, fmt.Sprintf("%d/%s/%d/%d", mode, sub.name, topo, p2), ex)
							n++
							for _, v := range viol {
								counts[v.kind]++
								if _, ok := examples[v.kind]; !ok {
									examples[v.kind] = v.detail + "\n      " + strings.Join(h.log, "\n      ")
								}
							}
							if !ex.next() {
								break
							}
						}
						total += n
						t.Logf("mode %d %s topo=%d p2=%d: %d schedules", mode, sub.name, topo, p2, n)
						keys := make([]string, 0, len(counts))
						for k := range counts {
							keys = append(keys, k)
						}
						sort.Strings(keys)
						for _, k := range keys {
							if allowedViolation(k) {
								t.Logf("   %s (allowed): %d", k, counts[k])
								continue
							}
							t.Errorf("mode %d %s topo %d p2 %d: %s: %d   e.g. %s", mode, sub.name, topo, p2, k, counts[k], examples[k])
						}
					}
				}
			}
		}
		t.Logf("TOTAL: %d", total)
	})
}
