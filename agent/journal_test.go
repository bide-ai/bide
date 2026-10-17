package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/internal/journalhook"
)

// stepPaths are the paths the engine drives a step through: a Journal's own.
func stepPaths() map[string]func() *Journal {
	return map[string]func() *Journal{
		"journal": func() *Journal { return memJournal() },
	}
}

// A Step that is not retry-safe and pauses from its body is ErrConfig, not a pause: the body may
// have done something before it paused, so its marker stays and the next attempt halts rather
// than run it again. The error does not wrap the pause, so no caller mistakes it for one.
func TestStep_PauseInASideEffectStepIsErrConfig(t *testing.T) {
	ctx := context.Background()
	for name, open := range stepPaths() {
		t.Run(name, func(t *testing.T) {
			d := open()
			ran := 0
			body := func(context.Context) (int, error) {
				ran++
				return 0, &InterruptPending{RunRef: RunRef{RunID: "r"}, Name: "confirm"}
			}
			_, err := d.Step(ctx, "r", "charge", body)
			var intr *InterruptPending
			if !errors.Is(err, ErrConfig) || errors.As(err, &intr) || IsPause(err) {
				t.Fatalf("Step = %v; want ErrConfig that is not a pause", err)
			}
			if !strings.Contains(err.Error(), "retry-safe") {
				t.Errorf("the error does not tell the developer what to do: %v", err)
			}
			_, err = d.Step(ctx, "r", "charge", body)
			var halt *OutcomeUnknown
			if !errors.As(err, &halt) || halt.Op != (OpRef{Kind: OpStep, ID: "charge"}) {
				t.Fatalf("the next attempt = %v; want *OutcomeUnknown on the step", err)
			}
			if ran != 1 {
				t.Fatalf("the body ran %d times, want once", ran)
			}
		})
	}
}

// A retry-safe Step may pause: the pause propagates as itself and the step runs again on resume.
func TestStep_PauseInARetrySafeStepPropagates(t *testing.T) {
	ctx := context.Background()
	for name, open := range stepPaths() {
		t.Run(name, func(t *testing.T) {
			d := open()
			answered := false
			body := func(context.Context) (string, error) {
				if !answered {
					return "", &InterruptPending{RunRef: RunRef{RunID: "r"}, Name: "confirm"}
				}
				return "ok", nil
			}
			safe := WithSafety(Safety{Idempotent: true})
			_, err := d.Step(ctx, "r", "ask", body, safe)
			var intr *InterruptPending
			if !errors.As(err, &intr) || errors.Is(err, ErrConfig) {
				t.Fatalf("Step = %v; want the *InterruptPending itself", err)
			}
			answered = true
			if v, err := d.Step(ctx, "r", "ask", body, safe); err != nil || v != "ok" {
				t.Fatalf("resumed Step = %q, %v", v, err)
			}
		})
	}
}

// Record.MarshalJSON writes the journal encoding: no HTML escaping, the claim and salt last, and
// the stored bytes (Raw) never. The bytes are pinned, since audit leaves commit to them.
func TestRecord_MarshalJSONGolden(t *testing.T) {
	r := Record{Name: "tool:c1", Kind: StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"h":"<b>a & b</b>"}`),
		IsError: true, AttemptedAt: 7, claim: "c1aim", salt: []byte{1, 2, 3}, raw: []byte("ignored")}
	const want = `{"name":"tool:c1","kind":"tool_result","tool_use_id":"c1","result":{"h":"<b>a & b</b>"},"is_error":true,"attempted_at":7,"claim":"c1aim","salt":"AQID"}`
	b, err := r.MarshalJSON()
	if err != nil || string(b) != want {
		t.Fatalf("MarshalJSON = %s, %v\nwant          %s", b, err, want)
	}
	if enc, err := EncodeRecord(r); err != nil || string(enc) != want {
		t.Fatalf("EncodeRecord = %s, %v; want the same bytes", enc, err)
	}
	if enc, err := marshalJournal([]Record{r}); err != nil || string(enc) != "["+want+"]" {
		t.Fatalf("a record inside another value encodes as %s, %v", enc, err)
	}
	var back Record
	if err := json.Unmarshal(b, &back); err != nil || back.ClaimID() != "c1aim" || string(back.Salt()) != "\x01\x02\x03" || back.Raw() != nil {
		t.Fatalf("UnmarshalJSON = %+v, %v; want the claim and salt back, and no stored bytes", back, err)
	}
	h, err := JournalEntry(headerStep, Record{Kind: StepHeader, Format: JournalFormat})
	if err != nil || !strings.HasPrefix(string(h), `{"name":"@journal","kind":"header","format":"`+JournalFormat+`","salt":"`) {
		t.Fatalf("the journal header encodes as %s, %v", h, err)
	}
}

// Salt and ClaimID hand out copies: a caller cannot change the record through them.
func TestRecord_SaltIsACopy(t *testing.T) {
	r := Record{salt: []byte{1, 2}, raw: []byte("x")}
	r.Salt()[0] = 9
	r.Raw()[0] = 'y'
	if r.salt[0] != 1 || r.raw[0] != 'x' {
		t.Fatal("modifying Salt's or Raw's result changed the record")
	}
}

func TestNewJournal_NilStoreIsErrConfig(t *testing.T) {
	var m *MemStore
	for _, s := range []Store{nil, m} {
		if _, err := NewJournal(s); !errors.Is(err, ErrConfig) {
			t.Errorf("NewJournal(%v) = %v, want ErrConfig", s, err)
		}
	}
	if _, err := NewJournal(NewMemStore(), nil); !errors.Is(err, ErrConfig) {
		t.Errorf("NewJournal with a nil option = %v, want ErrConfig", err)
	}
}

// Until 1.0 every pre-release writes one dev tag, never bumped per change, and 1.0 switches to
// "bide.journal.v1", so a 1.0 binary refuses every pre-release journal.
func TestJournalFormat_IsTheOneDevTag(t *testing.T) {
	if JournalFormat != "bide.journal.v1-dev" || !slices.Equal(supportedFormats, []string{"bide.journal.v1-dev"}) {
		t.Fatalf("JournalFormat = %q, supported %v; want the one pre-1.0 tag bide.journal.v1-dev", JournalFormat, supportedFormats)
	}
}

func TestJournalVersionError(t *testing.T) {
	e := &JournalVersionError{RunID: "r", Found: "bide.journal.v999", Supported: []string{JournalFormat}}
	if !errors.Is(e, ErrJournalVersion) || !errors.Is(e, ErrProtocol) || errors.Is(e, ErrStorage) {
		t.Fatal("JournalVersionError does not wrap exactly ErrJournalVersion's category, ErrProtocol")
	}
	if s := e.Error(); !strings.Contains(s, `"bide.journal.v999"`) || !strings.Contains(s, JournalFormat) || !strings.Contains(s, "run r") {
		t.Errorf("Error() = %q", s)
	}
	if s := (&JournalVersionError{RunID: "r", Supported: supportedFormats}).Error(); !strings.Contains(s, "no header") {
		t.Errorf("an unversioned journal's error = %q, want it to say the header is missing", s)
	}
}

