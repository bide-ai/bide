package agent_test

// Systematic fault exploration of the claim protocol. Every Insert on a claim-path key (attempt
// markers, not-started records, retry markers, the result) is a branch point with outcomes ok,
// error-not-committed, error-committed, cancel-after-ok, crash-before and crash-after. Budget per
// drive: at most 2 faults (errNC, errC, cancel) and at most 1 crash. exploreDrives() drives, each
// in the same process as the previous one (unless it crashed) or in a new one, then a clean
// verification drive in a new process (and one in the same process when the last drive did not
// crash). Every schedule is checked for the invariants hViolation names; the only outcome allowed
// among them is I4b, a halt when no durable record says the effect did not start (a halt is the
// safe answer there). The default run is bounded to fit CI; BIDE_EXPLORE=1 explores the full
// budget (minutes).

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
	"sync/atomic"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

type hOutcome int

const (
	hOK hOutcome = iota
	hErrNC
	hErrC
	hCancel
	hCrashBefore
	hCrashAfter
	hNumOutcomes
)

func (o hOutcome) String() string {
	return [...]string{"ok", "errNC", "errC", "cancel", "crashBefore", "crashAfter"}[o]
}

var errHFault = errors.New("injected store fault")
var errHCrash = errors.New("process crashed")

// hExplorer drives a stateless DFS over the branch points.
type hExplorer struct {
	mu     sync.Mutex // guards trace during a run: the scheduler and the drivers both add points
	prefix []int
	trace  []hPoint // points of the current run
}

// choose records a branch point with n alternatives and returns the alternative to take: the
// prefix's choice at this depth (0 past it, or when the prefix names one out of range).
func (e *hExplorer) choose(name string, drive, n int) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	k := len(e.trace)
	c := 0
	if k < len(e.prefix) {
		c = e.prefix[k]
	}
	if c >= n {
		c = 0
	}
	e.trace = append(e.trace, hPoint{name: name, drive: drive, choice: c, n: n})
	return c
}

// peek returns the choice the prefix makes at the next branch point (0 past it), for a point
// whose alternative must be acted on before the point is recorded (see cSched.run).
func (e *hExplorer) peek() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if k := len(e.trace); k < len(e.prefix) {
		return e.prefix[k]
	}
	return 0
}

type hPoint struct {
	name   string
	drive  int
	choice int
	n      int // alternatives available here
}

// hProc is one process: its own store value (so its own flights and pending claims) over the
// shared MemStore.
type hProc struct {
	h        *hHarness
	crashed  atomic.Bool
	lastWon  struct{ key, claim string } // the last marker this process's Insert won
	nsFailed map[string]bool             // not-started keys whose Insert failed in this process (not a crash)
}

type hHarness struct {
	mu     sync.Mutex
	mem    *agent.MemStore
	ex     *hExplorer
	drive  int // current drive, -1 for verification (no faults)
	faults map[int]int
	crash  map[int]int
	cancel context.CancelFunc
	log    []string
	fires  []hFire
	acked  map[string]string // not-started keys whose Insert returned ok -> kind returned
	result string            // name of the result key
}

type hFire struct {
	key, claim string
	drive      int
	val        string
}

func (h *hHarness) isClaimPath(name string) bool {
	return strings.HasPrefix(name, "attempt:") || name == h.result
}

func (h *hHarness) choose(name string) hOutcome {
	if h.drive < 0 || !h.isClaimPath(name) {
		return hOK
	}
	var alts []hOutcome
	alts = append(alts, hOK)
	if h.faults[h.drive] < 2 {
		alts = append(alts, hErrNC, hErrC, hCancel)
	}
	if h.crash[h.drive] < 1 {
		alts = append(alts, hCrashBefore, hCrashAfter)
	}
	c := h.ex.choose(name, h.drive, len(alts))
	o := alts[c]
	switch o {
	case hErrNC, hErrC, hCancel:
		h.faults[h.drive]++
	case hCrashBefore, hCrashAfter:
		h.crash[h.drive]++
	}
	return o
}

