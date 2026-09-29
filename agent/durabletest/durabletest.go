// Package durabletest checks that an implementation satisfies the agent.Durable contract's
// fidelity guarantee, so every store (in-memory, SQLite, Postgres, or your own) is held to the
// property the agent loop's resume relies on: the record Do returns on the live path is exactly
// the record a replay reads back, for any content a model or tool can produce.
//
// A store that returned the caller's own record live but a decoded copy on replay would let a
// resumed run rebuild a different conversation than the one it was having, so the model would
// read different bytes after a crash than without one. The suite feeds the store records whose
// encoding is easy to get wrong (HTML-significant characters, insignificant whitespace, U+2028,
// NUL, invalid UTF-8, unusual number forms, key order) and requires the live record, the
// memoized record, and the History record to be identical and in the journal's canonical form
// (agent.EncodeRecord).
package durabletest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Run runs the fidelity suite. open returns a handle on the store under test; run IDs are unique
// per call, so the suite can run repeatedly against a persistent backend.
func Run(t *testing.T, open func(t *testing.T) agent.Durable) {
	for _, c := range Cases() {
		t.Run(c.Name, func(t *testing.T) { fidelity(t, open(t), c) })
	}
	t.Run("ReturnedRecordIsACopy", func(t *testing.T) { returnedCopy(t, open(t)) })
	t.Run("Salted", func(t *testing.T) { salted(t, open(t)) })
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
		{Name: "model message",
			Record: agent.Record{Kind: agent.StepModel, Message: &msg, Usage: &agent.Usage{InputTokens: 3, OutputTokens: 4}}},
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

func runID(t *testing.T) string {
	return fmt.Sprintf("durabletest-%s-%d", t.Name(), time.Now().UnixNano())
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
	hist, err := d.History(ctx, id)
	if err != nil || len(hist) != 1 {
		t.Fatalf("History = %d records, %v; want 1", len(hist), err)
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
	want, err := agent.EncodeRecord(agent.Record{Name: "step", Kind: c.Record.Kind, Message: c.Record.Message, Usage: c.Record.Usage,
		ToolUseID: c.Record.ToolUseID, Result: c.Record.Result, IsError: c.Record.IsError, Reconciled: c.Record.Reconciled, Evidence: c.Record.Evidence,
		Salt: replay.Salt})
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
	hist, err := d.History(ctx, id)
	if err != nil || len(hist) != 1 {
		t.Fatalf("History = %d records, %v; want 1", len(hist), err)
	}
	hist[0].Result[2] = 'Y'
	again, err := d.History(ctx, id)
	if err != nil || len(again) != 1 {
		t.Fatalf("History = %d records, %v; want 1", len(again), err)
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
			return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`), Salt: chosen}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := d.History(ctx, id)
	if err != nil || len(hist) != 2 {
		t.Fatalf("History = %d records, %v; want 2", len(hist), err)
	}
	for _, r := range hist {
		if len(r.Salt) != agent.SaltSize || bytes.Equal(r.Salt, chosen) {
			t.Fatalf("record %q has salt %x, want a fresh %d-byte salt set by the store", r.Name, r.Salt, agent.SaltSize)
		}
	}
	if bytes.Equal(hist[0].Salt, hist[1].Salt) {
		t.Fatalf("two records share the salt %x", hist[0].Salt)
	}
}