// A redaction tombstone reads as a record that carries only its name and its stored bytes, and a
// run holding one cannot be driven again.
func TestJournal_RedactedRecord(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	j := mustJournal(m)
	if _, err := j.do(ctx, "r", "secret", func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: json.RawMessage(`"pii"`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	tomb := []byte(`{"redacted":{"leaf_hash":"abababababababababababababababababababababababababababababababab","at_ms":1700000000000}}`)
	m.mu.Lock()
	rn := m.runs["r"]
	rn.entries[rn.byName["secret"]].Data = tomb
	m.mu.Unlock()
	rec, ok, err := j.Get(ctx, "r", "secret")
	if err != nil || !ok || !rec.Redacted || rec.Name != "secret" || rec.Result != nil || string(rec.Raw()) != string(tomb) {
		t.Fatalf("Get of a tombstone = %+v, %v, %v", rec, ok, err)
	}
	if _, err := mustNew(NewScriptedModel(), j).Run(ctx, "r", UserText("hi")); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "redacted") {
		t.Fatalf("Run over a redacted run = %v, want ErrConfig naming the redaction", err)
	}
	for _, b := range []string{`{"redacted":{"at_ms":1}}`, `{"redacted":{"leaf_hash":"ab"},"x":1}`, `{"name":"x","redacted":{"leaf_hash":"ab"}}`} {
		if isTombstone([]byte(b)) {
			t.Errorf("%s reads as a tombstone", b)
		}
	}
}

func TestRunFilter_Admits(t *testing.T) {
	holds := func(names ...string) func(string) bool {
		return func(n string) bool {
			for _, h := range names {
				if h == n {
					return true
				}
			}
			return false
		}
	}
	for _, c := range []struct {
		f      RunFilter
		id     string
		names  []string
		lapsed bool
		want   bool
	}{
		{RunFilter{}, "a", nil, false, true},
		{RunFilter{After: "a"}, "a", nil, false, false},
		{RunFilter{After: "a"}, "b", nil, false, true},
		{RunFilter{Prefix: "t1/"}, "t1/x", nil, false, true},
		{RunFilter{Prefix: "t1/"}, "t2/x", nil, false, false},
		{RunFilter{Prefix: "t1/"}, "t1", nil, false, false},
		{RunFilter{ExcludeHolding: []string{"run:complete"}}, "a", []string{"run:complete"}, false, false},
		{RunFilter{ExcludeHolding: []string{"run:complete"}}, "a", []string{"run:start"}, false, true},
		{RunFilter{}, "a", nil, true, true}, // LeaseLapsed unset: the lease does not matter
		{RunFilter{LeaseLapsed: true}, "a", nil, false, false},
		{RunFilter{LeaseLapsed: true}, "a", nil, true, true},
		{RunFilter{LeaseLapsed: true, ExcludeHolding: []string{"run:complete"}}, "a", []string{"run:complete"}, true, false},
		{RunFilter{LeaseLapsed: true, Prefix: "t1/"}, "t2/x", nil, true, false},
	} {
		lapsed := func() bool {
			if !c.f.LeaseLapsed {
				t.Errorf("%+v.Admits(%q) asked about the lease without LeaseLapsed", c.f, c.id)
			}
			return c.lapsed
		}
		if got := c.f.Admits(c.id, holds(c.names...), lapsed); got != c.want {
			t.Errorf("%+v.Admits(%q holding %v, lapsed %v) = %v, want %v", c.f, c.id, c.names, c.lapsed, got, c.want)
		}
	}
}

// A run deleted from its store while a Journal remembers it gets a new header on its next drive,
// rather than records a reader would refuse as headerless.
func TestJournal_DeletedRunGetsANewHeader(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	j, _ := NewJournal(m)
	if _, err := j.open(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	delete(m.runs, "r")
	m.mu.Unlock()
	if _, err := j.open(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	if f, err := j.Format(ctx, "r"); err != nil || f != JournalFormat {
		t.Fatalf("Format after the drive = %q, %v; want the header written again", f, err)
	}
}

// The journal header is written directly, not through EncodeRecord; its bytes are exactly the
// header record's journal encoding, a fixed point like every record's.
func TestHeaderEntry_IsTheHeaderRecordsEncoding(t *testing.T) {
	b := headerEntry()
	r, err := DecodeStoredRecord("r", headerStep, b)
	if err != nil || r.Kind != StepHeader || r.Format != JournalFormat || len(r.Salt()) != SaltSize {
		t.Fatalf("the header decodes as %+v, %v", r, err)
	}
	if enc, err := EncodeRecord(r); err != nil || string(enc) != string(b) {
		t.Fatalf("the header's journal encoding is %s, %v; headerEntry wrote %s", enc, err, b)
	}
	if string(headerEntry()) == string(b) {
		t.Fatal("two headers share a salt")
	}
}

// Get of a name a run does not hold reports a run this version cannot read, rather than
// not-found, so no reader takes an old-format run for an empty one. An empty run is not-found.
func TestJournal_GetOfAMissingNameInAnUnreadableRun(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	if _, _, err := m.Insert(ctx, "old", "x", []byte(`{"name":"x","kind":"value"}`)); err != nil {
		t.Fatal(err)
	}
	j, _ := NewJournal(m)
	var v *JournalVersionError
	if _, ok, err := j.Get(ctx, "old", "missing"); !errors.As(err, &v) || ok {
		t.Fatalf("Get of a missing name in a headerless run = %v, %v; want a *JournalVersionError", ok, err)
	}
	if _, ok, err := j.Get(ctx, "empty", "missing"); err != nil || ok {
		t.Fatalf("Get in an empty run = %v, %v; want not-found", ok, err)
	}
}

// racedStore records another driver's result for the named step just before this driver's own
// Insert of it, as a driver that loaded the run earlier and ran a retry-safe tool would.
type racedStore struct {
	Store
	name string
	them []byte
}

func (r *racedStore) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == r.name && r.them != nil {
		if _, _, err := r.Store.Insert(ctx, runID, name, r.them); err != nil {
			return Entry{}, false, err
		}
		r.them = nil
	}
	return r.Store.Insert(ctx, runID, name, data)
}

// capturingModel records the tool results each request carries.
type capturingModel struct {
	Model
	seen []string
}

func (c *capturingModel) Stream(ctx context.Context, req Request) (*Stream, error) {
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if tr, ok := p.(ToolResult); ok {
				c.seen = append(c.seen, string(tr.Result))
			}
		}
	}
	return c.Model.Stream(ctx, req)
}

// A retry-safe tool's result is written without a read first, so another driver's result may win
// the Insert. The run then goes on with the stored result, never this driver's own: the result the
// model reads, the journal, and a replay all agree.
func TestLiveToolResult_LosingInsertUsesTheStoredResult(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	j2 := mustJournal(m)
	theirs, err := JournalEntry(ToolResultStep("c1"), Record{Kind: StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`"theirs"`)})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := NewJournal(&racedStore{Store: m, name: ToolResultStep("c1"), them: theirs})
	model := &capturingModel{Model: NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))}
	tool := MustFunc("lookup", "", func(context.Context, struct{}) (string, error) { return "mine", nil }, WithSafety(Safety{ReadOnly: true}))
	if _, err := mustNew(model, j, WithTools(tool)).Run(ctx, "r", UserText("hi")); err != nil {
		t.Fatal(err)
	}
	if len(model.seen) != 1 || model.seen[0] != `"theirs"` {
		t.Fatalf("the model read tool results %q; want only the stored \"theirs\"", model.seen)
	}
	rec, ok, err := j2.Get(ctx, "r", ToolResultStep("c1"))
	if err != nil || !ok || string(rec.Result) != `"theirs"` {
		t.Fatalf("the journal holds %s, %v, %v; want \"theirs\"", rec.Result, ok, err)
	}
}

// The known-good cache forgets the least recently used run, not the oldest added.
func TestRunSet_ForgetsTheLeastRecentlyUsed(t *testing.T) {
	var s runSet
	for i := range maxKnownRuns {
		s.add(fmt.Sprint(i))
	}
	if !s.has("0") { // "0" is now the most recently used
		t.Fatal("a member was forgotten early")
	}
	s.add("new")
	if !s.has("0") || s.has("1") || !s.has("new") {
		t.Fatalf("after one more run: has 0 %v, 1 %v, new %v; want 1 (the least recently used) forgotten", s.has("0"), s.has("1"), s.has("new"))
	}
}