func hField(data []byte) (kind, claim, result string) {
	var r struct {
		Kind   string          `json:"kind"`
		Claim  string          `json:"claim"`
		Result json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(data, &r)
	return r.Kind, r.Claim, string(r.Result)
}

func (p *hProc) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	h := p.h
	h.mu.Lock()
	if p.crashed.Load() {
		h.mu.Unlock()
		return agent.Entry{}, false, errHCrash
	}
	o := h.choose(name)
	if o != hOK {
		h.log = append(h.log, fmt.Sprintf("d%d %s %s", h.drive, o, name))
	}
	h.mu.Unlock()
	if (o == hErrNC || o == hErrC) && strings.HasPrefix(name, "attempt:not-started:") {
		if p.nsFailed == nil {
			p.nsFailed = map[string]bool{}
		}
		p.nsFailed[name] = true
	}
	switch o {
	case hErrNC:
		return agent.Entry{}, false, errHFault
	case hCrashBefore:
		p.crashed.Store(true)
		return agent.Entry{}, false, errHCrash
	}
	e, ins, err := h.mem.Insert(ctx, runID, name, data)
	switch o {
	case hErrC:
		return agent.Entry{}, false, errHFault
	case hCrashAfter:
		p.crashed.Store(true)
		return agent.Entry{}, false, errHCrash
	}
	if err == nil {
		h.mu.Lock()
		if strings.HasPrefix(name, "attempt:not-started:") {
			k, _, _ := hField(e.Data)
			wk, _, _ := hField(data)
			if wk == "not_started" {
				h.acked[name] = k
			}
		} else if strings.HasPrefix(name, "attempt:") {
			_, got, _ := hField(e.Data)
			_, mine, _ := hField(data)
			if got == mine {
				p.lastWon.key, p.lastWon.claim = name, mine
			}
		}
		h.mu.Unlock()
	}
	if o == hCancel && h.cancel != nil {
		h.cancel()
	}
	return e, ins, err
}

func (p *hProc) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	if p.crashed.Load() {
		return agent.Entry{}, false, errHCrash
	}
	return p.h.mem.Get(ctx, runID, name)
}

func (p *hProc) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	if p.crashed.Load() {
		return func(yield func(agent.Entry, error) bool) { yield(agent.Entry{}, errHCrash) }
	}
	return p.h.mem.Load(ctx, runID, after)
}

// fire is the side effect: it records which claim it ran under.
func (p *hProc) fire(drive int) (string, error) {
	h := p.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if p.crashed.Load() {
		return "", errHCrash
	}
	v := fmt.Sprintf("v%d", len(h.fires)+1)
	h.fires = append(h.fires, hFire{key: p.lastWon.key, claim: p.lastWon.claim, drive: drive, val: v})
	p.lastWon.key, p.lastWon.claim = "", ""
	return v, nil
}

type hSubject struct {
	name      string
	retrySafe bool
	result    string
	runOnce   func(ctx context.Context, p *hProc, runID string, drive int) (string, error)
}

var hRunSeq int
var hRunMu sync.Mutex

