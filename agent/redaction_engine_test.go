package agent

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// redactPlanned plans and redacts run with sel, failing the test on any error.
func redactPlanned(t *testing.T, j *Journal, run string, sel RedactionSelector) RedactionPlan {
	t.Helper()
	ctx := context.Background()
	plan, err := PlanRedaction(ctx, j, run, sel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Redact(ctx, j, plan, time.Now()); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	return plan
}

// An aborted saga re-walks its rollback when the run is driven again. Redacting every payload,
// and even asking for the saga records by prefix, leaves what was compensated readable: the
// compensation memos are protected, so the second report is the first.
func TestRedact_SagaRollbackSurvives(t *testing.T) {
	ctx := context.Background()
	l := newLedger()
	j := memJournal()
	drive := func() error {
		a := mustNew(&scriptModel{turns: [][]Emit{toolTurn("a1", "A", `{}`), toolTurn("a2", "boom", `{}`)}}, j,
			WithTools(l.write("A"), failTool("boom")))
		_, err := a.Run(ctx, "r", UserText("go"), WithSaga())
		return err
	}
	var first *SagaAborted
	if err := drive(); !errors.As(err, &first) {
		t.Fatalf("first drive = %v, want SagaAborted", err)
	}
	plan := redactPlanned(t, j, "r", RedactionSelector{Prefixes: []string{"@saga/", "@subrun/", "tool:", "@llm/", "run:start"}})
	kept := map[string]bool{}
	for _, k := range plan.Kept {
		kept[k.Name] = true
	}
	for _, r := range plan.Records {
		if strings.HasPrefix(r.Name, "@saga/") {
			t.Errorf("the plan redacts the saga record %q", r.Name)
		}
	}
	if !kept["@saga/compensate/a1"] {
		t.Errorf("the plan does not list the compensation memo as kept: %+v", plan.Kept)
	}
	// Driving the redacted run again is refused, as for any run that holds a redacted record: it is
	// not reported as a rollback that compensated nothing, and nothing runs again.
	if err := drive(); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "redacted") {
		t.Fatalf("re-drive of the redacted run = %v, want ErrConfig naming the redaction", err)
	}
	if l.undoCount["A"] != 1 {
		t.Errorf("the compensator for A ran %d times, want 1", l.undoCount["A"])
	}
}

// failOnce fails the first Insert of one name, as a crash between a result and its record would.
type failOnce struct {
	*MemStore
	name string
	done bool
}

func (f *failOnce) Insert(ctx context.Context, run, name string, data []byte) (Entry, bool, error) {
	if name == f.name && !f.done {
		f.done = true
		return Entry{}, false, errors.New("injected crash")
	}
	return f.MemStore.Insert(ctx, run, name, data)
}

// A sub-agent's run that finished, under a parent that crashed before recording the call, is read
// by the parent when it resumes. The store refuses to redact it while the parent is not over, so
// the resumed parent sees the call's real result; once the parent is over, the child may be erased.
func TestRedact_ChildOfLiveParentIsRefused(t *testing.T) {
	ctx := context.Background()
	ms := NewMemStore()
	fs := &failOnce{MemStore: ms, name: "tool:p1"}
	j := mustJournal(fs)
	child := mustNew(&scriptModel{turns: [][]Emit{textTurn("child answer with PII")}}, j)
	mk := func() *Agent {
		turns := [][]Emit{toolTurn("p1", "kid", `{"task":"x"}`), textTurn("parent done")}
		if fs.done {
			turns = turns[1:]
		}
		return mustNew(&scriptModel{turns: turns}, j, WithTools(MustSubAgent("kid", "", child)))
	}
	if _, err := mk().Run(ctx, "root", UserText("go")); err == nil {
		t.Fatal("the injected crash did not stop the parent")
	}
	var kids []string
	for id, err := range ms.Runs(ctx, RunFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		if id != "root" {
			kids = append(kids, id)
		}
	}
	if len(kids) == 0 {
		t.Fatal("no sub-run was started")
	}
	for _, id := range kids {
		plan, err := PlanRedaction(ctx, j, id, RedactionSelector{Payloads: true})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Refusal == "" {
			t.Errorf("the plan of %s, a finished child of a live parent, has no Refusal", id)
		}
		if _, err := Redact(ctx, j, plan, time.Now()); !errors.Is(err, ErrRedactParentLive) {
			t.Fatalf("Redact of %s = %v, want ErrRedactParentLive", id, err)
		}
	}
	res, err := mk().Run(ctx, "root", UserText("go"))
	if err != nil {
		t.Fatalf("the parent cannot resume: %v", err)
	}
	if got, ok, _ := j.Get(ctx, "root", "tool:p1"); !ok || strings.Contains(string(got.Result), "redacted") {
		t.Errorf("the parent recorded %s for the call", got.Result)
	}
	_ = res
	// The parent is over now: the child's records may be erased.
	for _, id := range kids {
		plan, err := PlanRedaction(ctx, j, id, RedactionSelector{Payloads: true})
		if err != nil || plan.Refusal != "" {
			t.Fatalf("plan of %s after the parent ended = %+v, %v", id, plan, err)
		}
		if _, err := Redact(ctx, j, plan, time.Now()); err != nil {
			t.Fatalf("Redact of %s after the parent ended: %v", id, err)
		}
	}
}

