package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
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

// RunStart is how a run was started, as its first drive records it: the input it answers (for a
// Session turn, the turn's message) and whether it runs as a saga (RunSaga, StreamSaga,
// RunSagaResult, or a sub-agent called inside a saga). A run's model turns and tool calls answer
// that input under that entry point's rules, so every later drive is held to it: resuming an
// unfinished run with another input, or through the other entry point (Run for a saga, RunSaga
// for a run), is ErrConfig. A finished run returns its recorded answer whatever it is passed, as
// before.
//
// A run whose earlier drives predate this record gets it on its first drive under this version,
// with the input and entry point that drive is given.
//
// Kind says what drives the run. A plan flow's run records RunKindFlow, its flow's name in Flow,
// and its input as JSON text in Input; resuming it with another input (compared as canonical
// JSON, so the input decoded from Input and encoded again resumes it), under another flow's name,
// or driving it as an agent run (or an agent run as a flow) is ErrConfig. The flow's topology is
// held by its own record, flow:digest, not here.
type RunStart struct {
	Input string   `json:"input"`
	Saga  bool     `json:"saga,omitempty"`
	Kind  RunKind  `json:"kind,omitempty"`
	Flow  *FlowRef `json:"flow,omitempty"`
}

// RunKind is what drives a run, as its run:start records it (see RunStart).
type RunKind string

const (
	// RunKindAgent is a run an Agent drives (Run, RunSaga, Stream, a sub-agent, a session turn).
	// A run:start with no kind, as agent runs record it, is this kind.
	RunKindAgent RunKind = "agent"
	// RunKindFlow is a run a plan flow drives (plan.Flow.Run). RunStart.Flow names the flow.
	RunKindFlow RunKind = "flow"
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
	case got.kind() == RunKindFlow && !sameCanonicalJSON(got.Input, want.Input):
		return fmt.Errorf("run %s was started with a different input (see RecordedStart); resume it with that input: %w", runID, ErrConfig)
	case got.kind() != RunKindFlow && got.Input != want.Input:
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

// sameCanonicalJSON reports whether a and b are the same JSON value under canonicalJSON, so a flow
// resumed with its recorded input decoded and encoded again (RecordedStart, then Run) is held to
// the input it started with even where the round trip changes the text: object key order, and a
// number's spelling or a precision float64 cannot hold. Text that is not JSON compares as text.
func sameCanonicalJSON(a, b string) bool {
	ca, errA := canonicalJSON(a)
	cb, errB := canonicalJSON(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ca == cb
}

// canonicalJSON re-encodes the JSON text s canonically: objects with their keys sorted, no
// insignificant whitespace, and every number as the exact decimal value it denotes (see
// canonicalNumber), so 1, 1.0 and 1e0 are one value while 2^53 and 2^53+1 are two: no number is
// rounded through a float.
func canonicalJSON(s string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", fmt.Errorf("canonical JSON: trailing data after the value")
	}
	var b strings.Builder
	if err := writeCanonical(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// canonicalNumber writes the JSON number text t (valid JSON number grammar) as its exact decimal
// value: "0" for zero, and otherwise an optional "-", the significant digits with no leading or
// trailing zero, "e", and the exponent that makes them the value. Two number texts denote the
// same decimal value if and only if their canonical forms are equal. A number whose exponent does
// not fit an int64 keeps its text.
func canonicalNumber(t string) string {
	neg := strings.HasPrefix(t, "-")
	mant, expText, hasExp := strings.Cut(strings.TrimPrefix(t, "-"), "e")
	if !hasExp {
		mant, expText, hasExp = strings.Cut(mant, "E")
	}
	var exp int64
	if hasExp {
		e, err := strconv.ParseInt(expText, 10, 64) // ParseInt takes a leading "+"
		if err != nil || e > 1<<62 || e < -(1<<62) {
			return t
		}
		exp = e
	}
	intPart, frac, _ := strings.Cut(mant, ".")
	digits := intPart + frac
	exp -= int64(len(frac))
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0" // every zero, -0 and 0e5 among them
	}
	trimmed := strings.TrimRight(digits, "0")
	exp += int64(len(digits) - len(trimmed))
	sign := ""
	if neg {
		sign = "-"
	}
	return sign + trimmed + "e" + strconv.FormatInt(exp, 10)
}

func writeCanonical(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			b.Write(kb)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case json.Number:
		b.WriteString(canonicalNumber(string(x)))
	default:
		eb, err := json.Marshal(x)
		if err != nil {
			return err
		}
		b.Write(eb)
	}
	return nil
}
