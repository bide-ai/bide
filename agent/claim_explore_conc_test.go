package agent_test

// Concurrent exploration of the claim protocol: phase 1 runs two drivers of one run at once, in one process (shared
// flights and pending claims) or in two; a deterministic scheduler serializes every store call
// and every effect call and explores their interleavings (bounded preemptions) together with
// faults on the claim-path Inserts. Phase 2 runs one more driver (in a surviving process of
// phase 1, or a new one) with faults; then clean verification drives.

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

type cDrvKey struct{}

func cDrv(ctx context.Context) int {
	if v, ok := ctx.Value(cDrvKey{}).(int); ok {
		return v
	}
	return -1
}

type cSched struct {
	mu      sync.Mutex
	waiting map[int]chan struct{}
	what    map[int]string
	active  map[int]bool // started and not done
	last    int
	preempt int
	maxPre  int
	ex      *hExplorer
	on      bool
	stuck   bool
}

func (s *cSched) yield(d int, what string) {
	s.mu.Lock()
	if !s.on || d < 0 || !s.active[d] {
		s.mu.Unlock()
		return
	}
	ch := make(chan struct{})
	s.waiting[d] = ch
	s.what[d] = what
	s.mu.Unlock()
	<-ch
}

func (s *cSched) point(n int) int {
	k := len(s.ex.trace)
	c := 0
	if k < len(s.ex.prefix) {
		c = s.ex.prefix[k]
	}
	if c >= n {
		c = 0
	}
	s.ex.trace = append(s.ex.trace, hPoint{name: "sched", choice: c, n: n})
	return c
}

// run drives the given drivers to completion under the scheduler.
func (s *cSched) run(drivers map[int]func()) {
	var wg sync.WaitGroup
	s.mu.Lock()
	s.on = true
	for d := range drivers {
		s.active[d] = true
	}
	s.mu.Unlock()
	for d, f := range drivers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f()
			s.mu.Lock()
			delete(s.active, d)
			s.mu.Unlock()
		}()
	}
	allDone := make(chan struct{})
	go func() { wg.Wait(); close(allDone) }()
	for {
		// quiescence: every active driver waiting, or nothing changed for a while.
		deadline := time.Now().Add(3 * time.Millisecond)
		for {
			s.mu.Lock()
			na, nw := len(s.active), len(s.waiting)
			s.mu.Unlock()
			if na == 0 {
				<-allDone
				s.mu.Lock()
				s.on = false
				s.mu.Unlock()
				return
			}
			if nw == na || (nw > 0 && time.Now().After(deadline)) {
				break
			}
			if nw == 0 && time.Now().After(deadline.Add(2*time.Second)) {
				s.mu.Lock()
				s.stuck = true
				s.mu.Unlock()
				<-allDone
				return
			}
			time.Sleep(20 * time.Microsecond)
		}
		s.mu.Lock()
		var ids []int
		for d := range s.waiting {
			ids = append(ids, d)
		}
		sort.Ints(ids)
		// default: keep running the last driver; alternatives are preemptions.
		order := ids
		if _, ok := s.waiting[s.last]; ok {
			order = []int{s.last}
			for _, d := range ids {
				if d != s.last {
					order = append(order, d)
				}
			}
		}
		n := len(order)
		if _, ok := s.waiting[s.last]; ok && s.preempt >= s.maxPre {
			n = 1
		}
		c := s.point(n)
		d := order[c]
		if c > 0 {
			s.preempt++
		}
		s.last = d
		ch := s.waiting[d]
		delete(s.waiting, d)
		s.mu.Unlock()
		close(ch)
	}
}

type cProc struct {
	h       *cHarness
	crashed atomic.Bool
}

type cHarness struct {
	mu       sync.Mutex
	mem      *agent.MemStore
	ex       *hExplorer
	sc       *cSched
	phase    int // 1, 2, or 0 for verification
	faults   map[int]int
	crashes  int
	cancels  map[int]context.CancelFunc
	lastWon  map[int][2]string
	acked    map[string]string
	nsFailed map[string]bool
	log      []string
	fires    []hFire
	result   string
}

