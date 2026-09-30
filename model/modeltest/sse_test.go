package modeltest

import (
	"fmt"
	"io"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// fatalTB records the first Fatalf and stops the check, as testing.T does, without failing the
// test that runs it.
type fatalTB struct {
	testing.TB
	msg string
}

type stopped struct{}

func (f *fatalTB) Helper() {}
func (f *fatalTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	panic(stopped{})
}

// failsOn reports the message check produced by calling Fatalf, or "" if it passed.
func failsOn(t *testing.T, check func(testing.TB)) (msg string) {
	t.Helper()
	tb := &fatalTB{TB: t}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(stopped); !ok {
				panic(r)
			}
			msg = tb.msg
		}
	}()
	check(tb)
	return ""
}

// CheckFinish accepts a neutral reason, and a provider's own reason passed through with Raw, and
// refuses an empty reason, a non-neutral reason that is not Raw, and discarded usage.
func TestCheckFinish(t *testing.T) {
	for name, tc := range map[string]struct {
		f    agent.Finish
		fail bool
	}{
		"stop":                 {agent.Finish{Reason: agent.FinishStop, Raw: "end_turn"}, false},
		"stop without a raw":   {agent.Finish{Reason: agent.FinishStop}, false},
		"tool_use":             {agent.Finish{Reason: agent.FinishToolUse, Raw: "tool_calls"}, false},
		"length":               {agent.Finish{Reason: agent.FinishLength, Raw: "MAX_TOKENS"}, false},
		"filtered":             {agent.Finish{Reason: agent.FinishFiltered, Raw: "refusal"}, false},
		"passed through":       {agent.Finish{Reason: "pause_turn", Raw: "pause_turn"}, false},
		"empty":                {agent.Finish{Raw: "end_turn"}, true},
		"empty with no raw":    {agent.Finish{}, true},
		"passed through wrong": {agent.Finish{Reason: "pause_turn", Raw: "PAUSE"}, true},
		"discarded":            {agent.Finish{Reason: agent.FinishStop, Discarded: agent.Usage{OutputTokens: 1}}, true},
	} {
		msg := failsOn(t, func(tb testing.TB) { CheckFinish(tb, tc.f) })
		if (msg != "") != tc.fail {
			t.Errorf("%s: CheckFinish(%+v) failed %v (%q), want %v", name, tc.f, msg != "", msg, tc.fail)
		}
	}
}

// ReadSSE applies CheckFinish to the Finish a reader sends.
func TestReadSSE_RefusesAnEmptyReason(t *testing.T) {
	reader := func(reason agent.FinishReason) func(io.ReadCloser, func(agent.Emit) bool) {
		return func(body io.ReadCloser, send func(agent.Emit) bool) {
			body.Close()
			send(agent.Emit{Event: agent.TextDelta{Text: "hi"}})
			send(agent.Emit{Event: agent.Finish{Reason: reason}})
		}
	}
	if msg := failsOn(t, func(tb testing.TB) { ReadSSE(tb, reader(""), nil) }); msg == "" {
		t.Error("ReadSSE accepted a Finish with an empty reason")
	}
	if msg := failsOn(t, func(tb testing.TB) { ReadSSE(tb, reader(agent.FinishStop), nil) }); msg != "" {
		t.Errorf("ReadSSE refused a stop: %s", msg)
	}
}
