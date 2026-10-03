package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// countDecodes counts the DecodeRecord calls, from any goroutine, whose bytes contain marker, until
// the test ends. A marker unique to the test keeps other tests' decodes out of the count.
func countDecodes(t *testing.T, marker string) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	h := func(b []byte) {
		if bytes.Contains(b, []byte(marker)) {
			n.Add(1)
		}
	}
	if !decodeHook.CompareAndSwap(nil, &h) {
		t.Fatal("another test holds the decode hook")
	}
	t.Cleanup(func() { decodeHook.Store(nil) })
	return &n
}

// A step MemStore.Do records is decoded once, by the encoding's own round trip: the record Do
// returns is that decoding, not a second decode of the same bytes. It is still exactly what
// History reads back for the step, and the caller's own copy.
func TestMemStoreDo_DecodesAWriteOnce(t *testing.T) {
	ctx := context.Background()
	const run = "decode-once-run"
	decodes := countDecodes(t, run)
	s := memJournal()
	msg := Message{Role: RoleAssistant, Parts: []Part{Text{Text: "hi <b>"}, ToolUse{ID: "c1", Name: "t", Args: json.RawMessage(`{ "a" : 1.50 }`)}}}
	got, err := s.do(ctx, run, "@llm/0", func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &msg, Usage: &Usage{InputTokens: 3}, Result: json.RawMessage(`"` + run + `"`)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := decodes.Load(); n != 1 {
		t.Fatalf("recording one step decoded it %d times, want once", n)
	}
	hist, err := s.History(ctx, run)
	if err != nil || len(hist) != 2 { // the journal header, then the step
		t.Fatalf("History = %v, %v", hist, err)
	}
	if !reflect.DeepEqual(got, hist[1]) {
		t.Fatalf("Do returned\n%#v\nHistory reads\n%#v", got, hist[1])
	}
	// The caller's copy is its own: changing it changes neither the journal nor the step's input.
	got.Message.Parts[0] = Text{Text: "changed"}
	got.Result[1] = 'X'
	again, _ := s.History(ctx, run)
	if !reflect.DeepEqual(again[1], hist[1]) {
		t.Fatal("changing the record Do returned changed the journal")
	}
	if msg.Parts[0] != (Text{Text: "hi <b>"}) {
		t.Fatal("changing the record Do returned changed the step's own record")
	}
}

// When the encoding's two passes differ, Do decodes the stored bytes, as History does, rather than
// hand back the first pass's decoding. They differ on invalid UTF-8 in a Go string field where the
// first pass writes it as the escape \ufffd and the second the character itself: in a
// GOEXPERIMENT=nojsonv2 build (CI runs one). The default build writes the character in both passes,
// so there the passes agree and one decode is all Do makes.
func TestMemStoreDo_UnstableEncodingDecodesStoredBytes(t *testing.T) {
	ctx := context.Background()
	const run = "decode-unstable-run"
	rec := Record{Name: "v", Kind: StepValue, Result: json.RawMessage(`"` + run + `"`), ApproverSignature: &ApproverSignature{Approver: "bad\xffutf8"}}
	first, err := marshalJournal(rec)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := EncodeRecord(rec)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(1)
	if !bytes.Equal(first, stored) {
		want = 2 // the round trip's decode, then the stored bytes'
	}
	decodes := countDecodes(t, run)
	s := memJournal()
	got, err := s.do(ctx, run, "v", func(context.Context) (Record, error) {
		rec.Name = ""
		return rec, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := decodes.Load(); n != want {
		t.Fatalf("recording the step decoded it %d times, want %d (passes agree: %v)", n, want, want == 1)
	}
	hist, _ := s.History(ctx, run)
	if !reflect.DeepEqual(got, hist[1]) || got.Approver() != "bad"+replacementChar+"utf8" {
		t.Fatalf("Do returned %#v, History reads %#v", got, hist[1])
	}
}

// A step name the encoding does not keep (invalid UTF-8 becomes U+FFFD) is refused as the stored
// record naming another step, whichever way Do comes by the decoding.
func TestMemStoreDo_NameNotKeptIsStorageError(t *testing.T) {
	s := memJournal()
	_, err := s.do(context.Background(), "r", "bad\xffname", func(context.Context) (Record, error) {
		return Record{Kind: StepValue}, nil
	})
	if err == nil {
		t.Fatal("Do recorded a step under a name its record does not carry")
	}
}

// Callers that share one write through the single flight, or read it after it landed, each get
// their own copy of the stored record.
func TestMemStoreDo_SharedWriteCopies(t *testing.T) {
	ctx := context.Background()
	s := memJournal()
	started, release := make(chan struct{}), make(chan struct{})
	fn := func(context.Context) (Record, error) {
		close(started)
		<-release
		return Record{Kind: StepValue, Result: json.RawMessage(`{"a":[1,2]}`)}, nil
	}
	var wg sync.WaitGroup
	out := make([]Record, 3)
	wg.Add(1)
	go func() {
		defer wg.Done()
		out[0], _ = s.do(ctx, "r", "v", fn)
	}()
	<-started
	for i := 1; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i], _ = s.do(ctx, "r", "v", func(context.Context) (Record, error) {
				t.Error("a second fn ran for a step already being recorded")
				return Record{}, nil
			})
		}()
	}
	close(release)
	wg.Wait()
	for i := range out {
		if string(out[i].Result) != `{"a":[1,2]}` {
			t.Fatalf("caller %d got %q", i, out[i].Result)
		}
	}
	out[0].Result[1] = 'X'
	for i := 1; i < 3; i++ {
		if string(out[i].Result) != `{"a":[1,2]}` {
			t.Fatalf("changing caller 0's record changed caller %d's: %q", i, out[i].Result)
		}
	}
}

