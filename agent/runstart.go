package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// runCompleteStep is the journal name of the terminal completion marker. The agent loop
// appends one StepValue Record under this name when a run returns its final answer, so a
// crash-recovery supervisor can tell a finished run from an in-flight one (see IsComplete
// and Recover) without inspecting the model output.
const runCompleteStep = "run:complete"

// runAbortedStep is the journal name of the terminal marker a saga records once its rollback
// has finished, so a recovery supervisor treats the aborted run as over.
const runAbortedStep = "run:aborted"

// runStartStep is the journal name of the record a run's first drive writes: how the run was
// started (see RunStart).
const runStartStep = "run:start"

// RunStart is how a run was started, as its first drive records it in run:start: the input it
// answers (for a Session turn, the turn's message), whether it runs as a saga, what drives it, and
// the per-run options its first caller chose. A run's model turns and tool calls answer that input
// under those options, so every later drive is held to them (the journaling rule):
//
//   - A drive that passes no option for a setting runs under the journaled value, or, where none
//     was journaled, the agent's live value. A recovery drive passes none.
//   - A later drive that passes another turn limit or token budget (WithMaxTurns,
//     WithTokenBudget) amends it: the amendment is journaled as run:limits:<n> and binds the
//     drives that load the run after it.
//   - A later drive that passes any other setting that differs from the journaled one (the
//     input, saga, tool filter, system prompt, sampling, tool choice, output mode, typed schema or
//     principal) is ErrConfig, before any model call.
//
// A finished run returns its recorded end: a completed run returns its answer only to a drive
// with the input it answered (another input is ErrConfig, since the answer is not that input's),
// and a cancelled or aborted run returns its end whatever it is passed. A run whose earlier drives
// predate this record gets it on its first drive under this version, with what that drive is
// given.
//
// Kind says what drives the run. A plan flow's run records RunKindFlow, its flow's name in Flow,
// and its input as JSON text in Input's text; resuming it with another input (compared as
// canonical JSON, so the input decoded and encoded again resumes it), under another flow's name,
// or driving it as an agent run (or an agent run as a flow) is ErrConfig. The flow's topology is
// held by its own record, flow:digest, not here.
//
// Input is journaled as a JSON string when it is a user message of one text part (as earlier
// versions wrote it), and as a message otherwise (an image input, say).
type RunStart struct {
	Input     Message                    `json:"input"`
	Saga      bool                       `json:"saga,omitempty"`
	Kind      RunKind                    `json:"kind,omitempty"`
	Session   *SessionRef                `json:"session,omitempty"`
	Flow      *FlowRef                   `json:"flow,omitempty"`
	Typed     *TypedStart                `json:"typed,omitempty"`
	Settings  RunSettings                `json:"settings,omitzero"`
	Principal *Principal                 `json:"principal,omitempty"`
	Tools     []string                   `json:"tools,omitempty"`
	Ext       map[string]json.RawMessage `json:"ext,omitempty"`
}

// RunSettings are the per-run values a run's first caller chose (RunOptions), as run:start
// journals them. A nil field is a setting the caller left alone: the agent's live value applies.
type RunSettings struct {
	MaxTurns     *int        `json:"max_turns,omitempty"`
	TokenBudget  *int        `json:"token_budget,omitempty"`
	SystemPrompt *string     `json:"system_prompt,omitempty"`
	Sampling     *Sampling   `json:"sampling,omitempty"`
	ToolChoice   *ToolChoice `json:"tool_choice,omitempty"`
}

// SessionRef names the session a session turn's run belongs to (RunKindSessionTurn): the
// session's ID, and the turn's index for a Send turn or its key for a SendOnce turn.
type SessionRef struct {
	ID   string `json:"id"`
	Turn *int   `json:"turn,omitempty"`
	Key  string `json:"key,omitempty"`
}

// TypedStart is how a typed run (RunTyped) was started: its output mode and the JSON
// schema of its answer type, in full and as a digest. Resuming a typed run through an untyped
// entry point, or with another answer type, is ErrConfig before any model call.
type TypedStart struct {
	Mode         OutputMode      `json:"mode"`
	SchemaDigest string          `json:"schema_digest"`
	Schema       json.RawMessage `json:"schema"`
}

// Principal is the part of a run's Identity that is journaled: on whose behalf it acts and under
// what authority. It is restored on every later drive, and a drive that passes another is
// ErrConfig. The Actor is not journaled: it is the deployment that drives the run now.
type Principal struct {
	OnBehalfOf   string `json:"on_behalf_of,omitempty"`
	AuthorityRef string `json:"authority_ref,omitempty"`
}

