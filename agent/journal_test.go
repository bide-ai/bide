package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// passThrough is a Durable that is not a Journal's: the engine drives it through Do and History
// only, the transitional path for a wrapper that intercepts Do.
type passThrough struct{ inner Durable }

func (p passThrough) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	return p.inner.Do(ctx, runID, name, fn)
}
func (p passThrough) History(ctx context.Context, runID string) ([]Record, error) {
	return p.inner.History(ctx, runID)
}

// Both paths the engine drives a step through: a Journal's own, and a Durable's Do.
func stepPaths() map[string]func() Durable {
	return map[string]func() Durable{
		"journal": func() Durable { return NewMemStore() },
		"durable": func() Durable { return passThrough{NewMemStore()} },
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
			_, err := Step(ctx, d, "r", "charge", body)
			var intr *InterruptPending
			if !errors.Is(err, ErrConfig) || errors.As(err, &intr) || IsPause(err) {
				t.Fatalf("Step = %v; want ErrConfig that is not a pause", err)
			}
			if !strings.Contains(err.Error(), "retry-safe") {
				t.Errorf("the error does not tell the developer what to do: %v", err)
			}
			_, err = Step(ctx, d, "r", "charge", body)
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
			safe := StepSafety(Safety{Idempotent: true})
			_, err := Step(ctx, d, "r", "ask", body, safe)
			var intr *InterruptPending
			if !errors.As(err, &intr) || errors.Is(err, ErrConfig) {
				t.Fatalf("Step = %v; want the *InterruptPending itself", err)
			}
			answered = true
			if v, err := Step(ctx, d, "r", "ask", body, safe); err != nil || v != "ok" {
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
	if _, err := m.Do(ctx, "r", "secret", func(context.Context) (Record, error) {
		return Record{Kind: StepValue, Result: json.RawMessage(`"pii"`)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	tomb := []byte(`{"redacted":{"leaf_hash":"ab12","at_ms":1700000000000}}`)
	m.mu.Lock()
	rn := m.runs["r"]
	rn.entries[rn.byName["secret"]].Data = tomb
	m.mu.Unlock()
	rec, ok, err := m.Journal().Get(ctx, "r", "secret")
	if err != nil || !ok || !rec.Redacted || rec.Name != "secret" || rec.Result != nil || string(rec.Raw()) != string(tomb) {
		t.Fatalf("Get of a tombstone = %+v, %v, %v", rec, ok, err)
	}
	if _, err := New(NewScriptedModel(), m).Run(ctx, "r", "hi"); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "redacted") {
		t.Fatalf("Run over a redacted run = %v, want ErrConfig naming the redaction", err)
	}
	for _, b := range []string{`{"redacted":{"at_ms":1}}`, `{"redacted":{"leaf_hash":"ab"},"x":1}`, `{"name":"x","redacted":{"leaf_hash":"ab"}}`} {
		if isTombstone([]byte(b)) {
			t.Errorf("%s reads as a tombstone", b)
		}
	}
}

// embedsMemStore is a test wrapper that embeds a MemStore and intercepts Do, as the crash-injecting
// stores in this package's tests do.
type embedsMemStore struct {
	*MemStore
	dos int
}

func (e *embedsMemStore) Do(ctx context.Context, runID, name string, fn func(context.Context) (Record, error)) (Record, error) {
	e.dos++
	return e.MemStore.Do(ctx, runID, name, fn)
}

// The engine writes through a store's Journal only when the Durable is that store: a wrapper that
// embeds a store gets the embedded store's Journal() method, but that Journal writes past the
// wrapper, so the engine keeps using the wrapper's own Do.
func TestJournalOf_KeepsAWrappersDo(t *testing.T) {
	m := NewMemStore()
	if journalOf(m) != m.Journal() {
		t.Fatal("journalOf(MemStore) is not its Journal")
	}
	j, _ := NewJournal(m)
	if journalOf(j) != j {
		t.Fatal("journalOf(Journal) is not the Journal")
	}
	w := &embedsMemStore{MemStore: m}
	if journalOf(w) != nil {
		t.Fatal("journalOf found the embedded store's Journal for a wrapper that intercepts Do")
	}
	if _, _, err := ClaimAttempt(context.Background(), w, "r", "attempt:x", Record{Kind: StepAttempt}); err != nil || w.dos != 1 {
		t.Fatalf("ClaimAttempt through the wrapper = %v, with %d calls to its Do; want it to go through Do", err, w.dos)
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
	theirs, err := JournalEntry(ToolResultStep("c1"), Record{Kind: StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`"theirs"`)})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := NewJournal(&racedStore{Store: m, name: ToolResultStep("c1"), them: theirs})
	model := &capturingModel{Model: NewScriptedModel(ToolTurn("c1", "lookup", `{}`), TextTurn("done"))}
	tool := Func("lookup", "", Safety{ReadOnly: true}, func(context.Context, struct{}) (string, error) { return "mine", nil })
	if _, err := New(model, j, tool).Run(ctx, "r", "hi"); err != nil {
		t.Fatal(err)
	}
	if len(model.seen) != 1 || model.seen[0] != `"theirs"` {
		t.Fatalf("the model read tool results %q; want only the stored \"theirs\"", model.seen)
	}
	rec, ok, err := m.Journal().Get(ctx, "r", ToolResultStep("c1"))
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
	if _, _, err := m.Insert(ctx, "old", runCompleteStep, []byte(`{"name":"run:complete","kind":"value"}`)); err != nil {
		t.Fatal(err)
	}
	var v *JournalVersionError
	if _, ok, err := RecordedStart(ctx, m, "old"); !errors.As(err, &v) || ok {
		t.Errorf("RecordedStart = %v, %v; want a *JournalVersionError", ok, err)
	}
	if done, err := IsComplete(ctx, m, "old"); !errors.As(err, &v) || done {
		t.Errorf("IsComplete = %v, %v; want a *JournalVersionError", done, err)
	}
	if held, err := hasValueStep(ctx, m, "old", runAbortedStep); !errors.As(err, &v) || held {
		t.Errorf("hasValueStep = %v, %v; want a *JournalVersionError", held, err)
	}
}
