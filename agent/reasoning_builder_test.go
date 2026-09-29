package agent

import (
	"reflect"
	"testing"
)

// Each thinking block becomes its own Reasoning part, in order: a signature ends a block, a
// redacted block stands alone and ends the one before it, and reasoning that never gets a
// signature (OpenAI-compatible reasoning_content) is kept as one part.
func TestMessage_ReasoningBlocks(t *testing.T) {
	for name, tc := range map[string]struct {
		events []Event
		want   []Part
	}{
		"unsigned": {
			[]Event{ReasoningDelta{Text: "a"}, ReasoningDelta{Text: "b"}, TextDelta{Text: "x"}},
			[]Part{Reasoning{Text: "ab"}, Text{Text: "x"}},
		},
		"signed blocks": {
			[]Event{ReasoningDelta{Text: "a"}, ReasoningDelta{Signature: "s1"}, ReasoningDelta{Text: "b"}, ReasoningDelta{Signature: "s2"}},
			[]Part{Reasoning{Text: "a", Signature: "s1"}, Reasoning{Text: "b", Signature: "s2"}},
		},
		"redacted between": {
			[]Event{ReasoningDelta{Text: "a"}, ReasoningDelta{Redacted: "R"}, ReasoningDelta{Text: "b"}},
			[]Part{Reasoning{Text: "a"}, Reasoning{Redacted: "R"}, Reasoning{Text: "b"}},
		},
	} {
		ch := make(chan Emit, len(tc.events)+1)
		for _, e := range tc.events {
			ch <- Emit{Event: e}
		}
		ch <- Emit{Event: Finish{}}
		close(ch)
		msg, _, err := NewStream(ch).Message()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(msg.Parts, tc.want) {
			t.Errorf("%s: parts = %+v, want %+v", name, msg.Parts, tc.want)
		}
	}
}
