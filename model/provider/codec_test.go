package provider

import (
	"encoding/json"
	"errors"
	"testing"
)

type upperCodec struct{ err error }

func (c upperCodec) EncodeToolResult(raw json.RawMessage) (string, error) {
	if c.err != nil {
		return "partial", c.err
	}
	return "ENCODED:" + string(raw), nil
}

// EncodeToolResultOr uses the codec when it succeeds, and the canonical JSON when there is no
// codec or the codec fails, so a codec error never drops a tool result.
func TestEncodeToolResultOr(t *testing.T) {
	raw := json.RawMessage(`{"a":1}`)
	for name, tc := range map[string]struct {
		c    ToolResultCodec
		want string
	}{
		"nil codec":    {nil, `{"a":1}`},
		"json codec":   {JSONToolResultCodec{}, `{"a":1}`},
		"codec":        {upperCodec{}, `ENCODED:{"a":1}`},
		"failed codec": {upperCodec{err: errors.New("boom")}, `{"a":1}`},
	} {
		if got := EncodeToolResultOr(tc.c, raw); got != tc.want {
			t.Errorf("%s: EncodeToolResultOr = %q, want %q", name, got, tc.want)
		}
	}
}
