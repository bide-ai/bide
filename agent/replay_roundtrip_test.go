package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

// buildFrom feeds emits through msgBuilder, the same path Stream.Message takes.
func buildFrom(t *testing.T, evs []Emit) Message {
	t.Helper()
	var b msgBuilder
	for _, e := range evs {
		if e.Err != nil {
			t.Fatalf("emit carries an error: %v", e.Err)
		}
		b.add(e.Event)
	}
	msg, err := b.finalize()
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	return msg
}

// reasoningKind is one shape of Reasoning part msgBuilder can produce.
type reasoningKind int

const (
	rkSigned       reasoningKind = iota // text + signature
	rkSignedNoText                      // signature only
	rkRedacted                          // redacted_thinking data
	rkUnsigned                          // text, no signature (a stream that ended mid-block)
	rkEmpty                             // no text, no signature
	numReasoningKinds
)

func (k reasoningKind) part(i int) Reasoning {
	switch k {
	case rkSigned:
		return Reasoning{Text: fmt.Sprintf("think-%d", i), Signature: fmt.Sprintf("sig-%d", i)}
	case rkSignedNoText:
		return Reasoning{Signature: fmt.Sprintf("sig-%d", i)}
	case rkRedacted:
		return Reasoning{Redacted: fmt.Sprintf("opaque-%d", i)}
	case rkUnsigned:
		return Reasoning{Text: fmt.Sprintf("think-%d", i)}
	default:
		return Reasoning{}
	}
}

// closed reports whether a block of this kind is closed by the event stream itself. An
// unsigned block (rkUnsigned, rkEmpty) stays open until a redacted block or the end of the
// stream closes it: the stream has no event that ends it otherwise, so a following text or
// signed block merges into it. msgBuilder therefore never produces an unsigned block that is
// followed by anything but a redacted block, and those layouts cannot round-trip by design.
func (k reasoningKind) closed() bool { return k == rkSigned || k == rkSignedNoText || k == rkRedacted }

// reasoningLayouts enumerates every sequence of up to maxLen reasoning kinds that msgBuilder
// can produce: an unsigned block is last or directly followed by a redacted block.
func reasoningLayouts(maxLen int) [][]reasoningKind {
	out := [][]reasoningKind{nil}
	frontier := [][]reasoningKind{nil}
	for n := 1; n <= maxLen; n++ {
		var next [][]reasoningKind
		for _, prefix := range frontier {
			for k := range numReasoningKinds {
				if len(prefix) > 0 && !prefix[len(prefix)-1].closed() && k != rkRedacted {
					continue
				}
				seq := append(append([]reasoningKind(nil), prefix...), k)
				next = append(next, seq)
			}
		}
		out = append(out, next...)
		frontier = next
	}
	return out
}

// toolLayouts covers no call, calls with and without args, and calls with and without a
// per-call signature (Gemini thoughtSignature).
func toolLayouts() [][]ToolUse {
	a := ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`)}
	aSig := ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`), Signature: "tsig-1"}
	b := ToolUse{ID: "c2", Name: "noargs"}
	bSig := ToolUse{ID: "c2", Name: "noargs", Signature: "tsig-2"}
	return [][]ToolUse{nil, {a}, {aSig}, {b}, {bSig}, {a, bSig}, {aSig, b}, {aSig, bSig}}
}

// canonicalMessages is every assistant message in msgBuilder's canonical layout (reasoning
// blocks, then at most one text, then tool calls) over the enumerated shapes.
func canonicalMessages() []Message {
	var out []Message
	for _, rs := range reasoningLayouts(3) {
		for _, text := range []string{"", "hi"} {
			for _, calls := range toolLayouts() {
				var parts []Part
				for i, k := range rs {
					parts = append(parts, k.part(i))
				}
				if text != "" {
					parts = append(parts, Text{Text: text})
				}
				for _, c := range calls {
					parts = append(parts, c)
				}
				out = append(out, Message{Role: RoleAssistant, Parts: parts})
			}
		}
	}
	return out
}

// TestEmitsFor_RoundTripsThroughBuilder: emitsFor is the inverse of msgBuilder, so every
// message the builder can produce comes back unchanged when its replayed events are
// assembled again. A replayed turn must look to a stream consumer exactly as it did live.
func TestEmitsFor_RoundTripsThroughBuilder(t *testing.T) {
	msgs := canonicalMessages()
	if len(msgs) < 1000 {
		t.Fatalf("enumerated only %d messages; the generator lost coverage", len(msgs))
	}
	fails := 0
	for _, want := range msgs {
		got := buildFrom(t, emitsFor(want, Usage{}))
		if !reflect.DeepEqual(got, want) {
			fails++
			if fails <= 5 {
				t.Errorf("round trip changed the message:\n want %#v\n  got %#v", want.Parts, got.Parts)
			}
		}
	}
	if fails > 0 {
		t.Fatalf("%d of %d messages did not round-trip", fails, len(msgs))
	}
}

