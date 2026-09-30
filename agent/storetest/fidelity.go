// fidelity.go holds the record-fidelity suite (RunDurable): the record a journal hands back on the
// live path is exactly the record a replay reads back, for any content a model or tool can
// produce.
//
// A journal that returned the caller's own record live but a decoded copy on replay would let a
// resumed run rebuild a different conversation than the one it was having, so the model would
// read different bytes after a crash than without one. The suite feeds records whose encoding is
// easy to get wrong (HTML-significant characters, insignificant whitespace, U+2028, NUL, invalid
// UTF-8, unusual number forms, key order) and requires the live record, the memoized record, and
// the History record to be identical and in the journal's canonical form (agent.EncodeRecord).

package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/internal/journalhook"
)

// RunDurable runs the record-fidelity suite against a Durable (a Journal, or a store's transitional
// Do and History). open returns a handle on the journal under test; run IDs are unique per call,
// so the suite can run repeatedly against a persistent backend. Run includes it, over a Journal on
// the store under test.
func RunDurable(t *testing.T, open func(t *testing.T) agent.Durable) {
	for _, c := range Cases() {
		t.Run(c.Name, func(t *testing.T) { fidelity(t, open(t), c) })
	}
	t.Run("ReturnedRecordIsACopy", func(t *testing.T) { returnedCopy(t, open(t)) })
	t.Run("Salted", func(t *testing.T) { salted(t, open(t)) })
	t.Run("RecordsAfterCancel", func(t *testing.T) { recordsAfterCancel(t, open(t)) })
	t.Run("StepAttemptSafety", func(t *testing.T) { stepAttemptSafety(t, open(t)) })
}

// Case is one record the suite round-trips. Want, when set, is the Result the store must hand
// back: the record's canonical form, which is the tool's JSON with insignificant whitespace
// removed and every other byte kept.
type Case struct {
	Name   string
	Record agent.Record
	Want   json.RawMessage
}

// Cases returns the records the suite round-trips, for stores that want to run further checks
// (such as comparing the bytes they persisted) on the same inputs.
func Cases() []Case {
	const (
		lineSep = "\xe2\x80\xa8" // U+2028, UTF-8 encoded
		paraSep = "\xe2\x80\xa9" // U+2029, UTF-8 encoded
		esc     = "\x5cu"        // a backslash and u: the prefix of a JSON \uXXXX escape
	)
	msg := agent.Message{Role: agent.RoleAssistant, Parts: []agent.Part{
		agent.Text{Text: "a <b> & c" + lineSep + "d \x00 bad\xffutf8"},
		agent.Reasoning{Text: "think <", Signature: "sig&>"},
		agent.ToolUse{ID: "c1", Name: "t", Args: json.RawMessage(`{ "q" : "a<b && c>d" ,  "n": 1.50 }`)},
		agent.Image{Mime: "image/png", Data: []byte{0, 1, 0xff, '<'}},
	}}
	return []Case{
		{Name: "HTML characters and whitespace",
			Record: agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{ "h" : "<b>a & b</b>",  "cmp": "x > y" }`)},
			Want:   json.RawMessage(`{"h":"<b>a & b</b>","cmp":"x > y"}`)},
		{Name: "key order",
			Record: agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"b":1,"a":2}`)},
			Want:   json.RawMessage(`{"b":1,"a":2}`)},
		{Name: "NUL escape",
			Record: agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`"before\u0000after"`)},
			Want:   json.RawMessage(`"before\u0000after"`)},
		{Name: "line separators",
			Record: agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`"a` + lineSep + `b` + paraSep + `c"`)},
			Want:   json.RawMessage(`"a` + esc + `2028b` + esc + `2029c"`)},
		{Name: "invalid UTF-8",
			Record: agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage("\"a\xffb\xc3\"")},
			Want:   json.RawMessage("\"a\xffb\xc3\"")},
		{Name: "number forms",
			Record: agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`[1.50, 1e20, -0.0, 9007199254740993, 123456789012345678901234567890]`)},
			Want:   json.RawMessage(`[1.50,1e20,-0.0,9007199254740993,123456789012345678901234567890]`)},
		{Name: "escaped HTML stays escaped",
			Record: agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`"` + esc + `003cb` + esc + `003e"`)},
			Want:   json.RawMessage(`"` + esc + `003cb` + esc + `003e"`)},
		{Name: "evidence",
			Record: agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`"ok"`), Reconciled: true, Evidence: json.RawMessage(`{ "log" : "<id=7> & done" }`)}},
		{Name: "read-only tool result",
			Record: agent.Record{Kind: agent.StepToolResult, ToolUseID: "c1", Result: json.RawMessage(`{"n":1}`), ReadOnly: true}},
		{Name: "model message",
			Record: agent.Record{Kind: agent.StepModel, Message: &msg, Usage: &agent.Usage{InputTokens: 3, OutputTokens: 4}}},
		{Name: "model message with discarded usage",
			Record: agent.Record{Kind: agent.StepModel, Message: &msg, Usage: &agent.Usage{InputTokens: 3, OutputTokens: 4},
				DiscardedUsage: &agent.Usage{InputTokens: 5, CacheReadTokens: 6}}},
		{Name: "failed model call spend",
			Record: agent.Record{Kind: agent.StepValue, DiscardedUsage: &agent.Usage{InputTokens: 7, OutputTokens: 8}}},
	}
}

