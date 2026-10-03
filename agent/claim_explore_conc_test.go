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
	"testing/synctest"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

type cDrvKey struct{}

func cDrv(ctx context.Context) int {
	if v, ok := ctx.Value(cDrvKey{}).(int); ok {
		return v
	}
	return -1
}

// The scheduler runs one driver at a time. A driver runs until it parks: at a hook (a store call
// or an effect call, the points the exploration interleaves), at a join of another driver's step
// call in flight in its process (it would wait there for that call), or at the end of a call it
// owned that another driver joined. Quiescence is exact: synctest.Wait returns once every other
// goroutine of the bubble is durably blocked, so the driver let run has parked, finished, or
// blocked inside the agent. No timers decide anything.
//
// A join is where the drivers would race without the scheduler: when a shared call returns, its
// owner and its joiners all resume, and whichever runs first may start the next shared call,
// which the others then join. The scheduler turns that race into a branch point (race[...]): one
// of them runs first, until it parks, then the others in order. It costs no preemption, so it
// covers both outcomes of every such race where the timing-based scheduler covered whichever
// the machine's timing picked.

// cPark kinds.
const (
	cAtHook  = iota // at a store or effect call: a candidate for the next decision
	cAtStart        // started, not yet run
	cAtJoin         // joined another driver's call in flight, and would wait for it to return
	cAtEnd          // owned a call that others joined; it has just returned
)

type cPark struct {
	ch       chan struct{}
	what     string
	kind     int
	returned func() bool // cAtJoin: whether the joined call has returned
	claimed  bool        // cAtJoin: queued by the end of the joined call
	joiners  []int       // cAtEnd: the drivers that joined the call and are released after it
}

type cSched struct {
	mu      sync.Mutex
	parked  map[int]*cPark
	active  map[int]bool // started and not done
	running int          // the driver let run; the others are parked or done
	queue   []int        // parked (not at a hook) drivers to let run, in order, before the next decision
	last    int
	preempt int
	maxPre  int
	ex      *hExplorer
	on      bool
	stuck   bool
	races   int   // race branch points recorded
	start   []int // the starters, while their start branch point is to be recorded
	startC  int   // the starter run first
	late    int   // joiners released after a call whose owner decoded nothing after it (an error)
}

// cCur is the scheduler the flight hooks park drivers for (see SetExploreFlightHooks).
var cCur atomic.Pointer[cSched]

// cFlightHooks installs the flight hooks for the test and removes them when it ends.
func cFlightHooks(t *testing.T) {
	restore := agent.SetExploreFlightHooks(
		func(returned func() bool) {
			if s := cCur.Load(); s != nil {
				s.join(returned)
			}
		},
		func() {
			if s := cCur.Load(); s != nil {
				s.decoded()
			}
		})
	t.Cleanup(restore)
}

// park parks driver d (s.mu held; released) until the scheduler lets it run.
func (s *cSched) park(d int, p *cPark) {
	if _, dup := s.parked[d]; dup {
		s.mu.Unlock()
		panic(fmt.Sprintf("cSched: driver %d parks at %q while parked at %q", d, p.what, s.parked[d].what))
	}
	p.ch = make(chan struct{})
	s.parked[d] = p
	s.mu.Unlock()
	<-p.ch
}

// yield is the hook at a store or effect call of the driver ctx names.
func (s *cSched) yield(d int, what string) {
	s.mu.Lock()
	if !s.on || d < 0 || !s.active[d] {
		s.mu.Unlock()
		return
	}
	if d != s.running {
		s.mu.Unlock()
		panic(fmt.Sprintf("cSched: driver %d reached %q while driver %d runs", d, what, s.running))
	}
	s.park(d, &cPark{what: what, kind: cAtHook})
}

// join is the hook of the running driver joining a call in flight.
func (s *cSched) join(returned func() bool) {
	s.mu.Lock()
	if !s.on || !s.active[s.running] {
		s.mu.Unlock()
		return
	}
	s.park(s.running, &cPark{what: "join", kind: cAtJoin, returned: returned})
}

// decoded is the hook at a record decode by the running driver. Right after a call it owned
// returns, the owner decodes its outcome: if drivers joined the call, the owner parks there, and
// the scheduler decides who runs first.
func (s *cSched) decoded() {
	s.mu.Lock()
	d := s.running
	if !s.on || !s.active[d] {
		s.mu.Unlock()
		return
	}
	js := s.returnedJoiners()
	if len(js) == 0 {
		s.mu.Unlock()
		return
	}
	for _, j := range js {
		s.parked[j].claimed = true
	}
	s.park(d, &cPark{what: "flight-end", kind: cAtEnd, joiners: js})
}

