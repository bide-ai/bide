package agent

// Adversarial review of the #104 second-round fixes (621c62d, ed71031). Copy into agent/ to run.

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
)

// C1: the content check of fix 4 compares the record the step built with the record the journal
// returns, decoded. A tool call whose args are not in compact JSON (a provider that streams
// `{"q": "x"}`) is compacted by the journal's encoding, so the decoded record never equals the
// built one: the turn's OnAnswer never runs, and its spend, taken for another driver's, is
// journaled again as late spend.
func TestAdv104c_NonCompactArgsFailContentCheck(t *testing.T) {
	ctx := context.Background()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		toolTurnWithUsage("c1", "lookup", `{"q": "x"}`, billed),
		textTurnWithUsage("final", billed),
	}}
	var answers int
	key := new(int)
	count := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			call.OnAnswer(key, func(context.Context, ModelResponse) { answers++ })
			return next(ctx, call)
		}
	}
	res, err := New(m, NewMemStore(), tool).Use(count).RunResult(ctx, "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, billed)
	if answers != 2 || res.Spend != want {
		t.Fatalf("answers = %d, want 2; Spend = %+v, want %+v", answers, res.Spend, want)
	}
}

// lookupErrDrive drives run "r" through d once: the @llm/0 write fails without landing and the
// read that should settle it fails too, so the drive keeps the turn's spend for the next drive.
func lookupErrDrive(t *testing.T, st *faultStore, d Durable) {
	t.Helper()
	st.mu.Lock()
	st.failNoCommit[modelStep(0)] = true
	st.mu.Unlock()
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := New(m, d).Use(armGet(st)).RunResult(context.Background(), "r", "go"); err == nil {
		t.Fatal("want the write failure")
	}
	st.mu.Lock()
	st.failGet = map[string]bool{}
	st.mu.Unlock()
}

// C2 control: the next drive through the same Journal journals the failed turn's spend.
func TestAdv104c_KeptSpendSameJournal(t *testing.T) {
	st := newFaultStore()
	j, _ := NewJournal(st)
	lookupErrDrive(t, st, j)
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	res, err := New(m, j).RunResult(context.Background(), "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, billed)
	if res.Spend != want {
		t.Fatalf("Spend = %+v, want %+v", res.Spend, want)
	}
}

// C2: the kept spend is keyed by the *Journal pointer, not by the store's identity (the key
// claims and in-flight steps share across Journals, storeIdentity). The run's next drive in the
// same process through another Journal over the same store (a Journal per request) never sees it:
// the failed call's billed spend is never journaled, and the entry is never freed.
func TestAdv104c_KeptSpendOtherJournalSameStore(t *testing.T) {
	st := newFaultStore()
	j1, _ := NewJournal(st)
	lookupErrDrive(t, st, j1)
	j2, _ := NewJournal(st)
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	res, err := New(m, j2).RunResult(context.Background(), "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	want := billed
	addUsage(&want, billed)
	pendingSpends.Lock()
	left := len(pendingSpends.m[pendingKey{j1, "r"}])
	pendingSpends.Unlock()
	if res.Spend != want || left != 0 {
		t.Fatalf("Spend = %+v, want %+v; entries still kept under the first Journal: %d", res.Spend, want, left)
	}
}

// C3: kept spend is never evicted. Each run whose lookup failed and that is not driven again in
// this process (the process that resumes it is another one, or the run is abandoned) leaves an
// entry, holding the drive's turn state and answer closures, for the life of the process.
func TestAdv104c_KeptSpendIsUnbounded(t *testing.T) {
	defer func(n int) { maxPendingSpends = n }(maxPendingSpends)
	maxPendingSpends = 16
	st := newFaultStore()
	j, _ := NewJournal(st)
	const n = 50
	for i := range n {
		st.mu.Lock()
		st.failNoCommit[modelStep(0)] = true
		st.mu.Unlock()
		m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
		_, _ = New(m, j).Use(armGet(st)).RunResult(context.Background(), "run-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "go")
		st.mu.Lock()
		st.failGet = map[string]bool{}
		st.mu.Unlock()
	}
	pendingSpends.Lock()
	kept := 0
	for k := range pendingSpends.m {
		if k.store == any(j) || k.store == j.id {
			kept++
		}
	}
	pendingSpends.Unlock()
	if kept > maxPendingSpends {
		t.Fatalf("%d runs kept %d pending entries, past the bound %d", n, kept, maxPendingSpends)
	}
	// The oldest runs were dropped, the newest kept.
	pendingSpends.Lock()
	_, newest := pendingSpends.m[pendingKey{j.id, "run-" + string(rune('a'+(n-1)%26)) + string(rune('a'+(n-1)/26))}]
	_, oldest := pendingSpends.m[pendingKey{j.id, "run-aa"}]
	pendingSpends.Unlock()
	if !newest || oldest {
		t.Fatalf("newest kept %v, oldest kept %v: want the oldest dropped first", newest, oldest)
	}
	// Driving a kept run takes its entry, and the count of entries held stays exact.
	m := &scriptModel{turns: [][]Emit{textTurnWithUsage("done", billed)}}
	if _, err := New(m, j).RunResult(context.Background(), "run-"+string(rune('a'+(n-1)%26))+string(rune('a'+(n-1)/26)), "go"); err != nil {
		t.Fatal(err)
	}
	pendingSpends.Lock()
	total := 0
	for _, ps := range pendingSpends.m {
		total += len(ps)
	}
	held := pendingSpends.n
	pendingSpends.Unlock()
	if held != total {
		t.Fatalf("pendingSpends counts %d entries, holds %d", held, total)
	}
}

// armGet makes reads of @llm/0 fail once the model has answered, before the turn's write.
func armGet(st *faultStore) Middleware {
	return func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			resp, err := next(ctx, call)
			st.mu.Lock()
			st.failGet[modelStep(0)] = true
			st.mu.Unlock()
			return resp, err
		}
	}
}

// Text with invalid UTF-8 is journaled as U+FFFD, so the record read back differs from the one
// the step built. The turn is still identified as the step's own (by the salt the journal
// stamps): its OnAnswer runs once and its spend is counted once.
func TestAdv104c_InvalidUTF8TextIsTheTurnsOwnRecord(t *testing.T) {
	ctx := context.Background()
	m := &scriptModel{turns: [][]Emit{{{Event: TextDelta{Text: "bad \xff byte"}}, {Event: Finish{Reason: FinishStop, Usage: billed}}}}}
	var answers int
	key := new(int)
	count := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			call.OnAnswer(key, func(context.Context, ModelResponse) { answers++ })
			return next(ctx, call)
		}
	}
	res, err := New(m, NewMemStore()).Use(count).RunResult(ctx, "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	if answers != 1 || res.Spend != billed {
		t.Fatalf("answers = %d, want 1; Spend = %+v, want %+v", answers, res.Spend, billed)
	}
}

