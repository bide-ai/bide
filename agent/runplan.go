// runplan.go holds the journaling rule (docs/design/api-v1.md, item 1; rules 8 to 12 of the P14
// contract): a drive's first step after its Load journals run:start (the first drive) or holds
// the drive to it (every later drive), writes a limit amendment for a later drive's different
// limit, and settles the values the drive runs under.

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// runPlan is what one drive runs under, settled once when it opens the run: the run's journaled
// start and limits over the agent's live defaults.
type runPlan struct {
	start      RunStart
	saga       bool
	maxTurns   int
	budget     int
	sysText    *string // the run's journaled system prompt; nil: the agent's
	sampling   Sampling
	toolChoice *ToolChoice
	filter     map[string]bool // the journaled tool filter; nil: none
	reqTools   []ToolSpec      // the specs every request of the drive is sent
	maxConc    int
	cancelKey  string  // the key the drive's cancellation checks read
	root       string  // a sub-run's tree root, whose cancellation the checks read too; "" for a root
	rootStore  Durable // the store root journals to (see rootStoreOf)
	checkTurn  bool    // a turn boundary has passed since the drive's Load
}

// errSagaRun is run's answer for a drive that passed no saga option of a run journaled as a saga:
// drive takes it through the saga path.
var errSagaRun = errors.New("agent: the run is a saga")

// limitAmendment is a run:limits:<n> record: the limits a later drive set.
type limitAmendment struct {
	MaxTurns    *int `json:"max_turns,omitempty"`
	TokenBudget *int `json:"token_budget,omitempty"`
}