// The engine's readers report a run this version cannot read as such, never as a run that has no
// start, is not complete, or holds no marker.
func TestReaders_RefuseAnUnreadableRun(t *testing.T) {
	ctx := context.Background()
	m := NewMemStore()
	j := mustJournal(m)
	if _, _, err := m.Insert(ctx, "old", runCompleteStep, []byte(`{"name":"run:complete","kind":"value"}`)); err != nil {
		t.Fatal(err)
	}
	var v *JournalVersionError
	if _, ok, err := RecordedStart(ctx, j, "old"); !errors.As(err, &v) || ok {
		t.Errorf("RecordedStart = %v, %v; want a *JournalVersionError", ok, err)
	}
	if done, err := IsComplete(ctx, j, "old"); !errors.As(err, &v) || done {
		t.Errorf("IsComplete = %v, %v; want a *JournalVersionError", done, err)
	}
	if held, err := hasValueStep(ctx, j, "old", runAbortedStep); !errors.As(err, &v) || held {
		t.Errorf("hasValueStep = %v, %v; want a *JournalVersionError", held, err)
	}
}

// These tests change a live input (the caller's input or entry point, the tool registry, a
// tool's approval gate) between two drives of one run, and check that every decision the second
// drive makes is the one the journal records, not the one the new live value would give.

// userTexts returns the text of every user message in msgs.
func userTexts(msgs []Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == RoleUser {
			out = append(out, m.Text())
		}
	}
	return out
}

// A run's earlier model turns answered the input it started with. Resumed with another input,
// the next model call would be sent the new message beside turns that answered the old one: a
// conversation neither input produced. The input is journaled when the run starts, and a resume
// with a different one is refused.
func TestResume_DifferentInputIsRefused(t *testing.T) {
	ctx := context.Background()
	var n int
	gate := &countingTool{name: "refund", approval: SingleApproval(), calls: &n}
	var got Request
	m := &captureModel{inner: NewScriptedModel(ToolTurn("c1", "refund", `{}`), TextTurn("refunded")), got: &got}
	store := memJournal()
	a := mustNew(m, store, WithTools(gate))

	var pa *ApprovalPending
	if _, err := a.Run(ctx, "r", UserText("refund order 17")); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *ApprovalPending", err)
	}
	if err := Approve(ctx, store, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	got = Request{}
	_, err := a.Run(ctx, "r", UserText("refund order 99"))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("resume with another input: err = %v, want ErrConfig; the model was sent user messages %q", err, userTexts(got.Messages))
	}
	if n != 0 {
		t.Fatalf("refused resume ran the tool %d times", n)
	}

	// The run's own input resumes it, and the model sees that input.
	res, err := a.Run(ctx, "r", UserText("refund order 17"))
	var out Message
	if res != nil {
		out = res.Message
	}
	if err != nil || textOf(out) != "refunded" {
		t.Fatalf("resume with the recorded input: %q, %v", textOf(out), err)
	}
	if u := userTexts(got.Messages); !slices.Equal(u, []string{"refund order 17"}) {
		t.Fatalf("model was sent user messages %q, want the run's input", u)
	}
	start, ok, err := RecordedStart(ctx, store, "r")
	if err != nil || !ok || start.Input.Text() != "refund order 17" || start.Saga {
		t.Fatalf("RecordedStart = %+v, %v, %v; want the run's input, not a saga", start, ok, err)
	}
}

// SendOnce documents that reusing a key with a different input is ErrConfig. That held only
// for a key whose turn had finished: a paused turn was resumed with the new message, and the
// session recorded the new message as the turn's input beside an answer to the old one.
func TestSession_SendOnceOpenTurnDifferentInputIsRefused(t *testing.T) {
	ctx := context.Background()
	var n int
	gate := &countingTool{name: "refund", approval: SingleApproval(), calls: &n}
	store := memJournal()
	a := mustNew(
		NewScriptedModel(ToolTurn("c1", "refund", `{}`), TextTurn("refunded")),
		store,
		WithTools(gate),
	)
	s, err := a.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	var pa *ApprovalPending
	if _, err := s.SendOnce(ctx, "k1", UserText("refund order 17")); !errors.As(err, &pa) {
		t.Fatalf("first send: err = %v, want *ApprovalPending", err)
	}
	if err := Approve(ctx, store, pa.RunID, pa.ToolUseID, true); err != nil {
		t.Fatal(err)
	}
	_, err = s.SendOnce(ctx, "k1", UserText("refund order 99"))
	if !errors.Is(err, ErrConfig) {
		t.Fatalf("SendOnce on an open key with another input: err = %v, want ErrConfig; transcript %q", err, userTexts(s.History()))
	}
	if n != 0 {
		t.Fatalf("refused send ran the tool %d times", n)
	}
	if _, err := s.SendOnce(ctx, "k1", UserText("refund order 17")); err != nil {
		t.Fatalf("SendOnce with the key's own input: %v", err)
	}
	if u := userTexts(s.History()); !slices.Equal(u, []string{"refund order 17"}) {
		t.Fatalf("transcript user messages %q, want the key's input", u)
	}
}

// A saga run resumed through Run (a recovery callback that always calls Run, say) lost its
// saga semantics: a failing step was journaled as an ordinary tool error, the model carried
// on, and the run finished with the earlier charge never compensated. The saga flag is journaled
// with the input (run:start), and every later drive runs under it: Run with no WithSaga drives a
// saga run as a saga.
func TestResume_SagaRunThroughRunKeepsItASaga(t *testing.T) {
	ctx := context.Background()
	var undone, gated int
	charge := MustCompensatedFunc("charge", "", func(context.Context, struct{}) (string, error) { return "ch_1", nil },
		func(context.Context, struct{}, string) error { undone++; return nil })
	gate := &countingTool{name: "gate", safety: Safety{ReadOnly: true}, approval: SingleApproval(), calls: &gated}
	book := MustFunc("book", "", func(context.Context, struct{}) (string, error) {
		return "", errors.New("no rooms left")
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "charge", `{}`), ToolTurn("c2", "gate", `{}`), ToolTurn("c3", "book", `{}`), TextTurn("done"))
	a := mustNew(m, store, WithTools(charge, gate, book))

	var pa *ApprovalPending
	if _, err := a.Run(ctx, "r", UserText("trip"), WithSaga()); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *ApprovalPending", err)
	}
	if err := Approve(ctx, store, "r", "c2", true); err != nil {
		t.Fatal(err)
	}
	// Run with no WithSaga drives a run journaled as a saga as the saga it is: the failure rolls
	// it back, once.
	var aborted *SagaAborted
	if _, err := a.Run(ctx, "r", UserText("trip")); !errors.As(err, &aborted) || undone != 1 {
		t.Fatalf("saga resumed through Run: err = %v, compensations %d; want *SagaAborted and 1", err, undone)
	}
	if _, err := a.Run(ctx, "r", UserText("trip"), WithSaga()); !errors.As(err, &aborted) || undone != 1 {
		t.Fatalf("saga driven again with WithSaga: err = %v, compensations %d; want *SagaAborted and still 1", err, undone)
	}
	start, ok, err := RecordedStart(ctx, store, "r")
	if err != nil || !ok || !start.Saga || start.Input.Text() != "trip" {
		t.Fatalf("RecordedStart = %+v, %v, %v; want the saga's input", start, ok, err)
	}
}

