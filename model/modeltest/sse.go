package modeltest

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// SSEResult is what an adapter's SSE reader made of one response body: every Emit it sent, in
// order, and the message, usage, and error a consumer reading them gets.
type SSEResult struct {
	Emits []agent.Emit
	Msg   agent.Message
	Usage agent.Usage
	Err   error
}

// ReadSSE runs an adapter's SSE reader (streamSSE, which reads body and sends each event through
// send) over body and checks the properties every response must keep, whatever its bytes:
//
//   - the reader sends at most one Finish, and nothing after it (the adapter ends the turn once;
//     agent.Stream would otherwise report agent.ErrStreamProtocol);
//   - every error it sends is its last Emit;
//   - a failed response fails with an agent.ErrModel (or agent.ErrTruncatedToolArgs, which the
//     message assembly reports for tool arguments that are not complete JSON).
func ReadSSE(t testing.TB, streamSSE func(body io.ReadCloser, send func(agent.Emit) bool), body []byte) SSEResult {
	t.Helper()
	var res SSEResult
	streamSSE(io.NopCloser(bytes.NewReader(body)), func(e agent.Emit) bool {
		res.Emits = append(res.Emits, e)
		return true
	})
	finished := false
	for i, e := range res.Emits {
		if finished {
			t.Fatalf("event %d (%#v) after the Finish; body %q", i, e, body)
		}
		if e.Err != nil && i != len(res.Emits)-1 {
			t.Fatalf("error %v is not the last event; body %q", e.Err, body)
		}
		if _, ok := e.Event.(agent.Finish); ok {
			finished = true
		}
	}
	ch := make(chan agent.Emit, len(res.Emits))
	for _, e := range res.Emits {
		ch <- e
	}
	close(ch)
	res.Msg, res.Usage, res.Err = agent.NewStream(ch).Message()
	if res.Err != nil && !errors.Is(res.Err, agent.ErrModel) && !errors.Is(res.Err, agent.ErrTruncatedToolArgs) {
		t.Fatalf("stream error %v (%T) is not an agent.ErrModel; body %q", res.Err, res.Err, body)
	}
	return res
}

// CheckSSEPrefix checks, on top of ReadSSE's properties for body and for its first cut bytes, that
// a response cut short is never a success that differs from the whole response's success: a
// connection that closes partway must not turn a truncated answer into a complete one. A fuzz
// target for an adapter's SSE reader calls it with arbitrary bytes and an arbitrary cut.
func CheckSSEPrefix(t testing.TB, streamSSE func(body io.ReadCloser, send func(agent.Emit) bool), body []byte, cut int) {
	t.Helper()
	full := ReadSSE(t, streamSSE, body)
	cut = min(max(cut, 0), len(body))
	pre := ReadSSE(t, streamSSE, body[:cut])
	if full.Err == nil && pre.Err == nil && !reflect.DeepEqual(full.Msg, pre.Msg) {
		t.Fatalf("the response cut at byte %d succeeded with a different message:\nwhole: %+v\ncut:   %+v\nbody %q", cut, full.Msg, pre.Msg, body)
	}
}