func (h *cHarness) choose(d int, name string) hOutcome {
	if h.phase == 0 || !(strings.HasPrefix(name, "attempt:") || name == h.result) {
		return hOK
	}
	// Faults per driver: phase 1 (the concurrent drivers) 1 in the full exploration, none by
	// default (its interleavings alone); phase 2 (one more driver) 2 in the full, 1 by default.
	maxF := 0
	switch {
	case h.phase == 1 && exploreFull():
		maxF = 1
	case h.phase == 2 && exploreFull():
		maxF = 2
	case h.phase == 2:
		maxF = 1
	}
	alts := []hOutcome{hOK}
	if h.faults[d] < maxF {
		alts = append(alts, hErrNC, hErrC, hCancel)
	}
	if h.crashes < 1 {
		alts = append(alts, hCrashBefore, hCrashAfter)
	}
	k := len(h.ex.trace)
	c := 0
	if k < len(h.ex.prefix) {
		c = h.ex.prefix[k]
	}
	if c >= len(alts) {
		c = 0
	}
	h.ex.trace = append(h.ex.trace, hPoint{name: name, drive: d, choice: c, n: len(alts)})
	o := alts[c]
	switch o {
	case hErrNC, hErrC, hCancel:
		h.faults[d]++
	case hCrashBefore, hCrashAfter:
		h.crashes++
	}
	return o
}

func (p *cProc) Insert(ctx context.Context, runID, name string, data []byte) (agent.Entry, bool, error) {
	d := cDrv(ctx)
	h := p.h
	h.sc.yield(d, "insert "+name)
	h.mu.Lock()
	if p.crashed.Load() {
		h.mu.Unlock()
		return agent.Entry{}, false, errHCrash
	}
	o := h.choose(d, name)
	h.log = append(h.log, fmt.Sprintf("p%d d%d insert %s %s", h.phase, d, name, o))
	if (o == hErrNC || o == hErrC) && strings.HasPrefix(name, "attempt:not-started:") {
		h.nsFailed[name] = true
	}
	h.mu.Unlock()
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
			if wk, _, _ := hField(data); wk == "not_started" {
				h.acked[name] = k
			}
		} else if strings.HasPrefix(name, "attempt:") {
			_, got, _ := hField(e.Data)
			_, mine, _ := hField(data)
			if got == mine {
				h.lastWon[d] = [2]string{name, mine}
			}
		}
		h.mu.Unlock()
	}
	if o == hCancel {
		if c := h.cancels[d]; c != nil {
			c()
		}
	}
	return e, ins, err
}

func (p *cProc) Get(ctx context.Context, runID, name string) (agent.Entry, bool, error) {
	p.h.sc.yield(cDrv(ctx), "get "+name)
	if p.crashed.Load() {
		return agent.Entry{}, false, errHCrash
	}
	return p.h.mem.Get(ctx, runID, name)
}

func (p *cProc) Load(ctx context.Context, runID string, after int64) iter.Seq2[agent.Entry, error] {
	return func(yield func(agent.Entry, error) bool) {
		p.h.sc.yield(cDrv(ctx), "load")
		if p.crashed.Load() {
			yield(agent.Entry{}, errHCrash)
			return
		}
		// snapshot at the yield, then stream it with no scheduling inside
		var es []agent.Entry
		var lerr error
		for e, err := range p.h.mem.Load(ctx, runID, after) {
			if err != nil {
				lerr = err
				break
			}
			es = append(es, e)
		}
		for _, e := range es {
			if !yield(e, nil) {
				return
			}
		}
		if lerr != nil {
			yield(agent.Entry{}, lerr)
		}
	}
}

func (p *cProc) fire(ctx context.Context) (string, error) {
	d := cDrv(ctx)
	h := p.h
	h.sc.yield(d, "fire")
	h.mu.Lock()
	defer h.mu.Unlock()
	if p.crashed.Load() {
		return "", errHCrash
	}
	v := fmt.Sprintf("v%d", len(h.fires)+1)
	w := h.lastWon[d]
	h.fires = append(h.fires, hFire{key: w[0], claim: w[1], drive: d, val: v})
	delete(h.lastWon, d)
	h.log = append(h.log, fmt.Sprintf("p%d d%d FIRE %s under %v", h.phase, d, v, w))
	return v, nil
}