// A run started through Run and resumed through saga Run would apply rollback to writes made
// under Run's rules; it is refused the same way.
func TestResume_RunThroughWithSagaIsRefused(t *testing.T) {
	ctx := context.Background()
	var gated int
	gate := &countingTool{name: "gate", safety: Safety{ReadOnly: true}, approval: SingleApproval(), calls: &gated}
	store := memJournal()
	a := mustNew(NewScriptedModel(ToolTurn("c1", "gate", `{}`), TextTurn("done")), store, WithTools(gate))
	var pa *ApprovalPending
	if _, err := a.Run(ctx, "r", UserText("go")); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *ApprovalPending", err)
	}
	if err := Approve(ctx, store, "r", "c1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(ctx, "r", UserText("go"), WithSaga()); !errors.Is(err, ErrConfig) {
		t.Fatalf("run resumed through saga Run: err = %v, want ErrConfig", err)
	}
	if _, err := a.Stream(ctx, "r", UserText("go")).Result(); err != nil {
		t.Fatalf("run resumed through Stream: %v", err)
	}
}

// A human's recorded denial is final. If the tool's approval gate is removed before the run is
// driven again, the gate no longer asks, and the denied call ran anyway.
func TestResume_RecordedDenialHoldsAfterGateRemoved(t *testing.T) {
	ctx := context.Background()
	var n int
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "wire", `{"usd":5000}`), TextTurn("ok"))
	gated := &countingTool{name: "wire", approval: SingleApproval(), calls: &n}
	var pa *ApprovalPending
	if _, err := mustNew(m, store, WithTools(gated)).Run(ctx, "r", UserText("pay")); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *ApprovalPending", err)
	}
	if err := Approve(ctx, store, "r", "c1", false); err != nil {
		t.Fatal(err)
	}
	ungated := &countingTool{name: "wire", safety: Safety{}, calls: &n}
	if _, err := mustNew(m, store, WithTools(ungated)).Run(ctx, "r", UserText("pay")); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n != 0 {
		t.Fatalf("a call a human denied ran %d times once its gate was removed", n)
	}
	recs, _ := store.History(ctx, "r")
	if !deniedResult(recs, "c1") {
		t.Fatalf("no denial recorded for c1: %+v", recs)
	}
}

// The same holds for an m-of-n gate whose terminal tally (a denial) was journaled and whose
// denial result was not, because the process died between the two writes: a deploy without the
// gate ran the call.
func TestResume_RecordedTallyDenialHoldsAfterGateRemoved(t *testing.T) {
	ctx := context.Background()
	var n int
	store := mustJournal(&failOnceStore{MemStore: NewMemStore(), name: ToolResultStep("c1")}) // the call's result: its denial
	m := NewScriptedModel(ToolTurn("c1", "wire", `{}`), TextTurn("ok"))
	gated := &countingTool{name: "wire", approval: &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}, calls: &n}
	a := mustNew(m, store, WithTools(gated), WithApproverVerifiers(fakeVerifiers("a", "b")))
	var pa *ApprovalPending
	if _, err := a.Run(ctx, "r", UserText("pay")); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *ApprovalPending", err)
	}
	approveAs(t, store, "r", "c1", "a", false) // 2 of 2 can no longer be reached
	if _, err := a.Run(ctx, "r", UserText("pay")); err == nil {
		t.Fatal("second drive: want the crash writing the denial")
	}
	ungated := &countingTool{name: "wire", safety: Safety{}, calls: &n}
	if _, err := mustNew(m, store, WithTools(ungated)).Run(ctx, "r", UserText("pay")); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n != 0 {
		t.Fatalf("a call its quorum denied ran %d times once its gate was removed", n)
	}
	recs, _ := store.History(ctx, "r")
	if !deniedResult(recs, "c1") {
		t.Fatalf("no denial recorded for c1: %+v", recs)
	}
}

// A denial recorded under a 1-of-1 gate holds when the gate becomes m-of-n before the run is
// driven again: the call is denied, not paused for approvers who were never asked.
func TestResume_RecordedDenialHoldsAfterGateTightened(t *testing.T) {
	ctx := context.Background()
	var n int
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "wire", `{}`), TextTurn("ok"))
	var pa *ApprovalPending
	if _, err := mustNew(m, store, WithTools(&countingTool{name: "wire", approval: SingleApproval(), calls: &n})).Run(ctx, "r", UserText("pay")); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *ApprovalPending", err)
	}
	if err := Approve(ctx, store, "r", "c1", false); err != nil {
		t.Fatal(err)
	}
	quorum := &countingTool{name: "wire", approval: &ApprovalPolicy{Need: 2, Approvers: []string{"a", "b"}}, calls: &n}
	if _, err := mustNew(m, store, WithTools(quorum), WithApproverVerifiers(fakeVerifiers("a", "b"))).Run(ctx, "r", UserText("pay")); err != nil {
		t.Fatalf("resume: %v", err)
	}
	recs, _ := store.History(ctx, "r")
	if n != 0 || !deniedResult(recs, "c1") {
		t.Fatalf("tool ran %d times, denial recorded %v; want 0 and true", n, deniedResult(recs, "c1"))
	}
}

func deniedResult(recs []Record, id string) bool {
	for _, r := range recs {
		if r.Kind == StepToolResult && r.ToolUseID == id && r.IsError && string(r.Result) == `"tool call denied by human"` {
			return true
		}
	}
	return false
}

