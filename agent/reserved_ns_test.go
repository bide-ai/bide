package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// capRunID is the shape of a cap ledger's run ID (see the note on caps): "@cap/" and encoded
// segments. Nothing writes one yet; the tests below put records in it directly.
const capRunID = "@cap/ns/refunds/usd/d2026-10-22/0123abcd"

func TestIsReservedRun(t *testing.T) {
	for id, want := range map[string]struct{ reserved, cap bool }{
		StopControlRun:           {true, false},
		capRunID:                 {true, true},
		"@cap/":                  {true, true},
		"@cap/x>@turn/0":         {true, true}, // a session under a cap-named id would be one too
		"@cap":                   {false, false},
		"@capx/y":                {false, false},
		"@stop/x":                {false, false},
		"@stopx":                 {false, false},
		"refund-42":              {false, false},
		"cap/x":                  {false, false},
		"r>c1":                   {false, false},
		"x@cap/y":                {false, false},
		"":                       {false, false},
		"r>@turn/0":              {false, false},
		strings.ToUpper("@cap/"): {false, false}, // run IDs are case-sensitive
	} {
		if got := IsReservedRun(id); got != want.reserved {
			t.Errorf("IsReservedRun(%q) = %v, want %v", id, got, want.reserved)
		}
		if got := IsCapRun(id); got != want.cap {
			t.Errorf("IsCapRun(%q) = %v, want %v", id, got, want.cap)
		}
	}
}

// T8: a caller can never name a run under @cap/, however it starts one.
func TestReservedNamespace_CallerRunIDIsRefused(t *testing.T) {
	ctx := context.Background()
	for _, id := range []string{"@cap/x", "@cap/", capRunID} {
		if err := checkRunID(ctx, id); !errors.Is(err, ErrConfig) {
			t.Errorf("checkRunID(%q) = %v, want ErrConfig", id, err)
		}
		// A session or turn context does not lift it.
		if err := checkRunID(withSessionRun(ctx, id+">@turn/0"), id+">@turn/0"); !errors.Is(err, ErrConfig) {
			t.Errorf("checkRunID(%q as a session turn) = %v, want ErrConfig", id+">@turn/0", err)
		}
	}
	for _, id := range []string{"@cap", "@capx/y", "cap/x", "a@cap/b", "refund-42"} {
		if err := checkRunID(ctx, id); err != nil {
			t.Errorf("checkRunID(%q) = %v, want nil", id, err)
		}
	}

	// Through the public entry points, and nothing is written.
	store := NewMemStore()
	a := mustNew(&scriptModel{turns: [][]Emit{textTurn("x")}}, mustJournal(store))
	if _, err := a.Run(ctx, capRunID, UserText("go")); !errors.Is(err, ErrConfig) {
		t.Errorf("Run under @cap/: %v, want ErrConfig", err)
	}
	if _, err := a.Session(ctx, "@cap/s"); !errors.Is(err, ErrConfig) {
		t.Errorf("Session(@cap/s): %v, want ErrConfig", err)
	}
	for id, err := range store.Runs(ctx, RunFilter{}) {
		t.Errorf("the store holds run %q (%v) after refused starts", id, err)
	}
}

// The record-name prefixes the cap ledger and the reasons use cannot be taken by a step.
func TestReservedNamespace_RecordNamesAreReserved(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"slot:0", "slot:", "cap-intent:c1", "cap-reserve:c1", "gate-reason:c1"} {
		if !IsReservedStepName(name) {
			t.Errorf("IsReservedStepName(%q) = false", name)
		}
		if err := checkStepName("Step", name); !errors.Is(err, ErrConfig) {
			t.Errorf("checkStepName(%q) = %v, want ErrConfig", name, err)
		}
		// A step through the agent is refused and writes nothing.
		j := memJournal()
		_, err := j.Step(ctx, "r", name, func(context.Context) (string, error) { return "x", nil })
		if !errors.Is(err, ErrConfig) {
			t.Errorf("Step(%q) = %v, want ErrConfig", name, err)
		}
	}
	for _, name := range []string{"slots:0", "slot", "cap-intents", "gate-reasons", "capture", "mygate-reason:x"} {
		if IsReservedStepName(name) {
			t.Errorf("IsReservedStepName(%q) = true: the prefix is too wide", name)
		}
	}
	// The new prefixes are classified for redaction on purpose (see TestRedactionClasses_CoverEveryReservedKey),
	// and the ledger's records are not erasable: a count and a re-drive read them.
	for _, name := range []string{"slot:0", "cap-intent:c1", "cap-reserve:c1", "gate-reason:c1"} {
		if RedactionProtected(name, StepValue) == "" {
			t.Errorf("RedactionProtected(%q) = \"\", want a reason", name)
		}
	}
}

