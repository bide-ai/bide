package gemini

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

// Gemini's finish reasons are mapped onto the neutral ones where they enter. A turn cut off at
// the token limit is "length" even when it made a tool call, and a turn a safety or recitation
// filter stopped is "filtered": neither is taken for a finished answer.
func TestStreamSSE_FinishReasonsAreMapped(t *testing.T) {
	text := func(r string) string {
		return "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]},\"finishReason\":\"" + r + "\"}]}\n\n"
	}
	call := func(r string) string {
		return "data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"f\",\"args\":{}}}]},\"finishReason\":\"" + r + "\"}]}\n\n"
	}
	for name, tc := range map[string]struct {
		src, reason string
		err         error
	}{
		"STOP":                    {text("STOP"), "stop", nil},
		"STOP with a call":        {call("STOP"), "tool_use", nil},
		"MAX_TOKENS":              {text("MAX_TOKENS"), "length", agent.ErrModel},
		"MAX_TOKENS, a call":      {call("MAX_TOKENS"), "length", agent.ErrModel},
		"SAFETY":                  {text("SAFETY"), "filtered", agent.ErrModel},
		"RECITATION":              {text("RECITATION"), "filtered", agent.ErrModel},
		"BLOCKLIST":               {text("BLOCKLIST"), "filtered", agent.ErrModel},
		"PROHIBITED_CONTENT":      {text("PROHIBITED_CONTENT"), "filtered", agent.ErrModel},
		"SPII":                    {text("SPII"), "filtered", agent.ErrModel},
		"IMAGE_SAFETY":            {text("IMAGE_SAFETY"), "filtered", agent.ErrModel},
		"MALFORMED_FUNCTION_CALL": {text("MALFORMED_FUNCTION_CALL"), "MALFORMED_FUNCTION_CALL", agent.ErrStreamProtocol},
		"OTHER":                   {text("OTHER"), "OTHER", agent.ErrStreamProtocol},
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
