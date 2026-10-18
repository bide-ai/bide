package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// endedRun builds a finished run "r" whose end marker is name: a completed run, or a started run
// that was cancelled.
func endedRun(t *testing.T, name string) (*MemStore, *Journal, *Agent) {
	t.Helper()
	ctx := context.Background()
	st := NewMemStore()
	j := mustJournal(st)
	a := mustNew(&scriptModel{turns: [][]Emit{textTurn("the answer, with a card 4111")}}, j)
	switch name {
	case runCompleteStep:
		if _, err := a.Run(ctx, "r", UserText("input")); err != nil {
			t.Fatal(err)
		}
	case runCancelledStep:
		b, err := marshalJournal(RunStart{Input: UserText("input")}.out())
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := j.putNew(ctx, "r", runStartStep, Record{Kind: StepValue, Result: b}); err != nil {
			t.Fatal(err)
		}
		if err := Cancel(ctx, j, "r", "the customer asked to stop"); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("no builder for %s", name)
	}
	return st, j, a
}

func redactEnd(t *testing.T, j *Journal, name string) {
	t.Helper()
	redactPlanned(t, j, "r", RedactionSelector{Names: []string{name}})
}

// An end marker can be erased (its result is personal data), and the run is still over: a tombstone
// at an end marker's name is that end, read by the name, never by the content that is gone.
func TestRedactEndMarker_TheRunStaysOver(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		state RunState
	}{{runCompleteStep, RunCompleted}, {runCancelledStep, RunCancelled}} {
		st, j, _ := endedRun(t, tc.name)
		before, err := Status(ctx, j, "r")
		if err != nil || before.State != tc.state || before.EndRedacted {
			t.Fatalf("%s: Status before = %+v, %v", tc.name, before, err)
		}
		redactEnd(t, j, tc.name)
		if e, _, _ := st.Get(ctx, "r", tc.name); !isTombstone(e.Data) {
			t.Fatalf("%s: the end marker row was not replaced", tc.name)
		}
		got, err := Status(ctx, j, "r")
		if err != nil || got.State != RunEnded || !got.EndRedacted || got.Terminal != "" || got.Records != before.Records {
			t.Errorf("%s: Status after = %+v, %v; want ended (kind not attested), EndRedacted, no Terminal text, the same record count", tc.name, got, err)
		}
		if done, err := IsComplete(ctx, j, "r"); err != nil || done {
			t.Errorf("%s: IsComplete = %v, %v; want false (how the run ended is not attested)", tc.name, done, err)
		}
		if ended, err := runEnded(ctx, j, "r"); err != nil || !ended {
			t.Errorf("%s: runEnded = %v, %v", tc.name, ended, err)
		}
		if err := Cancel(ctx, j, "r", "again"); !errors.Is(err, ErrRunEnded) {
			t.Errorf("%s: Cancel of a run with a redacted end marker = %v, want ErrRunEnded (not nil, even for a cancelled run: the kind is not attested)", tc.name, err)
		}
	}
}

// A run whose end marker was erased is never driven again, never recovered, never completed with
// an empty answer by a flow, and the sweeps and filters that look for unfinished runs skip it.
func TestRedactEndMarker_IsNeverResumed(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{runCompleteStep, runCancelledStep} {
		st, j, a := endedRun(t, name)
		redactEnd(t, j, name)
		if _, err := a.Run(ctx, "r", UserText("input")); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: Run of the redacted run = %v, want ErrConfig", name, err)
		}
		n, err := Recover(ctx, j, func(context.Context, string, RunStart) error {
			t.Errorf("%s: Recover resumed the run", name)
			return nil
		})
		if err != nil || n != 0 {
			t.Errorf("%s: Recover = %d, %v; want 0, nil", name, n, err)
		}
		for id, err := range st.Runs(ctx, recoverFilter) {
			if err != nil {
				t.Fatal(err)
			}
			t.Errorf("%s: recoverFilter lists %s", name, id)
		}
		for id, err := range st.Runs(ctx, sweepFilter) {
			if err != nil {
				t.Fatal(err)
			}
			t.Errorf("%s: sweepFilter lists %s", name, id)
		}
	}
}

