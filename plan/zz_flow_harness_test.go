package plan_test

// Systematic fault exploration of plan lowering (review of PR #103). Every Insert a drive makes
// (journal header, run:start, flow:digest, attempt markers, not-started records, node results,
// switch choices) is a branch point with outcomes ok, error-not-committed, error-committed,
// cancel-after-ok, crash-before and crash-after. Budget: at most 2 non-ok outcomes over the 3
// faulty drives, at most 1 crash per drive. Each faulty drive runs in the process of the previous
// drive (unless it crashed) or a new one, per plan. Then clean verification drives: the same
// process (if alive), then a new one; a halt is resolved with ResolveHaltRef when the halted
// node's body fired, and the run driven again.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/plan"
)

type fOutcome int

const (
	fOK fOutcome = iota
	fErrNC
	fErrC
	fCancel
	fCrashBefore
	fCrashAfter
)

func (o fOutcome) String() string {
	return [...]string{"ok", "errNC", "errC", "cancel", "crashBefore", "crashAfter"}[o]
}

var errFFault = errors.New("injected store fault")
var errFCrash = errors.New("process crashed")

type fExplorer struct {
	prefix []int
	trace  []fPoint
}

type fPoint struct{ choice, n int }

func (ex *fExplorer) next() bool {
	t := ex.trace
	for i := len(t) - 1; i >= 0; i-- {
		if t[i].choice+1 < t[i].n {
			ex.prefix = make([]int, i+1)
			for k := 0; k < i; k++ {
				ex.prefix[k] = t[k].choice
			}
			ex.prefix[i] = t[i].choice + 1
			ex.trace = nil
			return true
		}
	}
	return false
}

type fHarness struct {
	mu     sync.Mutex
	mem    *agent.MemStore
	ex     *fExplorer
	drive  int
	events int
	crash  map[int]int
	cancel context.CancelFunc
	log    []string
	fires  []fFire
	acked  map[string]bool // not-started keys whose Insert returned ok with a not_started record
}

type fFire struct {
	key, claim string
	drive      int
}

type fProc struct {
	h       *fHarness
	crashed bool
	lastWon map[string]string // step name -> claim of the marker this process last won for it
}

func (h *fHarness) choose() fOutcome {
	if h.drive < 0 {
		return fOK
	}
	alts := []fOutcome{fOK}
	if h.events < 2 {
		alts = append(alts, fErrNC, fErrC, fCancel)
		if h.crash[h.drive] < 1 {
			alts = append(alts, fCrashBefore, fCrashAfter)
		}
	}
	k := len(h.ex.trace)
	c := 0
	if k < len(h.ex.prefix) {
		c = h.ex.prefix[k]
	}
	h.ex.trace = append(h.ex.trace, fPoint{c, len(alts)})
	o := alts[c]
	if o != fOK {
		h.events++
	}
	if o == fCrashBefore || o == fCrashAfter {
		h.crash[h.drive]++
	}
	return o
}

type fEntry struct {
	Kind      string          `json:"kind"`
	Claim     string          `json:"claim"`
	ToolUseID string          `json:"tool_use_id"`
	Result    json.RawMessage `json:"result"`
}

func fParse(b []byte) fEntry {
	var e fEntry
	_ = json.Unmarshal(b, &e)
	return e
}

func (p *fProc) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	h := p.h
	h.mu.Lock()
	if p.crashed {
		h.mu.Unlock()
		return agent.Entry{}, false, errFCrash
	}
	o := h.choose()
	if o != fOK {
		h.log = append(h.log, fmt.Sprintf("d%d %s %s", h.drive, o, name))
	}
	h.mu.Unlock()
	switch o {
	case fErrNC:
		return agent.Entry{}, false, errFFault
	case fCrashBefore:
		p.crashed = true
		return agent.Entry{}, false, errFCrash
	}
	e, ins, err := h.mem.Insert(ctx, runID, name, data)
	switch o {
	case fErrC:
		return agent.Entry{}, false, errFFault
	case fCrashAfter:
		p.crashed = true
		return agent.Entry{}, false, errFCrash
	}
	if err == nil {
		got, mine := fParse(e.Data), fParse(data)
		h.mu.Lock()
		switch {
		case strings.HasPrefix(name, "attempt:not-started:"):
			if got.Kind == "not_started" && mine.Kind == "not_started" {
				h.acked[name] = true
			}
		case strings.HasPrefix(name, "attempt:"):
			if got.Claim != "" && got.Claim == mine.Claim {
				if p.lastWon == nil {
					p.lastWon = map[string]string{}
				}
				p.lastWon[mine.ToolUseID] = mine.Claim
			}
		}
		h.mu.Unlock()
	}
	if o == fCancel && h.cancel != nil {
		h.cancel()
	}
	return e, ins, err
}