// returnedJoiners are the drivers parked at a join whose call has returned and that no end has
// queued yet, in order (s.mu held).
func (s *cSched) returnedJoiners() []int {
	var js []int
	for j, p := range s.parked {
		if p.kind == cAtJoin && !p.claimed && p.returned() {
			js = append(js, j)
		}
	}
	sort.Ints(js)
	return js
}

// point records a scheduling decision among order (the drivers at a hook, the default first).
// Its name spells the drivers and their pending calls, so the trace of a run is its schedule
// signature (see cSigRecord).
func (s *cSched) point(order []int, n int) int {
	parts := make([]string, len(order))
	for i, d := range order {
		parts[i] = fmt.Sprintf("d%d:%s", d, s.parked[d].what)
	}
	return s.ex.choose("sched["+strings.Join(parts, ",")+"]", 0, n)
}

// release lets parked driver d run (s.mu held).
func (s *cSched) release(d int) {
	p := s.parked[d]
	delete(s.parked, d)
	s.running = d
	close(p.ch)
}

// cStuckAfter is how long, on the bubble's fake clock, every active driver may stay blocked
// outside the hooks before run reports a deadlock. It costs no real time.
const cStuckAfter = 10 * time.Minute

// run drives the given drivers to completion under the scheduler. It must run in a synctest
// bubble. The drivers start one at a time, in order.
func (s *cSched) run(drivers map[int]func()) {
	var wg sync.WaitGroup
	s.mu.Lock()
	s.on = true
	s.queue = s.queue[:0]
	for d := range drivers {
		s.active[d] = true
		s.queue = append(s.queue, d)
	}
	sort.Ints(s.queue)
	// The drivers start at once: which runs first matters when one joins a call another started
	// (they raced to start it). The branch is recorded once they have all run to their first park,
	// with alternatives only if a starter joined; the replayed choice is read ahead to pick the
	// order.
	s.start = append([]int(nil), s.queue...)
	s.startC = s.ex.peek()
	if s.startC >= len(s.start) {
		s.startC = 0
	}
	s.queue = append([]int{s.start[s.startC]}, append(append([]int(nil), s.start[:s.startC]...), s.start[s.startC+1:]...)...)
	s.running = -1
	s.mu.Unlock()
	cCur.Store(s)
	defer cCur.Store(nil)
	for d, f := range drivers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.mu.Lock()
			s.park(d, &cPark{what: "start", kind: cAtStart})
			f()
			s.mu.Lock()
			delete(s.active, d)
			s.mu.Unlock()
		}()
	}
	allDone := make(chan struct{})
	go func() { wg.Wait(); close(allDone) }()
	var blockedSince time.Time
	for {
		synctest.Wait()
		s.mu.Lock()
		if len(s.active) == 0 {
			s.on = false
			s.mu.Unlock()
			<-allDone
			return
		}
		// Drivers queued to run before the next decision: the starters, and the drivers of a
		// race, in the order the race branch chose.
		if len(s.queue) > 0 {
			d := s.queue[0]
			s.queue = s.queue[1:]
			if _, ok := s.parked[d]; ok {
				s.release(d)
			}
			s.mu.Unlock()
			blockedSince = time.Time{}
			continue
		}
		if s.start != nil {
			n := 1
			if _, p := s.parkedAt(cAtJoin); p != nil || s.startC > 0 {
				n = len(s.start)
			}
			parts := make([]string, len(s.start))
			for i, d := range s.start {
				parts[i] = fmt.Sprintf("d%d", d)
			}
			if n > 1 {
				s.races++
			}
			s.ex.choose("race[start:"+strings.Join(parts, ",")+"]", 0, n)
			s.start = nil
			s.mu.Unlock()
			continue
		}
		// The end of a joined call: its owner and its joiners race; branch on who runs first.
		if e, p := s.parkedAt(cAtEnd); p != nil {
			order := append([]int{e}, p.joiners...)
			parts := make([]string, len(order))
			for i, d := range order {
				parts[i] = fmt.Sprintf("d%d", d)
			}
			c := s.ex.choose("race["+strings.Join(parts, ",")+"]", 0, len(order))
			s.races++
			s.queue = append(s.queue, order[c])
			for i, d := range order {
				if i != c {
					s.queue = append(s.queue, d)
				}
			}
			s.mu.Unlock()
			continue
		}
		// A joined call that returned with no decode after it (it failed): its joiners run now,
		// after the owner, as they would have woken while it went on.
		if js := s.returnedJoiners(); len(js) > 0 {
			for _, j := range js {
				s.parked[j].claimed = true
			}
			s.late++
			s.queue = append(s.queue, js...)
			s.mu.Unlock()
			continue
		}
		var ids []int
		for d, p := range s.parked {
			if p.kind == cAtHook {
				ids = append(ids, d)
			}
		}
		if len(ids) == 0 {
			// Every active driver is blocked outside the hooks, or at a join of a call whose owner
			// is. A driver on a timer wakes when the bubble's clock advances, which this sleep
			// does at once (the clock is fake); a driver blocked for good is a deadlock, reported
			// once the fake clock has moved on by cStuckAfter with no driver at a hook.
			s.mu.Unlock()
			if blockedSince.IsZero() {
				blockedSince = time.Now()
			}
			if time.Since(blockedSince) > cStuckAfter {
				s.mu.Lock()
				s.stuck = true
				s.on = false
				s.mu.Unlock()
				return
			}
			time.Sleep(time.Second)
			continue
		}
		blockedSince = time.Time{}
		sort.Ints(ids)
		// default: keep running the last driver; alternatives are preemptions.
		_, lastAt := s.parked[s.last]
		lastAt = lastAt && s.parked[s.last].kind == cAtHook
		order := ids
		if lastAt {
			order = []int{s.last}
			for _, d := range ids {
				if d != s.last {
					order = append(order, d)
				}
			}
		}
		n := len(order)
		if lastAt && s.preempt >= s.maxPre {
			n = 1
		}
		c := s.point(order, n)
		d := order[c]
		if c > 0 {
			s.preempt++
		}
		s.last = d
		s.release(d)
		s.mu.Unlock()
	}
}