// Two drivers of one run answer its turn with records equal in every journaled field; one records
// it. The salt the journal holds tells them apart: the other's request is journaled as late spend,
// so both requests count, and only the recording driver runs its answer functions.
func TestAdv104c_IdenticalRecordsOfTwoDrivers(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	g := &twoGate{open: make(chan struct{})}
	var answers atomic.Int32
	key := new(int)
	count := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			call.OnAnswer(key, func(context.Context, ModelResponse) { answers.Add(1) })
			return next(ctx, call)
		}
	}
	var wg sync.WaitGroup
	for range 2 {
		j, err := NewJournal(procStore{mem})
		if err != nil {
			t.Fatal(err)
		}
		m := &gatedTurn{g: g, text: "same", u: billed}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := New(m, j).Use(count).RunResult(ctx, "r", "go"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	j, _ := NewJournal(procStore{mem})
	res, err := New(&scriptModel{}, j).RunResult(ctx, "r", "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Spend != twice(billed) || answers.Load() != 1 {
		t.Fatalf("Spend = %+v, want both requests' %+v; answers ran %d times, want 1", res.Spend, twice(billed), answers.Load())
	}
}

// A store that journals no salt (it breaks the contract) is compared by the journal encoding: a
// record read back in its canonical form is the one built, and a different one is not.
func TestAdv104c_OwnRecordWithoutSalt(t *testing.T) {
	msg := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q": "x"}`)}, Text{Text: "bad \xff"}}}
	built := Record{Kind: StepModel, Message: &msg, Usage: &billed, Finish: FinishToolUse}
	if err := stampSalt(&built); err != nil {
		t.Fatal(err)
	}
	b, err := EncodeRecord(Record{Name: "@llm/0", Kind: StepModel, Message: &msg, Usage: &billed, Finish: FinishToolUse})
	if err != nil {
		t.Fatal(err)
	}
	held, err := DecodeRecord(b)
	if err != nil {
		t.Fatal(err)
	}
	if !ownRecord(held, built) {
		t.Fatal("the record read back in canonical form is not taken as the one built")
	}
	other := held
	other.Finish = FinishStop
	if ownRecord(other, built) {
		t.Fatal("a different record is taken as the one built")
	}
	salted := held
	salted.salt = make([]byte, SaltSize)
	if ownRecord(salted, built) {
		t.Fatal("a record with another salt is taken as the one built")
	}
}
