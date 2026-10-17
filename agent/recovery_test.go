package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// answerModel answers directly on its first turn (no tool calls), so a run completes.
type answerModel struct{}

func (answerModel) Stream(_ context.Context, _ Request) (*Stream, error) {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: TextDelta{Text: "done"}}
	ch <- Emit{Event: Finish{Reason: "stop"}}
	close(ch)
	return NewStream(ch), nil
}

// approvalModel calls a tool that requires human approval, so the run pauses durably.
type approvalModel struct{}

func (approvalModel) Stream(_ context.Context, req Request) (*Stream, error) {
	ch := make(chan Emit, 2)
	ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "pay", Name: "charge", ArgsFragment: []byte(`{}`)}}
	ch <- Emit{Event: Finish{Reason: "tool_use"}}
	close(ch)
	return NewStream(ch), nil
}

// TestRecover_SkipsCompletedRedrivesIncomplete drives two runs to completion and leaves
// one paused for approval, then asserts Recover re-drives only the incomplete run.
func TestRecover_SkipsCompletedRedrivesIncomplete(t *testing.T) {
	ctx := context.Background()
	store := memJournal()

	// Two runs that finish (their journals get a completion marker).
	done := mustNew(answerModel{}, store)
	if _, err := done.Run(ctx, "done1", UserText("hi")); err != nil {
		t.Fatalf("done1: %v", err)
	}
	if _, err := done.Run(ctx, "done2", UserText("hi")); err != nil {
		t.Fatalf("done2: %v", err)
	}
	// One run that pauses for human approval (never reaches the terminal marker).
	charge := MustFunc("charge", "charge a card", func(context.Context, struct{}) (string, error) { return "charged", nil }, WithApproval(SingleApproval()))
	paused := mustNew(approvalModel{}, store, WithTools(charge))
	_, err := paused.Run(ctx, "paused1", UserText("hi"))
	var pa *ApprovalPending
	if !errors.As(err, &pa) {
		t.Fatalf("paused1 should await approval, got %v", err)
	}

	// IsComplete distinguishes the two classes directly.
	for _, id := range []string{"done1", "done2"} {
		if ok, err := IsComplete(ctx, store, id); err != nil || !ok {
			t.Fatalf("IsComplete(%s) = %v, %v; want true, nil", id, ok, err)
		}
	}
	if ok, err := IsComplete(ctx, store, "paused1"); err != nil || ok {
		t.Fatalf("IsComplete(paused1) = %v, %v; want false, nil", ok, err)
	}

	// Recover: the resume callback records which runIDs it was asked to re-drive.
	var asked []string
	n, err := Recover(ctx, store, func(ctx context.Context, runID string, _ RunStart) error {
		asked = append(asked, runID)
		_, err := paused.Run(ctx, runID, UserText("hi")) // still paused -> a pause error, treated as success
		return err
	})
	if err != nil {
		t.Fatalf("Recover returned an error for a still-paused run: %v", err)
	}
	if n != 1 {
		t.Fatalf("Recover re-drove %d runs, want 1 (only the incomplete one)", n)
	}
	sort.Strings(asked)
	if len(asked) != 1 || asked[0] != "paused1" {
		t.Fatalf("Recover asked to resume %v, want [paused1] only", asked)
	}
}

// TestRecover_StillPausedCountsAsRecovered confirms a re-driven run that is still waiting
// is a SUCCESS (no error joined) and is counted as recovered.
func TestRecover_StillPausedCountsAsRecovered(t *testing.T) {
	ctx := context.Background()
	store := memJournal()

	charge := MustFunc("charge", "charge a card", func(context.Context, struct{}) (string, error) { return "charged", nil }, WithApproval(SingleApproval()))
	a := mustNew(approvalModel{}, store, WithTools(charge))
	if _, err := a.Run(ctx, "p", UserText("hi")); !IsPause(err) {
		t.Fatalf("run should pause, got %v", err)
	}

	n, err := Recover(ctx, store, func(ctx context.Context, runID string, _ RunStart) error {
		_, err := a.Run(ctx, runID, UserText("hi")) // re-drives, still pauses for approval
		return err
	})
	if err != nil {
		t.Fatalf("a still-paused resume must not surface as an error, got %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered = %d, want 1", n)
	}
}

// TestRecover_NeedsLister confirms Recover reports a config error for a store that cannot
// enumerate its runs.
func TestRecover_NeedsLister(t *testing.T) {
	_, err := Recover(context.Background(), mustJournal(noListStore{}), func(context.Context, string, RunStart) error { return nil })
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("err = %v, want ErrConfig for a non-Lister store", err)
	}
}

// noListStore is a Store that does NOT implement Lister (or Leaser). It stores nothing: every
// Insert reports stored, and every read finds nothing.
type noListStore struct{}

func (noListStore) Insert(_ context.Context, _, name string, data []byte) (Entry, bool, error) {
	return Entry{Seq: 1, Name: name, Data: data}, true, nil
}
func (noListStore) Get(context.Context, string, string) (Entry, bool, error) {
	return Entry{}, false, nil
}
func (noListStore) Load(context.Context, string, int64) iter.Seq2[Entry, error] {
	return func(func(Entry, error) bool) {}
}