// openPlan settles the plan of a drive of runID for d over the run's journal recs (the drive's
// Load), which holds no end marker. It journals run:start for a first drive (rule 8), holds a
// later drive to it (rules 9 and 11), journals a different limit as an amendment (rule 10), and
// binds the identity, Waker and clock the drive runs with to the returned context. wrote names
// the records it wrote or tried to write (run:start, run:limits:<n>), whether its insert won or
// lost to another drive's: the caller loads the run again before it goes on whenever wrote is not
// empty (model 10's DStart and DAmend return to DOpen; see Agent.run). It is out of line so the
// loop's frame does not grow.
//
//go:noinline
func (a *Agent) openPlan(ctx context.Context, runID string, d *driveSpec, recs []Record) (_ context.Context, _ *runPlan, wrote []string, _ error) {
	// protocol:lifecycle begin DStart DAmend
	var (
		start  RunStart
		found  bool
		amends []limitAmendment
	)
	for _, r := range recs {
		if r.Kind != StepValue {
			continue
		}
		switch {
		case r.Name == runStartStep:
			if err := json.Unmarshal(r.Result, &start); err != nil {
				return ctx, nil, nil, fmt.Errorf("decode %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
			}
			found = true
		case strings.HasPrefix(r.Name, "run:limits:"):
			var l limitAmendment
			if err := json.Unmarshal(r.Result, &l); err != nil {
				return ctx, nil, nil, fmt.Errorf("decode %s (run %s): %w (%w)", r.Name, runID, err, ErrStorage)
			}
			amends = append(amends, l)
		}
	}
	idn, explicitID := a.driveIdentity(ctx, d)
	if !found {
		if d.resume {
			return ctx, nil, nil, fmt.Errorf("run %s: %w", runID, ErrNotStarted)
		}
		want, err := a.newStart(d, idn)
		if err != nil {
			return ctx, nil, nil, err
		}
		b, err := marshalJournal(want)
		if err != nil {
			return ctx, nil, nil, fmt.Errorf("encode %s (run %s): %w (%w)", runStartStep, runID, err, ErrConfig)
		}
		// First writer wins: the drive runs under the stored entry, a concurrent first drive's if
		// that one landed first (rule 8). The drive's own entry needs no check against itself.
		got, inserted, err := a.putStart(ctx, runID, b)
		if err != nil {
			return ctx, nil, nil, fmt.Errorf("record %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
		}
		if inserted {
			start = want
		} else if err := json.Unmarshal(got, &start); err != nil {
			return ctx, nil, nil, fmt.Errorf("decode %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
		}
		// DStart returns to DOpen whether the insert won or lost: a lost insert means another drive
		// wrote the run first, and that drive (or a Cancel) may have written more since the Load.
		// Either way the open knows run:start (its own, or the one it was handed back).
		wrote, found = startWritten, !inserted
	}
	if found {
		if err := a.holdDrive(runID, d, start, idn, explicitID); err != nil {
			return ctx, nil, nil, err
		}
	}
	// A later drive's different limit is journaled before it drives (rule 10).
	lim := journaledLimits(start.Settings, amends)
	for n := len(amends); ; n++ {
		amend := limitAmendment{}
		if v := d.cfg.maxTurns; v != nil && !sameInt(lim.MaxTurns, v) {
			amend.MaxTurns = v
		}
		if v := d.cfg.tokenBudget; v != nil && !sameInt(lim.TokenBudget, v) {
			amend.TokenBudget = v
		}
		if amend.MaxTurns == nil && amend.TokenBudget == nil {
			break
		}
		b, err := marshalJournal(amend)
		if err != nil {
			return ctx, nil, nil, fmt.Errorf("encode %s (run %s): %w (%w)", runLimitsStep(n), runID, err, ErrConfig)
		}
		rec, err := putRecord(ctx, a.store, runID, runLimitsStep(n), Record{Kind: StepValue, Result: b})
		if err != nil {
			return ctx, nil, nil, fmt.Errorf("record %s (run %s): %w (%w)", runLimitsStep(n), runID, err, ErrStorage)
		}
		var got limitAmendment
		if err := json.Unmarshal(rec.Result, &got); err != nil {
			return ctx, nil, nil, fmt.Errorf("decode %s (run %s): %w (%w)", runLimitsStep(n), runID, err, ErrStorage)
		}
		lim = applyAmendment(lim, got)                       // ours, or a concurrent drive's that took the index first
		wrote = append(slices.Clip(wrote), runLimitsStep(n)) // never into startWritten's array
	}
	// protocol:lifecycle end
	p := &runPlan{start: start, saga: start.Saga, maxTurns: a.maxTurns, budget: a.tokenBudget, maxConc: a.maxConc,
		sampling: a.sampling, toolChoice: a.toolChoice, sysText: start.Settings.SystemPrompt}
	if lim.MaxTurns != nil {
		p.maxTurns = *lim.MaxTurns
	}
	if lim.TokenBudget != nil {
		p.budget = *lim.TokenBudget
	}
	if s := start.Settings.Sampling; s != nil {
		p.sampling = overlaySampling(a.sampling, *s)
	}
	if tc := start.Settings.ToolChoice; tc != nil {
		p.toolChoice = tc
	}
	if d.cfg.maxConc != nil {
		p.maxConc = *d.cfg.maxConc
	}
	p.reqTools = a.specList // never written through: each request is sent its own copy
	if start.Tools != nil {
		p.reqTools = a.requestTools()
		p.filter = make(map[string]bool, len(start.Tools))
		for _, n := range start.Tools {
			p.filter[n] = true
		}
		p.reqTools = slices.DeleteFunc(p.reqTools, func(s ToolSpec) bool { return !p.allows(s.Name, a.terminalTool) })
	}
	p.cancelKey = runCancelledStep
	if p.saga {
		p.cancelKey = runCancelRequestedStep // Cancel writes only the request on a saga (rule 5)
	}
	if root := treeRootID(runID); root != runID {
		p.root = root // a Cancel of the tree's root cancels this sub-run too
		p.rootStore = rootStoreOf(ctx, root, a.store)
		ctx = withRootStore(ctx, root, p.rootStore, a.store) // for the sub-runs this one starts
	}
	// The drive's identity: its Actor live, the principal journaled.
	if idn.Actor != "" || start.Principal != nil {
		run := Identity{Actor: idn.Actor}
		if pr := start.Principal; pr != nil {
			run.OnBehalfOf, run.AuthorityRef = pr.OnBehalfOf, pr.AuthorityRef
		}
		ctx = ContextWithIdentity(ctx, run)
	}
	if d.cfg.waker != nil {
		ctx = ContextWithWaker(ctx, d.cfg.waker)
	}
	if d.cfg.clock != nil {
		ctx = ContextWithClock(ctx, d.cfg.clock)
	}
	return ctx, p, wrote, nil
}

// startWritten is openPlan's wrote for a first drive that inserted run:start, or lost the insert to
// another drive's, and wrote no amendment (shared: never appended to in place).
var startWritten = []string{runStartStep}

// onlyWritten loads runID again after its open wrote the records wrote, and reports whether the
// run holds nothing that before (the open's Load) did not but those records and the journal
// header. Over a Journal it is one Load whose entries are not decoded (their names suffice); over
// another Durable, one History.
func (a *Agent) onlyWritten(ctx context.Context, runID string, before []Record, wrote []string) (bool, error) {
	if j := journalOf(a.store); j != nil {
		for e, err := range j.store.Load(ctx, runID, -1) {
			if err != nil {
				return false, storageErr("load "+runID, err)
			}
			if !knownToOpen(e.Name, before, wrote) {
				return false, nil
			}
		}
		return true, nil
	}
	recs, err := a.store.History(ctx, runID)
	if err != nil {
		return false, storageErr("load history "+runID, err)
	}
	for _, r := range recs {
		if !knownToOpen(r.Name, before, wrote) {
			return false, nil
		}
	}
	return true, nil
}

// knownToOpen reports whether the record name is one the open knew of: in before, one it wrote, or
// the journal header.
func knownToOpen(name string, before []Record, wrote []string) bool {
	if name == headerStep || slices.Contains(wrote, name) {
		return true
	}
	for _, r := range before {
		if r.Name == name {
			return true
		}
	}
	return false
}

// allows reports whether the plan's filter admits a call of tool name: any tool when there is no
// filter, and a typed run's answer tool always.
func (p *runPlan) allows(name, terminal string) bool {
	return p.filter == nil || p.filter[name] || name != "" && name == terminal
}

// refusal is why a call of tool name is refused at dispatch, or "" if it is not: a tool the
// run's filter leaves out, or any tool but a typed run's answer tool under a tool choice of none.
func (p *runPlan) refusal(name, terminal string) string {
	switch {
	case !p.allows(name, terminal):
		return "its tool filter leaves it out"
	case p.toolChoice != nil && p.toolChoice.Mode == "none" && (name == "" || name != terminal):
		return "its tool choice is none"
	}
	return ""
}

// driveIdentity is the identity the drive was given: its WithIdentity, else its context's (which
// a sub-agent's run inherits from its parent), else the agent's. explicit is false for the
// agent's: an agent default is live, never compared with the journal.
func (a *Agent) driveIdentity(ctx context.Context, d *driveSpec) (id Identity, explicit bool) {
	if d.cfg.identity != nil {
		return *d.cfg.identity, true
	}
	if id, ok := IdentityFrom(ctx); ok {
		return id, true
	}
	if a.identity != nil {
		return *a.identity, false
	}
	return Identity{}, false
}

// newStart is the run:start a first drive of d journals, with the options it was given. It
// checks what can only be checked against the agent: a filter names the agent's tools, and a
// forced tool choice is inside the filter.
func (a *Agent) newStart(d *driveSpec, idn Identity) (RunStart, error) {
	if d.input == nil {
		return RunStart{}, fmt.Errorf("agent: a run's first drive needs its input: %w", ErrConfig)
	}
	s := RunStart{Input: *d.input, Saga: d.cfg.saga, Session: d.session, Typed: d.typed, Tools: d.cfg.tools,
		Settings: RunSettings{MaxTurns: d.cfg.maxTurns, TokenBudget: d.cfg.tokenBudget, SystemPrompt: d.cfg.systemPrompt,
			Sampling: d.cfg.sampling, ToolChoice: d.cfg.toolChoice}}
	s.Kind = d.runKind() // always written: a run:start with no kind is a legacy one (see RunStart.legacy)
	if idn.OnBehalfOf != "" || idn.AuthorityRef != "" {
		s.Principal = &Principal{OnBehalfOf: idn.OnBehalfOf, AuthorityRef: idn.AuthorityRef}
	}
	if s.Typed != nil && s.Typed.Mode == "" {
		t := *s.Typed
		t.Mode = OutputTool
		s.Typed = &t
	}
	for _, n := range s.Tools {
		if _, ok := a.tools[n]; !ok && n != a.terminalTool {
			return RunStart{}, fmt.Errorf("WithToolFilter: the agent has no tool %q: %w", n, ErrConfig)
		}
	}
	if s.Tools != nil {
		tc := d.cfg.toolChoice
		if tc == nil {
			tc = a.toolChoice
		}
		if tc != nil && tc.Mode == "tool" && !slices.Contains(s.Tools, tc.Name) {
			return RunStart{}, fmt.Errorf("WithToolFilter: the tool choice forces %q, which the filter leaves out: %w", tc.Name, ErrConfig)
		}
	}
	return s, nil
}

// holdDrive refuses (ErrConfig) a drive of d whose setting differs from runID's journaled start:
// what drives it, its input, saga flag, typed schema and output mode, tool filter, system prompt,
// sampling, tool choice and principal (rule 11). Limits are not compared: a different limit is an
// amendment (rule 10). A setting the drive did not pass is the journaled one (rule 9).
func (a *Agent) holdDrive(runID string, d *driveSpec, start RunStart, idn Identity, explicitID bool) error {
	mismatch := func(what string) error {
		return fmt.Errorf("run %s was started with another %s (see RecordedStart); drive it with the journaled one, or with none: %w", runID, what, ErrConfig)
	}
	switch {
	case !start.admits(d.runKind()):
		return fmt.Errorf("run %s was started as a run of kind %q, not %q; drive it the way it was started (see RecordedStart): %w", runID, start.kind(), d.runKind(), ErrConfig)
	case d.input != nil && !sameMessage(start.Input, *d.input):
		return mismatch("input")
	case start.Saga && !d.cfg.saga:
		if d.strictSaga {
			return fmt.Errorf("run %s was started as a saga; resume it with RunSaga (or StreamSaga): %w", runID, ErrConfig)
		}
		return errSagaRun
	case !start.Saga && d.cfg.saga:
		return fmt.Errorf("run %s was not started as a saga; resume it without WithSaga (Run, Stream, ResumeRun): %w", runID, ErrConfig)
	case !start.legacy() && (start.Typed == nil) != (d.typed == nil):
		if start.Typed != nil {
			return fmt.Errorf("run %s is a typed run; resume it with RunTypedMessage or ResumeTyped and its answer type: %w", runID, ErrConfig)
		}
		return fmt.Errorf("run %s is not a typed run; resume it with RunMessage or ResumeRun: %w", runID, ErrConfig)
	case d.typed != nil && start.Typed != nil && d.typed.SchemaDigest != start.Typed.SchemaDigest:
		return mismatch("answer type (its schema digest differs)")
	case d.typed != nil && start.Typed != nil && d.typed.Mode != "" && d.typed.Mode != start.Typed.Mode:
		return mismatch("output mode")
	case d.typed == nil && d.cfg.outputMode != "":
		return fmt.Errorf("WithOutputMode applies to a typed run (RunTypedMessage), and run %s is not one: %w", runID, ErrConfig)
	case d.cfg.tools != nil && !slices.Equal(d.cfg.tools, start.Tools):
		return mismatch("tool filter")
	case d.cfg.systemPrompt != nil && !sameSetting(d.cfg.systemPrompt, start.Settings.SystemPrompt):
		return mismatch("system prompt")
	case d.cfg.sampling != nil && !sameSetting(d.cfg.sampling, start.Settings.Sampling):
		return mismatch("sampling")
	case d.cfg.toolChoice != nil && !sameSetting(d.cfg.toolChoice, start.Settings.ToolChoice):
		return mismatch("tool choice")
	case explicitID && (idn.OnBehalfOf != "" || idn.AuthorityRef != "") &&
		(start.Principal == nil || *start.Principal != (Principal{OnBehalfOf: idn.OnBehalfOf, AuthorityRef: idn.AuthorityRef})):
		return mismatch("principal (OnBehalfOf, AuthorityRef)")
	}
	return nil
}

// sameValue reports whether two optional settings hold equal values: both nil, or both set and
// deeply equal.
func sameSetting[T any](x, y *T) bool {
	if x == nil || y == nil {
		return x == y
	}
	return reflect.DeepEqual(*x, *y)
}

func sameInt(x, y *int) bool { return sameSetting(x, y) }

// journaledLimits is the run's limits: run:start's, with each amendment applied in order.
func journaledLimits(s RunSettings, amends []limitAmendment) limitAmendment {
	l := limitAmendment{MaxTurns: s.MaxTurns, TokenBudget: s.TokenBudget}
	for _, a := range amends {
		l = applyAmendment(l, a)
	}
	return l
}

func applyAmendment(l, a limitAmendment) limitAmendment {
	if a.MaxTurns != nil {
		l.MaxTurns = a.MaxTurns
	}
	if a.TokenBudget != nil {
		l.TokenBudget = a.TokenBudget
	}
	return l
}

// overlaySampling returns base with each control run sets replaced by run's.
func overlaySampling(base, run Sampling) Sampling {
	s := cloneSampling(base)
	r := cloneSampling(run)
	if r.Temperature != nil {
		s.Temperature = r.Temperature
	}
	if r.TopP != nil {
		s.TopP = r.TopP
	}
	if r.MaxTokens != nil {
		s.MaxTokens = r.MaxTokens
	}
	if r.Stop != nil {
		s.Stop = r.Stop
	}
	if r.Seed != nil {
		s.Seed = r.Seed
	}
	return s
}

// planSystem is the system prompt of a drive under p: the run's journaled text, which takes
// precedence over the agent's text or function, or else the agent's.
func (a *Agent) planSystem(ctx context.Context, p *runPlan, run RunInfo) (string, error) {
	if p.sysText != nil {
		return *p.sysText, nil
	}
	return a.systemMessage(ctx, run)
}

// refuseFiltered records the refusal of a call (see runPlan.refusal: why says why), an error result
// the model reads, and returns its tool-result message.
//
//go:noinline
func (a *Agent) refuseFiltered(ctx context.Context, runID string, tu ToolUse, why string) (*Message, error) {
	text, err := marshalJournal(fmt.Sprintf("tool %q is not available in this run (%s)", cutName(tu.Name), why))
	if err != nil {
		return nil, fmt.Errorf("encode the refusal of call %s: %w (%w)", tu.ID, err, ErrStorage)
	}
	rec, err := putRecord(ctx, a.store, runID, ToolResultStep(tu.ID), Record{Kind: StepToolResult, ToolUseID: tu.ID, IsError: true, Result: text})
	if err != nil {
		return nil, err
	}
	return &Message{Role: RoleTool, Parts: []Part{ToolResult{ToolUseID: tu.ID, Result: rec.Result, IsError: rec.IsError}}}, nil
}

// putStart inserts runID's run:start holding b, first writer wins, and returns the value the
// journal holds and whether this call stored it.
func (a *Agent) putStart(ctx context.Context, runID string, b json.RawMessage) (json.RawMessage, bool, error) {
	rec := Record{Kind: StepValue, Result: b}
	j := journalOf(a.store)
	if j == nil {
		got, err := putRecord(ctx, a.store, runID, runStartStep, rec)
		if err != nil {
			return nil, false, err
		}
		return got.Result, bytes.Equal(got.Result, b), nil
	}
	if err := j.ensureHeader(ctx, runID); err != nil {
		return nil, false, err
	}
	data, err := JournalEntry(runStartStep, rec)
	if err != nil {
		return nil, false, fmt.Errorf("encode step %q: %w (%w)", runStartStep, err, ErrStorage)
	}
	e, inserted, err := j.store.Insert(ctx, runID, runStartStep, data)
	if err != nil {
		return nil, false, storageErr(fmt.Sprintf("record step %q of run %s", runStartStep, runID), err)
	}
	if inserted {
		return b, true, nil // the drive's own entry: nothing to decode
	}
	got, err := decodeStored(runID, runStartStep, e.Data)
	return got.Result, false, err
}
