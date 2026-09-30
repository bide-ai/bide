package gemini

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

// Gemini's finish reasons are mapped onto the neutral ones where they enter, and the Finish keeps
// Gemini's own value in Raw. A turn cut off at the token limit is "length" even when it made a
// tool call, and a turn a safety or recitation filter stopped is "filtered": neither is taken for
// a finished answer.
func TestStreamSSE_FinishReasonsAreMapped(t *testing.T) {
	text := func(r string) string {
		return "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]},\"finishReason\":\"" + r + "\"}]}\n\n"
	}
	call := func(r string) string {
		return "data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"f\",\"args\":{}}}]},\"finishReason\":\"" + r + "\"}]}\n\n"
	}
	for name, tc := range map[string]struct {
		src    string
		reason agent.FinishReason
		raw    string
		err    error
	}{
		"STOP":                    {text("STOP"), agent.FinishStop, "STOP", nil},
		"STOP with a call":        {call("STOP"), agent.FinishToolUse, "STOP", nil},
		"MAX_TOKENS":              {text("MAX_TOKENS"), agent.FinishLength, "MAX_TOKENS", agent.ErrOutputTruncated},
		"MAX_TOKENS, a call":      {call("MAX_TOKENS"), agent.FinishLength, "MAX_TOKENS", agent.ErrOutputTruncated},
		"SAFETY":                  {text("SAFETY"), agent.FinishFiltered, "SAFETY", agent.ErrOutputFiltered},
		"RECITATION":              {text("RECITATION"), agent.FinishFiltered, "RECITATION", agent.ErrOutputFiltered},
		"BLOCKLIST":               {text("BLOCKLIST"), agent.FinishFiltered, "BLOCKLIST", agent.ErrOutputFiltered},
		"PROHIBITED_CONTENT":      {text("PROHIBITED_CONTENT"), agent.FinishFiltered, "PROHIBITED_CONTENT", agent.ErrOutputFiltered},
		"SPII":                    {text("SPII"), agent.FinishFiltered, "SPII", agent.ErrOutputFiltered},
		"IMAGE_SAFETY":            {text("IMAGE_SAFETY"), agent.FinishFiltered, "IMAGE_SAFETY", agent.ErrOutputFiltered},
		"MALFORMED_FUNCTION_CALL": {text("MALFORMED_FUNCTION_CALL"), "MALFORMED_FUNCTION_CALL", "MALFORMED_FUNCTION_CALL", agent.ErrStreamProtocol},
		"OTHER":                   {text("OTHER"), "OTHER", "OTHER", agent.ErrStreamProtocol},
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

// A turn with no finishReason is not an answer: the stream stopped partway (ErrIncompleteResponse),
// and the mapping never turns an empty or unspecified reason into a natural stop.
func TestStreamSSE_NoReasonIsNotAnAnswer(t *testing.T) {
	for _, saw := range []bool{false, true} {
		switch r := mapFinishReason("", saw); r {
		case "", agent.FinishStop, agent.FinishToolUse:
			t.Errorf("mapFinishReason(\"\", %v) = %q, which the core takes as a finished turn", saw, r)
		}
	}
	noReason := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n"
	if _, _, err := testStream(noReason).Message(); !errors.Is(err, agent.ErrIncompleteResponse) {
		t.Errorf("no finishReason: err %v, want ErrIncompleteResponse", err)
	}
	emptyReason := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]},\"finishReason\":\"\"}]}\n\n"
	if _, _, err := testStream(emptyReason).Message(); !errors.Is(err, agent.ErrIncompleteResponse) {
		t.Errorf("empty finishReason: err %v, want ErrIncompleteResponse", err)
	}
	unspecified := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]},\"finishReason\":\"FINISH_REASON_UNSPECIFIED\"}]}\n\n"
	if _, _, err := testStream(unspecified).Message(); !errors.Is(err, agent.ErrStreamProtocol) {
		t.Errorf("FINISH_REASON_UNSPECIFIED: err %v, want ErrStreamProtocol", err)
	}
}