// runStartWire is RunStart's journal form, with the input held raw (see RunStart).
type runStartWire struct {
	Input     json.RawMessage            `json:"input"`
	Saga      bool                       `json:"saga,omitempty"`
	Kind      RunKind                    `json:"kind,omitempty"`
	Session   *SessionRef                `json:"session,omitempty"`
	Flow      *FlowRef                   `json:"flow,omitempty"`
	Typed     *TypedStart                `json:"typed,omitempty"`
	Settings  RunSettings                `json:"settings,omitzero"`
	Principal *Principal                 `json:"principal,omitempty"`
	Tools     []string                   `json:"tools,omitempty"`
	Ext       map[string]json.RawMessage `json:"ext,omitempty"`
}

// MarshalJSON writes s's journal form: the input as a JSON string when it is a user message of
// one text part, and as a message otherwise.
func (s RunStart) MarshalJSON() ([]byte, error) { return marshalJournal(s.out()) }

// out is s as MarshalJSON writes it. One encoding pass: the input is encoded in place, as
// marshalJournal would encode it alone. The engine encodes it directly (marshalJournal(s.out())),
// so the method's output is not encoded a second time.
func (s RunStart) out() runStartOut {
	var in any = s.Input
	if t, ok := plainUserText(s.Input); ok {
		in = t
	}
	return runStartOut{Input: in, Saga: s.Saga, Kind: s.Kind, Session: s.Session, Flow: s.Flow,
		Typed: s.Typed, Settings: s.Settings, Principal: s.Principal, Tools: s.Tools, Ext: s.Ext}
}

// runStartOut is runStartWire as MarshalJSON writes it: the input a string or a Message, encoded in
// the same pass as the rest (its fields and tags are runStartWire's).
type runStartOut struct {
	Input     any                        `json:"input"`
	Saga      bool                       `json:"saga,omitempty"`
	Kind      RunKind                    `json:"kind,omitempty"`
	Session   *SessionRef                `json:"session,omitempty"`
	Flow      *FlowRef                   `json:"flow,omitempty"`
	Typed     *TypedStart                `json:"typed,omitempty"`
	Settings  RunSettings                `json:"settings,omitzero"`
	Principal *Principal                 `json:"principal,omitempty"`
	Tools     []string                   `json:"tools,omitempty"`
	Ext       map[string]json.RawMessage `json:"ext,omitempty"`
}

// UnmarshalJSON reads s's journal form (see MarshalJSON).
func (s *RunStart) UnmarshalJSON(b []byte) error {
	var w runStartWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	st, err := w.start()
	if err != nil {
		return err
	}
	*s = st
	return nil
}

// start is the RunStart w is the journal form of: its input decoded (see MarshalJSON), the rest as
// w holds it.
func (w *runStartWire) start() (RunStart, error) {
	s := RunStart{Saga: w.Saga, Kind: w.Kind, Session: w.Session, Flow: w.Flow, Typed: w.Typed,
		Settings: w.Settings, Principal: w.Principal, Tools: w.Tools, Ext: w.Ext}
	in := bytes.TrimSpace(w.Input)
	switch {
	case len(in) == 0 || bytes.Equal(in, []byte("null")):
	case in[0] == '"':
		var t string
		if err := json.Unmarshal(in, &t); err != nil {
			return RunStart{}, err
		}
		s.Input = UserText(t)
	default:
		if err := json.Unmarshal(in, &s.Input); err != nil {
			return RunStart{}, err
		}
	}
	return s, nil
}

// plainUserText reports whether m is a user message of exactly one text part, and its text.
func plainUserText(m Message) (string, bool) {
	if m.Role != RoleUser || len(m.Parts) != 1 {
		return "", false
	}
	t, ok := m.Parts[0].(Text)
	return t.Text, ok
}

// RunKind is what drives a run, as its run:start records it (see RunStart).
type RunKind string

const (
	// RunKindAgent is a run an Agent drives (Run, Stream, a sub-agent, a session turn).
	// A run:start journaled before P14 records no kind (an agent run's did not; a flow's always
	// recorded its kind): any agent entry point may drive it (a plain run, a session turn, a typed
	// run), as before, since the record does not say which started it. P14 writes the kind of
	// every run it starts.
	RunKindAgent RunKind = "agent"
	// RunKindFlow is a run a plan flow drives (plan.Flow.Run). RunStart.Flow names the flow.
	RunKindFlow RunKind = "flow"
	// RunKindSessionTurn is a Session turn's run (Session.Send, Session.SendOnce).
	// RunStart.Session names the session. Only the session drives it: it is seeded with the
	// session's transcript, and only the session records the turn (see Recover).
	RunKindSessionTurn RunKind = "session_turn"
)