// A completed write whose tool is no longer registered when the saga rolls back cannot be
// compensated. It was skipped without a word, so SagaAborted reported a clean rollback while
// the write stood.
func TestSaga_UnregisteredWriteIsReportedUncompensated(t *testing.T) {
	ctx := context.Background()
	var reserved, gated int
	reserve := &countingTool{name: "reserve", safety: Safety{}, calls: &reserved}
	gate := &countingTool{name: "gate", safety: Safety{ReadOnly: true}, approval: SingleApproval(), calls: &gated}
	book := MustFunc("book", "", func(context.Context, struct{}) (string, error) {
		return "", errors.New("no rooms left")
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", "reserve", `{}`), ToolTurn("c2", "gate", `{}`), ToolTurn("c3", "book", `{}`), TextTurn("done"))
	var pa *ApprovalPending
	if _, err := mustNew(m, store, WithTools(reserve, gate, book)).Run(ctx, "r", UserText("trip"), WithSaga()); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *ApprovalPending", err)
	}
	if reserved != 1 {
		t.Fatalf("reserve ran %d times, want 1", reserved)
	}
	if err := Approve(ctx, store, "r", "c2", true); err != nil {
		t.Fatal(err)
	}
	_, err := mustNew(m, store, WithTools(gate, book)).Run(ctx, "r", UserText("trip"), WithSaga()) // reserve is no longer registered
	var aborted *SagaAborted
	if !errors.As(err, &aborted) {
		t.Fatalf("resume: err = %v, want *SagaAborted", err)
	}
	if aborted.CompensateErr != nil || !slices.Contains(aborted.Uncompensated, "reserve") {
		t.Fatalf("rollback reported compensated %q, uncompensated %q, stopped by %v: want the completed reserve write listed and the rollback finished", aborted.Compensated, aborted.Uncompensated, aborted.CompensateErr)
	}
}

// A call that was attempted as a side effect and has no result may have taken effect: rollback
// stops for a human. Driven again after its tool was unregistered, rollback skipped the call,
// reported a clean rollback, and marked the run aborted, so the halt was lost.
func TestSaga_UnregisteredAttemptedCallStillHalts(t *testing.T) {
	ctx := context.Background()
	// reserve and book run concurrently in one turn; once reserve has been called, book fails,
	// the saga cancels reserve, and reserve's attempt marker stays without a result.
	entered := make(chan struct{})
	reserve := MustFunc("reserve", "", func(ctx context.Context, _ struct{}) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	})
	book := MustFunc("book", "", func(context.Context, struct{}) (string, error) {
		<-entered
		return "", errors.New("no rooms left")
	})
	turn := []Emit{
		{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "reserve", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: ToolCallDelta{Index: 1, ID: "c2", Name: "book", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: Finish{Reason: "tool_use"}},
	}
	store := NewMemStore()
	j := mustJournal(store)
	wantHalt := func(err error) {
		t.Helper()
		var aborted *SagaAborted
		var halt *OutcomeUnknown
		if !errors.As(err, &aborted) || !errors.As(aborted.CompensateErr, &halt) || halt.Op.ID != "c1" {
			t.Fatalf("rollback: err = %v, want *SagaAborted stopped by a *OutcomeUnknown on c1", err)
		}
	}
	_, err := mustNew(&scriptModel{turns: [][]Emit{turn}}, j, WithTools(reserve, book)).Run(ctx, "r", UserText("trip"), WithSaga())
	wantHalt(err)
	_, err = mustNew(&scriptModel{}, j, WithTools(book)).Run(ctx, "r", UserText("trip"), WithSaga()) // reserve is no longer registered
	wantHalt(err)
	if done, _ := hasValueStep(ctx, j, "r", runAbortedStep); done {
		t.Fatal("the run was marked aborted with a call of unknown outcome")
	}
}

// RecordedStart finds the start record among the run's other values: an answer recorded for an
// interrupt before the run's first drive comes first in its journal.
func TestRecordedStart_AmongOtherValues(t *testing.T) {
	ctx := context.Background()
	store := memJournal()
	if err := store.AnswerInterrupt(ctx, "r", "k", "early"); err != nil {
		t.Fatal(err)
	}
	if _, err := mustNew(NewScriptedModel(TextTurn("ok")), store).Run(ctx, "r", UserText("hello")); err != nil {
		t.Fatal(err)
	}
	start, ok, err := RecordedStart(ctx, store, "r")
	if err != nil || !ok || !sameMessage(start.Input, UserText("hello")) || start.Saga || start.Kind != RunKindAgent {
		t.Fatalf("RecordedStart = %+v, %v, %v; want the run's input", start, ok, err)
	}
	if _, ok, err := RecordedStart(ctx, store, "never"); ok || err != nil {
		t.Fatalf("RecordedStart for a run never driven: ok %v, err %v", ok, err)
	}
}

// relabelSaga runs a saga whose first call is the tool first, completes it, pauses at a gate,
// and then resumes with the tool replaced by then; the third call fails, so the saga rolls back.
// It returns the rollback's *SagaAborted.
func relabelSaga(t *testing.T, first Tool, then ...Tool) *SagaAborted {
	t.Helper()
	ctx := context.Background()
	var gated int
	gate := &countingTool{name: "gate", safety: Safety{ReadOnly: true}, approval: SingleApproval(), calls: &gated}
	book := MustFunc("book", "", func(context.Context, struct{}) (string, error) {
		return "", errors.New("no rooms left")
	})
	store := memJournal()
	m := NewScriptedModel(ToolTurn("c1", first.Spec().Name, `{}`), ToolTurn("c2", "gate", `{}`), ToolTurn("c3", "book", `{}`), TextTurn("done"))
	var pa *ApprovalPending
	if _, err := mustNew(m, store, WithTools(first, gate, book)).Run(ctx, "r", UserText("trip"), WithSaga()); !errors.As(err, &pa) {
		t.Fatalf("first drive: err = %v, want *ApprovalPending", err)
	}
	if err := Approve(ctx, store, "r", "c2", true); err != nil {
		t.Fatal(err)
	}
	_, err := mustNew(m, store, WithTools(append([]Tool{gate, book}, then...)...)).Run(ctx, "r", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil {
		t.Fatalf("resume: err = %v, want a finished rollback", err)
	}
	return aborted
}

// A completed write whose tool was relabelled ReadOnly before the rollback was skipped as if it
// had changed nothing, and the rollback reported clean. The call ran as a write, so it is
// compensated if its tool can undo it and listed as uncompensated if not.
func TestSaga_RelabelledReadOnlyWriteIsStillRolledBack(t *testing.T) {
	var reserved int
	write := &countingTool{name: "reserve", safety: Safety{}, calls: &reserved}
	relabelled := &countingTool{name: "reserve", safety: Safety{ReadOnly: true}, calls: &reserved}
	if got := relabelSaga(t, write, relabelled); !slices.Contains(got.Uncompensated, "reserve") {
		t.Fatalf("rollback listed uncompensated %q: the completed reserve write is missing", got.Uncompensated)
	}

	var undone int
	undo := func(context.Context, struct{}, string) error { undone++; return nil }
	do := func(context.Context, struct{}) (string, error) { return "ok", nil }
	got := relabelSaga(t, MustCompensatedFunc("hold", "", do, undo), MustCompensatedFunc("hold", "", do, undo, WithSafety(Safety{ReadOnly: true})))
	if undone != 1 || !slices.Contains(got.Compensated, "hold") {
		t.Fatalf("compensations %d, compensated %q; want the relabelled write compensated once", undone, got.Compensated)
	}
}

// The reverse: a call that ran ReadOnly changed nothing, so a rollback after its tool was
// relabelled a write, or unregistered, has nothing to report for it.
func TestSaga_ReadOnlyCallStaysSkipped(t *testing.T) {
	var n int
	read := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &n}
	relabelled := &countingTool{name: "lookup", safety: Safety{}, calls: &n}
	if got := relabelSaga(t, read, relabelled); len(got.Uncompensated) != 0 {
		t.Fatalf("a read-only call relabelled a write is listed uncompensated: %q", got.Uncompensated)
	}
	if got := relabelSaga(t, read); len(got.Uncompensated) != 0 {
		t.Fatalf("an unregistered read-only call is listed uncompensated: %q", got.Uncompensated)
	}
}

// A call with no result has no record of the safety it ran under, so rollback goes by its tool's
// safety now: a ReadOnly call cut off by the abort changed nothing and is skipped. (A retry-safe
// call cut off and since relabelled a side effect is the case this cannot catch: see
// KNOWN-LIMITATIONS.)
func TestSaga_CutOffReadOnlyCallIsSkipped(t *testing.T) {
	entered := make(chan struct{})
	lookup := MustFunc("lookup", "", func(ctx context.Context, _ struct{}) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}, WithSafety(Safety{ReadOnly: true}))
	book := MustFunc("book", "", func(context.Context, struct{}) (string, error) {
		<-entered // fail once the read is in flight, so the abort cuts it off
		return "", errors.New("no rooms left")
	})
	turn := []Emit{
		{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: ToolCallDelta{Index: 1, ID: "c2", Name: "book", ArgsFragment: json.RawMessage(`{}`)}},
		{Event: Finish{Reason: "tool_use"}},
	}
	_, err := mustNew(&scriptModel{turns: [][]Emit{turn}}, memJournal(), WithTools(lookup, book)).Run(context.Background(), "r", UserText("trip"), WithSaga())
	var aborted *SagaAborted
	if !errors.As(err, &aborted) || aborted.CompensateErr != nil || len(aborted.Uncompensated) != 0 {
		t.Fatalf("rollback: err = %v; want a clean rollback with the cut-off read skipped", err)
	}
}

type covJournalOpt struct{ err error }

func (o covJournalOpt) applyJournal(*journalConfig) error { return o.err }