func (p *fProc) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	if p.crashed {
		return agent.Entry{}, false, errFCrash
	}
	return p.h.mem.Get(ctx, runID, name)
}

func (p *fProc) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	if p.crashed {
		return func(yield func(agent.Entry, error) bool) { yield(agent.Entry{}, errFCrash) }
	}
	return p.h.mem.Load(ctx, runID, after)
}

// fire is a node body's side effect, keyed by the node key the body runs under.
func (p *fProc) fire(key string, drive int) error {
	h := p.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if p.crashed {
		return errFCrash
	}
	claim := p.lastWon[key]
	delete(p.lastWon, key)
	h.fires = append(h.fires, fFire{key: key, claim: claim, drive: drive})
	return nil
}

// fSubject is a small flow. build returns the flow for a drive in process p; effects are the node
// keys whose body is a side effect (at most once); never are keys whose body must never run;
// values are each node key's output, for a resolution; want is the flow's output.
type fSubject struct {
	name    string
	in      int
	want    string
	effects map[string]bool
	never   map[string]bool
	values  map[string]any
	build   func(p *fProc, drive int) (*plan.Flow[int, string], error)
}

type fLoopState struct {
	N int `json:"n"`
}

func fSubjects() []fSubject {
	eff := func(ks ...string) map[string]bool {
		m := map[string]bool{}
		for _, k := range ks {
			m[k] = true
		}
		return m
	}
	return []fSubject{
		{
			name: "linear", in: 1, want: "20",
			effects: eff("node:a", "node:b"),
			values:  map[string]any{"node:a": 2, "node:b": 20, "node:c": "20"},
			build: func(p *fProc, d int) (*plan.Flow[int, string], error) {
				b := plan.New[int, string]("linear")
				a := b.Step("a", func(_ context.Context, n int) (int, error) { return n + 1, p.fire("node:a", d) })
				bb := b.Step("b", func(_ context.Context, n int) (int, error) { return n * 10, p.fire("node:b", d) })
				c := b.Step("c", func(_ context.Context, n int) (string, error) { return fmt.Sprint(n), nil }, plan.ReadOnly())
				b.Edge(a, bb)
				b.Edge(bb, c)
				return b.Build()
			},
		},
		{
			name: "switch", in: 1, want: "b2",
			effects: eff("node:a", "node:b", "node:c"),
			never:   eff("node:c"),
			values:  map[string]any{"node:a": 2, "node:b": "b2"},
			build: func(p *fProc, d int) (*plan.Flow[int, string], error) {
				b := plan.New[int, string]("switch")
				a := b.Step("a", func(_ context.Context, n int) (int, error) { return n + 1, p.fire("node:a", d) })
				bb := b.Step("b", func(_ context.Context, n int) (string, error) { return fmt.Sprintf("b%d", n), p.fire("node:b", d) })
				c := b.Step("c", func(_ context.Context, n int) (string, error) { return fmt.Sprintf("c%d", n), p.fire("node:c", d) })
				b.Switch(a, plan.When(func(v int) bool { return v > 0 }, bb), plan.Else(c))
				return b.Build()
			},
		},
		{
			name: "loop", in: 0, want: "done2",
			effects: eff("node:iter:0:inc", "node:iter:1:inc", "node:done"),
			never:   eff("node:iter:2:inc"),
			values: map[string]any{"node:iter:0:inc": fLoopState{1}, "node:iter:1:inc": fLoopState{2}, "node:done": "done2",
				"node:seed": fLoopState{0}, "node:iter:0:check": fLoopState{1}, "node:iter:1:check": fLoopState{2}},
			build: func(p *fProc, d int) (*plan.Flow[int, string], error) {
				b := plan.New[int, string]("loop")
				seed := b.Step("seed", func(_ context.Context, n int) (fLoopState, error) { return fLoopState{n}, nil }, plan.ReadOnly())
				inc := b.Step("inc", func(_ context.Context, s fLoopState) (fLoopState, error) {
					return fLoopState{s.N + 1}, p.fire(fmt.Sprintf("node:iter:%d:inc", s.N), d)
				})
				check := b.Step("check", func(_ context.Context, s fLoopState) (fLoopState, error) { return s, nil }, plan.ReadOnly())
				done := b.Step("done", func(_ context.Context, s fLoopState) (string, error) {
					return fmt.Sprintf("done%d", s.N), p.fire("node:done", d)
				})
				b.Edge(seed, inc)
				b.Edge(inc, check)
				b.Switch(check, plan.LoopBack(5, func(s fLoopState) bool { return s.N < 2 }, inc).Named("again"), plan.Else(done))
				return b.Build()
			},
		},
		{
			name: "join", in: 1, want: "z2+3",
			effects: eff("node:y", "node:z", "node:merge"),
			values:  map[string]any{"node:split": 2, "node:y": 3, "node:z": "z2", "node:merge": "z2+3"},
			build: func(p *fProc, d int) (*plan.Flow[int, string], error) {
				b := plan.New[int, string]("join")
				split := b.Step("split", func(_ context.Context, n int) (int, error) { return n * 2, nil }, plan.ReadOnly())
				y := b.Step("y", func(_ context.Context, n int) (int, error) { return n + 1, p.fire("node:y", d) })
				z := b.Step("z", func(_ context.Context, n int) (string, error) { return fmt.Sprintf("z%d", n), p.fire("node:z", d) })
				b.Edge(split, y)
				b.Edge(split, z)
				b.Join2("merge", y, z, func(_ context.Context, a int, s string) (string, error) {
					return fmt.Sprintf("%s+%d", s, a), p.fire("node:merge", d)
				})
				return b.Build()
			},
		},
		{
			name: "mixed-idempotent", in: 1, want: "20",
			effects: eff(append([]string{"node:a"}, fMut()...)...),
			values:  map[string]any{"node:a": 2, "node:b": 20, "node:c": "20"},
			build: func(p *fProc, d int) (*plan.Flow[int, string], error) {
				b := plan.New[int, string]("mixed")
				a := b.Step("a", func(_ context.Context, n int) (int, error) { return n + 1, p.fire("node:a", d) })
				bb := b.Step("b", func(_ context.Context, n int) (int, error) { return n * 10, p.fire("node:b", d) }, plan.Idempotent())
				c := b.Step("c", func(_ context.Context, n int) (string, error) { return fmt.Sprint(n), nil }, plan.ReadOnly())
				b.Edge(a, bb)
				b.Edge(bb, c)
				return b.Build()
			},
		},
	}
}