// OutputMode is how a typed run (RunTyped) collects its answer: OutputTool (the default)
// through a final_answer tool call whose arguments are the answer, or OutputNative through the
// provider's native structured-output constraint (a JSON-schema response format).
type OutputMode string

const (
	// OutputTool collects a typed run's answer as the arguments of a final_answer tool call (the
	// default; see RunTyped).
	OutputTool OutputMode = "tool"
	// OutputNative collects a typed run's answer through the provider's native structured output
	// (WithOutputMode(OutputNative)).
	OutputNative OutputMode = "native"
)

// FlowRef names the plan flow that drives a run of kind RunKindFlow.
type FlowRef struct {
	Name string `json:"name"`
}

// kind is s's kind, with a run:start that records none read as RunKindAgent.
func (s RunStart) kind() RunKind {
	if s.Kind == "" {
		return RunKindAgent
	}
	return s.Kind
}

// legacy reports whether s was journaled before P14: it records no kind (P14 always writes one)
// and no typed start. Such a run was started by an agent entry point (a flow's start has always
// recorded its kind) that the record does not name: a plain run, a session turn, a SendOnce turn
// or a typed run.
func (s RunStart) legacy() bool { return s.Kind == "" && s.Typed == nil }

// admits reports whether an agent's drive of kind k may drive the run s started: a drive of s's
// own kind, and, for a legacy start (see legacy), a drive of any kind, since the record does not
// say which agent entry point started the run. (A flow holds its start through holdToStart, which
// reads a legacy start as an agent run's.)
func (s RunStart) admits(k RunKind) bool {
	return s.legacy() || s.kind() == k
}