// show renders a record for a failure message, with its raw JSON fields as quoted text.
func show(r agent.Record) string {
	s := fmt.Sprintf("result=%q evidence=%q", string(r.Result), string(r.Evidence))
	if r.Message != nil {
		s += " message="
		for _, p := range r.Message.Parts {
			switch v := p.(type) {
			case agent.ToolUse:
				s += fmt.Sprintf("[tool_use args=%q]", string(v.Args))
			default:
				s += fmt.Sprintf("[%+q]", fmt.Sprintf("%+v", v))
			}
		}
	}
	return s
}

// runIDs numbers the run IDs this process generates, so two are never equal.
var runIDs atomic.Int64

func runID(t *testing.T) string {
	return fmt.Sprintf("storetest-%s-%d-%d", t.Name(), time.Now().UnixNano(), runIDs.Add(1))
}

// history returns runID's records after its journal header, failing the test if the journal does
// not start with one.
func history(t *testing.T, d agent.Durable, runID string) []agent.Record {
	t.Helper()
	hist, err := d.History(context.Background(), runID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) == 0 {
		return nil
	}
	if hist[0].Kind != agent.StepHeader || hist[0].Name != "@journal" || hist[0].Format != agent.JournalFormat {
		t.Fatalf("History starts with %q (%s), want the journal header naming %s", hist[0].Name, hist[0].Kind, agent.JournalFormat)
	}
	return hist[1:]
}

func fidelity(t *testing.T, d agent.Durable, c Case) {
	ctx := context.Background()
	id := runID(t)
	live, err := d.Do(ctx, id, "step", func(context.Context) (agent.Record, error) { return c.Record, nil })
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	memo, err := d.Do(ctx, id, "step", func(context.Context) (agent.Record, error) {
		t.Fatal("a recorded step ran again")
		return agent.Record{}, nil
	})
	if err != nil {
		t.Fatalf("memoized Do: %v", err)
	}
	hist := history(t, d, id)
	if len(hist) != 1 {
		t.Fatalf("History = %d records after the header, want 1", len(hist))
	}
	replay := hist[0]
	if !reflect.DeepEqual(live, replay) {
		t.Fatalf("the live record differs from the replayed one\nlive:   %s\nreplay: %s", show(live), show(replay))
	}
	if !reflect.DeepEqual(memo, replay) {
		t.Fatalf("the memoized record differs from the replayed one\nmemo:   %s\nreplay: %s", show(memo), show(replay))
	}
	if c.Want != nil && !bytes.Equal(live.Result, c.Want) {
		t.Fatalf("Result = %q, want %q", live.Result, c.Want)
	}
	// The replayed record is in canonical form: encoding it again yields the same record, so
	// an audit leaf computed from it is the bytes the store holds.
	enc, err := agent.EncodeRecord(replay)
	if err != nil {
		t.Fatalf("EncodeRecord: %v", err)
	}
	back, err := agent.DecodeRecord(enc)
	if err != nil {
		t.Fatalf("DecodeRecord: %v", err)
	}
	if !reflect.DeepEqual(back, replay) {
		t.Fatalf("the replayed record is not a fixed point of the journal encoding\nreplay: %s\nagain:  %s", show(replay), show(back))
	}
	want, err := agent.EncodeRecord(withSalt(t, agent.Record{Name: "step", Kind: c.Record.Kind, Message: c.Record.Message, Usage: c.Record.Usage, DiscardedUsage: c.Record.DiscardedUsage,
		ToolUseID: c.Record.ToolUseID, Result: c.Record.Result, IsError: c.Record.IsError, Reconciled: c.Record.Reconciled, Evidence: c.Record.Evidence, ReadOnly: c.Record.ReadOnly,
	}, replay.Salt()))
	if err != nil {
		t.Fatalf("EncodeRecord(input): %v", err)
	}
	if !bytes.Equal(enc, want) {
		t.Fatalf("the replayed record encodes to\n%q\nbut the recorded one encodes to\n%q", enc, want)
	}
}

