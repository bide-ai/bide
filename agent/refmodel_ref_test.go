package agent_test

// Model-based differential testing, part one: the scenario language, its random generator, and
// the reference model.
//
// A scenario is a scripted run tree: a root script whose turns call tools (in parallel when a turn
// holds several calls), with sub-agent calls that run scripts of their own, approval-gated calls
// with a scripted decision, calls that fail, and compensable calls, plus a crash schedule for the
// real runtime. The reference model below interprets a scenario as a plain Go program: no journal,
// no concurrency, no crashes, no pauses other than an approval that is never decided. It computes
// what an observer of a correct runtime sees once the run has settled: the final answer (or the
// pause, or the saga abort and its rollback), the side effects fired, the compensations run, and
// the conversation each model call is shown.
//
// The reference is deliberately written from bide's documented semantics, not from its code. It
// calls nothing in package agent. Where a semantic is subtle, the comment at that point in the
// reference states it; those comments are the specification the real runtime is held to.
//
// Crashes do not appear in the reference at all: the property is that they make no difference.
// That holds only with an operator who resolves each halt with the call's true outcome, which the
// harness plays (see rmWorld.reconcile): a call the provider saw gets the outcome the provider
// saw; one it never saw is carried out by hand and recorded if the run is going forward, and
// recorded as not performed if it is rolling back. A reconciler that records anything else changes
// what the model reads, and the run's course with it.
//
// Not modeled (covered by their own tests): m-of-n approvals (mofn_dst_test.go), sessions,
// Interrupt/Sleep/Await, tool and model middleware, model errors, and overlapping drivers.

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"strings"
)

// rmKind is the kind of a scripted call.
type rmKind int

const (
	rmRO   rmKind = iota // ReadOnly: no side effect
	rmIdem               // Idempotent: a side effect that is safe to repeat
	rmFX                 // not retry-safe: a side effect that must fire at most once
	rmSub                // a sub-agent call running a script of its own
)

func (k rmKind) String() string { return [...]string{"ro", "idem", "fx", "sub"}[k] }

// rmDecision is the scripted human decision on an approval-gated call.
type rmDecision int

const (
	rmApprove rmDecision = iota
	rmDeny
	rmNever // never decided: the run stays paused on this call
)

func (d rmDecision) String() string { return [...]string{"approve", "deny", "never"}[d] }

// rmCall is one scripted tool call. ID is unique across the whole scenario, so every effect and
// compensation is attributed to exactly one call.
type rmCall struct {
	ID       string
	Kind     rmKind
	Comp     bool // the tool has a compensator (saga rollback can undo it)
	Gated    bool // the tool requires a human decision before it runs
	Decision rmDecision
	Fail     bool      // the tool returns an error (and, being atomic, makes no change)
	CompFail bool      // the compensator returns an error
	Sub      *rmScript // Kind == rmSub: the script the sub-agent runs
}

// tool is the name of the tool this call invokes. Sub-agent tools are named by the depth of the
// agent they reach (sub1 from the root, sub2 from a sub-agent).
func (c *rmCall) tool(depth int) string {
	if c.Kind == rmSub {
		return fmt.Sprintf("sub%d", depth+1)
	}
	return rmToolName(c.Kind, c.Comp, c.Gated)
}

func rmToolName(k rmKind, comp, gated bool) string {
	n := k.String()
	if comp {
		n += "_c"
	}
	if gated {
		n += "_g"
	}
	return n
}

// args is the call's JSON arguments as the model sends them.
func (c *rmCall) args() string {
	if c.Kind == rmSub {
		return fmt.Sprintf(`{"task":%q}`, c.Sub.Name)
	}
	return fmt.Sprintf(`{"k":%q}`, c.ID)
}

// rmScript is one agent run's script: a sequence of tool turns, then a final answer. Its Name is
// the run's input (the user turn), which is how the scripted model tells the runs apart.
type rmScript struct {
	Name  string
	Depth int
	Turns [][]*rmCall
}

// rmScenario is one generated test case.
type rmScenario struct {
	Seed    uint64
	Saga    bool  // drive the root with RunSaga
	MaxConc int   // SetMaxConcurrency for every agent (0 = unbounded)
	Dead    bool  // crash mode: true = the process dies at the crash (every later write fails too)
	Crashes []int // per drive attempt, the persist that fails (0 or past the end = none)
	API     int   // how the root is driven: 0 Run/RunSaga, 1 RunResult/RunSagaResult, 2 Stream/StreamSaga
	Root    *rmScript
}