// NewJournal refuses a nil option, and returns the error an option reports.
func TestNewJournal_OptionErrors(t *testing.T) {
	if _, err := NewJournal(NewMemStore(), nil); !errors.Is(err, ErrConfig) {
		t.Fatalf("nil option = %v; want ErrConfig", err)
	}
	want := errors.New("bad option")
	if _, err := NewJournal(NewMemStore(), covJournalOpt{}, covJournalOpt{want}); !errors.Is(err, want) {
		t.Fatalf("failing option = %v; want its error", err)
	}
	if j, err := NewJournal(NewMemStore(), covJournalOpt{}); err != nil || j == nil {
		t.Fatalf("passing option = %v, %v", j, err)
	}
}

// A nil journal is refused as ErrConfig by checkJournal and has no identity.
func TestJournal_NilJournal(t *testing.T) {
	var j *Journal
	if err := checkJournal(j); !errors.Is(err, ErrConfig) {
		t.Fatalf("checkJournal(nil) = %v; want ErrConfig", err)
	}
	if _, ok := j.identity(); ok {
		t.Fatal("a nil journal has an identity")
	}
	if err := checkJournal(memJournal()); err != nil {
		t.Fatalf("checkJournal(journal) = %v", err)
	}
}

// seedHeader stores a header entry holding data as run r's first entry, the way an older or
// foreign writer might.
func seedHeader(t *testing.T, s Store, r string, data string) {
	t.Helper()
	if _, _, err := s.Insert(context.Background(), r, headerStep, []byte(data)); err != nil {
		t.Fatal(err)
	}
}

// A run whose header names a format this version does not support, or is not a header at all, is
// refused by every entry point before a step's function runs or anything is written.
func TestJournal_UnreadableHeaderRefusesEverything(t *testing.T) {
	ctx := context.Background()
	future := `{"name":"` + headerStep + `","kind":"` + string(StepHeader) + `","format":"bide-journal-999"}`
	cases := map[string]struct {
		first    string // data of the run's first entry, stored as the header
		versions bool   // want a *JournalVersionError (else ErrStorage)
	}{
		"future format":     {future, true},
		"not a header":      {`{"name":"x","kind":"value"}`, false},
		"not json":          {`<<<`, false},
		"header wrong kind": {`{"name":"` + headerStep + `","kind":"value","format":"x"}`, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := NewMemStore()
			seedHeader(t, s, "r", c.first)
			j := mustJournal(s)
			check := func(what string, err error) {
				t.Helper()
				var ve *JournalVersionError
				switch {
				case err == nil:
					t.Errorf("%s succeeded on an unreadable header", what)
				case c.versions && !errors.As(err, &ve):
					t.Errorf("%s = %v; want a *JournalVersionError", what, err)
				case !c.versions && !errors.Is(err, ErrStorage):
					t.Errorf("%s = %v; want ErrStorage", what, err)
				}
			}
			ran := false
			fn := func(context.Context) (Record, error) {
				ran = true
				return Record{Kind: StepValue, Result: []byte(`1`)}, nil
			}
			_, err := j.do(ctx, "r", "s", fn)
			check("do", err)
			_, err = j.doFresh(ctx, "r", "s", fn)
			check("doFresh", err)
			_, err = j.put(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)})
			check("put", err)
			_, _, err = j.putNew(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)})
			check("putNew", err)
			_, err = j.History(ctx, "r")
			check("History", err)
			_, _, err = j.Get(ctx, "r", headerStep)
			check("Get", err)
			if c.versions {
				if f, err := j.Format(ctx, "r"); err != nil || f != "bide-journal-999" {
					t.Errorf("Format = %q, %v; want the format reported, not refused", f, err)
				}
			} else {
				_, err = j.Format(ctx, "r")
				check("Format", err)
			}
			if ran {
				t.Error("a step's function ran against an unreadable run")
			}
		})
	}
}

// Format of a run whose first entry is not a header is a JournalVersionError, of an empty run is
// "", and of a run this version wrote is the current format.
func TestJournal_Format(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	j := mustJournal(s)
	if f, err := j.Format(ctx, "none"); f != "" || err != nil {
		t.Fatalf("Format of an empty run = %q, %v", f, err)
	}
	if _, err := j.put(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)}); err != nil {
		t.Fatal(err)
	}
	if f, err := j.Format(ctx, "r"); err != nil || f != JournalFormat {
		t.Fatalf("Format = %q, %v; want %q", f, err, JournalFormat)
	}
	if _, _, err := s.Insert(ctx, "raw", "step", []byte(`{"name":"step","kind":"value"}`)); err != nil {
		t.Fatal(err)
	}
	var ve *JournalVersionError
	if _, err := mustJournal(s).Format(ctx, "raw"); !errors.As(err, &ve) || ve.Found != "" {
		t.Fatalf("Format of a run with no header first = %v; want a *JournalVersionError with no format found", err)
	}
}

// A store that cannot write the header makes every write fail, and the step's function does not
// run: a run's format is settled before a side effect could fire.
func TestJournal_HeaderWriteFailureStopsBeforeTheEffect(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(covInsertFail{NewMemStore(), headerStep})
	ran := false
	fn := func(context.Context) (Record, error) {
		ran = true
		return Record{Kind: StepValue, Result: []byte(`1`)}, nil
	}
	if _, err := j.do(ctx, "r", "s", fn); !errors.Is(err, ErrStorage) {
		t.Errorf("do = %v; want ErrStorage", err)
	}
	if _, err := j.doFresh(ctx, "r", "s", fn); !errors.Is(err, ErrStorage) {
		t.Errorf("doFresh = %v; want ErrStorage", err)
	}
	if _, err := j.put(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)}); !errors.Is(err, ErrStorage) {
		t.Errorf("put = %v; want ErrStorage", err)
	}
	if _, _, err := j.putNew(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)}); !errors.Is(err, ErrStorage) {
		t.Errorf("putNew = %v; want ErrStorage", err)
	}
	if ran {
		t.Error("a step's function ran though the run's header could not be written")
	}
}

// A record write the store refuses is ErrStorage, and the same step is recorded by a later try.
func TestJournal_PutNewInsertFailure(t *testing.T) {
	ctx := context.Background()
	st := &failOnceInsert{Store: NewMemStore(), sub: "s"}
	j := mustJournal(st)
	if _, _, err := j.putNew(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)}); !errors.Is(err, ErrStorage) {
		t.Fatalf("putNew over a failing store = %v; want ErrStorage", err)
	}
	rec, inserted, err := j.putNew(ctx, "r", "s", Record{Kind: StepValue, Result: []byte(`1`)})
	if err != nil || !inserted || string(rec.Result) != `1` {
		t.Fatalf("putNew on the retry = %+v, %v, %v", rec, inserted, err)
	}
}

// failOnceInsert fails the first Insert of an entry named sub.
type failOnceInsert struct {
	Store
	sub  string
	done bool
}

func (f *failOnceInsert) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == f.sub && !f.done {
		f.done = true
		return Entry{}, false, errCovBlip
	}
	return f.Store.Insert(ctx, runID, name, data)
}

// A record the codec cannot encode is ErrStorage, naming the step, and nothing is recorded.
func TestJournal_UnencodableRecord(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	bad := Record{Kind: StepValue, Result: []byte(`{not json`)}
	if _, _, err := j.putNew(ctx, "r", "s", bad); !errors.Is(err, ErrStorage) {
		t.Errorf("putNew = %v; want ErrStorage", err)
	}
	if _, err := j.do(ctx, "r", "t", func(context.Context) (Record, error) { return bad, nil }); !errors.Is(err, ErrStorage) {
		t.Errorf("do = %v; want ErrStorage", err)
	}
	for _, n := range []string{"s", "t"} {
		if _, ok, _ := j.Get(ctx, "r", n); ok {
			t.Errorf("an unencodable record was stored as %q", n)
		}
	}
}