// returnedCopy: a caller that modifies the record Do or History handed it cannot change the
// journal or what another caller reads.
func returnedCopy(t *testing.T, d agent.Durable) {
	ctx := context.Background()
	id := runID(t)
	live, err := d.Do(ctx, id, "step", func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"abc"`)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	live.Result[1] = 'X'
	hist := history(t, d, id)
	if len(hist) != 1 {
		t.Fatalf("History = %d records after the header, want 1", len(hist))
	}
	hist[0].Result[2] = 'Y'
	again := history(t, d, id)
	if len(again) != 1 {
		t.Fatalf("History = %d records after the header, want 1", len(again))
	}
	if string(again[0].Result) != `"abc"` {
		t.Fatalf("the journal holds %s after callers modified their copies, want \"abc\"", again[0].Result)
	}
}

// salted: every record a store journals carries a fresh agent.SaltSize salt (agent.JournalEntry),
// distinct per record and never the one the step returned. The audit trail needs it so that an
// inclusion proof does not let its holder confirm a guessed neighbouring record.
func salted(t *testing.T, d agent.Durable) {
	ctx := context.Background()
	id := runID(t)
	chosen := bytes.Repeat([]byte{7}, agent.SaltSize)
	for _, name := range []string{"a", "b"} {
		if _, err := d.Do(ctx, id, name, func(context.Context) (agent.Record, error) {
			return withSalt(t, agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, chosen), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := d.History(ctx, id)
	if err != nil || len(all) != 3 {
		t.Fatalf("History = %d records, %v; want the header and 2", len(all), err)
	}
	for _, r := range all {
		if len(r.Salt()) != agent.SaltSize || bytes.Equal(r.Salt(), chosen) {
			t.Fatalf("record %q has salt %x, want a fresh %d-byte salt set by the journal", r.Name, r.Salt(), agent.SaltSize)
		}
	}
	if bytes.Equal(all[1].Salt(), all[2].Salt()) || bytes.Equal(all[0].Salt(), all[1].Salt()) {
		t.Fatalf("two records share a salt: %x, %x, %x", all[0].Salt(), all[1].Salt(), all[2].Salt())
	}
}

// recordsAfterCancel: once fn has returned a record, Do journals it even if the caller's context
// was cancelled while fn ran. fn may have fired a side effect (a charge, a sent message) before
// the cancellation arrived, and a driver loses its context whenever its lease lapses or its
// process shuts down; a store that dropped the record then would leave the effect with no
// recorded outcome, so the resumed run halts for a human although the outcome was known.
func recordsAfterCancel(t *testing.T, d agent.Durable) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := runID(t)
	got, err := d.Do(ctx, id, "effect", func(context.Context) (agent.Record, error) {
		cancel() // the driver's context is cancelled while the effect is in flight
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"fired"`)}, nil
	})
	if err != nil {
		t.Fatalf("Do dropped the outcome of a step whose fn returned, because the context was cancelled meanwhile: %v", err)
	}
	if string(got.Result) != `"fired"` {
		t.Fatalf("Do returned %s, want \"fired\"", got.Result)
	}
	if hist := history(t, d, id); len(hist) != 1 || string(hist[0].Result) != `"fired"` {
		t.Fatalf("History = %+v; want the one recorded outcome", hist)
	}
}

// stepAttemptSafety: agent.Step goes by the safety a step was attempted under. A retry-safe step
// looks for an earlier attempt marker without recording one; and a step attempted as a side effect
// halts on resume even when it is declared retry-safe by then.
func stepAttemptSafety(t *testing.T, d agent.Durable) {
	ctx := context.Background()
	id := runID(t)
	safe := agent.StepSafety(agent.Safety{ReadOnly: true})
	if _, err := agent.Step(ctx, d, id, "read", func(context.Context) (int, error) { return 1, nil }, safe); err != nil {
		t.Fatal(err)
	}
	if hist := history(t, d, id); len(hist) != 1 || hist[0].Name != "read" {
		t.Fatalf("History after one retry-safe step = %v; want only the step's record", hist)
	}

	ran := 0
	write := func(context.Context) (int, error) { ran++; return 0, fmt.Errorf("lost the answer") }
	if _, err := agent.Step(ctx, d, id, "write", write); err == nil {
		t.Fatal("the failing step succeeded")
	}
	_, err := agent.Step(ctx, d, id, "write", write, safe)
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) || halt.Op.ID != "write" || halt.AttemptedAt.IsZero() {
		t.Fatalf("resume err = %v, want *OutcomeUnknown for write with its attempt time", err)
	}
	if ran != 1 {
		t.Fatalf("the side-effecting step ran %d times, want 1", ran)
	}
}

// withSalt returns r with its salt replaced by salt: a record no journal writes.
func withSalt(t *testing.T, r agent.Record, salt []byte) agent.Record {
	t.Helper()
	return journalhook.WithSalt(r, salt).(agent.Record)
}
