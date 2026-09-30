package openai

import (
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func reasonOf(src string) (string, error) {
	var reason string
	for ev, err := range testStream(src).Events() {
		if err != nil {
			break
		}
		if f, ok := ev.(agent.Finish); ok {
			reason = f.Reason
		}
	}
	_, _, err := testStream(src).Message()
	return reason, err
}

// OpenAI's finish reasons are mapped onto the neutral ones where they enter, so a turn cut off
// at the token limit or by the content filter is never taken for a finished answer.
func TestStreamSSE_FinishReasonsAreMapped(t *testing.T) {
	text := func(r string) string {
		return "data: {\"choices\":[{\"delta\":{\"content\":\"The total is\"},\"finish_reason\":\"" + r + "\"}]}\n\ndata: [DONE]\n\n"
	}
	call := func(r string) string {
		return "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"" + r + "\"}]}\n\ndata: [DONE]\n\n"
	}
	for name, tc := range map[string]struct {
		src, reason string
		err         error
	}{
		"stop":           {text("stop"), "stop", nil},
		"tool_calls":     {call("tool_calls"), "tool_use", nil},
		"function_call":  {call("function_call"), "tool_use", nil},
		"length":         {text("length"), "length", agent.ErrOutputTruncated},
		"content_filter": {text("content_filter"), "filtered", agent.ErrOutputFiltered},
		"unknown":        {text("something_new"), "something_new", agent.ErrStreamProtocol},
	} {
		reason, err := reasonOf(tc.src)
		if reason != tc.reason {
			t.Errorf("%s: Finish.Reason = %q, want %q", name, reason, tc.reason)
		}
		if tc.err == nil && err != nil || tc.err != nil && !errors.Is(err, tc.err) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.err)
		}
	}
}