// A stored record that does not decode is reported as ErrStorage by Get and History, not skipped.
func TestJournal_UndecodableStoredRecord(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	j := mustJournal(s)
	if _, err := j.put(ctx, "r", "ok", Record{Kind: StepValue, Result: []byte(`1`)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "r", "bad", []byte(`{"name":`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Get(ctx, "r", "bad"); !errors.Is(err, ErrStorage) {
		t.Errorf("Get = %v; want ErrStorage", err)
	}
	recs, err := j.History(ctx, "r")
	if !errors.Is(err, ErrStorage) || recs != nil {
		t.Errorf("History = %v, %v; want nil and ErrStorage", recs, err)
	}
	var got int
	for _, err := range j.Records(ctx, "r") {
		if err == nil {
			got++
		}
	}
	if got != 2 { // the header and "ok", before the bad one
		t.Errorf("Records yielded %d good records before the bad one; want 2", got)
	}
}

// Stopping Records early stops the load, and a failing Load is yielded once as ErrStorage.
func TestJournal_RecordsStopsAndLoadFaults(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	for i := range 3 {
		if _, err := j.put(ctx, "r", fmt.Sprintf("s%d", i), Record{Kind: StepValue, Result: []byte(`1`)}); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, err := range j.Records(ctx, "r") {
		if err != nil {
			t.Fatal(err)
		}
		if n++; n == 2 {
			break
		}
	}
	if n != 2 {
		t.Fatalf("early break read %d records", n)
	}
	lf := mustJournal(loadFails{NewMemStore()})
	for _, err := range lf.Records(ctx, "r") {
		if !errors.Is(err, ErrStorage) {
			t.Fatalf("Records over a failing Load = %v; want ErrStorage", err)
		}
	}
	if _, err := lf.Format(ctx, "r"); !errors.Is(err, ErrStorage) {
		t.Fatalf("Format over a failing Load = %v; want ErrStorage", err)
	}
}

// loadFails yields an error from every Load.
type loadFails struct{ Store }

func (loadFails) Load(context.Context, string, int64) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) { yield(Entry{}, errCovBlip) }
}

// The claim memo is bounded: past maxPendingClaims ids the oldest keys are dropped (a dropped id
// only costs a halt a later claim would have avoided), the newest stay, and keys already taken
// do not pile up in its order list.
func TestClaimMemo_IsBounded(t *testing.T) {
	c := &claimMemo{m: map[flightKey]map[string]struct{}{}}
	key := func(i int) flightKey { return flightKey{nil, "r", fmt.Sprintf("k%d", i)} }
	for i := 0; i < maxPendingClaims+10; i++ {
		c.remember(key(i), "id")
	}
	if c.n > maxPendingClaims {
		t.Fatalf("memo holds %d ids; the bound is %d", c.n, maxPendingClaims)
	}
	if got := c.takeAll(key(0)); len(got) != 0 {
		t.Errorf("the oldest key survived the bound: %v", got)
	}
	if got := c.takeAll(key(maxPendingClaims + 9)); len(got) != 1 {
		t.Errorf("the newest key was dropped: %v", got)
	}
	// Keys taken as soon as they are remembered leave stale entries in order; they are compacted.
	d := &claimMemo{m: map[flightKey]map[string]struct{}{}}
	for i := 0; i < 2*maxPendingClaims+5; i++ {
		d.remember(key(i), "id")
		d.takeAll(key(i))
	}
	if len(d.order) > 2*maxPendingClaims {
		t.Errorf("order list holds %d keys though none is live", len(d.order))
	}
	if d.n != 0 {
		t.Errorf("memo holds %d ids after all were taken", d.n)
	}
}

// The hooks package plan reaches the journal through refuse, with ErrConfig, anything that is not
// the type they expect, rather than panicking on a failed assertion or recording something wrong.
func TestJournalHook_RefusesWrongTypes(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	body := func(context.Context) (json.RawMessage, error) { return json.RawMessage(`1`), nil }
	recFn := func(context.Context) (any, error) { return Record{Kind: StepValue, Result: json.RawMessage(`1`)}, nil }
	cases := map[string]func() error{
		"Do journal": func() error { _, err := journalhook.Do(ctx, 7, "r", "s", recFn); return err },
		"DoFresh journal": func() error {
			_, err := journalhook.DoFresh(ctx, "j", "r", "s", recFn)
			return err
		},
		"Do result type": func() error {
			_, err := journalhook.Do(ctx, j, "r", "s", func(context.Context) (any, error) { return "not a record", nil })
			return err
		},
		"Step journal":  func() error { _, err := journalhook.Step(ctx, 7, "r", "node:a", Safety{}, body); return err },
		"Step node key": func() error { _, err := journalhook.Step(ctx, j, "r", "tool:a", Safety{}, body); return err },
		"Step safety":   func() error { _, err := journalhook.Step(ctx, j, "r", "node:a", "safe", body); return err },
		"Gate journal": func() error {
			_, err := journalhook.Gate(ctx, 7, "r", "node:a", "a", nil, SingleApproval(), nil)
			return err
		},
		"Gate policy": func() error { _, err := journalhook.Gate(ctx, j, "r", "node:a", "a", nil, "pol", nil); return err },
		"Gate verifiers": func() error {
			_, err := journalhook.Gate(ctx, j, "r", "node:a", "a", nil, SingleApproval(), 7)
			return err
		},
		"Begin journal": func() error { _, _, _, err := journalhook.Begin(ctx, 7, "r", RunStart{}); return err },
		"Begin start":   func() error { _, _, _, err := journalhook.Begin(ctx, j, "r", "start"); return err },
		"Complete journal": func() error {
			_, err := journalhook.Complete(ctx, 7, "r", json.RawMessage(`1`))
			return err
		},
	}
	for name, call := range cases {
		if err := call(); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: = %v; want ErrConfig", name, err)
		}
	}
	if _, ok, err := j.Get(ctx, "r", "s"); err != nil || ok {
		t.Errorf("a step whose result was not a Record was recorded: %v, %v", ok, err)
	}
}

// A flow cancelled before a node runs does not run the node: Step returns ErrRunCancelled without
// calling the body, and Complete cannot complete a cancelled run (the first end marker is its end).
func TestJournalHook_CancelledFlowRunsNothing(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	start := RunStart{Input: UserText(`{}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}
	if _, _, done, err := journalhook.Begin(ctx, j, "r1", start); err != nil || done {
		t.Fatalf("Begin = %v, %v", done, err)
	}
	if err := Cancel(ctx, j, "r1", "stop"); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := journalhook.Step(ctx, j, "r1", "node:a", Safety{ReadOnly: true}, func(context.Context) (json.RawMessage, error) {
		called = true
		return json.RawMessage(`1`), nil
	})
	if !errors.Is(err, ErrRunCancelled) || called {
		t.Fatalf("Step on a cancelled flow = %v (body called: %v); want ErrRunCancelled, no call", err, called)
	}
	if _, err := journalhook.Complete(ctx, j, "r1", json.RawMessage(`"done"`)); !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("Complete on a cancelled flow = %v; want ErrRunCancelled", err)
	}
	if _, _, _, err := journalhook.Begin(ctx, j, "r1", start); !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("Begin on a cancelled flow = %v; want ErrRunCancelled", err)
	}
}

// A flow's completion is final: Complete returns the first recorded result, and Begin of the
// finished run returns it as done.
func TestJournalHook_CompleteIsFinal(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	start := RunStart{Input: UserText(`{"a":1}`), Kind: RunKindFlow, Flow: &FlowRef{Name: "f"}}
	if _, _, done, err := journalhook.Begin(ctx, j, "r1", start); err != nil || done {
		t.Fatalf("Begin = %v, %v", done, err)
	}
	if got, err := journalhook.Complete(ctx, j, "r1", json.RawMessage(`"first"`)); err != nil || string(got) != `"first"` {
		t.Fatalf("Complete = %s, %v", got, err)
	}
	if got, err := journalhook.Complete(ctx, j, "r1", json.RawMessage(`"second"`)); err != nil || string(got) != `"first"` {
		t.Fatalf("second Complete = %s, %v; want the first result", got, err)
	}
	_, got, done, err := journalhook.Begin(ctx, j, "r1", start)
	if err != nil || !done || string(got) != `"first"` {
		t.Fatalf("Begin of a finished flow = %s, %v, %v", got, done, err)
	}
	other := start
	other.Input = UserText(`{"a":2}`)
	if _, _, _, err := journalhook.Begin(ctx, j, "r1", other); !errors.Is(err, ErrConfig) {
		t.Fatalf("Begin with another input = %v; want ErrConfig", err)
	}
}

// The test-only record hooks copy: they do not alias the caller's bytes, and a nil salt or raw
// clears the field.
func TestJournalHook_RecordSettersCopy(t *testing.T) {
	salt := []byte("salt")
	r := journalhook.WithSalt(Record{Kind: StepValue}, salt).(Record)
	salt[0] = 'X'
	if string(r.salt) != "salt" {
		t.Errorf("WithSalt aliased the caller's slice: %q", r.salt)
	}
	if r := journalhook.WithSalt(r, nil).(Record); r.salt != nil {
		t.Errorf("WithSalt(nil) = %q; want cleared", r.salt)
	}
	raw := json.RawMessage(`{"a":1}`)
	r = journalhook.WithRaw(Record{}, raw).(Record)
	raw[2] = 'b'
	if string(r.raw) != `{"a":1}` {
		t.Errorf("WithRaw aliased the caller's slice: %s", r.raw)
	}
	if r := journalhook.WithRaw(r, nil).(Record); r.raw != nil {
		t.Errorf("WithRaw(nil) = %s; want cleared", r.raw)
	}
	if r := journalhook.WithClaim(Record{}, "c1").(Record); r.claim != "c1" {
		t.Errorf("WithClaim = %q", r.claim)
	}
}

// valStore is a Store with a value receiver (a struct holding a pointer to its data).
type revValStore struct{ m *MemStore }

func (s revValStore) Insert(ctx context.Context, r, n string, d []byte) (Entry, bool, error) {
	return s.m.Insert(ctx, r, n, d)
}
func (s revValStore) Get(ctx context.Context, r, n string) (Entry, bool, error) {
	return s.m.Get(ctx, r, n)
}
func (s revValStore) Load(ctx context.Context, r string, a int64) iter.Seq2[Entry, error] {
	return s.m.Load(ctx, r, a)
}

type revWrap struct{ inner Store }

func (w *revWrap) Insert(ctx context.Context, r, n string, d []byte) (Entry, bool, error) {
	return w.inner.Insert(ctx, r, n, d)
}
func (w *revWrap) Get(ctx context.Context, r, n string) (Entry, bool, error) {
	return w.inner.Get(ctx, r, n)
}
func (w *revWrap) Load(ctx context.Context, r string, a int64) iter.Seq2[Entry, error] {
	return w.inner.Load(ctx, r, a)
}
func (w *revWrap) Unwrap() Store { return w.inner }

// Two Journals over one pointer wrapper of a value-typed store share its identity: the innermost
// pointer on the Unwrap chain (the value store beneath cannot be compared safely).
func TestStoreIdentity_PointerWrapperOverAValueStore(t *testing.T) {
	w := &revWrap{inner: revValStore{m: NewMemStore()}}
	j1, j2 := newJournal(w), newJournal(w)
	if !sameStore(j1, j2) {
		t.Fatalf("two Journals over the same wrapper %p are not one store (ids %p, %p)", w, j1.id, j2.id)
	}
}

// ReportWritten says whether this call's Insert stored the record. Of concurrent identical
// submissions through separate Journals over one store, exactly one sees true; a resubmission, and a
// call refused before it writes, see false.
func TestSubmitDecision_ReportWritten(t *testing.T) {
	ctx := context.Background()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	vf := fakeVerifiers("a")
	store := NewMemStore()
	j := mustJournal(store)
	var charged int
	if _, err := mofnRun(j, "r", true, pol, vf, &charged); err == nil {
		t.Fatal("run did not pause")
	}
	sig := fakeSign("a", ApprovalDecisionBytes(subjectOf(t, j, "r", "c1"), "a", true, testDecidedAt))
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: sig}

	const n = 8
	wrote := make([]bool, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			// Each call has a Journal of its own over the store, as separate handlers of one process
			// (or processes sharing a database) do.
			if err := SubmitDecision(ctx, mustJournal(store), d, WithDecisionCheck(vf), ReportWritten(&wrote[i])); err != nil {
				t.Errorf("call %d: %v", i, err)
			}
		})
	}
	close(start)
	wg.Wait()
	got := 0
	for _, w := range wrote {
		if w {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("%d of %d concurrent identical submissions report they wrote, want exactly 1: %v", got, n, wrote)
	}

	var again bool
	if err := SubmitDecision(ctx, j, d, ReportWritten(&again)); err != nil || again {
		t.Fatalf("a resubmission: %v, wrote %v", err, again)
	}
	bad := d
	bad.Signature = []byte("forged")
	var refused = true
	if err := SubmitDecision(ctx, j, bad, WithDecisionCheck(vf), ReportWritten(&refused)); !errors.Is(err, ErrInvalidApproval) || refused {
		t.Fatalf("a refused decision: %v, wrote %v", err, refused)
	}
	if err := SubmitDecision(ctx, j, d, ReportWritten(nil)); !errors.Is(err, ErrConfig) {
		t.Fatalf("a nil pointer: %v", err)
	}
}

// A record another writer stored first under the same name is the journal's: this call did not write.
func TestSubmitDecision_ReportWrittenWhenInsertLoses(t *testing.T) {
	ctx := context.Background()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	vf := fakeVerifiers("a")
	inner := NewMemStore()
	j := mustJournal(inner)
	var charged int
	if _, err := mofnRun(j, "r", true, pol, vf, &charged); err == nil {
		t.Fatal("run did not pause")
	}
	sig := fakeSign("a", ApprovalDecisionBytes(subjectOf(t, j, "r", "c1"), "a", true, testDecidedAt))
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: sig}
	name := DecisionStep(d)
	rival := Record{Name: name, Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
		Approver: "a", ApproverAlg: fakeAlg, Signature: sig, DecidedAt: FormatTimestamp(d.DecidedAt), RecordedAt: "2026-10-13T11:00:00.000Z"}}
	data, err := JournalEntry(name, rival)
	if err != nil {
		t.Fatal(err)
	}
	// The rival's Insert lands between this call's read of the step and its own Insert.
	var wrote bool
	racy := mustJournal(&insertRival{Store: inner, name: name, data: data})
	if err := SubmitDecision(ctx, racy, d, ReportWritten(&wrote)); err != nil || wrote {
		t.Fatalf("SubmitDecision = %v, wrote %v; want nil, false", err, wrote)
	}
}

// insertRival stores a rival record under name just before the first Insert of that name.
type insertRival struct {
	Store
	name string
	data []byte
	once sync.Once
}

func (s *insertRival) Unwrap() Store { return s.Store }

func (s *insertRival) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == s.name {
		s.once.Do(func() { s.Store.Insert(ctx, runID, name, s.data) })
	}
	return s.Store.Insert(ctx, runID, name, data)
}