// A tombstone written over an attempt marker by anything but a redaction keeps the Merkle leaf, so
// signed heads verify; the engine must not read it as "never attempted" and fire the effect again.
func TestRedact_TombstonedAttemptMarkerDoesNotRefire(t *testing.T) {
	ctx := context.Background()
	ms := NewMemStore()
	fs := &failOnce{MemStore: ms, name: "tool:c1"}
	j := mustJournal(fs)
	fired := 0
	pay := MustFunc("pay", "", func(context.Context, struct{}) (struct{}, error) { fired++; return struct{}{}, nil })
	mk := func(turns [][]Emit) *Agent { return mustNew(&scriptModel{turns: turns}, j, WithTools(pay)) }
	if _, err := mk([][]Emit{toolTurn("c1", "pay", `{}`), textTurn("done")}).Run(ctx, "r", UserText("go")); err == nil {
		t.Fatal("the injected crash did not stop the run")
	}
	r := ms.runs["r"]
	i := r.byName["attempt:tool:c1"]
	tomb, err := NewTombstone("attempt:tool:c1", r.entries[i].Data, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.entries[i].Data = tomb
	_, err = mk([][]Emit{textTurn("done")}).Run(ctx, "r", UserText("go"))
	if err == nil {
		t.Errorf("a run whose attempt marker is a tombstone was driven")
	}
	if fired != 1 {
		t.Errorf("the effect fired %d times, want 1", fired)
	}
}

// A tombstone at a protected name was not written by a redaction: every read of the journal
// reports corruption (ErrStorage), never a run in some state.
func TestRedact_TombstoneAtAProtectedNameIsCorruption(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"run:complete", "audit:x", "@saga/compensate/c", "run:limits:1", "approval-tally:x"} {
		st := NewMemStore()
		j := mustJournal(st)
		a := mustNew(&scriptModel{turns: [][]Emit{textTurn("answer")}}, j)
		if _, err := a.Run(ctx, "r", UserText("input")); err != nil {
			t.Fatal(err)
		}
		b, err := JournalEntry(name, Record{Kind: StepValue, Result: []byte(`1`)})
		if err != nil {
			t.Fatal(err)
		}
		if name != "run:complete" {
			if _, _, err := st.Insert(ctx, "r", name, b); err != nil {
				t.Fatal(err)
			}
		}
		r := st.runs["r"]
		i := r.byName[name]
		data := r.entries[i].Data
		tomb, err := NewTombstone(name, data, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		r.entries[i].Data = tomb
		if _, err := Status(ctx, j, "r"); !errors.Is(err, ErrStorage) {
			t.Errorf("%s: Status = %v, want ErrStorage", name, err)
		}
		if _, err := j.History(ctx, "r"); !errors.Is(err, ErrStorage) {
			t.Errorf("%s: History = %v, want ErrStorage", name, err)
		}
		n, err := Recover(ctx, j, func(context.Context, string, RunStart) error {
			t.Errorf("%s: Recover resumed the run", name)
			return nil
		})
		_ = n
		_ = err
	}
}

// Status of a run whose run:start was redacted says so: it started, by a name nothing outside the
// database attests.
func TestRedact_StatusLabelsARedactedStart(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	a := mustNew(&scriptModel{turns: [][]Emit{textTurn("answer")}}, j)
	if _, err := a.Run(ctx, "r", UserText("input")); err != nil {
		t.Fatal(err)
	}
	if st, _ := Status(ctx, j, "r"); st.StartRedacted {
		t.Fatalf("an intact run reports StartRedacted")
	}
	redactPlanned(t, j, "r", RedactionSelector{Names: []string{"run:start"}})
	st, err := Status(ctx, j, "r")
	if err != nil || st.State != RunCompleted || !st.StartRedacted {
		t.Fatalf("Status = %+v, %v; want completed with StartRedacted", st, err)
	}
}

// Every key family the engine writes has a line in redactionClasses, so a record added later is
// classified on purpose (protected, payload or bookkeeping) rather than redactable by default.
func TestRedactionClasses_CoverEveryReservedKey(t *testing.T) {
	for _, p := range reservedPrefixes {
		if p == "@" {
			continue // its families are checked below
		}
		if _, _, ok := classOf(p); !ok {
			t.Errorf("reserved prefix %q has no line in redactionClasses", p)
		}
	}
	src, err := os.ReadFile("keys.go")
	if err != nil {
		t.Fatal(err)
	}
	line := ""
	for _, l := range strings.Split(string(src), "\n") {
		if strings.Contains(l, `"@",`) && strings.Contains(l, "engine-internal steps") {
			line = l
		}
	}
	if line == "" {
		t.Fatal(`the "@" line of reservedPrefixes is gone; update this test`)
	}
	fams := regexp.MustCompile(`@[a-z-]+(?:/|\b)`).FindAllString(line, -1)
	if len(fams) < 6 {
		t.Fatalf("found only %v among the @ keys of %q", fams, line)
	}
	for _, f := range fams {
		if _, _, ok := classOf(f); !ok {
			t.Errorf("engine key family %q has no line in redactionClasses", f)
		}
	}
	// Spot-check the classes that matter to a re-drive.
	for name, protected := range map[string]bool{"@saga/compensate/x": true, "@saga/args/x": true, "@subrun/a/b": true, "@llm/0": false, "@spend/x": false, "run:start": false, "run:complete": true, "tool:x": false} {
		if got := RedactionProtected(name, StepValue) != ""; got != protected {
			t.Errorf("RedactionProtected(%q) = %v, want %v", name, got, protected)
		}
	}
}
