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
// A finished run returns its recorded end whatever it is passed. A run whose earlier drives
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

// TypedStart is how a typed run (RunTypedMessage) was started: its output mode and the JSON
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
func (s RunStart) MarshalJSON() ([]byte, error) {
	var in json.RawMessage
	var err error
	if t, ok := plainUserText(s.Input); ok {
		in, err = marshalJournal(t)
	} else {
		in, err = marshalJournal(s.Input)
	}
	if err != nil {
		return nil, err
	}
	return marshalJournal(runStartWire{Input: in, Saga: s.Saga, Kind: s.Kind, Session: s.Session, Flow: s.Flow,
		Typed: s.Typed, Settings: s.Settings, Principal: s.Principal, Tools: s.Tools, Ext: s.Ext})
}

// UnmarshalJSON reads s's journal form (see MarshalJSON).
func (s *RunStart) UnmarshalJSON(b []byte) error {
	var w runStartWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*s = RunStart{Saga: w.Saga, Kind: w.Kind, Session: w.Session, Flow: w.Flow, Typed: w.Typed,
		Settings: w.Settings, Principal: w.Principal, Tools: w.Tools, Ext: w.Ext}
	in := bytes.TrimSpace(w.Input)
	switch {
	case len(in) == 0 || bytes.Equal(in, []byte("null")):
	case in[0] == '"':
		var t string
		if err := json.Unmarshal(in, &t); err != nil {
			return err
		}
		s.Input = UserText(t)
	default:
		if err := json.Unmarshal(in, &s.Input); err != nil {
			return err
		}
	}
	return nil
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
	// RunKindAgent is a run an Agent drives (Run, RunSaga, Stream, a sub-agent, a session turn).
	// A run:start with no kind, as agent runs record it, is this kind.
	RunKindAgent RunKind = "agent"
	// RunKindFlow is a run a plan flow drives (plan.Flow.Run). RunStart.Flow names the flow.
	RunKindFlow RunKind = "flow"
	// RunKindSessionTurn is a Session turn's run (Session.SendMessage, Session.SendMessageOnce).
	// RunStart.Session names the session. Only the session drives it: it is seeded with the
	// session's transcript, and only the session records the turn (see Recover).
	RunKindSessionTurn RunKind = "session_turn"
)

// OutputMode is how a typed run (RunTypedMessage) collects its answer: OutputTool (the default)
// through a final_answer tool call whose arguments are the answer, or OutputNative through the
// provider's native structured-output constraint (a JSON-schema response format).
type OutputMode string

const (
	OutputTool   OutputMode = "tool"
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
//	if start.Saga {
//	    _, err = a.RunSaga(ctx, runID, start.Input)
//	} else {
//	    _, err = a.Run(ctx, runID, start.Input)
//	}
func RecordedStart(ctx context.Context, d Durable, runID string) (RunStart, bool, error) {
	r, ok, err := lookup(ctx, d, runID, runStartStep)
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
func beginRun(ctx context.Context, d Durable, runID string, want RunStart) (json.RawMessage, bool, error) {
	if want.kind() == RunKindFlow {
		// A flow's input is held by its canonical JSON (see equalJSON); one that has none (a
		// repeated key, a lone surrogate) could not be told apart from another, so it is refused.
		if _, err := canonicalJSON(want.Input.Text()); err != nil {
			return nil, false, fmt.Errorf("run %s: the flow input: %w (%w)", runID, err, ErrConfig)
		}
	}
	b, err := marshalJournal(want)
	if err != nil {
		return nil, false, fmt.Errorf("encode %s (run %s): %w (%w)", runStartStep, runID, err, ErrConfig)
	}
	var recs []Record
	if j := journalOf(d); j != nil {
		rec, inserted, err := j.putNew(ctx, runID, runStartStep, Record{Kind: StepValue, Result: b})
		if err != nil {
			return nil, false, fmt.Errorf("record %s (run %s): %w", runStartStep, runID, err)
		}
		if inserted {
			return nil, false, nil // a new run: nothing to hold it to, and no completion
		}
		recs = []Record{rec}
	}
	done, ok, err := lookup(ctx, d, runID, runCompleteStep)
	if err != nil {
		return nil, false, err
	}
	if err := holdToStart(ctx, d, runID, recs, want); err != nil {
		return nil, false, err
	}
	if ok && done.Kind == StepValue {
		return done.Result, true, nil
	}
	return nil, false, nil
}

// checkStartKind refuses (ErrConfig) a drive of kind want of a run whose recorded start in recs is
// of another kind. A run with no recorded start passes.
func checkStartKind(runID string, recs []Record, want RunKind) error {
	for _, r := range recs {
		if r.Kind != StepValue || r.Name != runStartStep {
			continue
		}
		var got RunStart
		if err := json.Unmarshal(r.Result, &got); err != nil {
			return fmt.Errorf("decode %s (run %s): %w (%w)", runStartStep, runID, err, ErrStorage)
		}
		if got.kind() != want {
			return fmt.Errorf("run %s was started as a run of kind %q, not %q; drive it the way it was started (see RecordedStart): %w", runID, got.kind(), want, ErrConfig)
		}
		return nil
	}
	return nil
}

// holdToStart records want as runID's start if the run has none, and otherwise checks want
// against the recorded one: a drive that differs is ErrConfig, since the run's journal answers
// the recorded input under the recorded entry point's rules. recs is the run's journal as the
// drive read it; a start recorded there is checked without another read.
func holdToStart(ctx context.Context, d Durable, runID string, recs []Record, want RunStart) error {
	var rec Record
	found := false
	for _, r := range recs {
		if r.Kind == StepValue && r.Name == runStartStep {
			rec, found = r, true
			break
		}
	}
	if !found {
		b, err := marshalJournal(want)
		if err != nil {
			return fmt.Errorf("encode %s (run %s): %w (%w)", runStartStep, runID, err, ErrConfig)
		}
		rec, err = putRecord(ctx, d, runID, runStartStep, Record{Kind: StepValue, Result: b})
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
		return fmt.Errorf("run %s was started as a saga; resume it with RunSaga (or StreamSaga): %w", runID, ErrConfig)
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
