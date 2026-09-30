package agent_test

// Model-based differential testing, part two: the real runtime under test, the harness that drives
// it to quiescence through crashes, halts and approvals, and the properties that compare it with
// the reference model (refmodel_ref_test.go).
//
// For each scenario, the harness runs the real runtime against a MemStore wrapped to fail the
// scheduled persists, and re-drives the run the way an operator would until it settles: a crash is
// resumed, a PendingApproval gets the scenario's decision (Approve) and is resumed, and a
// ResumeHalt is reconciled with the call's true outcome (ResolveHalt) and resumed. It then checks:
//
//   - the settled outcome (answer, pause, or saga abort with its compensated and uncompensated
//     lists) equals the reference's;
//   - every model call was shown exactly the conversation the reference shows it, crash or not;
//   - a non-retry-safe side effect fired exactly as often as in the reference (0 or 1), a
//     retry-safe one fired if and only if it does in the reference, and compensations ran for
//     the same calls in the same (first-run) order, each receiving its call's recorded result;
//   - a run that never crashed never halted;
//   - IsComplete agrees with the outcome, for the root and for every sub-run;
//   - re-driving a settled run returns the same outcome and appends nothing to any journal;
//   - the journal replays (agent.Replay, fresh store, no crashes) to the same outcome with the
//     reference's side effects.
//
// Everything goes through bide's public API: run IDs, sub-run IDs, and the calls to approve or
// resolve are read off the errors the runtime returns (and a sub-run's ID off RunScope inside its
// model call), never built from journal key formats.
//
// TestRefModel_Randomized runs random scenarios with random crash schedules; BIDE_REFMODEL_N sets
// how many (500 by default, 300 under the race detector) and BIDE_REFMODEL_SEED the first seed.
// TestRefModel_CrashSweep crashes each generated scenario of a smaller set at every write of every
// drive attempt in turn. A failing scenario is shrunk to a minimal one before it is reported.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// errRMCrash is the injected storage failure. Like a real store's, it wraps ErrStorage.
var errRMCrash = fmt.Errorf("injected storage failure: %w", agent.ErrStorage)

// rmCrashStore fails the crashAt-th persist: the step's work (and any side effect in it) has run,
// but its record is not written. With dead set the process is taken to have died there: every
// later persist fails too, and no new step starts. Otherwise the failure is transient and the
// process carries on.
type rmCrashStore struct {
	inner   agent.Durable
	crashAt int
	dead    bool

	mu      sync.Mutex
	writes  int
	crashed bool
}

func (c *rmCrashStore) down() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead && c.crashed
}

func (c *rmCrashStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (agent.Record, error)) (agent.Record, error) {
	if c.down() {
		return agent.Record{}, errRMCrash
	}
	return c.inner.Do(ctx, runID, name, func(ctx context.Context) (agent.Record, error) {
		rec, err := fn(ctx)
		if err != nil {
			return rec, err
		}
		c.mu.Lock()
		c.writes++
		fail := c.writes == c.crashAt || (c.dead && c.crashed)
		if c.writes == c.crashAt {
			c.crashed = true
		}
		c.mu.Unlock()
		if fail {
			return agent.Record{}, errRMCrash
		}
		return rec, nil
	})
}

func (c *rmCrashStore) History(ctx context.Context, runID string) ([]agent.Record, error) {
	return c.inner.History(ctx, runID)
}

// rmWorld is the outside world of one scenario: the providers the tools act on, and what the
// harness observes. It outlives every drive attempt (each attempt is a fresh process).
type rmWorld struct {
	sc      *rmScenario
	ref     *rmRef
	calls   map[string]*rmCall
	scripts map[string]*rmScript
	conv    bool // check each model call's conversation against the reference

	mu       sync.Mutex
	fired    map[string]int   // effects that took effect, by call
	lost     map[string]bool  // Lost calls whose answer has been lost once
	outcome  map[string]rmRes // a call's true outcome, from its first execution
	comps    []string         // compensations that took effect, in order
	runIDs   map[string]string
	problems []string
}