// clone deep-copies a scenario (for shrinking).
func (s *rmScenario) clone() *rmScenario {
	c := *s
	c.Crashes = append([]int(nil), s.Crashes...)
	c.Root = s.Root.clone()
	return &c
}

func (s *rmScript) clone() *rmScript {
	c := &rmScript{Name: s.Name, Depth: s.Depth}
	for _, turn := range s.Turns {
		var t []*rmCall
		for _, call := range turn {
			cc := *call
			if call.Sub != nil {
				cc.Sub = call.Sub.clone()
			}
			t = append(t, &cc)
		}
		c.Turns = append(c.Turns, t)
	}
	return c
}

// calls indexes every call in the scenario by ID, and scripts every script by name.
func (s *rmScenario) index() (map[string]*rmCall, map[string]*rmScript) {
	calls, scripts := map[string]*rmCall{}, map[string]*rmScript{}
	var walk func(*rmScript)
	walk = func(sc *rmScript) {
		scripts[sc.Name] = sc
		for _, turn := range sc.Turns {
			for _, c := range turn {
				calls[c.ID] = c
				if c.Sub != nil {
					walk(c.Sub)
				}
			}
		}
	}
	walk(s.Root)
	return calls, scripts
}

func (s *rmScenario) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "seed=%d saga=%v maxConc=%d dead=%v crashes=%v api=%d\n", s.Seed, s.Saga, s.MaxConc, s.Dead, s.Crashes, s.API)
	var walk func(*rmScript, string)
	walk = func(sc *rmScript, indent string) {
		fmt.Fprintf(&b, "%s%s:\n", indent, sc.Name)
		for i, turn := range sc.Turns {
			fmt.Fprintf(&b, "%s  turn %d:", indent, i)
			for _, c := range turn {
				fmt.Fprintf(&b, " %s", c.describe(sc.Depth))
			}
			b.WriteString("\n")
			for _, c := range turn {
				if c.Sub != nil {
					walk(c.Sub, indent+"    ")
				}
			}
		}
		fmt.Fprintf(&b, "%s  turn %d: final\n", indent, len(sc.Turns))
	}
	walk(s.Root, "  ")
	return b.String()
}

func (c *rmCall) describe(depth int) string {
	s := c.ID + "=" + c.tool(depth)
	if c.Kind == rmSub {
		s += "(" + c.Sub.Name + ")"
	}
	if c.Gated {
		s += "[" + c.Decision.String() + "]"
	}
	if c.Fail {
		s += "[fail]"
	}
	if c.CompFail {
		s += "[compfail]"
	}
	return s
}

// ---------------------------------------------------------------------------
// Generator
// ---------------------------------------------------------------------------

type rmGen struct {
	rng     *rand.Rand
	calls   int
	scripts int
	subs    int
}

// rmGenerate builds a random scenario from seed.
func rmGenerate(seed uint64) *rmScenario {
	g := &rmGen{rng: rand.New(rand.NewPCG(seed, 0xB1DE5EED))}
	sc := &rmScenario{Seed: seed, Saga: g.rng.IntN(2) == 0, Dead: g.rng.IntN(2) == 0}
	sc.MaxConc = []int{0, 1, 2}[g.rng.IntN(3)]
	sc.API = g.rng.IntN(3)
	if sc.Saga {
		// A saga's first failure cancels the calls still in flight in its turn, and which of them
		// had started by then depends on scheduling. Sequential execution makes that set, and so
		// the rollback the reference predicts, deterministic.
		sc.MaxConc = 1
	}
	sc.Root = g.script(0)
	// The crash schedule: a quarter of scenarios run crash-free, the rest crash one to three times.
	writes := 6 + 4*g.calls
	for range g.rng.IntN(4) {
		sc.Crashes = append(sc.Crashes, 1+g.rng.IntN(writes))
	}
	return sc
}

func (g *rmGen) script(depth int) *rmScript {
	s := &rmScript{Name: fmt.Sprintf("S%d", g.scripts), Depth: depth}
	g.scripts++
	turns := g.rng.IntN(4)
	if depth == 0 {
		turns = 1 + g.rng.IntN(3)
	}
	for range turns {
		n := 1
		switch r := g.rng.IntN(10); {
		case r >= 8:
			n = 3
		case r >= 5:
			n = 2
		}
		var turn []*rmCall
		for range n {
			turn = append(turn, g.call(depth))
		}
		s.Turns = append(s.Turns, turn)
	}
	return s
}