// TestEmitsFor_NamedLayouts pins the layouts that motivated the property, so a failure names
// the shape that broke.
func TestEmitsFor_NamedLayouts(t *testing.T) {
	cases := map[string][]Part{
		"redacted then text": {Reasoning{Redacted: "opaque"}, Text{Text: "hi"}},
		"two signed blocks": {
			Reasoning{Text: "a", Signature: "s1"}, Reasoning{Text: "b", Signature: "s2"}, Text{Text: "hi"},
		},
		"redacted between signed blocks": {
			Reasoning{Text: "a", Signature: "s1"}, Reasoning{Redacted: "opaque"},
			Reasoning{Text: "b", Signature: "s2"}, Text{Text: "hi"},
		},
		"unsigned block closed by redacted": {Reasoning{Text: "a"}, Reasoning{Redacted: "opaque"}, Text{Text: "hi"}},
		"unsigned block last":               {Reasoning{Text: "a"}, Text{Text: "hi"}},
		"empty unsigned block":              {Reasoning{}, Text{Text: "hi"}},
		"tool call signature": {
			Text{Text: "hi"}, ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{}`), Signature: "tsig"},
		},
	}
	for name, parts := range cases {
		t.Run(name, func(t *testing.T) {
			want := Message{Role: RoleAssistant, Parts: parts}
			if got := buildFrom(t, emitsFor(want, Usage{})); !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip changed the message:\n want %#v\n  got %#v", want.Parts, got.Parts)
			}
		})
	}
}

// TestEmitsFor_UnsignedBlockMergesByDesign pins the layouts the property excludes. The
// stream has no event that closes an unsigned thinking block, so replaying one followed by
// another thinking block merges the two. msgBuilder never produces these layouts; if it ever
// does, this test and the exclusion in reasoningKind.closed must be revisited.
func TestEmitsFor_UnsignedBlockMergesByDesign(t *testing.T) {
	cases := map[string]struct{ in, want []Part }{
		"unsigned then signed": {
			in:   []Part{Reasoning{Text: "a"}, Reasoning{Text: "b", Signature: "s"}},
			want: []Part{Reasoning{Text: "ab", Signature: "s"}},
		},
		"unsigned then unsigned": {
			in:   []Part{Reasoning{Text: "a"}, Reasoning{Text: "b"}},
			want: []Part{Reasoning{Text: "ab"}},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := buildFrom(t, emitsFor(Message{Role: RoleAssistant, Parts: c.in}, Usage{}))
			if want := (Message{Role: RoleAssistant, Parts: c.want}); !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, want %#v", got.Parts, want.Parts)
			}
		})
	}
}

// TestReplay_StreamKeepsRedactedReasoningAndCallSignature drives the public replay path: a
// run recorded with a redacted thinking block and a signed tool call is replayed with Replay,
// and both the replay model's stream and the replayed run's journal carry the same message.
func TestReplay_StreamKeepsRedactedReasoningAndCallSignature(t *testing.T) {
	ctx := context.Background()
	turn1 := []Emit{
		{Event: ReasoningDelta{Text: "plan", Signature: "s1"}},
		{Event: ReasoningDelta{Redacted: "opaque"}},
		{Event: ToolCallDelta{Index: 0, ID: "c1", Name: "lookup", ArgsFragment: json.RawMessage(`{"q":"x"}`), Signature: "tsig"}},
		{Event: Finish{Reason: "tool_use"}},
	}
	wantTurn1 := Message{Role: RoleAssistant, Parts: []Part{
		Reasoning{Text: "plan", Signature: "s1"},
		Reasoning{Redacted: "opaque"},
		ToolUse{ID: "c1", Name: "lookup", Args: json.RawMessage(`{"q":"x"}`), Signature: "tsig"},
	}}

	rec := NewMemStore()
	var calls1 int
	tool1 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls1}
	if _, err := New(&scriptModel{turns: [][]Emit{turn1, textTurn("done")}}, rec, tool1).Run(ctx, "orig", "go"); err != nil {
		t.Fatal(err)
	}
	if got := modelMessages(t, rec, "orig"); len(got) == 0 || !reflect.DeepEqual(got[0], wantTurn1) {
		t.Fatalf("recorded turn = %#v, want %#v", got, wantTurn1)
	}

	// The replay model's stream assembles to the recorded message.
	rm, err := Replay(ctx, rec, "orig")
	if err != nil {
		t.Fatal(err)
	}
	s, err := rm.Stream(ctx, Request{})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Message()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantTurn1) {
		t.Fatalf("replayed stream message:\n want %#v\n  got %#v", wantTurn1.Parts, got.Parts)
	}

	// A full replayed run journals the same model turns as the original.
	rm, err = Replay(ctx, rec, "orig")
	if err != nil {
		t.Fatal(err)
	}
	var calls2 int
	tool2 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls2}
	fresh := NewMemStore()
	if _, err := New(rm, fresh, tool2).Run(ctx, "replay", "go"); err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if a, b := modelMessages(t, rec, "orig"), modelMessages(t, fresh, "replay"); !reflect.DeepEqual(a, b) {
		t.Fatalf("replayed run journaled different model turns:\n orig   %#v\n replay %#v", a, b)
	}
}

func modelMessages(t *testing.T, d Durable, runID string) []Message {
	t.Helper()
	recs, err := d.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []Message
	for _, r := range recs {
		if r.Kind == StepModel && r.Message != nil {
			out = append(out, *r.Message)
		}
	}
	return out
}