var fRunSeq int
var fResolvedTotal int
var fRunMu sync.Mutex

type fViolation struct{ kind, detail string }

func fDrive(sub fSubject, p *fProc, runID string, drive int, ctx context.Context) (string, error) {
	flow, err := sub.build(p, drive)
	if err != nil {
		return "", err
	}
	j, err := agent.NewJournal(p)
	if err != nil {
		return "", err
	}
	return flow.Run(ctx, j, runID, sub.in)
}

func fRun(sub fSubject, sameProc [2]bool, ex *fExplorer) (viol []fViolation, h *fHarness) {
	h = &fHarness{mem: agent.NewMemStore(), ex: ex, crash: map[int]int{}, acked: map[string]bool{}}
	fRunMu.Lock()
	fRunSeq++
	runID := fmt.Sprintf("f%d", fRunSeq) // pending claims are process-global: a fresh run each time
	fRunMu.Unlock()
	add := func(kind, f string, a ...any) { viol = append(viol, fViolation{kind, fmt.Sprintf(f, a...)}) }

	var p *fProc
	type res struct {
		v   string
		err error
	}
	var results []res
	for d := 0; d < 3; d++ {
		if p == nil || p.crashed || (d > 0 && !sameProc[d-1]) {
			p = &fProc{h: h}
		}
		ctx, cancel := context.WithCancel(context.Background())
		h.mu.Lock()
		h.drive, h.cancel = d, cancel
		h.mu.Unlock()
		v, err := fDrive(sub, p, runID, d, ctx)
		cancel()
		h.log = append(h.log, fmt.Sprintf("d%d -> %q, %v", d, v, err))
		results = append(results, res{v, err})
	}
	h.mu.Lock()
	h.drive = -1
	h.mu.Unlock()
	if !p.crashed {
		v, err := fDrive(sub, p, runID, 3, context.Background())
		h.log = append(h.log, fmt.Sprintf("verify same-proc -> %q, %v", v, err))
		results = append(results, res{v, err})
	}
	var final res
	resolved := 0
	for i := 0; ; i++ {
		np := &fProc{h: h}
		v, err := fDrive(sub, np, runID, 4+i, context.Background())
		h.log = append(h.log, fmt.Sprintf("verify new-proc -> %q, %v", v, err))
		final = res{v, err}
		results = append(results, final)
		halt, ok := errors.AsType[*agent.OutcomeUnknown](err)
		if !ok || i >= 6 {
			break
		}
		fired := 0
		for _, f := range h.fires {
			if f.key == halt.Op.ID {
				fired++
			}
		}
		if fired == 0 {
			// Halt with no fire: was a not-started record for its live marker acknowledged?
			ackedButLive := false
			for e, err := range h.mem.Load(context.Background(), runID, -1) {
				if err != nil {
					break
				}
				r := fParse(e.Data)
				if r.Kind == "attempt" && r.ToolUseID == halt.Op.ID && h.acked["attempt:not-started:"+r.Claim+":"+e.Name] {
					if x, ok, _ := h.mem.Get(context.Background(), runID, "attempt:not-started:"+r.Claim+":"+e.Name); !ok || fParse(x.Data).Kind != "not_started" {
						ackedButLive = true
					}
				}
			}
			if ackedButLive {
				add("I4-acked-notstarted-but-halts", "%s never fired; a not-started write was acknowledged, yet the run halts", halt.Op.ID)
			} else if !sub.effects[halt.Op.ID] {
				add("I4-retrysafe-halts", "retry-safe %s halts: %v", halt.Op.ID, err)
			} else {
				add("I4b-halt-no-durable-proof", "%s never fired; halts (no acknowledged not-started)", halt.Op.ID)
			}
			break
		}
		val, ok := sub.values[halt.Op.ID]
		if !ok {
			add("I7-unexpected-halt-key", "halt on %s", halt.Op.ID)
			break
		}
		if rerr := agent.ResolveHaltRef(context.Background(), h.mem, halt.Ref(), agent.Outcome{Result: val}, agent.WithoutLiveDriverCheck()); rerr != nil {
			add("I7-resolve-failed", "ResolveHaltRef(%s): %v", halt.Op.ID, rerr)
			break
		}
		resolved++
		fResolvedTotal++
		h.log = append(h.log, "resolved "+halt.Op.ID)
	}

	// I1: at most once per effect key; I2: every effect fired under a claim its process won.
	count := map[string]int{}
	for _, f := range h.fires {
		count[f.key]++
		if sub.effects[f.key] && f.claim == "" {
			add("I2-unclaimed-fire", "%+v", f)
		}
		if sub.never[f.key] {
			add("I3-pruned-node-fired", "%+v", f)
		}
	}
	for k, n := range count {
		if sub.effects[k] && n > 1 {
			add("I1-double-fire", "%s fired %d times: %+v", k, n, h.fires)
		}
	}
	// I5: every successful drive returns the flow's output; the final drive succeeds or halts.
	for i, r := range results {
		if r.err == nil && r.v != sub.want {
			add("I5-wrong-output", "drive %d returned %q, want %q", i, r.v, sub.want)
		}
	}
	if final.err != nil {
		if _, ok := errors.AsType[*agent.OutcomeUnknown](final.err); !ok {
			add("I6-final-error", "final clean drive failed: %v", final.err)
		}
	} else {
		// I8: a completed run conforms.
		flow, _ := sub.build(&fProc{h: h}, 9)
		ok, diffs, err := flow.Conform(context.Background(), h.mem, runID)
		if err != nil || !ok {
			add("I8-conform-false-positive", "Conform = %v %v %v", ok, diffs, err)
		}
	}
	return viol, h
}