// TestRecover_WakerRebuild is Part 2: after a "crash" (fresh MemWaker, timers lost),
// Recover with a Waker-bound resume re-registers the sleeping run's wake, so advancing the
// clock and firing the waker resumes it to completion. The timer set is rebuilt purely by
// re-driving the run: Sleep sees the Waker on ctx and re-registers its journaled wake.
func TestRecover_WakerRebuild(t *testing.T) {
	var clk int64 = 1000
	now := func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) }
	store := memJournal()
	a := mustNew(sleepModel{}, store, WithTools(waitTool()))

	// The run sleeps for an hour and pauses durably. Its wake time is journaled.
	if _, err := a.Run(context.Background(), "sleeper", UserText("go"), WithClock(now)); !errorsIsSleeping(err) {
		t.Fatalf("run should sleep, got %v", err)
	}

	// Crash: a brand-new MemWaker has an EMPTY timer set (the in-memory timers are lost).
	var w *MemWaker
	w = NewMemWaker(func(ctx context.Context, runID string) error {
		_, err := a.Run(ctx, runID, UserText("go"), WithClock(now), WithWaker(w))
		return err
	})

	// Recover rebuilds the timer set: it re-drives the incomplete run with a Waker-bound
	// resume, and Sleep re-registers the journaled wake on the fresh waker.
	n, err := Recover(context.Background(), store, func(ctx context.Context, runID string, _ RunStart) error {
		_, err := a.Run(ctx, runID, UserText("go"), WithClock(now), WithWaker(w))
		return err
	})
	if err != nil {
		t.Fatalf("Recover: %v (a still-sleeping run is not a failure)", err)
	}
	if n != 1 {
		t.Fatalf("recovered = %d, want 1", n)
	}

	// Before the wake time, nothing fires (proving the re-registered timer honors it).
	if fired, err := w.Fire(context.Background(), now()); fired != 0 || err != nil {
		t.Fatalf("nothing should fire before the wake time, fired %d err %v", fired, err)
	}

	// Advance past the wake time and fire: the rebuilt timer resumes the run to completion.
	atomic.StoreInt64(&clk, 1000+3600)
	fired, err := w.Fire(context.Background(), now())
	if err != nil || fired != 1 {
		t.Fatalf("the rebuilt waker should resume exactly one run, fired %d err %v", fired, err)
	}
	if ok, err := IsComplete(context.Background(), store, "sleeper"); err != nil || !ok {
		t.Fatalf("run should be complete after the rebuilt timer fired, IsComplete = %v, %v", ok, err)
	}
}