// RecordedStart returns how runID was started (see RunStart), and ok=false for a run whose
// journal holds no such record: one never driven, or one not driven since before the record
// existed. A session's turn run (IsSessionRun) records its message, but only the session can
// drive it (it seeds the turn with the transcript before that message), so Recover never hands
// one to its callback. A recovery callback uses it to re-drive a run with its own input and
// entry point:
//
//	start, ok, err := agent.RecordedStart(ctx, store, runID)
//	if err != nil {
//	    return err
//	}
//	if !ok {
//	    start = startFor(runID) // the deployment's own record, for a run not driven under this version
//	}
//	if start.Kind == agent.RunKindFlow {
//	    return driveFlow(ctx, start.Flow.Name, runID, start.Input) // the flow's Run, with the input decoded
//	}
//	_, err = a.Run(ctx, runID, start.Input) // a saga's later drives run as the saga
func RecordedStart(ctx context.Context, d *Journal, runID string) (RunStart, bool, error) {
	r, ok, err := d.Get(ctx, runID, runStartStep)
	if err != nil || !ok || r.Kind != StepValue {
		return RunStart{}, false, err
	}
	var s RunStart
	if err := json.Unmarshal(r.Result, &s); err != nil {
		return RunStart{}, false, fmt.Errorf("decode %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
	}
	return s, true, nil
}

// beginRun is journalhook.Begin: the completion of a finished run, or else want held as the run's
// start (see holdToStart).
func beginRun(ctx context.Context, d *Journal, runID string, want RunStart) (json.RawMessage, bool, error) {
	if want.kind() == RunKindFlow {
		// A flow's input is held by its canonical JSON (see equalJSON); one that has none (a
		// repeated key, a lone surrogate) could not be told apart from another, so it is refused.
		if _, err := canonicalJSON(want.Input.Text()); err != nil {
			return nil, false, fmt.Errorf("run %s: the flow input: %w (%w)", runID, err, ErrConfig)
		}
	}
	b, err := marshalJournal(want.out())
	if err != nil {
		return nil, false, fmt.Errorf("encode %s (run %s): %w (%w)", runStartStep, runID, err, ErrConfig)
	}
	rec, inserted, err := d.putNew(ctx, runID, runStartStep, Record{Kind: StepValue, Result: b})
	if err != nil {
		return nil, false, fmt.Errorf("record %s (run %s): %w", runStartStep, runID, err)
	}
	if inserted {
		return nil, false, nil // a new run: nothing to hold it to, and no completion
	}
	recs := []Record{rec}
	end, ok, err := firstEndOf(ctx, d, runID, runCompleteStep, runCancelledStep)
	if err != nil {
		return nil, false, err
	}
	if err := holdToStart(ctx, d, runID, recs, want); err != nil {
		return nil, false, err
	}
	switch {
	case ok && end.name == runCancelledStep:
		return nil, false, fmt.Errorf("run %s: %w", runID, ErrRunCancelled) // its first end marker
	case ok:
		return end.rec.Result, true, nil
	}
	return nil, false, nil
}

// checkFinishedStart refuses (ErrConfig) a drive of kind want, with the given input (nil: the
// journaled one), of a finished run whose recorded start in recs is of another kind or answered
// another input: the recorded answer is that input's, not this one's (#137's R137-2; an unfinished
// run is held to its start by holdDrive). A run with no recorded start passes, as a run journaled
// by a version that recorded none.
func checkFinishedStart(runID string, recs []Record, want RunKind, input *Message) error {
	for _, r := range recs {
		if r.Kind != StepValue || r.Name != runStartStep {
			continue
		}
		var got RunStart
		if err := json.Unmarshal(r.Result, &got); err != nil {
			return fmt.Errorf("decode %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
		}
		if !got.admits(want) {
			return fmt.Errorf("run %s was started as a run of kind %q, not %q; drive it the way it was started (see RecordedStart): %w", runID, got.kind(), want, ErrConfig)
		}
		if input != nil && !sameMessage(got.Input, *input) {
			return fmt.Errorf("run %s finished answering a different input (see RecordedStart); its answer is not this input's: %w", runID, ErrConfig)
		}
		return nil
	}
	return nil
}

// holdToStart records want as runID's start if the run has none, and otherwise checks want
// against the recorded one: a drive that differs is ErrConfig, since the run's journal answers
// the recorded input under the recorded entry point's rules. recs is the run's journal as the
// drive read it; a start recorded there is checked without another read.
func holdToStart(ctx context.Context, d *Journal, runID string, recs []Record, want RunStart) error {
	var rec Record
	found := false
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == runStartStep {
			rec, found = r, true
			break
		}
	}
	if !found {
		b, err := marshalJournal(want.out())
		if err != nil {
			return fmt.Errorf("encode %s (run %s): %w (%w)", runStartStep, runID, err, ErrConfig)
		}
		rec, err = d.put(ctx, runID, runStartStep, Record{Kind: StepValue, Result: b})
		if err != nil {
			return fmt.Errorf("record %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
		}
	}
	var got RunStart
	if err := json.Unmarshal(rec.Result, &got); err != nil {
		return fmt.Errorf("decode %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
	}
	switch {
	case got.kind() != want.kind():
		return fmt.Errorf("run %s was started as a run of kind %q, not %q; drive it the way it was started (see RecordedStart): %w", runID, got.kind(), want.kind(), ErrConfig)
	case got.kind() == RunKindFlow && (got.Flow == nil || want.Flow == nil || got.Flow.Name != want.Flow.Name):
		return fmt.Errorf("run %s was started by flow %s, not %s; resume it with the flow it started with: %w", runID, flowName(got.Flow), flowName(want.Flow), ErrConfig)
	case got.Saga && !want.Saga:
		return fmt.Errorf("run %s was started as a saga; drive it with WithSaga(): %w", runID, ErrConfig)
	case !got.Saga && want.Saga:
		return fmt.Errorf("run %s was not started as a saga; resume it with Run (or Stream): %w", runID, ErrConfig)
	case got.kind() == RunKindFlow:
		same, err := equalJSON([]byte(got.Input.Text()), []byte(want.Input.Text()))
		if err != nil {
			return fmt.Errorf("run %s: compare its input with the recorded one: %w (%w)", runID, err, ErrConfig)
		}
		if !same {
			return fmt.Errorf("run %s was started with a different input (see RecordedStart); resume it with that input: %w", runID, ErrConfig)
		}
	case got.kind() != RunKindFlow && !sameMessage(got.Input, want.Input):
		return fmt.Errorf("run %s was started with a different input (see RecordedStart); resume it with that input: %w", runID, ErrConfig)
	}
	return nil
}

// flowName quotes f's name for an error, or says there is none.
func flowName(f *FlowRef) string {
	if f == nil {
		return "(none recorded)"
	}
	return fmt.Sprintf("%q", f.Name)
}

// sameMessage reports whether a and b are the same message: their journal encodings are equal.
func sameMessage(a, b Message) bool {
	if ta, ok := plainUserText(a); ok {
		tb, ok := plainUserText(b)
		return ok && ta == tb
	}
	x, errA := marshalJournal(a)
	y, errB := marshalJournal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}