// TestZZExploreFlowLowering explores every fault schedule within the budget above for each
// subject flow and fails on any violation of the invariants: at most one fire per effect, every
// fire under a claim its process won, no pruned node fired, a halt only where the node's effect may
// have fired or its not-started record was not acknowledged, the flow's output from every
// successful drive, a final drive that completes (or halts, and is resolved), and a completed run
// that conforms. A halt of a node that never fired, with no acknowledged not-started record
// (I4b), is the safe outcome of a process that died between its claim and its body, not a
// violation; it is counted.
//
// By default it explores every subject with every drive in a new process, and the linear subject
// also with every drive in the process of the one before (which exercises a process's remembered
// claims), which keeps it under a minute with -race; BIDE_EXPLORE=1 explores all four process
// plans for every subject, and BIDE_SUBJECT=<name> restricts it to one subject.
func TestZZExploreFlowLowering(t *testing.T) {
	full := os.Getenv("BIDE_EXPLORE") != ""
	allPlans := [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}}
	only := os.Getenv("BIDE_SUBJECT")
	total := 0
	for _, sub := range fSubjects() {
		if only != "" && sub.name != only {
			continue
		}
		plans := allPlans
		if !full {
			plans = [][2]bool{{false, false}}
			if sub.name == "linear" {
				plans = append(plans, [2]bool{true, true})
			}
		}
		for _, pl := range plans {
			ex := &fExplorer{}
			n := 0
			counts := map[string]int{}
			examples := map[string]string{}
			for {
				viol, h := fRun(sub, pl, ex)
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
			t.Logf("%s plan(same-proc d2=%v d3=%v): %d schedules", sub.name, pl[0], pl[1], n)
			for _, k := range keys {
				if k == "I4b-halt-no-durable-proof" {
					t.Logf("   %s (safe halt): %d", k, counts[k])
					continue
				}
				t.Errorf("   %s: %d   e.g. %s", k, counts[k], examples[k])
			}
		}
	}
	t.Logf("TOTAL schedules explored: %d, halts resolved: %d", total, fResolvedTotal)
}

func fMut() []string {
	if os.Getenv("BIDE_MUT") != "" {
		return []string{"node:b"}
	}
	return nil
}