func (g *rmGen) call(depth int) *rmCall {
	g.calls++
	c := &rmCall{ID: fmt.Sprintf("c%d", g.calls)}
	switch r := g.rng.IntN(10); {
	case r < 2:
		c.Kind = rmRO
	case r < 4:
		c.Kind = rmIdem
	case r < 8:
		c.Kind = rmFX
	default:
		c.Kind = rmFX
		if depth < 2 && g.subs < 3 {
			c.Kind = rmSub
		}
	}
	if c.Kind == rmSub {
		g.subs++
		c.Sub = g.script(depth + 1)
		return c
	}
	c.Fail = g.rng.IntN(6) == 0
	c.Gated = g.rng.IntN(5) == 0
	if c.Gated {
		switch r := g.rng.IntN(10); {
		case r < 6:
			c.Decision = rmApprove
		case r < 9:
			c.Decision = rmDeny
		default:
			c.Decision = rmNever
		}
	}
	if c.Kind != rmRO {
		c.Comp = g.rng.IntN(2) == 0
		c.CompFail = c.Comp && g.rng.IntN(15) == 0
	}
	return c
}

// ---------------------------------------------------------------------------
// Conversation and tool outputs: what the model reads
// ---------------------------------------------------------------------------

// rmEntry is one message of a conversation, in a form both the reference and the real runtime's
// requests convert to.
type rmEntry struct {
	Role    string
	Text    string // user text
	Calls   string // assistant tool calls: id:name:args, comma separated
	ID      string // tool result: the call it answers
	Result  string // tool result: its JSON
	IsError bool
}

func (e rmEntry) String() string {
	switch e.Role {
	case "assistant":
		return "assistant[" + e.Calls + "]"
	case "tool":
		return fmt.Sprintf("tool[%s=%s err=%v]", e.ID, e.Result, e.IsError)
	default:
		return e.Role + "[" + e.Text + "]"
	}
}

func rmConvString(conv []rmEntry) string {
	parts := make([]string, len(conv))
	for i, e := range conv {
		parts[i] = e.String()
	}
	return strings.Join(parts, " ")
}

// rmFinalText is a script's final answer: its name and a digest of the whole conversation the
// model was shown, so any difference in what the model read changes the observable answer.
func rmFinalText(script string, conv []rmEntry) string {
	h := fnv.New64a()
	h.Write([]byte(rmConvString(conv)))
	return fmt.Sprintf("%s done %016x", script, h.Sum64())
}

// rmOK is a successful call's result, rmFailText a failing call's error text.
func rmOK(id string) string       { return fmt.Sprintf(`{"done":%q}`, id) }
func rmFailText(id string) string { return id + " failed" }

// rmDenied is the result a denied call is given: bide's documented denial text, as a JSON string.
const rmDenied = `"tool call denied by human"`

func rmJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ---------------------------------------------------------------------------
// Reference model
// ---------------------------------------------------------------------------

// rmOutKind classifies how a run settled.
type rmOutKind int

const (
	rmCompleted rmOutKind = iota // final answer
	rmPaused                     // waiting on an approval nobody gives
	rmAborted                    // saga rolled back
	rmStuck                      // saga rollback stopped on a failing compensation step
	rmTripped                    // internal to the reference: a saga step failed, roll back
	rmOther                      // the real runtime returned something else (always a failure)
)

func (k rmOutKind) String() string {
	return [...]string{"completed", "paused", "aborted", "stuck", "tripped", "other"}[k]
}

// rmOutcome is the observable end state of a run.
type rmOutcome struct {
	Kind   rmOutKind
	Final  string   // completed: the final answer
	Paused string   // paused: the gated call awaiting a decision
	Comp   []string // aborted/stuck: tool names compensated, in the order undone
	Uncomp []string // aborted/stuck: completed writes nothing could undo
	Err    string   // other: the error
}

func (o rmOutcome) String() string {
	switch o.Kind {
	case rmCompleted:
		return "completed " + o.Final
	case rmPaused:
		return "paused on " + o.Paused
	case rmAborted, rmStuck:
		return fmt.Sprintf("%s comp=%v uncomp=%v", o.Kind, o.Comp, o.Uncomp)
	default:
		return fmt.Sprintf("%s %s", o.Kind, o.Err)
	}
}

func (o rmOutcome) equal(p rmOutcome) bool {
	return o.String() == p.String()
}

type rmRes struct {
	content string
	isError bool
}