// A recovery pass skips a ledger run, as it skips the control run, and does not read it.
func TestReservedNamespace_RecoverSkipsCapRuns(t *testing.T) {
	ctx := context.Background()
	if recoverable(capRunID) || recoverable(StopControlRun) {
		t.Fatal("a reserved run is recoverable")
	}
	if !recoverable("r") {
		t.Fatal("an ordinary run is not recoverable")
	}
	store := NewMemStore()
	seedRun(t, store, "r")
	seedRun(t, store, capRunID)
	var mu sync.Mutex
	var driven []string
	resume := func(_ context.Context, id string, _ RunStart) error {
		mu.Lock()
		driven = append(driven, id)
		mu.Unlock()
		return nil
	}
	n, err := Recover(ctx, mustJournal(store), resume)
	if err != nil || n != 1 || !slices.Equal(driven, []string{"r"}) {
		t.Fatalf("Recover: n = %d, driven = %v, err = %v; want only r", n, driven, err)
	}
}

func TestReservedNamespace_RecoverLoopSkipsCapRuns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := NewMemStore()
	seedRun(t, store, "r")
	seedRun(t, store, capRunID)
	var mu sync.Mutex
	var driven []string
	resume := func(_ context.Context, id string, _ RunStart) error {
		mu.Lock()
		driven = append(driven, id)
		mu.Unlock()
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RecoverLoop(ctx, mustJournal(store), resume, WithRecoverInterval(5*time.Millisecond))
	}()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(driven) > 0 })
	time.Sleep(60 * time.Millisecond) // several more passes
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	for _, id := range driven {
		if id != "r" {
			t.Fatalf("RecoverLoop drove %q (all driven: %v)", id, driven)
		}
	}
}