type cSubject struct {
	name      string
	retrySafe bool
	result    string
	run       func(ctx context.Context, p *cProc, runID string) (string, error)
}

func cSubjects() []cSubject {
	step := func(rs bool) func(ctx context.Context, p *cProc, runID string) (string, error) {
		return func(ctx context.Context, p *cProc, runID string) (string, error) {
			j, err := agent.NewJournal(p)
			if err != nil {
				return "", err
			}
			var opts []agent.StepOption
			if rs {
				opts = append(opts, agent.StepSafety(agent.Safety{Idempotent: true}))
			}
			return agent.Step(ctx, j, runID, "pay", p.fire, opts...)
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
			msg, err := agent.New(m, j, charge).SetMaxConcurrency(1).Run(ctx, runID, "hi")
			return msg.Text(), err
		}
	}
	return []cSubject{
		{name: "step-effect", result: "pay", run: step(false)},
		{name: "tool-effect", result: "tool:c1", run: tool(false)},
		{name: "step-retrysafe", retrySafe: true, result: "pay", run: step(true)},
		{name: "tool-retrysafe", retrySafe: true, result: "tool:c1", run: tool(true)},
	}
}

var cRunSeq int

// cRun: topo 0 = both phase-1 drivers in one process, 1 = two processes. p2 0 = phase 2 in
// driver 1's process (if alive), 1 = a new process.
func cRun(sub cSubject, topo, p2 int, ex *hExplorer, maxPre int) (viol []hViolation, h *cHarness) {
	sc := &cSched{waiting: map[int]chan struct{}{}, what: map[int]string{}, active: map[int]bool{}, ex: ex, maxPre: maxPre, last: 1}
	h = &cHarness{mem: agent.NewMemStore(), ex: ex, sc: sc, faults: map[int]int{}, cancels: map[int]context.CancelFunc{},
		lastWon: map[int][2]string{}, acked: map[string]string{}, nsFailed: map[string]bool{}, result: sub.result}
	cRunSeq++
	runID := fmt.Sprintf("c%d", cRunSeq)
	pa := &cProc{h: h}
	pb := pa
	if topo == 1 {
		pb = &cProc{h: h}
	}
	type res struct {
		v   string
		err error
	}
	out := map[int]res{}
	var omu sync.Mutex
	drive := func(d int, p *cProc) func() {
		ctx, cancel := context.WithCancel(context.WithValue(context.Background(), cDrvKey{}, d))
		h.cancels[d] = cancel
		return func() {
			v, err := sub.run(ctx, p, runID)
			cancel()
			omu.Lock()
			out[d] = res{v, err}
			omu.Unlock()
		}
	}
	h.phase = 1
	sc.run(map[int]func(){1: drive(1, pa), 2: drive(2, pb)})
	if sc.stuck {
		viol = append(viol, hViolation{"STUCK", "phase 1 deadlock"})
		return
	}
	h.phase = 2
	p3 := pa
	if p2 == 1 || pa.crashed.Load() {
		p3 = &cProc{h: h}
	}
	sc.last = 3
	sc.run(map[int]func(){3: drive(3, p3)})
	h.phase = 0
	sameHalted := false
	if !p3.crashed.Load() {
		sc.run(map[int]func(){4: drive(4, p3)})
		var hl *agent.ResumeHalt
		sameHalted = errors.As(out[4].err, &hl)
	}
	sc.run(map[int]func(){5: drive(5, &cProc{h: h})})
	for d := 1; d <= 5; d++ {
		if r, ok := out[d]; ok {
			h.log = append(h.log, fmt.Sprintf("d%d -> %q, %v", d, r.v, r.err))
		}
	}

	byName := map[string][3]string{}
	var markers [][3]string
	for e, err := range h.mem.Load(context.Background(), runID, -1) {
		if err != nil {
			break
		}
		k, c, r := hField(e.Data)
		byName[e.Name] = [3]string{k, c, r}
		if k == "attempt" {
			markers = append(markers, [3]string{e.Name, c, ""})
		}
	}
	add := func(kind, f string, a ...any) { viol = append(viol, hViolation{kind, fmt.Sprintf(f, a...)}) }
	if !sub.retrySafe {
		if len(h.fires) > 1 {
			add("I1-double-fire", "fired %d times: %+v", len(h.fires), h.fires)
		}
		for _, f := range h.fires {
			if f.claim == "" {
				add("I2-unclaimed-fire", "fire %+v under no won marker", f)
				continue
			}
			if e, ok := byName["attempt:not-started:"+f.claim+":"+f.key]; ok && e[0] == "not_started" {
				add("I2-fired-and-voided", "fire %+v", f)
			}
		}
	}
	stored, hasStored := byName[sub.result]
	if hasStored {
		okv := false
		for _, f := range h.fires {
			if stored[2] == `"`+f.val+`"` {
				okv = true
			}
		}
		if !okv {
			add("I3-result-without-fire", "stored %s, fires %+v", stored[2], h.fires)
		}
	}
	if strings.HasPrefix(sub.name, "step") {
		for d, r := range out {
			if r.err == nil && (!hasStored || `"`+r.v+`"` != stored[2]) {
				add("I3-returned-other-value", "drive %d returned %q, stored %s", d, r.v, stored[2])
			}
		}
	}
	final := out[5]
	var halt *agent.ResumeHalt
	if errors.As(final.err, &halt) {
		if sub.retrySafe {
			add("I4-retrysafe-halts", "%v", final.err)
		} else if len(h.fires) == 0 {
			acked := false
			for _, m := range markers {
				if k, ok := h.acked["attempt:not-started:"+m[1]+":"+m[0]]; ok && k != "not_started" {
					acked = true
				}
			}
			if acked {
				add("I4-acked-notstarted-but-halts", "never fired; a not-started write was acknowledged; halts")
			} else {
				add("I4b-halt-no-durable-proof", "never fired; halts")
			}
		}
	} else if final.err != nil {
		add("I5-final-error", "%v", final.err)
	}
	if sameHalted && !sub.retrySafe && len(h.fires) == 0 {
		for _, m := range markers {
			ns := "attempt:not-started:" + m[1] + ":" + m[0]
			if e, ok := byName[ns]; h.nsFailed[ns] && (!ok || e[0] != "not_started") {
				add("I4c-sameproc-knows-yet-halts", "")
				break
			}
		}
	}
	return viol, h
}