func newRMWorld(sc *rmScenario, ref *rmRef, conv bool) *rmWorld {
	calls, scripts := sc.index()
	return &rmWorld{sc: sc, ref: ref, calls: calls, scripts: scripts, conv: conv,
		fired: map[string]int{}, lost: map[string]bool{}, outcome: map[string]rmRes{}, runIDs: map[string]string{}}
}

func (w *rmWorld) problem(format string, args ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.problems) < 20 {
		w.problems = append(w.problems, fmt.Sprintf(format, args...))
	}
}

// perform executes a call against its provider. The caller holds w.mu.
func (w *rmWorld) perform(c *rmCall) (rmRes, bool) {
	res, ok := rmRes{content: rmOK(c.ID)}, true
	if c.Fail {
		res, ok = rmRes{content: rmJSONString(rmFailText(c.ID)), isError: true}, false
	} else if c.Kind != rmRO {
		w.fired[c.ID]++
	}
	if _, seen := w.outcome[c.ID]; !seen {
		w.outcome[c.ID] = res
	}
	return res, ok
}

func (w *rmWorld) callFor(tool string, args json.RawMessage) *rmCall {
	var in struct {
		K string `json:"k"`
	}
	_ = json.Unmarshal(args, &in)
	c := w.calls[in.K]
	if c == nil || rmToolName(c.Kind, c.Comp, c.Gated) != tool {
		w.problem("tool %s called with unexpected args %s", tool, args)
		return nil
	}
	return c
}

// rmTool is a scripted tool. Like a real client, it starts nothing once its context is cancelled.
type rmTool struct {
	w           *rmWorld
	kind        rmKind
	comp, gated bool
}

func (t *rmTool) Name() string                { return rmToolName(t.kind, t.comp, t.gated) }
func (t *rmTool) Description() string         { return "" }
func (t *rmTool) ArgsSchema() json.RawMessage { return nil }
func (t *rmTool) Safety() agent.Safety {
	s := agent.Safety{RequiresApproval: t.gated}
	switch t.kind {
	case rmRO:
		s.ReadOnly = true
	case rmIdem:
		s.Idempotent = true
	}
	return s
}