// parkedAt returns the lowest driver parked with the given kind, and its park (s.mu held).
func (s *cSched) parkedAt(kind int) (int, *cPark) {
	best, bp := -1, (*cPark)(nil)
	for d, p := range s.parked {
		if p.kind == kind && (bp == nil || d < best) {
			best, bp = d, p
		}
	}
	return best, bp
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
	c := h.ex.choose(name, d, len(alts))
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
				opts = append(opts, agent.WithSafety(agent.Safety{Idempotent: true}))
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
			res, err := agenttest.MustNew(m, j, agent.WithTools(charge), agent.WithMaxConcurrency(1)).Run(ctx, runID, agent.UserText("hi"))
			var msg agent.Message
			if res != nil {
				msg = res.Message
			}
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

// cSigRecord appends the signature of the schedule ex just ran (its trace: every scheduling
// decision with the waiting drivers, and every fault choice) to the file BIDE_EXPLORE_SIGS names,
// one line per schedule prefixed by cfg, so two versions of the harness can be compared for
// coverage. It does nothing when BIDE_EXPLORE_SIGS is unset.
func cSigRecord(t *testing.T, cfg string, ex *hExplorer) {
	path := os.Getenv("BIDE_EXPLORE_SIGS")
	if path == "" {
		return
	}
	var b strings.Builder
	b.WriteString(t.Name() + "/" + cfg + "\t")
	for i, p := range ex.trace {
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%s@%d=%d/%d", p.name, p.drive, p.choice, p.n)
	}
	b.WriteString("\n")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		t.Fatal(err)
	}
}

// cRun: topo 0 = both phase-1 drivers in one process, 1 = two processes. p2 0 = phase 2 in
// driver 1's process (if alive), 1 = a new process.
func cRun(sub cSubject, topo, p2 int, ex *hExplorer, maxPre int) (viol []hViolation, h *cHarness) {
	sc := &cSched{parked: map[int]*cPark{}, active: map[int]bool{}, ex: ex, maxPre: maxPre, last: 1}
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
	// The scheduler needs a synctest bubble: synctest.Wait is its quiescence detector.
	synctest.Test(t, func(t *testing.T) {
		cFlightHooks(t)
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
						cSigRecord(t, fmt.Sprintf("%s/%d/%d", sub.name, topo, p2), ex)
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
	})
}