// TestExploreClaimProtocolConcurrent explores two concurrent drivers under the deterministic
// scheduler, with faults. The default run is bounded (one preemption, no faults while the two
// drivers run concurrently, one in the driver after them); set BIDE_EXPLORE=1 for the full
// exploration (two preemptions, one fault per concurrent driver, two after; minutes).
func TestExploreClaimProtocolConcurrent(t *testing.T) {
	maxPre := 1
	if exploreFull() {
		maxPre = 2
	}
	total := 0
	only := os.Getenv("BIDE_SUBJECT")
	for _, sub := range cSubjects() {
		if only != "" && only != sub.name {
			continue
		}
		for topo := 0; topo < 2; topo++ {
			for p2 := 0; p2 < 2; p2++ {
				ex := &hExplorer{}
				n := 0
				counts := map[string]int{}
				examples := map[string]string{}
				for {
					viol, h := cRun(sub, topo, p2, ex, maxPre)
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
				t.Logf("%s topo=%s phase2=%s: %d schedules", sub.name, []string{"one-process", "two-processes"}[topo], []string{"driver1-process", "new-process"}[p2], n)
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
					t.Errorf("   %s: %d   e.g. %s", k, counts[k], examples[k])
				}
			}
		}
	}
	t.Logf("TOTAL concurrent schedules explored: %d", total)
}