// rmRefRun is the reference's record of one script's run: what a saga rollback walks, and what a
// resumed run does not do again.
type rmRefRun struct {
	reached int       // turns whose calls the model has made
	calls   []*rmCall // every call the model made, in order
	results map[string]rmRes
	failed  string // the saga step whose failure aborted the run
	outcome rmOutcome
	rolled  *rmRollback // a rollback runs once; walking it again reports the same lists
}

type rmRollback struct {
	comp, uncomp []string
	stuck        bool
}

// rmRef interprets a scenario.
type rmRef struct {
	sc       *rmScenario
	saga     bool
	calls    map[string]*rmCall
	decided  map[string]bool      // gated calls a human has decided
	fired    map[string]int       // side effects that took effect, by call
	comps    []string             // compensations that ran, in order
	requests map[string][]rmEntry // "script/turn" -> the conversation that model call is shown
	runs     map[string]*rmRefRun // by script name
}

func rmReference(sc *rmScenario) *rmRef {
	calls, _ := sc.index()
	return &rmRef{sc: sc, saga: sc.Saga, calls: calls, decided: map[string]bool{}, fired: map[string]int{},
		requests: map[string][]rmEntry{}, runs: map[string]*rmRefRun{}}
}

// root runs the scenario to the end. An approval pause is a real pause, not a formality: the run
// stops with the gated call unrun (and, when the pause came from inside a sub-agent, with that
// sub-agent's siblings in the turn run to completion), the human decides, and the run resumes. A
// resumed run does again nothing it has already done: a call with a result is not run again, and
// a sub-agent resumes its own run.
func (r *rmRef) root() rmOutcome {
	for {
		o := r.run(r.sc.Root)
		if o.Kind == rmPaused {
			if c := r.calls[o.Paused]; c.Decision != rmNever && !r.decided[c.ID] {
				r.decided[c.ID] = true
				continue
			}
		}
		return o
	}
}

// run drives one script: forward, then, in a saga whose step failed, the rollback.
func (r *rmRef) run(s *rmScript) rmOutcome {
	o := r.forward(s)
	if o.Kind == rmTripped {
		rb := r.rollback(s)
		o = rmOutcome{Kind: rmAborted, Comp: rb.comp, Uncomp: rb.uncomp}
		if rb.stuck {
			o.Kind = rmStuck
		}
	}
	r.runs[s.Name].outcome = o
	return o
}

// invoke performs a call's forward action: a failing call makes no change (saga steps and tools
// are atomic), a successful one fires its side effect once.
func (r *rmRef) invoke(c *rmCall) (rmRes, bool) {
	if c.Fail {
		return rmRes{content: rmJSONString(rmFailText(c.ID)), isError: true}, false
	}
	if c.Kind != rmRO {
		r.fired[c.ID]++
	}
	return rmRes{content: rmOK(c.ID)}, true
}

func (r *rmRef) forward(s *rmScript) rmOutcome {
	run := r.runs[s.Name]
	if run == nil {
		run = &rmRefRun{results: map[string]rmRes{}}
		r.runs[s.Name] = run
	}
	if run.failed != "" {
		return rmOutcome{Kind: rmTripped}
	}
	conv := []rmEntry{{Role: "user", Text: s.Name}}
	for k := 0; ; k++ {
		r.requests[fmt.Sprintf("%s/%d", s.Name, k)] = append([]rmEntry(nil), conv...)
		if k == len(s.Turns) {
			return rmOutcome{Kind: rmCompleted, Final: rmFinalText(s.Name, conv)}
		}
		turn := s.Turns[k]
		var calls []string
		for _, c := range turn {
			calls = append(calls, c.ID+":"+c.tool(s.Depth)+":"+c.args())
		}
		conv = append(conv, rmEntry{Role: "assistant", Calls: strings.Join(calls, ",")})
		if run.reached == k {
			run.calls = append(run.calls, turn...)
			run.reached++
		}

		// Approvals are settled for the whole turn before any of its calls runs: a denial becomes
		// the call's error result, and an undecided call pauses the turn with nothing in it run.
		for _, c := range turn {
			if _, ok := run.results[c.ID]; ok || !c.Gated {
				continue
			}
			switch {
			case !r.decided[c.ID]:
				return rmOutcome{Kind: rmPaused, Paused: c.ID}
			case c.Decision == rmDeny:
				run.results[c.ID] = rmRes{content: rmDenied, isError: true}
			}
		}
		// The turn's calls run in order (the real runtime may run them concurrently; outside a saga
		// no call's outcome depends on another's). A pause from inside a sub-agent does not stop
		// its siblings; the turn reports the first call's pause once all have run.
		pause := ""
		for _, c := range turn {
			if _, ok := run.results[c.ID]; ok {
				continue
			}
			if c.Kind == rmSub {
				o := r.run(c.Sub)
				switch o.Kind {
				case rmCompleted:
					run.results[c.ID] = rmRes{content: rmJSONString(o.Final)}
				case rmPaused:
					if pause == "" {
						pause = o.Paused
					}
				default: // the sub-agent's saga aborted: this step failed
					run.failed = c.ID
					return rmOutcome{Kind: rmTripped}
				}
				continue
			}
			res, ok := r.invoke(c)
			if !ok && r.saga {
				// A saga step failed. The calls after it in the turn never start: the failure
				// cancels them, and a cancelled call has had no effect.
				run.failed = c.ID
				return rmOutcome{Kind: rmTripped}
			}
			run.results[c.ID] = res
		}
		if pause != "" {
			return rmOutcome{Kind: rmPaused, Paused: pause}
		}
		// Results reach the model in the order the model made the calls.
		for _, c := range turn {
			res := run.results[c.ID]
			conv = append(conv, rmEntry{Role: "tool", ID: c.ID, Result: res.content, IsError: res.isError})
		}
	}
}

