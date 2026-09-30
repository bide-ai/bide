package agent

import (
	"errors"
	"strings"
	"testing"
)

func deltas(evs ...Event) *Stream {
	ch := make(chan Emit, len(evs)+1)
	for _, e := range evs {
		ch <- Emit{Event: e}
	}
	ch <- Emit{Event: Finish{Reason: FinishToolUse}}
	close(ch)
	return NewStream(ch)
}

// A tool call's ID and name are set once, by the fragment that starts it. A later fragment for the
// same index that names a different ID or tool is a second call arriving under the first one's
// index (a block started twice, or a server that omits the index), not more of the first: merged,
// one call would be dropped or run with the other's arguments.
func TestStream_ToolCallIdentityIsSetOnce(t *testing.T) {
	for name, s := range map[string]*Stream{
		"new id": deltas(
			ToolCallDelta{Index: 0, ID: "a", Name: "refund"},
			ToolCallDelta{Index: 0, ID: "b", Name: "refund", ArgsFragment: []byte(`{"amount":5}`)}),
		"new name": deltas(
			ToolCallDelta{Index: 0, ID: "a", Name: "refund"},
			ToolCallDelta{Index: 0, Name: "charge", ArgsFragment: []byte(`{}`)}),
	} {
		msg, _, err := s.Message()
		if !errors.Is(err, ErrStreamProtocol) {
			t.Errorf("%s: got %+v, %v; want ErrStreamProtocol", name, msg.toolUses(), err)
		}
	}
}

// Fragments that repeat the call's own ID and name, or carry neither, continue it.
func TestStream_ToolCallFragmentsContinueTheCall(t *testing.T) {
	msg, _, err := deltas(
		ToolCallDelta{Index: 0, ID: "a", Name: "refund", ArgsFragment: []byte(`{"amount":`)},
		ToolCallDelta{Index: 0, ID: "a", Name: "refund", ArgsFragment: []byte(`5`)},
		ToolCallDelta{Index: 0, ArgsFragment: []byte(`}`)}).Message()
	if err != nil {
		t.Fatal(err)
	}
	if uses := msg.toolUses(); len(uses) != 1 || uses[0].ID != "a" || string(uses[0].Args) != `{"amount":5}` {
		t.Fatalf("calls = %+v", uses)
	}
}

// The error names the first fragment that broke the framing.
func TestStream_ToolCallIdentityErrorNamesTheFirst(t *testing.T) {
	_, _, err := deltas(
		ToolCallDelta{Index: 0, ID: "a", Name: "refund"},
		ToolCallDelta{Index: 0, ID: "first-intruder"},
		ToolCallDelta{Index: 0, ID: "second-intruder"}).Message()
	if !errors.Is(err, ErrStreamProtocol) || !strings.Contains(err.Error(), "first-intruder") {
		t.Fatalf("err = %v, want ErrStreamProtocol naming the first intruder", err)
	}
}