// The notification sweep does not scan a ledger run: a pending-approval record in one (nothing
// writes it; a hostile or buggy writer could) raises no notification, while the same record in an
// ordinary run does.
func TestReservedNamespace_NotifyPendingSkipsCapRuns(t *testing.T) {
	clk := sweepClock()
	j := memJournal()
	putGate(t, j, capRunID, ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	trail, sink := NewMemNotifyTrail(), &fakeSink{}
	if err := sweep(t, j, sink, trail, clk); err != nil || sink.count() != 0 {
		t.Fatalf("the sweep notified a gate in a ledger run: %v, %d calls", err, sink.count())
	}
	putGate(t, j, "r", ApprovalWait{ToolUseID: "c1", ToolName: "refund", Single: true})
	if err := sweep(t, j, sink, trail, clk); err != nil || sink.count() != 1 {
		t.Fatalf("the control run r was not notified: %v, %d calls", err, sink.count())
	}
}

// The one-time backfill lists the events of ordinary runs and none of a ledger run's trail.
func TestReservedNamespace_BackfillSkipsCapRuns(t *testing.T) {
	ctx := context.Background()
	clk := sweepClock()
	j := memJournal()
	tr := NewMemNotifyTrail()
	at := clk.now()
	for _, run := range []string{"run00", capRunID} {
		key := HaltedKey(run, OpTool, "c1", 100)
		recs := []NotifyRecord{
			{Kind: NotifyObserved, Key: key, RunID: run, At: at, EventKind: string(NotificationOutcomeUnknown), EventAt: at, DeadlineAt: at.Add(time.Minute)},
			{Kind: NotifyClaimed, Key: key, RunID: run, At: at, Holder: "old", Until: at.Add(time.Minute)},
			{Kind: NotifyAcked, Key: key, RunID: run, At: at, Attempts: 1, Status: 200},
		}
		for k, r := range recs {
			if err := tr.Append(ctx, NotifyTrailName(run), k, r); err != nil {
				t.Fatal(err)
			}
		}
		putMarker(t, j, run, "c1", at.UnixMilli())
	}
	e := testEngine(t, j, &fakeSink{}, tr, clk, "p")
	if err := e.backfillOpen(ctx); err != nil {
		t.Fatal(err)
	}
	_, recs, err := e.openHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var runs []string
	for _, w := range FoldWatch(recs) {
		runs = append(runs, w.RunID)
	}
	if !slices.Equal(runs, []string{"run00"}) {
		t.Fatalf("the backfill listed runs %v, want only run00", runs)
	}
}

// #: a tool-use ID that holds '#' is refused on a live turn, with a sentinel of its own that is a
// model fault and a protocol fault (and is not an ID reuse).
func TestCheckToolUseIDs_HashIsRefused(t *testing.T) {
	for _, id := range []string{"a#taint", "#", "call_1#", "x#y#z", "café#", "#taint"} {
		m := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: id, Name: "lookup", Args: json.RawMessage(`{}`)}}}
		err := checkToolUseIDs(m, map[string]bool{})
		if !errors.Is(err, ErrToolUseIDInvalid) || !errors.Is(err, ErrModel) || !errors.Is(err, ErrProtocol) {
			t.Errorf("checkToolUseIDs(%q) = %v, want ErrToolUseIDInvalid wrapping ErrModel and ErrProtocol", id, err)
		}
		if errors.Is(err, ErrToolUseIDReused) {
			t.Errorf("checkToolUseIDs(%q) = %v, reported as an ID reuse", id, err)
		}
		if errors.Is(err, ErrConfig) {
			t.Errorf("checkToolUseIDs(%q) = %v, is a config error", id, err)
		}
	}
	// Other IDs, including every character an encoding escapes, are unchanged.
	for _, id := range []string{"call_1", "toolu_01A", "a%23b", "a:b/c>d@e~f", "x taint", "a b", "id-with-dash"} {
		m := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: id, Name: "lookup", Args: json.RawMessage(`{}`)}}}
		if err := checkToolUseIDs(m, map[string]bool{}); err != nil {
			t.Errorf("checkToolUseIDs(%q) = %v, want nil", id, err)
		}
	}
	// The existing refusals keep their sentinel.
	m := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{}`)}}}
	if err := checkToolUseIDs(m, map[string]bool{"c1": true}); !errors.Is(err, ErrToolUseIDReused) || errors.Is(err, ErrToolUseIDInvalid) {
		t.Errorf("a reused ID = %v, want ErrToolUseIDReused only", err)
	}
}

// A tool-use ID that starts with "node:" is refused on a live turn like one with '#': the keys and gate
// subjects of a plan flow's nodes (and a node's escalation instance, "node:x:taint") are spelled so, and no
// model-chosen ID may share them. An ID that only contains or ends like one is unchanged. A turn replayed from
// a journal is not checked (checkToolUseIDs is called on live turns only).
func TestCheckToolUseIDs_NodePrefixIsRefused(t *testing.T) {
	for _, id := range []string{"node:x", "node:x:taint", "node:iter:2:x:taint", "node:iter:taint", "node:", "node:taint", "node:x:foo", "node:x:step:y", "node:a#b:taint"} {
		m := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: id, Name: "lookup", Args: json.RawMessage(`{}`)}}}
		err := checkToolUseIDs(m, map[string]bool{})
		if !errors.Is(err, ErrToolUseIDInvalid) || !errors.Is(err, ErrModel) || !errors.Is(err, ErrProtocol) {
			t.Errorf("checkToolUseIDs(%q) = %v, want ErrToolUseIDInvalid wrapping ErrModel and ErrProtocol", id, err)
		}
		if errors.Is(err, ErrToolUseIDReused) || errors.Is(err, ErrConfig) {
			t.Errorf("checkToolUseIDs(%q) = %v, reported as a reuse or a config error", id, err)
		}
	}
	for _, id := range []string{"x:taint", "call:taint", "xnode:x", "a:node:x", "Node:x", "nodes:x", "no", "call_node:1"} {
		m := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: id, Name: "lookup", Args: json.RawMessage(`{}`)}}}
		if err := checkToolUseIDs(m, map[string]bool{}); err != nil {
			t.Errorf("checkToolUseIDs(%q) = %v, want nil", id, err)
		}
	}
}

// A call named "node:x" in a journal written before the prefix was refused keeps the escalation subject it
// had ("node:x#taint", the ID and "#taint", as for every call), so a decision recorded under it still counts.
func TestEscalationSubject_OfAnOldNodePrefixedCallIsUnchanged(t *testing.T) {
	for _, id := range []string{"node:x", "node:iter:2:x", "node:x:foo", "node:"} {
		if got := escalationID(id); got != id+"#taint" {
			t.Errorf("escalationID(%q) = %q, want %q", id, got, id+"#taint")
		}
	}
}

// A turn with a '#' ID fails the run without running the tool or journaling the turn.
func TestRun_HashToolUseIDIsAnError(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1#taint", "lookup", `{}`), textTurn("done")}}
	store := memJournal()
	_, err := mustNew(m, store, WithTools(tool)).Run(context.Background(), "r", UserText("go"))
	if !errors.Is(err, ErrToolUseIDInvalid) || !errors.Is(err, ErrModel) || !errors.Is(err, ErrProtocol) {
		t.Fatalf("err = %v (tool ran %d times), want ErrToolUseIDInvalid", err, calls)
	}
	if calls != 0 {
		t.Fatalf("tool ran %d times, want 0", calls)
	}
	recs, _ := store.History(context.Background(), "r")
	for _, r := range recs {
		if r.Kind == StepModel {
			t.Fatalf("the rejected turn was journaled: %+v", r)
		}
	}
}

// A retry middleware asks the model again, and the retry's valid ID completes the run.
func TestRun_HashToolUseIDIsRetriedByMiddleware(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		toolTurn("c1#taint", "lookup", `{"q":"first"}`), // rejected
		toolTurn("c1", "lookup", `{"q":"first"}`),       // the retry
		textTurn("done"),
	}}
	retryOnce := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			resp, err := next(ctx, call)
			if errors.Is(err, ErrModel) {
				return next(ctx, call)
			}
			return resp, err
		}
	}
	store := memJournal()
	res, err := mustNew(m, store, WithTools(tool), WithMiddleware(retryOnce)).Run(context.Background(), "r", UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Message.Text() != "done" || calls != 1 {
		t.Fatalf("out = %q, tool ran %d times; want done and 1", res.Message.Text(), calls)
	}
	recs, _ := store.History(context.Background(), "r")
	for _, r := range recs {
		if r.Kind == StepModel {
			for _, tu := range r.Message.toolUses() {
				if strings.Contains(tu.ID, "#") {
					t.Fatalf("the rejected turn was journaled: %+v", tu)
				}
			}
		}
	}
}

// A response a middleware substitutes is checked as well (the journal records what the chain
// returns).
func TestRun_HashToolUseIDBySubstitutedResponseIsAnError(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{textTurn("unused")}}
	substitute := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			return ModelResponse{Message: Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "a#b", Name: "lookup", Args: json.RawMessage(`{}`)}}}}, nil
		}
	}
	_, err := mustNew(m, memJournal(), WithTools(tool), WithMiddleware(substitute)).Run(context.Background(), "r", UserText("go"))
	if !errors.Is(err, ErrToolUseIDInvalid) || calls != 0 {
		t.Fatalf("err = %v (tool ran %d times), want ErrToolUseIDInvalid and 0", err, calls)
	}
}

// A journal written before the check, holding a '#' in a tool-use ID, still replays: recorded
// turns are not re-checked, only live ones. The run then continues live with a valid ID.
func TestRun_ReplaysJournalWithHashToolUseID(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	asst := func(id string) *Message {
		return &Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: id, Name: "lookup", Args: json.RawMessage(`{}`)}}}
	}
	for _, r := range []struct {
		name string
		rec  Record
	}{
		{"@llm/0", Record{Kind: StepModel, Message: asst("old#id")}},
		{ToolResultStep("old#id"), Record{Kind: StepToolResult, ToolUseID: "old#id", Result: json.RawMessage(`1`)}},
	} {
		if _, err := store.do(ctx, "r", r.name, func(context.Context) (Record, error) { return r.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{textTurn("done")}}
	res, err := mustNew(m, store, WithTools(tool)).Run(ctx, "r", UserText("go"))
	if err != nil {
		t.Fatalf("replay of a recorded journal with a '#' ID failed: %v", err)
	}
	if res.Message.Text() != "done" || calls != 0 {
		t.Fatalf("out = %q, tool ran %d times; want done and 0", res.Message.Text(), calls)
	}
	// A live turn that sends that ID again is refused as a '#' ID.
	m2 := &scriptModel{turns: [][]Emit{toolTurn("old#id", "lookup", `{}`), textTurn("done")}}
	store2 := memJournal()
	for _, r := range []struct {
		name string
		rec  Record
	}{
		{"@llm/0", Record{Kind: StepModel, Message: asst("old#id")}},
		{ToolResultStep("old#id"), Record{Kind: StepToolResult, ToolUseID: "old#id", Result: json.RawMessage(`1`)}},
	} {
		if _, err := store2.do(ctx, "r", r.name, func(context.Context) (Record, error) { return r.rec, nil }); err != nil {
			t.Fatal(err)
		}
	}
	_, err = mustNew(m2, store2, WithTools(tool)).Run(ctx, "r", UserText("go"))
	if !errors.Is(err, ErrToolUseIDInvalid) {
		t.Fatalf("a live turn with a historic '#' ID: %v, want ErrToolUseIDInvalid", err)
	}
}

// Every exported call that writes records to a run a caller names refuses a run under @cap/ with
// ErrConfig and writes nothing: a caller cannot create, extend or pre-claim a ledger run. (Reads
// are not refused, and @stop keeps its existing behaviour in these calls.)
func TestReservedNamespace_ExportedWritesRefuseCapRuns(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	j := mustJournal(store)
	run := capRunID
	calls := map[string]func() error{
		"Approve": func() error { return Approve(ctx, j, run, "c1", true) },
		"SubmitDecision": func() error {
			return SubmitDecision(ctx, j, Decision{RunID: run, ToolUseID: "c1", ApproverID: "a", Approved: true})
		},
		"Cancel": func() error { return Cancel(ctx, j, run, "why") },
		"ResolveHalt": func() error {
			return ResolveHalt(ctx, j, HaltRef{RunID: run, Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: 1})
		},
		"AnswerInterrupt": func() error { return j.AnswerInterrupt(ctx, run, "n", 1) },
		"Signal":          func() error { return j.Signal(ctx, run, "n", 1) },
		"Enqueue":         func() error { return j.Enqueue(ctx, run, "ch", "k", 1) },
		"Ack":             func() error { return Ack(ctx, j, run, "ch", "k") },
		"ClaimAttempt": func() error {
			_, _, err := ClaimAttempt(ctx, j, run, "n", Record{Kind: StepAttempt})
			return err
		},
		"Step": func() error {
			_, err := j.Step(ctx, run, "n", func(context.Context) (int, error) { return 1, nil })
			return err
		},
		"Lease": func() error {
			_, err := Lease(ctx, j, run, func(context.Context) error { return nil })
			return err
		},
		"Parallel": func() error {
			_, err := j.Parallel(ctx, run, []Task[int]{{Name: "t", Fn: func(context.Context) (int, error) { return 1, nil }}})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), name+": run ID") {
			t.Errorf("%s on %q = %v, want ErrConfig from %s's own run ID check", name, run, err, name)
		}
	}
	for id, err := range store.Runs(ctx, RunFilter{}) {
		t.Errorf("the store holds run %q (%v) after refused writes", id, err)
	}
	// The same calls on an ordinary run are not refused for its name.
	if err := j.Signal(ctx, "r", "n", 1); err != nil {
		t.Errorf("Signal on an ordinary run: %v", err)
	}
}