func hSubjects() []hSubject {
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
			return j.Step(ctx, runID, "pay", func(context.Context) (string, error) { return p.fire(drive) }, opts...)
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
			res, err := agenttest.MustNew(m, j, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, runID, agent.UserText("hi"))
			var msg agent.Message
			if res != nil {
				msg = res.Message
			}
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

type hViolation struct {
	kind, detail string
}

// hRun runs one schedule; it returns the violations it found.
func hRun(sub hSubject, plan [2]bool, ex *hExplorer) (viol []hViolation, h *hHarness) {
	h = &hHarness{mem: agent.NewMemStore(), ex: ex, faults: map[int]int{}, crash: map[int]int{}, acked: map[string]string{}, result: sub.result}
	hRunMu.Lock()
	hRunSeq++
	runID := fmt.Sprintf("r%d", hRunSeq) // pending claims are process-global: a fresh run each time
	hRunMu.Unlock()
	var p *hProc
	type res struct {
		v   string
		err error
	}
	var results []res
	for d := 0; d < exploreDrives(); d++ {
		if p == nil || p.crashed.Load() || (d > 0 && !plan[d-1]) {
			p = &hProc{h: h}
		}
		ctx, cancel := context.WithCancel(context.Background())
		h.mu.Lock()
		h.drive, h.cancel = d, cancel
		h.mu.Unlock()
		v, err := sub.runOnce(ctx, p, runID, d)
		cancel()
		h.log = append(h.log, fmt.Sprintf("d%d -> %q, %v", d, v, err))
		results = append(results, res{v, err})
	}
	lastAlive := p
	if lastAlive.crashed.Load() {
		lastAlive = nil
	}
	h.mu.Lock()
	h.drive = -1
	h.mu.Unlock()
	// verification drives, no faults: the same process first (if alive), then a new one.
	var vres []res
	sameHalted := false
	if lastAlive != nil {
		v, err := sub.runOnce(context.Background(), lastAlive, runID, 3)
		h.log = append(h.log, fmt.Sprintf("verify same-proc -> %q, %v", v, err))
		vres = append(vres, res{v, err})
		var hl *agent.OutcomeUnknown
		sameHalted = errors.As(err, &hl)
	}
	np := &hProc{h: h}
	v, err := sub.runOnce(context.Background(), np, runID, 4)
	h.log = append(h.log, fmt.Sprintf("verify new-proc -> %q, %v", v, err))
	vres = append(vres, res{v, err})

	// Collect the journal.
	type ent struct{ name, kind, claim, result string }
	var ents []ent
	for e, err := range h.mem.Load(context.Background(), runID, -1) {
		if err != nil {
			break
		}
		k, c, r := hField(e.Data)
		ents = append(ents, ent{e.Name, k, c, r})
	}
	byName := map[string]ent{}
	for _, e := range ents {
		byName[e.name] = e
	}
	add := func(kind, f string, a ...any) { viol = append(viol, hViolation{kind, fmt.Sprintf(f, a...)}) }

	// I1: at most once.
	if !sub.retrySafe && len(h.fires) > 1 {
		add("I1-double-fire", "fired %d times: %+v", len(h.fires), h.fires)
	}
	// I2: a fire never coexists with a not-started record for its claim, and runs under a won claim.
	if !sub.retrySafe {
		for _, f := range h.fires {
			if f.claim == "" {
				add("I2-unclaimed-fire", "fire %+v under no won marker", f)
				continue
			}
			ns := "attempt:not-started:" + f.claim + ":" + f.key
			if e, ok := byName[ns]; ok && e.kind == "not_started" {
				add("I2-fired-and-voided", "fire %+v and %s is not_started", f, ns)
			}
		}
	}
	// I3: a recorded result is never replaced: every successful drive returned the stored value
	// (for Step), and the stored value came from a fire.
	stored, hasStored := byName[sub.result]
	if hasStored {
		okv := false
		for _, f := range h.fires {
			if stored.result == `"`+f.val+`"` {
				okv = true
			}
		}
		if !okv && !strings.Contains(stored.result, "not started") {
			// a tool error result is legitimate when the tool errored; the tool here never errors
			// except on a crash, which records nothing.
			add("I3-result-without-fire", "stored %s = %s, fires %+v", sub.result, stored.result, h.fires)
		}
	}
	if strings.HasPrefix(sub.name, "step") {
		for i, r := range append(results, vres...) {
			if r.err == nil && (!hasStored || `"`+r.v+`"` != stored.result) {
				add("I3-returned-other-value", "drive %d returned %q, stored %v %s", i, r.v, hasStored, stored.result)
			}
		}
	}
	if sameHalted && !sub.retrySafe && len(h.fires) == 0 {
		for _, e := range ents {
			ns := "attempt:not-started:" + e.claim + ":" + e.name
			if e.kind == "attempt" && lastAlive.nsFailed[ns] {
				if x, ok := byName[ns]; !ok || x.kind != "not_started" {
					add("I4c-sameproc-remembers-yet-halts", "the live marker's claim is remembered by the process that halts")
				}
			}
		}
	}
	// I4: liveness. The final new-process drive halts only if the effect may have run, or no
	// durable proof that it did not was ever acknowledged.
	final := vres[len(vres)-1]
	var halt *agent.OutcomeUnknown
	if errors.As(final.err, &halt) {
		if sub.retrySafe {
			add("I4-retrysafe-halts", "retry-safe subject halts: %v", final.err)
		} else if len(h.fires) == 0 {
			// Which live attempt halts it, and was a not-started write acknowledged for it?
			ackedButLive := false
			for _, e := range ents {
				if e.kind != "attempt" {
					continue
				}
				ns := "attempt:not-started:" + e.claim + ":" + e.name
				if k, ok := h.acked[ns]; ok && k != "not_started" {
					ackedButLive = true
				}
			}
			if ackedButLive {
				add("I4-acked-notstarted-but-halts", "effect never fired, a not-started write was acknowledged, yet the run halts")
			} else {
				add("I4b-halt-no-durable-proof", "effect never fired; halts (no acknowledged not-started)")
			}
		}
	} else if final.err != nil {
		add("I5-final-error", "final clean drive failed: %v", final.err)
	}
	return viol, h
}

// hNext advances the explorer; false when the space is exhausted.
func (ex *hExplorer) next() bool {
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

// exploreFull reports whether BIDE_EXPLORE asks for the full exploration.
func exploreFull() bool { return os.Getenv("BIDE_EXPLORE") != "" }

// exploreDrives is the number of faulted drives: 3 in the full exploration, 2 by default.
func exploreDrives() int {
	if exploreFull() {
		return 3
	}
	return 2
}

// allowedViolation reports whether an exploration finding is an allowed outcome rather than a
// failure: a halt with no durable proof that the effect did not start.
func allowedViolation(kind string) bool { return kind == "I4b-halt-no-durable-proof" }

func TestExploreClaimProtocol(t *testing.T) {
	plans := [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}}
	if !exploreFull() {
		plans = [][2]bool{{false, false}, {true, false}} // two drives: only the first plan entry is read
	}
	total := 0
	for _, sub := range hSubjects() {
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
			t.Logf("%s plan(same-proc d2=%v d3=%v): %d schedules", sub.name, plan[0], plan[1], n)
			for _, k := range keys {
				if allowedViolation(k) {
					t.Logf("   %s (allowed): %d", k, counts[k])
					continue
				}
				t.Errorf("   %s: %d   e.g. %s", k, counts[k], examples[k])
			}
		}
	}
	t.Logf("TOTAL schedules explored: %d", total)
}