// A live run decodes each record it writes once: its budget and token totals are kept up to date
// from the records it writes, not by decoding them again or reading the journal back each turn.
// Its reads decode nothing: the Load once run:start is written (model 10's DStart returns to
// DOpen) needs only the entries' names, and the point reads of the end markers (at the turn
// boundary, and after run:complete) find none but run:complete, which carries no marker.
// TestBudget_FirstDriveAndCompletion and its neighbours hold the store round trips themselves.
func TestLiveRun_DecodesEachWriteOnce(t *testing.T) {
	const marker = "journal-ops-marker"
	decodes := countDecodes(t, marker)
	tool := MustFunc("noop", "no-op", func(context.Context, struct{}) (string, error) { return "ok", nil }, WithSafety(Safety{ReadOnly: true}))
	a := mustNew(NewScriptedModel(ToolTurn("c1-"+marker, "noop", `{}`), TextTurn("done "+marker)), memJournal(),
		WithTools(tool), WithTokenBudget(1_000_000))
	if _, err := a.Run(context.Background(), "r", UserText("go "+marker)); err != nil {
		t.Fatal(err)
	}
	// run:start, @llm/0, the tool's result and @llm/1 carry the marker (the input, the call's ID,
	// the answer). The three steps are decoded once each, as they are recorded; run:start is
	// written from its encoding and never decoded on the live path.
	if n := decodes.Load(); n != 3 {
		t.Fatalf("the run decoded the records that carry the marker %d times, want 3 (each step once)", n)
	}
}

// A raw value holding one line or paragraph separator alone is written with that separator's
// escape: the encoder's fast check for the separators' lead byte must skip neither replacement.
// The default JSON build escapes both itself, so this pins the check in a GOEXPERIMENT=nojsonv2
// build (CI runs one).
func TestEncodeRecord_SeparatorAloneEscaped(t *testing.T) {
	for _, c := range []struct{ sep, esc string }{{lineSep, jsonEscape + "2028"}, {paraSep, jsonEscape + "2029"}} {
		b, err := EncodeRecord(Record{Name: "v", Kind: StepValue, Result: json.RawMessage(`"` + c.sep + `"`)})
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"name":"v","kind":"value","result":"` + c.esc + `"}`; string(b) != want {
			t.Errorf("EncodeRecord = %q, want %q", b, want)
		}
	}
}
