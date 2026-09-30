package openai

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// finishOf streams src and returns the Finish the adapter reports and the error Message returns.
func finishOf(src string) (agent.Finish, error) {
	var fin agent.Finish
	for ev, err := range testStream(src).Events() {
		if err != nil {
			break
		}
		if f, ok := ev.(agent.Finish); ok {
			fin = f
		}
	}
	_, _, err := testStream(src).Message()
	return fin, err
}

// OpenAI's finish reasons are mapped onto the neutral ones where they enter, so a turn cut off
// at the token limit or by the content filter is never taken for a finished answer, and the
// Finish keeps OpenAI's own value in Raw. The reason is never empty: a stream that ends with
// [DONE] and no finish_reason (some compatible servers omit it) is a natural stop.
func TestStreamSSE_FinishReasonsAreMapped(t *testing.T) {
	text := func(r string) string {
		return "data: {\"choices\":[{\"delta\":{\"content\":\"The total is\"},\"finish_reason\":\"" + r + "\"}]}\n\ndata: [DONE]\n\n"
	}
	call := func(r string) string {
		return "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"" + r + "\"}]}\n\ndata: [DONE]\n\n"
	}
	for name, tc := range map[string]struct {
		src    string
		reason agent.FinishReason
		raw    string
		err    error
	}{
		"stop":               {text("stop"), agent.FinishStop, "stop", nil},
		"tool_calls":         {call("tool_calls"), agent.FinishToolUse, "tool_calls", nil},
		"function_call":      {call("function_call"), agent.FinishToolUse, "function_call", nil},
		"length":             {text("length"), agent.FinishLength, "length", agent.ErrOutputTruncated},
		"content_filter":     {text("content_filter"), agent.FinishFiltered, "content_filter", agent.ErrOutputFiltered},
		"unknown":            {text("something_new"), "something_new", "something_new", agent.ErrStreamProtocol},
		"no finish_reason":   {"data: {\"choices\":[{\"delta\":{\"content\":\"The total is\"}}]}\n\ndata: [DONE]\n\n", agent.FinishStop, "", nil},
		"null finish_reason": {"data: {\"choices\":[{\"delta\":{\"content\":\"The total is\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n", agent.FinishStop, "", nil},
	} {
		f, err := finishOf(tc.src)
		if f.Reason != tc.reason || f.Raw != tc.raw {
			t.Errorf("%s: Finish reason %q raw %q, want %q and %q", name, f.Reason, f.Raw, tc.reason, tc.raw)
		}
		if f.Discarded != (agent.Usage{}) {
			t.Errorf("%s: Finish.Discarded = %+v, want zero from a live adapter", name, f.Discarded)
		}
		if tc.err == nil && err != nil || tc.err != nil && !errors.Is(err, tc.err) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.err)
		}
	}
}

// Under a forced tool_choice OpenAI can end a turn that calls a tool with finish_reason "stop":
// the call is kept, since the content decides. "tool_calls" with no call lost the calls it was
// for, so it is not an answer.
func TestStreamSSE_ReasonAndCallsDisagree(t *testing.T) {
	stopWithCall := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	msg, _, err := testStream(stopWithCall).Message()
	if err != nil || len(msg.Parts) != 1 {
		t.Errorf("stop with a call: %+v, %v; want the call", msg, err)
	} else if tu, ok := msg.Parts[0].(agent.ToolUse); !ok || tu.ID != "c1" {
		t.Errorf("stop with a call: part %+v, want the call c1", msg.Parts[0])
	}
	callsWithoutCall := "data: {\"choices\":[{\"delta\":{\"content\":\"Let me check.\"},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	if _, _, err := testStream(callsWithoutCall).Message(); !errors.Is(err, agent.ErrStreamProtocol) {
		t.Errorf("tool_calls with no call: err %v, want ErrStreamProtocol", err)
	}
}