// What a redaction never replaces is unchanged: the other run: records, the header, the attempt
// markers. Only the end markers are open.
func TestRedactEndMarker_OnlyTheEndMarkersAreOpen(t *testing.T) {
	for name, protected := range map[string]bool{
		runCompleteStep: false, runAbortedStep: false, runCancelledStep: false,
		runCancelRequestedStep: true, "run:limits:1": true, "@journal": true, "audit:x": true,
	} {
		kind := StepValue
		if name == "@journal" {
			kind = StepHeader
		}
		if got := RedactionProtected(name, kind) != ""; got != protected {
			t.Errorf("RedactionProtected(%q) = %v, want %v", name, got, protected)
		}
	}
	// A tombstone written over any other run: record is still corruption.
	if _, err := decodeStored("r", runCancelRequestedStep, mustTombstone(t, runCancelRequestedStep)); !errors.Is(err, ErrStorage) {
		t.Errorf("a tombstone at run:cancel-requested decodes: %v", err)
	}
	// An end marker's tombstone decodes as a redacted record that carries its name.
	r, err := decodeStored("r", runCompleteStep, mustTombstone(t, runCompleteStep))
	if err != nil || !r.Redacted || r.Name != runCompleteStep {
		t.Errorf("a tombstone at run:complete = %+v, %v", r, err)
	}
	// A tombstone for another record's name in an end marker's row is still refused.
	if _, err := decodeStored("r", runCompleteStep, mustTombstone(t, "other")); !errors.Is(err, ErrStorage) {
		t.Errorf("a tombstone naming another record in run:complete's row decodes: %v", err)
	}
}