func (t *rmTool) Call(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
	c := t.w.callFor(t.Name(), args)
	if c == nil {
		return nil, errors.New("unexpected call")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t.w.mu.Lock()
	res, ok := t.w.perform(c)
	lose := c.Lost && !t.w.lost[c.ID]
	t.w.lost[c.ID] = true
	t.w.mu.Unlock()
	if lose {
		return nil, fmt.Errorf("%s: connection lost after the request: %w", c.ID, agent.ErrToolOutcomeUnknown)
	}
	if !ok {
		return nil, errors.New(rmFailText(c.ID))
	}
	return json.RawMessage(res.content), nil
}

// rmCompTool is a scripted tool with a compensator.
type rmCompTool struct{ *rmTool }

func (t rmCompTool) Compensate(_ context.Context, args, result json.RawMessage) error {
	c := t.w.callFor(t.Name(), args)
	if c == nil {
		return errors.New("unexpected compensation")
	}
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	if string(result) != rmOK(c.ID) {
		t.w.problems = append(t.w.problems, fmt.Sprintf("compensate %s got result %s, want %s", c.ID, result, rmOK(c.ID)))
	}
	if t.w.fired[c.ID] == 0 {
		t.w.problems = append(t.w.problems, fmt.Sprintf("compensated %s, whose effect never fired", c.ID))
	}
	if c.CompFail {
		return errors.New("compensate " + c.ID + " failed")
	}
	t.w.comps = append(t.w.comps, c.ID)
	return nil
}

func (w *rmWorld) tools() []agent.Tool {
	var ts []agent.Tool
	for _, k := range []rmKind{rmRO, rmIdem, rmFX} {
		for _, comp := range []bool{false, true} {
			if comp && k == rmRO {
				continue
			}
			for _, gated := range []bool{false, true} {
				t := &rmTool{w: w, kind: k, comp: comp, gated: gated}
				if comp {
					ts = append(ts, rmCompTool{t})
				} else {
					ts = append(ts, t)
				}
			}
		}
	}
	return ts
}

// agents builds a fresh process's agent tree over store: the root, a sub-agent it can call, and
// one that sub-agent can call.
func (w *rmWorld) agents(store agent.Durable, model agent.Model) *agent.Agent {
	var build func(depth int) *agent.Agent
	build = func(depth int) *agent.Agent {
		ts := w.tools()
		if depth < 2 {
			ts = append(ts, agent.SubAgent(fmt.Sprintf("sub%d", depth+1), "", build(depth+1)))
		}
		return agent.New(model, store, ts...).SetMaxConcurrency(w.sc.MaxConc)
	}
	return build(0)
}

// rmConvOf converts a model request into the reference's conversation form, and names the script
// the request belongs to (its first user turn).
func rmConvOf(msgs []agent.Message) ([]rmEntry, string) {
	var conv []rmEntry
	script := ""
	for _, m := range msgs {
		switch m.Role {
		case agent.RoleUser:
			if script == "" {
				script = m.Text()
			}
			conv = append(conv, rmEntry{Role: "user", Text: m.Text()})
		case agent.RoleAssistant:
			var calls []string
			for _, p := range m.Parts {
				if tu, ok := p.(agent.ToolUse); ok {
					calls = append(calls, tu.ID+":"+tu.Name+":"+string(tu.Args))
				}
			}
			conv = append(conv, rmEntry{Role: "assistant", Calls: strings.Join(calls, ","), Text: m.Text()})
		case agent.RoleTool:
			for _, p := range m.Parts {
				if tr, ok := p.(agent.ToolResult); ok {
					conv = append(conv, rmEntry{Role: "tool", ID: tr.ToolUseID, Result: string(tr.Result), IsError: tr.IsError})
				}
			}
		default:
			conv = append(conv, rmEntry{Role: string(m.Role), Text: m.Text()})
		}
	}
	return conv, script
}

// rmModel is the scripted model. It is replay-safe: the turn it plays is a pure function of the
// request (which script, and how many assistant turns it holds), and the final answer digests the
// whole conversation it was shown.
type rmModel struct{ w *rmWorld }

func (m *rmModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	conv, name := rmConvOf(req.Messages)
	turn := 0
	for _, e := range conv {
		if e.Role == "assistant" {
			turn++
		}
	}
	w := m.w
	s := w.scripts[name]
	w.observe(ctx, name, turn, conv)
	var emits []agent.Emit
	switch {
	case s == nil || turn > len(s.Turns):
		emits = []agent.Emit{{Event: agent.TextDelta{Text: "unexpected model call"}}, {Event: agent.Finish{Reason: "stop"}}}
	case turn == len(s.Turns):
		emits = []agent.Emit{{Event: agent.TextDelta{Text: rmFinalText(name, conv)}}, {Event: agent.Finish{Reason: "stop"}}}
	default:
		for i, c := range s.Turns[turn] {
			emits = append(emits, agent.Emit{Event: agent.ToolCallDelta{Index: i, ID: c.ID, Name: c.tool(s.Depth), ArgsFragment: json.RawMessage(c.args())}})
		}
		emits = append(emits, agent.Emit{Event: agent.Finish{Reason: "tool_use"}})
	}
	ch := make(chan agent.Emit, len(emits))
	for _, e := range emits {
		ch <- e
	}
	close(ch)
	return agent.NewStream(ch), nil
}

// observe records the run ID a script runs under and checks the conversation a live model call
// is shown against the reference.
func (w *rmWorld) observe(ctx context.Context, script string, turn int, conv []rmEntry) {
	if scope := agent.RunScope(ctx); scope != "" {
		w.mu.Lock()
		if prev, ok := w.runIDs[script]; ok && prev != scope {
			w.problems = append(w.problems, fmt.Sprintf("script %s ran under two run IDs: %s and %s", script, prev, scope))
		}
		w.runIDs[script] = scope
		w.mu.Unlock()
	}
	if !w.conv {
		return
	}
	want, ok := w.ref.requests[fmt.Sprintf("%s/%d", script, turn)]
	switch {
	case !ok:
		w.problem("unexpected model call for %s turn %d, shown %s", script, turn, rmConvString(conv))
	case rmConvString(want) != rmConvString(conv):
		w.problem("model call for %s turn %d was shown\n\t%s\nwant\n\t%s", script, turn, rmConvString(conv), rmConvString(want))
	}
}

// rmReplayModel serves each run of the tree its own journaled turns (agent.Replay), telling the
// runs apart by RunScope, which a sub-run's model calls carry.
type rmReplayModel struct {
	src  agent.Durable
	root string

	mu     sync.Mutex
	models map[string]agent.Model
}

func (m *rmReplayModel) Stream(ctx context.Context, req agent.Request) (*agent.Stream, error) {
	id := agent.RunScope(ctx)
	if id == "" {
		id = m.root
	}
	m.mu.Lock()
	rm, ok := m.models[id]
	if !ok {
		var err error
		if rm, err = agent.Replay(ctx, m.src, id); err != nil {
			m.mu.Unlock()
			return nil, err
		}
		m.models[id] = rm
	}
	m.mu.Unlock()
	return rm.Stream(ctx, req)
}

const rmRunID = "rm"

// rmObserved is how a drive went.
type rmObserved struct {
	out        rmOutcome
	attempts   int
	crashed    bool // an injected failure fired
	halts      int  // halts on calls that did not lose their answer (only a crash explains them)
	unsettled  bool
	lastErrMsg string
}

// drive re-drives the run until it settles, as an operator would.
func (w *rmWorld) drive(mem *agent.MemStore, model agent.Model, crashes []int, dead bool) rmObserved {
	ctx := context.Background()
	var obs rmObserved
	stuck := 0
	for obs.attempts = 1; obs.attempts <= 60; obs.attempts++ {
		crashAt := 0
		if i := obs.attempts - 1; i < len(crashes) {
			crashAt = crashes[i]
		}
		st := &rmCrashStore{inner: mem, crashAt: crashAt, dead: dead}
		root := w.agents(st, model)
		out, err := rmDriveOnce(ctx, root, w.sc)
		if st.crashed {
			obs.crashed = true
		}
		if err != nil {
			obs.lastErrMsg = err.Error()
		}
		var halt *agent.ResumeHalt
		var pa *agent.PendingApproval
		var sa *agent.SagaAborted
		switch {
		case err == nil:
			obs.out = rmOutcome{Kind: rmCompleted, Final: out.Text()}
			return obs
		case errors.Is(err, errRMCrash), errors.Is(err, agent.ErrToolOutcomeUnknown):
			continue // resume: a crash, or a call that lost its answer (the resume halts on it)
		case errors.As(err, &halt):
			if c := w.calls[halt.ToolUseID]; c == nil || !c.Lost {
				obs.halts++
			}
			w.reconcile(mem, halt, false)
		case errors.As(err, &pa):
			c := w.calls[pa.ToolUseID]
			if c == nil || !c.Gated {
				obs.out = rmOutcome{Kind: rmOther, Err: "paused on a call that is not gated: " + err.Error()}
				return obs
			}
			if c.Decision == rmNever {
				obs.out = rmOutcome{Kind: rmPaused, Paused: pa.ToolUseID}
				return obs
			}
			if aerr := agent.Approve(ctx, mem, pa.RunID, pa.ToolUseID, c.Decision == rmApprove); aerr != nil {
				obs.out = rmOutcome{Kind: rmOther, Err: "approve: " + aerr.Error()}
				return obs
			}
		case errors.As(err, &sa):
			o := rmOutcome{Kind: rmAborted, Comp: sa.Compensated, Uncomp: sa.Uncompensated}
			ce := sa.CompensateErr
			switch {
			case ce == nil:
				obs.out = o
				return obs
			case errors.Is(ce, errRMCrash):
				continue
			case errors.As(ce, &halt):
				if c := w.calls[halt.ToolUseID]; c == nil || !c.Lost {
					obs.halts++
				}
				w.reconcile(mem, halt, true)
			default:
				// A rollback stopped by a failing step. Re-drive a few times: one that stays
				// stopped is settled as stuck.
				if stuck++; stuck >= 3 {
					o.Kind = rmStuck
					obs.out = o
					return obs
				}
			}
		default:
			obs.out = rmOutcome{Kind: rmOther, Err: err.Error()}
			return obs
		}
	}
	obs.unsettled = true
	obs.out = rmOutcome{Kind: rmOther, Err: "did not settle: " + obs.lastErrMsg}
	return obs
}

// rmDriveOnce drives the root once through the API the scenario names.
func rmDriveOnce(ctx context.Context, root *agent.Agent, sc *rmScenario) (agent.Message, error) {
	in := sc.Root.Name
	switch {
	case sc.API == 1 && sc.Saga:
		res, err := root.RunSagaResult(ctx, rmRunID, in)
		if err != nil {
			return agent.Message{}, err
		}
		return res.Message, nil
	case sc.API == 1:
		res, err := root.RunResult(ctx, rmRunID, in)
		if err != nil {
			return agent.Message{}, err
		}
		return res.Message, nil
	case sc.API == 2 && sc.Saga:
		return root.StreamSaga(ctx, rmRunID, in).Final()
	case sc.API == 2:
		return root.Stream(ctx, rmRunID, in).Final()
	case sc.Saga:
		return root.RunSaga(ctx, rmRunID, in)
	default:
		return root.Run(ctx, rmRunID, in)
	}
}

// reconcile resolves a halt the way a reconciler that can query the provider does: with the
// call's true outcome. A call the provider never saw is, going forward, carried out by hand and
// its outcome recorded (the run then continues as if it had run normally); during a rollback it
// is recorded as not performed, so there is nothing to undo.
func (w *rmWorld) reconcile(mem *agent.MemStore, h *agent.ResumeHalt, rollingBack bool) {
	c := w.calls[h.ToolUseID]
	if c == nil {
		w.problem("halt on an unknown call: %v", h)
		return
	}
	w.mu.Lock()
	res, seen := w.outcome[c.ID]
	if !seen {
		if rollingBack {
			res = rmRes{content: rmJSONString("not performed"), isError: true}
		} else {
			res, _ = w.perform(c)
		}
	}
	w.mu.Unlock()
	if err := agent.ResolveHalt(context.Background(), mem, h.RunID, h.ToolUseID, json.RawMessage(res.content), res.isError); err != nil {
		w.problem("ResolveHalt(%s, %s): %v", h.RunID, h.ToolUseID, err)
	}
}

// histLens reports how many records each known run's journal holds.
func (w *rmWorld) histLens(mem *agent.MemStore) map[string]int {
	out := map[string]int{}
	ids := []string{rmRunID}
	w.mu.Lock()
	for _, id := range w.runIDs {
		ids = append(ids, id)
	}
	w.mu.Unlock()
	for _, id := range ids {
		recs, _ := mem.History(context.Background(), id)
		out[id] = len(recs)
	}
	return out
}

// effects compares the side effects and compensations w saw with the reference's.
func (w *rmWorld) effects(label string) []string {
	var ps []string
	w.mu.Lock()
	defer w.mu.Unlock()
	ids := make([]string, 0, len(w.calls))
	for id := range w.calls {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		c := w.calls[id]
		got, want := w.fired[id], w.ref.fired[id]
		switch c.Kind {
		case rmFX:
			if got != want {
				ps = append(ps, fmt.Sprintf("%s: side effect %s fired %d times, want %d", label, id, got, want))
			}
		case rmIdem:
			if (got > 0) != (want > 0) {
				ps = append(ps, fmt.Sprintf("%s: retry-safe effect %s fired %d times, want %s", label, id, got, map[bool]string{true: "at least once", false: "never"}[want > 0]))
			}
		}
	}
	var order []string
	for _, id := range w.comps {
		if !slices.Contains(order, id) {
			order = append(order, id)
		}
	}
	if !slices.Equal(order, w.ref.comps) {
		ps = append(ps, fmt.Sprintf("%s: compensations ran for %v, want %v", label, order, w.ref.comps))
	}
	return ps
}

// rmCheck runs one scenario through the real runtime and reports every property it breaks.
func rmCheck(sc *rmScenario) []string {
	ps, _ := rmCheckObs(sc)
	return ps
}

// rmCheckObs is rmCheck, also reporting how the drive went.
func rmCheckObs(sc *rmScenario) (ps []string, obs rmObserved) {
	ctx := context.Background()
	ref := rmReference(sc)
	want := ref.root()

	w := newRMWorld(sc, ref, true)
	mem := agent.NewMemStore()
	obs = w.drive(mem, &rmModel{w: w}, sc.Crashes, sc.Dead)
	ps = append(ps, w.problems...)
	if !obs.out.equal(want) {
		ps = append(ps, fmt.Sprintf("outcome %v, want %v", obs.out, want))
	}
	ps = append(ps, w.effects("run")...)
	if !obs.crashed && obs.halts > 0 {
		ps = append(ps, fmt.Sprintf("a run with no crash halted %d times", obs.halts))
	}
	if obs.out.Kind == rmOther {
		return ps, obs // nothing settled to check further
	}

	// IsComplete agrees with the outcome, for the root and every sub-run that ran.
	if done, err := agent.IsComplete(ctx, mem, rmRunID); err != nil || done != (obs.out.Kind == rmCompleted) {
		ps = append(ps, fmt.Sprintf("IsComplete(root) = %v, %v; outcome %v", done, err, obs.out.Kind))
	}
	w.mu.Lock()
	runIDs := maps.Clone(w.runIDs)
	w.mu.Unlock()
	for script, id := range runIDs {
		r := ref.runs[script]
		if r == nil {
			ps = append(ps, fmt.Sprintf("sub-run %s (%s) ran, but never does in the reference", script, id))
			continue
		}
		done, err := agent.IsComplete(ctx, mem, id)
		if err != nil || done != (r.outcome.Kind == rmCompleted) {
			ps = append(ps, fmt.Sprintf("IsComplete(%s) = %v, %v; reference outcome %v", id, done, err, r.outcome.Kind))
		}
	}

	// Re-driving a settled run changes nothing.
	before := w.histLens(mem)
	firedBefore := fmt.Sprint(w.fired, w.comps)
	w.conv = false
	again := w.drive(mem, &rmModel{w: w}, nil, false)
	if !again.out.equal(obs.out) {
		ps = append(ps, fmt.Sprintf("re-driving the settled run: outcome %v, want %v", again.out, obs.out))
	}
	if after := w.histLens(mem); fmt.Sprint(after) != fmt.Sprint(before) {
		ps = append(ps, fmt.Sprintf("re-driving the settled run changed the journals: %v -> %v", before, after))
	}
	if fmt.Sprint(w.fired, w.comps) != firedBefore {
		ps = append(ps, "re-driving the settled run fired side effects or compensations")
	}

	// The journal replays to the same outcome, with the reference's effects.
	rw := newRMWorld(sc, ref, false)
	rep := rw.drive(agent.NewMemStore(), &rmReplayModel{src: mem, root: rmRunID, models: map[string]agent.Model{}}, nil, false)
	ps = append(ps, rw.problems...)
	if !rep.out.equal(obs.out) {
		ps = append(ps, fmt.Sprintf("replay: outcome %v, want %v", rep.out, obs.out))
	}
	if rep.halts > 0 {
		ps = append(ps, fmt.Sprintf("replay halted %d times", rep.halts))
	}
	ps = append(ps, rw.effects("replay")...)
	return ps, obs
}

// rmFails reports whether sc breaks a property in any of tries runs (scheduling varies between
// runs, so a failure found once may need a few tries to reproduce).
func rmFails(sc *rmScenario, tries int) []string {
	for range tries {
		if ps := rmCheck(sc); len(ps) > 0 {
			return ps
		}
	}
	return nil
}

// rmMutations lists the one-step simplifications of sc, each applied in place.
func rmMutations(sc *rmScenario) []func() {
	var ms []func()
	for i := range sc.Crashes {
		ms = append(ms, func() { sc.Crashes = slices.Delete(sc.Crashes, i, i+1) })
	}
	if sc.Dead {
		ms = append(ms, func() { sc.Dead = false })
	}
	if sc.MaxConc != 1 {
		ms = append(ms, func() { sc.MaxConc = 1 })
	}
	if sc.API != 0 {
		ms = append(ms, func() { sc.API = 0 })
	}
	var walk func(*rmScript)
	walk = func(s *rmScript) {
		for ti := range s.Turns {
			ms = append(ms, func() { s.Turns = slices.Delete(s.Turns, ti, ti+1) })
		}
		for ti, turn := range s.Turns {
			for ci, c := range turn {
				ms = append(ms, func() {
					s.Turns[ti] = slices.Delete(s.Turns[ti], ci, ci+1)
					if len(s.Turns[ti]) == 0 {
						s.Turns = slices.Delete(s.Turns, ti, ti+1)
					}
				})
				if c.Kind == rmSub {
					ms = append(ms, func() { c.Kind, c.Sub = rmFX, nil })
					walk(c.Sub)
					continue
				}
				if c.Gated {
					ms = append(ms, func() { c.Gated, c.Decision = false, rmApprove })
				}
				if c.Fail {
					ms = append(ms, func() { c.Fail = false })
				}
				if c.CompFail {
					ms = append(ms, func() { c.CompFail = false })
				}
				if c.Lost {
					ms = append(ms, func() { c.Lost = false })
				}
				if c.Comp && !c.CompFail {
					ms = append(ms, func() { c.Comp = false })
				}
				if c.Kind != rmRO {
					ms = append(ms, func() { c.Kind, c.Comp, c.CompFail, c.Lost = rmRO, false, false, false })
				}
			}
		}
	}
	walk(sc.Root)
	return ms
}

// rmShrink greedily applies simplifications that keep sc failing.
func rmShrink(sc *rmScenario, ps []string) (*rmScenario, []string) {
	for progress := true; progress; {
		progress = false
		n := len(rmMutations(sc.clone()))
		for i := range n {
			c := sc.clone()
			rmMutations(c)[i]()
			if cps := rmFails(c, 3); cps != nil {
				sc, ps, progress = c, cps, true
				break
			}
		}
	}
	return sc, ps
}

func rmEnvInt(name string, def uint64) uint64 {
	if v, err := strconv.ParseUint(os.Getenv(name), 10, 64); err == nil {
		return v
	}
	return def
}

// TestRefModel_Randomized drives random scenarios through the real runtime and the reference
// model and requires them to agree.
func TestRefModel_Randomized(t *testing.T) {
	def := uint64(500)
	if rmRace {
		def = 300 // the race detector slows it several times over
	}
	n := rmEnvInt("BIDE_REFMODEL_N", def)
	if testing.Short() {
		n = 50
	}
	first := rmEnvInt("BIDE_REFMODEL_SEED", 1)
	start := time.Now()
	for seed := first; seed < first+n; seed++ {
		sc := rmGenerate(seed)
		ps := rmCheck(sc)
		if len(ps) == 0 {
			continue
		}
		min, mps := rmShrink(sc, ps)
		t.Fatalf("seed %d breaks the reference model.\nscenario:\n%s\nshrunk to:\n%s\nproblems:\n\t%s",
			seed, sc, min, strings.Join(mps, "\n\t"))
	}
	t.Logf("%d scenarios in %v", n, time.Since(start).Round(time.Millisecond))
}

// TestRefModel_CrashSweep fails every persist in turn, one per scenario, in both crash modes, over
// a set of generated scenarios: every write of every drive attempt (a scenario that pauses for
// approvals is driven several times) is a crash point once.
func TestRefModel_CrashSweep(t *testing.T) {
	seeds := uint64(40)
	if rmRace {
		seeds = 8 // the race detector slows it several times over
	}
	if testing.Short() {
		seeds = 5
	}
	start := time.Now()
	points := 0
	for seed := uint64(1); seed <= seeds; seed++ {
		_, clean := rmCheckObs(rmGenerate(seed))
		for _, dead := range []bool{false, true} {
			for attempt := range clean.attempts {
				for k := 1; ; k++ {
					sc := rmGenerate(seed)
					sc.Dead, sc.Crashes = dead, append(make([]int, attempt), k)
					ps, obs := rmCheckObs(sc)
					if len(ps) > 0 {
						min, mps := rmShrink(sc, ps)
						t.Fatalf("seed %d, crash at write %d of attempt %d breaks the reference model.\nscenario:\n%s\nshrunk to:\n%s\nproblems:\n\t%s",
							seed, k, attempt+1, sc, min, strings.Join(mps, "\n\t"))
					}
					if !obs.crashed {
						break // past the attempt's last write
					}
					points++
				}
			}
		}
	}
	t.Logf("%d crash points in %v", points, time.Since(start).Round(time.Millisecond))
}