// TestRetriableOnResume: only ReadOnly and Idempotent make a call retry-safe.
func TestRetriableOnResume(t *testing.T) {
	cases := []struct {
		name string
		s    Safety
		want bool
	}{
		{"read-only", Safety{ReadOnly: true}, true},
		{"idempotent", Safety{Idempotent: true}, true},
		{"bare", Safety{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.retriableOnResume(); got != tc.want {
				t.Fatalf("retriableOnResume() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A saga that aborted and finished its rollback is over. Recover must not re-drive it and report
// it as a failure on every pass, which buries real recovery failures in noise.
func TestRecover_SkipsAFinishedSagaAbort(t *testing.T) {
	store := memJournal()
	fail := MustFunc("book", "book it", func(context.Context, struct{}) (string, error) { return "", errors.New("no seats") })
	a := mustNew(NewScriptedModel(ToolTurn("b1", "book", `{}`), TextTurn("done")), store, WithTools(fail))
	var aborted *SagaAborted
	if _, err := a.Run(context.Background(), "r1", UserText("go"), WithSaga()); !errors.As(err, &aborted) {
		t.Fatalf("setup: %v", err)
	}
	var redriven int
	for pass := range 2 {
		n, err := Recover(context.Background(), store, func(ctx context.Context, runID string, _ RunStart) error {
			redriven++
			_, err := a.Run(ctx, runID, UserText("go"), WithSaga())
			return err
		})
		if n != 0 || err != nil {
			t.Fatalf("pass %d: Recover re-drove %d runs, err = %v; want the finished abort left alone", pass, n, err)
		}
	}
	if redriven != 0 {
		t.Fatalf("the finished abort was re-driven %d times", redriven)
	}
}

// recorder is a Resumer that records the runs it was handed.
type recorder struct {
	mu   sync.Mutex
	runs []string
}

func (r *recorder) resume(_ context.Context, runID string, _ RunStart) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, runID)
	return nil
}

// Recover refuses a nil Resumer and a store that cannot list runs, before touching any run.
func TestRecover_Refusals(t *testing.T) {
	ctx := context.Background()
	if _, err := Recover(ctx, memJournal(), nil); !errors.Is(err, ErrConfig) {
		t.Errorf("nil Resumer = %v; want ErrConfig", err)
	}
	rec := &recorder{}
	if _, err := Recover(ctx, mustJournal(plainStore{NewMemStore()}), rec.resume); !errors.Is(err, ErrConfig) {
		t.Errorf("a store with no Lister = %v; want ErrConfig", err)
	}
	if _, err := Recover(ctx, memJournal(), rec.resume, WithNotifier(nil, NewMemNotifyTrail())); !errors.Is(err, ErrConfig) {
		t.Errorf("a nil notifier sink = %v; want ErrConfig", err)
	}
	if len(rec.runs) != 0 {
		t.Errorf("refused passes resumed %v", rec.runs)
	}
}

// A run whose recorded start cannot be read or decoded is reported (ErrStorage) and not resumed,
// and it does not stop the pass: the healthy runs around it are still recovered.
func TestRecover_ContinuesPastAnUnreadableStart(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	seedRun(t, s, "a-ok")
	seedRun(t, s, "z-ok")
	j := mustJournal(s)
	writeRaw(t, j, "m-bad", runStartStep, Record{Kind: StepValue, Result: []byte(`[1]`)})
	writeRaw(t, j, "m-bad", "seed", Record{Kind: StepValue})
	rec := &recorder{}
	n, err := Recover(ctx, j, rec.resume)
	if !errors.Is(err, ErrStorage) {
		t.Fatalf("Recover = %v; want the bad start reported as ErrStorage", err)
	}
	if n != 2 || len(rec.runs) != 2 || rec.runs[0] != "a-ok" || rec.runs[1] != "z-ok" {
		t.Fatalf("recovered %d runs %v; want a-ok and z-ok only", n, rec.runs)
	}
}

// A store that cannot read run:start at all is a storage error for each run, and no run is resumed
// blind.
func TestRecover_UnreadableStartIsStorageError(t *testing.T) {
	s := NewMemStore()
	seedRun(t, s, "a")
	rec := &recorder{}
	_, err := Recover(context.Background(), mustJournal(covGetFailLister{covGetFail{s, runStartStep}, s}), rec.resume)
	if !errors.Is(err, ErrStorage) || len(rec.runs) != 0 {
		t.Fatalf("Recover = %v, resumed %v; want ErrStorage and none", err, rec.runs)
	}
}

// covGetFailLister keeps the Lister of the store covGetFail wraps.
type covGetFailLister struct {
	covGetFail
	l *MemStore
}

func (c covGetFailLister) Unwrap() Store { return c.l }

// A listing that fails partway is reported (ErrStorage) with the runs recovered before it counted.
func TestRecover_ListingFailureKeepsWhatWasRecovered(t *testing.T) {
	s := covRunFault{MemStore: NewMemStore(), runsErr: true}
	seedRun(t, s.MemStore, "a")
	rec := &recorder{}
	n, err := Recover(context.Background(), mustJournal(s), rec.resume)
	if !errors.Is(err, ErrStorage) || n != 1 || len(rec.runs) != 1 {
		t.Fatalf("Recover = %d, %v (resumed %v); want 1 recovered and ErrStorage", n, err, rec.runs)
	}
}

// A run in a journal format this version cannot read is reported as a *JournalVersionError (not as
// an unstarted run, which a later pass would skip silently), and not resumed.
func TestRecover_ForeignFormatIsReportedAsSuch(t *testing.T) {
	s := NewMemStore()
	seedHeader(t, s, "old", `{"name":"`+headerStep+`","kind":"`+string(StepHeader)+`","format":"bide-journal-999"}`)
	rec := &recorder{}
	_, err := Recover(context.Background(), mustJournal(s), rec.resume)
	var ve *JournalVersionError
	if !errors.As(err, &ve) || ve.RunID != "old" || len(rec.runs) != 0 {
		t.Fatalf("Recover = %v, resumed %v; want a *JournalVersionError for old and none resumed", err, rec.runs)
	}
}

// skipValue returns where a JSON value ends: at the separator or closer after it, len(b) when the
// value ends the input, and -1 for a value that does not close.
func TestSkipValue(t *testing.T) {
	for _, c := range []struct {
		in   string
		from int
		want int
	}{
		{`1,2`, 0, 1},
		{`1}`, 0, 1},
		{`12`, 0, 2},
		{`"a,b",1`, 0, 5},
		{`"a\"b",1`, 0, 6},
		{`"a\\",1`, 0, 5},
		{`{"a":[1,2]},3`, 0, 11},
		{`[[1],2]]`, 0, 7},
		{`"unterminated`, 0, -1},
		{`"ends with escape\`, 0, -1},
		{`{"a":1`, 0, -1},
		{`[1,[2]`, 0, -1},
	} {
		if got := skipValue([]byte(c.in), c.from); got != c.want {
			t.Errorf("skipValue(%q, %d) = %d; want %d", c.in, c.from, got, c.want)
		}
	}
}

func TestSkipSpace(t *testing.T) {
	b := []byte(" \t\r\n x ")
	if got := skipSpace(b, 0); got != 5 {
		t.Errorf("skipSpace = %d; want 5", got)
	}
	if got := skipSpace(b, 6); got != 7 {
		t.Errorf("skipSpace at the trailing space = %d; want 7 (end)", got)
	}
	if got := skipSpace(b, 7); got != 7 {
		t.Errorf("skipSpace at the end = %d; want 7", got)
	}
}

// startMembersOnly admits an object of exactly the four members, once each, spelled exactly, and
// nothing else: the caller falls back to the full decoding for the rest.
func TestStartMembersOnly(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{`{"name":"a","kind":"b","result":{"x":[1,2]},"salt":"s"}`, true},
		{` { "salt" : "s" , "result" : 1 , "kind" : "b" , "name" : "a" } `, true},
		{`{"name":"a","kind":"b","result":1,"salt":"s","extra":1}`, false},
		{`{"name":"a","name":"a","kind":"b","result":1,"salt":"s"}`, false},
		{`{"Name":"a","kind":"b","result":1,"salt":"s"}`, false},
		{`{"na\u006de":"a","kind":"b","result":1,"salt":"s"}`, false},
		{`[1]`, false},
		{``, false},
		{`{`, false},
		{`{"name"`, false},
		{`{"name":"a",}`, false},
		{`{"name":"unterminated`, false},
	} {
		if got := startMembersOnly([]byte(c.in)); got != c.want {
			t.Errorf("startMembersOnly(%q) = %v; want %v", c.in, got, c.want)
		}
	}
}

// decodePlainStart reads only the exact record a plain start is written as: a truncated string,
// an escape, a bad salt or any other framing is not read.
func TestDecodePlainStart_RefusesWhatIsNotExact(t *testing.T) {
	salt := base64.StdEncoding.EncodeToString(make([]byte, SaltSize))
	if len(salt) != saltLen {
		t.Fatalf("test salt has %d bytes; the journal writes %d", len(salt), saltLen)
	}
	good := plainStartPrefix + `hi` + plainStartKind + salt + `"}`
	if st, ok := decodePlainStart([]byte(good)); !ok || st.Input.Text() != "hi" || st.Kind != RunKindAgent {
		t.Fatalf("decodePlainStart(good) = %+v, %v", st, ok)
	}
	withAt := plainStartPrefix + `hi` + plainStartAt + `2026-10-13T00:00:00.000Z` + plainStartLegacy + salt + `"}`
	if st, ok := decodePlainStart([]byte(withAt)); !ok || st.StartedAt != "2026-10-13T00:00:00.000Z" {
		t.Fatalf("decodePlainStart(with start time) = %+v, %v", st, ok)
	}
	legacy := plainStartPrefix + `hi` + plainStartLegacy + salt + `"}`
	if st, ok := decodePlainStart([]byte(legacy)); !ok || st.Kind != "" {
		t.Fatalf("decodePlainStart(legacy) = %+v, %v", st, ok)
	}
	for name, b := range map[string]string{
		"no input end":       plainStartPrefix + `hi`,
		"escaped input":      plainStartPrefix + `h\ni` + plainStartKind + salt + `"}`,
		"control in input":   plainStartPrefix + "h\ni" + plainStartKind + salt + `"}`,
		"unterminated time":  plainStartPrefix + `hi` + plainStartAt + `2026`,
		"escaped time":       plainStartPrefix + `hi` + plainStartAt + `20\n26` + plainStartLegacy + salt + `"}`,
		"unknown framing":    plainStartPrefix + `hi","extra":1},"salt":"` + salt + `"}`,
		"short salt":         plainStartPrefix + `hi` + plainStartKind + `AAAA"}`,
		"salt not base64":    plainStartPrefix + `hi` + plainStartKind + "!" + salt[1:] + `"}`,
		"trailing garbage":   good + ` `,
		"not the run:start":  `{"name":"other"}`,
		"time then no salt":  plainStartPrefix + `hi` + plainStartAt + `t` + `"}`,
		"legacy bad closing": plainStartPrefix + `hi` + plainStartLegacy + salt,
	} {
		if st, ok := decodePlainStart([]byte(b)); ok {
			t.Errorf("%s: decodePlainStart accepted %q as %+v", name, b, st)
		}
	}
}

// A reconciler that resolves a halt before the provider's record has settled would re-fire
// the effect. WithMinHaltAge refuses to resolve a halt younger than the grace period,
// measured from the attempt marker, so early resolution cannot happen even by mistake.
func TestResolveHalt_MinHaltAge(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seed := func() *MemStore {
		s := NewMemStore()
		j := mustJournal(s)
		// Attempt marker stamped at base, no result: an unknown-outcome halt.
		_, _ = j.do(ctx, "r", toolAttemptStep("c1"), func(context.Context) (Record, error) {
			return Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: base.UnixMilli()}, nil
		})
		return s
	}

	// Too soon: 30s after the attempt, 60s grace -> *HaltTooYoung, nothing recorded.
	s := seed()
	j := mustJournal(s)
	err := ResolveHalt(ctx, j, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "charged", IsError: false}, WithMinHaltAge(60*time.Second), WithClock(func() time.Time { return base.Add(30 * time.Second) }))
	var young *HaltTooYoung
	if !errors.As(err, &young) {
		t.Fatalf("err = %v, want *HaltTooYoung", err)
	}
	if h, _ := j.History(ctx, "r"); hasResult(h, "c1") {
		t.Fatal("a too-young halt must not record a result")
	}

	// Past the grace: 90s after the attempt -> resolves and records the result.
	s = seed()
	if err := ResolveHalt(ctx, j, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "charged", IsError: false}, WithMinHaltAge(60*time.Second), WithClock(func() time.Time { return base.Add(90 * time.Second) })); err != nil {
		t.Fatalf("resolve after grace: %v", err)
	}
	if h, _ := j.History(ctx, "r"); !hasResult(h, "c1") {
		t.Fatal("resolve after grace should record the result")
	}

	// No attempt timestamp to measure against: fail closed rather than resolve blind.
	bare := memJournal()
	if err := ResolveHalt(ctx, bare, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "x", IsError: false}, WithMinHaltAge(time.Second)); err == nil {
		t.Fatal("min-halt-age with no attempt marker should error, not resolve")
	}
}

// A reconciled resolution is journaled distinctly (Reconciled + Evidence, signed with the
// record), so a later reader can tell a reconciled outcome from a clean one and re-check
// what the reconciler read. A plain resolve carries no such annotation.
func TestResolveHalt_Evidence(t *testing.T) {
	ctx := context.Background()
	s := memJournal()
	for _, id := range []string{"c1", "c2"} { // both calls halted: their markers are recorded
		if _, err := s.do(ctx, "r", toolAttemptStep(id), func(context.Context) (Record, error) {
			return Record{Kind: StepAttempt, ToolUseID: id, AttemptedAt: 1}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	ev := map[string]string{"message_id": "msg_123", "source": "provider log"}
	if err := ResolveHalt(ctx, s, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}, Outcome{Result: "sent", IsError: false}, WithEvidence(ev)); err != nil {
		t.Fatalf("reconciled resolve: %v", err)
	}
	rec := resultFor(t, s, "r", "c1")
	if !rec.Reconciled {
		t.Fatal("WithEvidence should mark the result Reconciled")
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Evidence, &got); err != nil || got["message_id"] != "msg_123" {
		t.Fatalf("evidence = %s (err %v), want the reconciler's record", rec.Evidence, err)
	}

	// A plain resolve is a clean outcome: no reconciliation annotation.
	if err := ResolveHalt(ctx, s, HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c2"}, Cause: HaltCrashed}, Outcome{Result: "sent", IsError: false}); err != nil {
		t.Fatalf("plain resolve: %v", err)
	}
	rec2 := resultFor(t, s, "r", "c2")
	if rec2.Reconciled || rec2.Evidence != nil {
		t.Fatalf("plain resolve must not be reconciled (got Reconciled=%v Evidence=%s)", rec2.Reconciled, rec2.Evidence)
	}
}

// The halt surfaces when the effect was attempted, so a reconciler can honor a grace period.
func TestOutcomeUnknown_AttemptedAt(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	at := time.Date(2026, 2, 2, 3, 4, 5, 0, time.UTC)
	asst := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{}`)}}}
	_, _ = store.do(ctx, "r", "@llm/0", func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &asst}, nil
	})
	_, _ = store.do(ctx, "r", toolAttemptStep("c1"), func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: at.UnixMilli()}, nil
	})

	var calls int
	write := &countingTool{name: "charge", safety: Safety{}, calls: &calls} // not retry-safe
	_, err := mustNew(&scriptModel{}, store, WithTools(write)).Run(ctx, "r", UserText("hi"))

	var halt *OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("err = %v, want *OutcomeUnknown", err)
	}
	if !halt.AttemptedAt.Equal(at) {
		t.Fatalf("halt.AttemptedAt = %v, want %v", halt.AttemptedAt, at)
	}
}

func hasResult(recs []Record, id string) bool {
	for _, r := range recs {
		if r.Kind == StepToolResult && r.ToolUseID == id {
			return true
		}
	}
	return false
}

func resultFor(t *testing.T, s *Journal, runID, id string) Record {
	t.Helper()
	recs, err := s.History(context.Background(), runID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	for _, r := range recs {
		if r.Kind == StepToolResult && r.ToolUseID == id {
			return r
		}
	}
	t.Fatalf("no tool result for %s", id)
	return Record{}
}

// Finding L1 of model 10 (spec/tla/README.md, "Recovery cost: the pickup bound"): a RecoverLoop
// pass visits every unfinished run it lists, halted ones included, one slot at a time, so a dead
// holder's run listed after halted runs waited for their visits, and a lease that lapsed just
// after the pass tried the run waited for the whole next pass. A dead holder's run must be taken
// over within one interval of its lease lapsing, however many halted runs are listed before it.
//
// The halted runs h1..hH sort before the dead holder's run z, as older runs do; each visit to a
// halted run takes one interval (its resume replays to the halt and pauses again), and the full
// pass drives one run at a time. The dead holder's lease lapses at every phase of the pass, in
// steps of a quarter interval, and the takeover is measured from the lapse to the moment resume
// is called for z, on the bubble's clock.
func TestRecoverLoop_TakesOverALapsedLeaseWithinAnIntervalBehindHaltedRuns(t *testing.T) {
	const (
		interval = 20 * time.Millisecond
		visit    = interval // one halted run's visit
		step     = interval / 4
	)
	for halted := 0; halted <= 3; halted++ {
		for phase := time.Duration(0); phase <= time.Duration(halted+2)*visit; phase += step {
			t.Run(fmt.Sprintf("halted=%d/lapse=+%v", halted, phase), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					if d := lapsedTakeover(t, halted, visit, interval, 100*time.Millisecond+phase); d > interval {
						t.Fatalf("the dead holder's run was taken over %v after its lease lapsed, behind %d halted runs; want within one interval (%v)", d, halted, interval)
					}
				})
			})
		}
	}
}

// lapsedTakeover runs RecoverLoop at concurrency 1 over halted runs h1..hN and a run z whose dead
// holder's lease lapses deadTTL after the loop starts, and returns how long after the lapse the
// loop called resume for z.
func lapsedTakeover(t *testing.T, halted int, visit, interval, deadTTL time.Duration) time.Duration {
	t.Helper()
	ctx := context.Background()
	s := NewMemStore()
	j := mustJournal(s)
	for i := 1; i <= halted; i++ {
		seedRun(t, s, fmt.Sprintf("h%d", i))
	}
	seedRun(t, s, "z")
	if ok, _ := s.AcquireLease(ctx, "z", "dead-worker#0", deadTTL); !ok {
		t.Fatal("setup: the dead worker should hold z")
	}
	lapse := time.Now().Add(deadTTL)
	drivenAt := make(chan time.Time, 1)
	stop := runLoop(t, j, func(ctx context.Context, id string, _ RunStart) error {
		if id != "z" {
			time.Sleep(visit)
			return &OutcomeUnknown{RunRef: RunRef{RunID: id}} // halted: stays unfinished
		}
		select {
		case drivenAt <- time.Now():
		default:
		}
		_, err := j.do(ctx, id, runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil })
		return err
	}, WithRecoverInterval(interval), WithRecoverConcurrency(1), WithLeaseTTL(10*interval))
	defer stop()
	select {
	case at := <-drivenAt:
		if at.Before(lapse) {
			t.Fatalf("the loop drove z %v before its dead holder's lease lapsed", lapse.Sub(at))
		}
		return at.Sub(lapse)
	case <-time.After(time.Minute):
		t.Fatal("the loop never took over the dead holder's run")
		return 0
	}
}

// A run the full pass is waiting to start (every slot of the full pass busy) whose lease lapses
// meanwhile is driven by the lapsed loop, and when the full pass gets its slot it sees the run in
// flight and does not try it again: one lease acquisition for the run, and the drive's entry stays
// marked in flight until the drive returns.
func TestRecoverLoop_LoopsDoNotStartARunTheOtherIsDriving(t *testing.T) {
	synctest.Test(t, testRecoverLoopLoopsDoNotStartARunTheOtherIsDriving)
}

func testRecoverLoopLoopsDoNotStartARunTheOtherIsDriving(t *testing.T) {
	ctx := context.Background()
	s := &countingStore{MemStore: NewMemStore()}
	j := mustJournal(s)
	seedRun(t, s.MemStore, "h")
	seedRun(t, s.MemStore, "z")
	if ok, _ := s.MemStore.AcquireLease(ctx, "z", "dead-worker#0", 30*time.Millisecond); !ok {
		t.Fatal("setup: the dead worker should hold z")
	}
	release := make(chan struct{})
	zStarted := make(chan struct{})
	stop := runLoop(t, j, func(ctx context.Context, id string, _ RunStart) error {
		if id == "h" {
			time.Sleep(50 * time.Millisecond) // holds the full pass's only slot past z's lapse
			return &OutcomeUnknown{RunRef: RunRef{RunID: id}}
		}
		close(zStarted)
		<-release
		return nil
	}, WithRecoverInterval(20*time.Millisecond), WithRecoverConcurrency(1), WithLeaseTTL(time.Second))
	defer stop()
	<-zStarted                        // the lapsed loop's pass at 40ms
	time.Sleep(30 * time.Millisecond) // the full pass gets its slot back at 50ms and reaches z
	if n := s.acquires("z"); n != 1 {
		t.Fatalf("z was leased %d times while the lapsed loop drove it, want once", n)
	}
	time.Sleep(100 * time.Millisecond) // later passes of both loops
	if n := s.acquires("z"); n != 1 {
		t.Fatalf("z was leased %d times while the lapsed loop drove it, want once", n)
	}
	close(release)
}

// A run whose lease lapsed while this process still drives it (a drive that ignores the
// cancellation its lost lease caused) is not started again by the lapsed loop.
func TestRecoverLoop_LapsedLoopSkipsARunItsProcessIsDriving(t *testing.T) {
	synctest.Test(t, testRecoverLoopLapsedLoopSkipsARunItsProcessIsDriving)
}

func testRecoverLoopLapsedLoopSkipsARunItsProcessIsDriving(t *testing.T) {
	s := &failRenewStore{countingStore{MemStore: NewMemStore()}}
	j := mustJournal(s)
	seedRun(t, s.MemStore, "r")
	release := make(chan struct{})
	started := make(chan struct{})
	stop := runLoop(t, j, func(ctx context.Context, id string, _ RunStart) error {
		close(started)
		<-release // ignores ctx: still driving after the lease lapsed
		return nil
	}, WithRecoverInterval(20*time.Millisecond), WithLeaseTTL(100*time.Millisecond))
	defer stop()
	<-started
	time.Sleep(300 * time.Millisecond) // the lease lapsed at 100ms; lapsed passes every 20ms since
	if !s.MemStore.lapsedNow("r") {
		t.Fatal("setup: r's lease should have lapsed")
	}
	if n := s.acquires("r"); n != 1 {
		t.Fatalf("r was leased %d times while this process drove it, want once", n)
	}
	close(release)
}

// failRenewStore is a countingStore whose renewals fail, so a drive's lease lapses at its TTL.
type failRenewStore struct{ countingStore }

func (*failRenewStore) RenewLease(context.Context, string, string, time.Duration) (bool, error) {
	return false, errors.New("renewal refused")
}

// lapsedNow reports whether runID's lease has lapsed on m's clock.
func (m *MemStore) lapsedNow(runID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	return m.lapsed(runID, m.now())
}

// Over a store that implements Lister but not Leaser there are no leases to lapse, and RecoverLoop
// runs no lapsed loop: every listing is the full pass's.
func TestRecoverLoop_NoLapsedLoopWithoutLeaser(t *testing.T) {
	synctest.Test(t, testRecoverLoopNoLapsedLoopWithoutLeaser)
}

func testRecoverLoopNoLapsedLoopWithoutLeaser(t *testing.T) {
	s := &unleasedStore{m: NewMemStore()}
	stop := runLoop(t, mustJournal(s), func(context.Context, string, RunStart) error { return nil }, WithRecoverInterval(10*time.Millisecond))
	time.Sleep(55 * time.Millisecond)
	_ = stop()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full != 6 || s.lapsed != 0 {
		t.Fatalf("%d full and %d lapsed listings in 55ms, want 6 and 0", s.full, s.lapsed)
	}
}

// unleasedStore is a Store that implements Lister and not Leaser, counting its listings.
type unleasedStore struct {
	m            *MemStore
	mu           sync.Mutex
	full, lapsed int
}

func (u *unleasedStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	return u.m.Insert(ctx, runID, name, data)
}

func (u *unleasedStore) Get(ctx context.Context, runID, name string) (Entry, bool, error) {
	return u.m.Get(ctx, runID, name)
}

func (u *unleasedStore) Load(ctx context.Context, runID string, after int64) iter.Seq2[Entry, error] {
	return u.m.Load(ctx, runID, after)
}

func (u *unleasedStore) Runs(ctx context.Context, f RunFilter) iter.Seq2[string, error] {
	u.mu.Lock()
	if f.LeaseLapsed {
		u.lapsed++
	} else {
		u.full++
	}
	u.mu.Unlock()
	return u.m.Runs(ctx, f)
}

// The lapsed loop drives only the unfinished runs whose lease lapsed: while the full pass's only
// slot is taken, it leaves alone a run nobody leases, a run whose lease is live, and a finished run
// whose holder died before releasing its lease.
func TestRecoverLoop_LapsedLoopDrivesOnlyLapsedUnfinishedRuns(t *testing.T) {
	synctest.Test(t, testRecoverLoopLapsedLoopDrivesOnlyLapsedUnfinishedRuns)
}

func testRecoverLoopLapsedLoopDrivesOnlyLapsedUnfinishedRuns(t *testing.T) {
	ctx := context.Background()
	// Reaping is off, so the finished run's lapsed lease stays and the listing must exclude it.
	s := &noReapStore{countingStore{MemStore: NewMemStore()}}
	j := mustJournal(s)
	for _, id := range []string{"a", "done", "live", "none"} {
		seedRun(t, s.MemStore, id)
	}
	if _, err := j.do(ctx, "done", runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil }); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.MemStore.AcquireLease(ctx, "done", "dead-worker#0", time.Millisecond); !ok {
		t.Fatal("setup: the dead worker should hold done")
	}
	if ok, _ := s.MemStore.AcquireLease(ctx, "live", "other-worker#0", time.Hour); !ok {
		t.Fatal("setup: the other worker should hold live")
	}
	release := make(chan struct{})
	stop := runLoop(t, j, func(ctx context.Context, id string, _ RunStart) error {
		if id == "a" {
			<-release // holds the full pass's only slot
		}
		return nil
	}, WithRecoverInterval(20*time.Millisecond), WithRecoverConcurrency(1))
	defer stop()
	time.Sleep(200 * time.Millisecond) // ten passes of the lapsed loop
	for _, id := range []string{"done", "live", "none"} {
		if n := s.acquires(id); n != 0 {
			t.Errorf("run %s was leased %d times while the full pass was busy, want none: the lapsed loop drove it", id, n)
		}
	}
	close(release)
}

// MemStore's lapsed listing agrees with AcquireLease at the expiry instant: a lease is lapsed from
// the moment another holder can take it.
func TestMemStore_LeaseLapsedAtExpiry(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	seedRun(t, s, "r")
	t0 := time.Unix(1000, 0)
	s.setNow(func() time.Time { return t0 })
	if ok, _ := s.AcquireLease(ctx, "r", "dead#0", time.Second); !ok {
		t.Fatal("setup: dead#0 should hold r")
	}
	list := func() []string {
		var out []string
		for id, err := range s.Runs(ctx, RunFilter{LeaseLapsed: true}) {
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}
	s.setNow(func() time.Time { return t0.Add(time.Second - 1) })
	if got := list(); len(got) != 0 {
		t.Fatalf("a nanosecond before expiry, Runs(LeaseLapsed) = %v, want none", got)
	}
	s.setNow(func() time.Time { return t0.Add(time.Second) })
	if got := list(); len(got) != 1 {
		t.Fatalf("at expiry, Runs(LeaseLapsed) = %v, want [r]", got)
	}
	if ok, _ := s.AcquireLease(ctx, "r", "new#0", time.Second); !ok {
		t.Fatal("at expiry AcquireLease refused a lease Runs(LeaseLapsed) listed as lapsed")
	}
}

// A pass skips a run this process is already driving without waiting for a slot: with the only
// slot held by that run's long drive, the full pass still ends at once and the next one starts on
// schedule.
func TestRecoverLoop_PassDoesNotWaitForASlotForARunInFlight(t *testing.T) {
	synctest.Test(t, testRecoverLoopPassDoesNotWaitForASlotForARunInFlight)
}

func testRecoverLoopPassDoesNotWaitForASlotForARunInFlight(t *testing.T) {
	s := &countingStore{MemStore: NewMemStore()}
	j := mustJournal(s)
	seedRun(t, s.MemStore, "a")
	release := make(chan struct{})
	stop := runLoop(t, j, func(ctx context.Context, id string, _ RunStart) error {
		<-release
		return nil
	}, WithRecoverInterval(20*time.Millisecond), WithRecoverConcurrency(1))
	defer stop()
	time.Sleep(105 * time.Millisecond)
	s.mu.Lock()
	lists := s.lists
	s.mu.Unlock()
	if lists != 6 {
		t.Fatalf("%d full passes in 105ms with a 20ms interval, want 6: a pass waited for a slot for the run already in flight", lists)
	}
	close(release)
}

// A failed ReapLeases is a genuine failure: it goes to the WithRecoverErrors handler as a storage
// error, and the lapsed loop still lists and drives the lapsed runs.
func TestRecoverLoop_ReportsAFailedReap(t *testing.T) {
	synctest.Test(t, testRecoverLoopReportsAFailedReap)
}

func testRecoverLoopReportsAFailedReap(t *testing.T) {
	s := &failReapStore{countingStore{MemStore: NewMemStore()}}
	j := mustJournal(s)
	seedRun(t, s.MemStore, "z")
	if ok, _ := s.MemStore.AcquireLease(context.Background(), "z", "dead-worker#0", time.Millisecond); !ok {
		t.Fatal("setup: the dead worker should hold z")
	}
	var mu sync.Mutex
	var errs []error
	driven := make(chan struct{})
	var once sync.Once
	stop := runLoop(t, j, func(context.Context, string, RunStart) error {
		once.Do(func() { close(driven) })
		return &OutcomeUnknown{RunRef: RunRef{RunID: "z"}}
	}, WithRecoverInterval(20*time.Millisecond), WithRecoverErrors(func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}))
	defer stop()
	<-driven
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(errs) == 0 || !errors.Is(errs[0], ErrStorage) || !errors.Is(errs[0], errReap) {
		t.Fatalf("reported %v, want the reap failure wrapped as ErrStorage", errs)
	}
}

var errReap = errors.New("reap refused")

// failReapStore is a countingStore whose ReapLeases fails.
type failReapStore struct{ countingStore }

func (*failReapStore) ReapLeases(context.Context, []string) (int, error) { return 0, errReap }

// noReapStore is a countingStore whose ReapLeases deletes nothing.
type noReapStore struct{ countingStore }

func (*noReapStore) ReapLeases(context.Context, []string) (int, error) { return 0, nil }

// Review of #126: a holder that writes run:complete and dies before its deferred ReleaseLease
// leaves a lapsed lease row on a finished run. Neither loop ever acquires (and so never releases)
// it: the lapsed listing excludes the finished run, and so does the full pass. The row stays for
// good, and every lapsed pass on every worker reads it again, so the lapsed loop's per-interval
// cost grows with every such crash.
func TestRV126_LapsedRowOnFinishedRunIsNeverReaped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s := NewMemStore()
		j := mustJournal(s)
		seedRun(t, s, "f")
		if _, err := j.do(ctx, "f", runCompleteStep, func(context.Context) (Record, error) { return Record{Kind: StepValue}, nil }); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.AcquireLease(ctx, "f", "dead-worker#0", time.Millisecond); !ok {
			t.Fatal("setup: the dead worker should hold f")
		}
		stop := runLoop(t, j, func(context.Context, string, RunStart) error { return nil }, WithRecoverInterval(20*time.Millisecond))
		time.Sleep(2 * time.Second) // a hundred passes of each loop
		synctest.Wait()
		_ = stop()
		s.mu.Lock()
		_, stale := s.leases["f"]
		s.mu.Unlock()
		if stale {
			t.Fatal("the lapsed lease row of finished run f is still in the store after 100 lapsed passes: nothing reaps it, so each one adds to every later lapsed pass's cost")
		}
	})
}