// rollback undoes a saga run's completed writes in reverse call order, recursing into sub-agent
// runs, per RunSaga's documented contract:
//   - the step that failed made no change (steps are atomic), and neither did a call that
//     returned an error or was denied;
//   - a ReadOnly call needs no undo;
//   - a sub-agent call is rolled back into, whatever its own outcome; a sub-run's rollback runs
//     once, and walking it again reports what it undid without undoing it again;
//   - a sub-agent whose own saga failed rolled itself back before the parent's rollback began,
//     so its rollback comes first, whatever its place in the call order;
//   - a call that never started needs no undo, except that a retry-safe call with a compensator
//     is run to learn its result and then compensated (bide cannot tell a retry-safe call that
//     never started from one whose result a crash lost, so it treats both alike); if that run
//     fails, the rollback stops (stuck), since bide cannot tell a lasting failure from a passing
//     one, and a later RunSaga tries it again;
//   - a retry-safe call with no compensator and no result may have taken effect: uncompensated;
//   - a completed write with a compensator is compensated, one without is uncompensated;
//   - a compensation that fails stops the rollback (stuck); writes before it stay as they are.
func (r *rmRef) rollback(s *rmScript) rmRollback {
	run := r.runs[s.Name]
	if run == nil {
		return rmRollback{} // the sub-agent call never started
	}
	if run.rolled != nil {
		return *run.rolled
	}
	var rb rmRollback
	done := func() rmRollback { run.rolled = &rb; return rb }
	if f := r.calls[run.failed]; f != nil && f.Kind == rmSub {
		// The failed step was a sub-agent, which rolled itself back first.
		sub := r.rollback(f.Sub)
		rb.comp = append(rb.comp, sub.comp...)
		rb.uncomp = append(rb.uncomp, sub.uncomp...)
		if sub.stuck {
			rb.stuck = true
			return done()
		}
	}
	for i := len(run.calls) - 1; i >= 0; i-- {
		c := run.calls[i]
		depth := s.Depth
		if c.ID == run.failed {
			continue
		}
		res, ok := run.results[c.ID]
		if ok && res.isError {
			continue
		}
		if c.Kind == rmSub {
			sub := r.rollback(c.Sub)
			rb.comp = append(rb.comp, sub.comp...)
			rb.uncomp = append(rb.uncomp, sub.uncomp...)
			if sub.stuck {
				rb.stuck = true
				return done()
			}
			continue
		}
		if c.Kind == rmRO {
			continue
		}
		if !ok {
			if c.Kind == rmFX {
				continue // never started
			}
			if !c.Comp {
				rb.uncomp = append(rb.uncomp, c.tool(depth))
				continue
			}
			if _, fine := r.invoke(c); !fine {
				rb.uncomp = append(rb.uncomp, c.tool(depth))
				rb.stuck = true
				return done()
			}
		}
		if c.Comp {
			if c.CompFail {
				rb.stuck = true
				return done()
			}
			r.comps = append(r.comps, c.ID)
			rb.comp = append(rb.comp, c.tool(depth))
			continue
		}
		rb.uncomp = append(rb.uncomp, c.tool(depth))
	}
	return done()
}