func mustTombstone(t *testing.T, name string) []byte {
	t.Helper()
	b, err := NewTombstone(name, []byte(`{"name":"`+name+`","kind":"value"}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A flow run whose completion was erased is not reported finished with an empty output: beginning
// it again is refused, whichever end marker it holds.
func TestRedactEndMarker_AFlowIsNotBegunAgain(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{runCompleteStep, runCancelledStep} {
		j := mustJournal(NewMemStore())
		want := RunStart{Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}, Input: UserText(`{"q":"input"}`)}
		if _, _, _, err := beginRun(ctx, j, "r", want); err != nil {
			t.Fatal(err)
		}
		if name == runCompleteStep {
			if _, err := j.put(ctx, "r", runCompleteStep, Record{Kind: StepValue, Result: []byte(`{"flow":"f","output":"4111"}`)}); err != nil {
				t.Fatal(err)
			}
		} else if err := Cancel(ctx, j, "r", "stop"); err != nil {
			t.Fatal(err)
		}
		if _, out, done, err := beginRun(ctx, j, "r", want); name == runCompleteStep && (err != nil || !done || len(out) == 0) {
			t.Fatalf("before the erasure: %s, %v, %v, %v", name, out, done, err)
		}
		redactEnd(t, j, name)
		_, out, done, err := beginRun(ctx, j, "r", want)
		if !errors.Is(err, ErrConfig) || done || len(out) != 0 {
			t.Errorf("%s: beginRun on the redacted run = %s, done %v, %v; want ErrConfig and no completion", name, out, done, err)
		}
	}
}

// The Payloads preset leaves an end marker alone, and the plan says so: the final answer is erased
// only by naming the marker, so a pass over every payload never silently leaves it or changes how a
// run ended.
func TestRedactEndMarker_PayloadsLeaveItAndSayWhy(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{runCompleteStep, runCancelledStep} {
		_, j, _ := endedRun(t, name)
		plan, err := PlanRedaction(ctx, j, "r", RedactionSelector{Payloads: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range plan.Records {
			if rec.Name == name {
				t.Errorf("Payloads selected %s", name)
			}
		}
		var said bool
		for _, k := range plan.Kept {
			said = said || k.Name == name && strings.Contains(k.Reason, "end marker not selected") && strings.Contains(k.Reason, "name "+name)
		}
		if !said {
			t.Errorf("%s: the plan does not say the end marker was not selected: %+v", name, plan.Kept)
		}
		named, err := PlanRedaction(ctx, j, "r", RedactionSelector{Names: []string{name}})
		if err != nil || len(named.Records) != 1 || named.Records[0].Name != name {
			t.Fatalf("a plan naming %s = %+v, %v; want that one record", name, named.Records, err)
		}
		for _, k := range named.Kept {
			if k.Name == name {
				t.Errorf("a plan that names %s also keeps it: %+v", name, k)
			}
		}
	}
}

// A redacted run:complete is an end of unknown kind, not a completion: completedAnswer, which says a
// run completed and what it answered, does not read one, and neither does a redacted record of
// another name.
func TestRedactEndMarker_ACompletionIsNotReadFromATombstone(t *testing.T) {
	answer := Message{Role: RoleAssistant, Parts: []Part{Text{"hi"}}}
	model := Record{Kind: StepModel, Name: "m", Message: &answer}
	for _, name := range []string{runCompleteStep, "tool:x"} {
		if got, ok := completedAnswer([]Record{model, {Name: name, Redacted: true}}); ok {
			t.Errorf("completedAnswer over a redacted %s = %+v, true; want no completion", name, got)
		}
	}
	// firstEnd finds the end by the name and reads no kind from it.
	end, ok := firstEnd([]Record{model, {Name: runAbortedStep, Redacted: true}})
	if !ok || end.name != "" || !end.rec.Redacted {
		t.Errorf("firstEnd over a redacted run:aborted = %+v, %v; want an end whose kind is empty", end, ok)
	}
}

// renamedEnd returns a copy of run "r" in which the erased end marker is moved to the name to, in
// the row and inside the tombstone, as someone with write access could do it with no key: the leaf
// hash, so every root and signed head, is unchanged.
func renamedEnd(t *testing.T, st *MemStore, from, to string) *MemStore {
	t.Helper()
	ctx := context.Background()
	out := NewMemStore()
	for e, err := range st.Load(ctx, "r", -1) {
		if err != nil {
			t.Fatal(err)
		}
		name, data := e.Name, e.Data
		if name == from {
			leaf, ok := TombstoneLeafHash(data)
			if !ok {
				t.Fatalf("%s is not a tombstone", from)
			}
			name = to
			data = []byte(`{"redacted":{"leaf_hash":"` + hex.EncodeToString(leaf) + `","at_ms":1,"name":"` + to + `"}}`)
		}
		if _, _, err := out.Insert(ctx, "r", name, data); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// How a run ended is not attested once its end marker is erased: the name of a tombstone is outside the
// leaf hash, so renaming one (completed to cancelled, completed to aborted) changes nothing any reader
// reports. Every reader says "over, kind unknown" for the original and the renamed run alike.
func TestRedactEndMarker_ARenameChangesNothing(t *testing.T) {
	ctx := context.Background()
	st, j, a := endedRun(t, runCompleteStep)
	redactEnd(t, j, runCompleteStep)
	base, err := Status(ctx, j, "r")
	if err != nil || base.State != RunEnded || !base.EndRedacted {
		t.Fatalf("Status of the erased run = %+v, %v", base, err)
	}
	for _, to := range []string{runCancelledStep, runAbortedStep} {
		rj := mustJournal(renamedEnd(t, st, runCompleteStep, to))
		got, err := Status(ctx, rj, "r")
		if err != nil || got != base {
			t.Errorf("renamed to %s: Status = %+v, %v; want the unrenamed %+v", to, got, err, base)
		}
		if done, err := IsComplete(ctx, rj, "r"); err != nil || done {
			t.Errorf("renamed to %s: IsComplete = %v, %v", to, done, err)
		}
		if err := Cancel(ctx, rj, "r", "again"); !errors.Is(err, ErrRunEnded) {
			t.Errorf("renamed to %s: Cancel = %v; want ErrRunEnded, not the nil of an already cancelled run", to, err)
		}
		ra := mustNew(&scriptModel{turns: [][]Emit{textTurn("x")}}, rj)
		if _, err := ra.Run(ctx, "r", UserText("input")); !errors.Is(err, ErrConfig) || errors.Is(err, ErrRunCancelled) {
			t.Errorf("renamed to %s: Run = %v; want ErrConfig (a run that is over, kind not attested), not ErrRunCancelled", to, err)
		}
		if _, err := ra.Run(ctx, "r", UserText("input")); err == nil {
			t.Errorf("renamed to %s: a second Run succeeded", to)
		}
		n, err := Recover(ctx, rj, func(context.Context, string, RunStart) error {
			t.Errorf("renamed to %s: Recover resumed the run", to)
			return nil
		})
		if err != nil || n != 0 {
			t.Errorf("renamed to %s: Recover = %d, %v", to, n, err)
		}
	}
	// The same drive of the unrenamed run.
	if _, err := a.Run(ctx, "r", UserText("input")); !errors.Is(err, ErrConfig) || errors.Is(err, ErrRunCancelled) {
		t.Errorf("Run of the erased run = %v; want ErrConfig", err)
	}
}

// An erased run:aborted does not attest an abort: the notifier's sweep raises no abort event for it
// (its name is outside the leaf hash, so a renamed tombstone would otherwise page for a run that
// completed), and JournalNotifyEvents reports none, while the same run unerased is caught up.
func TestRedactEndMarker_AnErasedAbortRaisesNoEvent(t *testing.T) {
	ctx := context.Background()
	for _, erase := range []bool{false, true} {
		clk := sweepClock()
		j := memJournal()
		start, _ := json.Marshal(RunStart{StartedAt: clk.now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)})
		if _, _, err := j.putEntry(ctx, "r", runStartStep, Record{Kind: StepValue, Result: start}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := j.putEntry(ctx, "r", runAbortedStep, Record{Kind: StepValue, Result: mustJSON("refund failed; rolled back")}); err != nil {
			t.Fatal(err)
		}
		if erase {
			redactPlanned(t, j, "r", RedactionSelector{Names: []string{runAbortedStep}})
		}
		sink := &fakeSink{}
		_ = sweep(t, j, sink, NewMemNotifyTrail(), clk)
		if got := sink.count() > 0; got == erase {
			t.Errorf("erased=%v: the sweep paged = %v; want it to page only for the run that is not erased", erase, got)
		}
		var events int
		for _, e := range JournalNotifyEvents("r", mustHistory(t, j, "r")) {
			if e.Kind == NotificationRunAborted {
				events++
			}
		}
		if (events > 0) == erase {
			t.Errorf("erased=%v: JournalNotifyEvents reported %d abort event(s)", erase, events)
		}
	}
}
