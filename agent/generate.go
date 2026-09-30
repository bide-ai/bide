package agent

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
)

// generate asks the model for turn call.Turn of run call.RunID through the agent's middleware
// chain (see callChain). ts is the turn's state: its request counter, the run's spend meter, the
// run's journal and, for a streaming run, the turn's sink.
func (a *Agent) generate(ctx context.Context, call ModelCall, usedIDs map[string]bool, ts *turnState) (ModelResponse, error) {
	return callChain(ctx, a.mw, call, ts, usedIDs)
}

// newTurnSink returns the sink of streaming turn seq, which forwards to fire.
func newTurnSink(seq int, fire func(AgentEvent)) *turnSink { return &turnSink{seq: seq, fire: fire} }

// turnState is one model turn's state, shared by every request the turn sends. Middleware cannot
// reach it: a ModelCall carries it unexported, and the agent's model handler accepts only a call
// that carries its own turn's.
type turnState struct {
	attempts atomic.Int64 // requests numbered so far
	meter    *spendMeter  // every request's usage, for the run's budget and Result.Spend
	journal  Durable      // the run's journal, for middleware that records a step (WithRetrieval); nil outside a run
	sink     *turnSink    // the live token stream (Agent.Stream); nil when not streaming
}

// callChain sends call through mws (first = outermost) to the model handler of turn ts, and
// checks the response the chain returns. usedIDs holds the tool-use IDs already in the run's
// conversation (see checkToolUseIDs).
func callChain(ctx context.Context, mws []Middleware, call ModelCall, ts *turnState, usedIDs map[string]bool) (ModelResponse, error) {
	h := clipped(baseHandler(ts, usedIDs))
	for i := len(mws) - 1; i >= 0; i-- {
		h = clipped(mws[i](h))
	}
	call.turn = ts
	resp, err := h(ctx, call)
	if err == nil {
		// The response the chain returns is checked again, since a middleware can return one it did
		// not get from the handler it wraps (a fallback, a cache), and that response is what the
		// run records.
		resp, err = checkResponse(resp, usedIDs)
	}
	if err != nil {
		ts.sink.close()
		return ModelResponse{}, err
	}
	ts.sink.finish(resp)
	return resp, nil
}

// clipped wraps h so that every call reaches it with Request.Messages and Request.Tools clipped
// to their length: an append by h, or by anything h calls, allocates, rather than write into
// spare capacity that the caller, or a sibling handler given the same call, also holds.
func clipped(h ModelHandler) ModelHandler {
	return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
		call.Request.Messages = slices.Clip(call.Request.Messages)
		call.Request.Tools = slices.Clip(call.Request.Tools)
		return h(ctx, call)
	}
}

// errForeignCall is the model handler's refusal of a ModelCall that does not carry its turn's
// state: one a middleware built instead of deriving it from the call it received.
var errForeignCall = fmt.Errorf("agent: a ModelCall reached the model handler without its turn's state: a middleware passed on a ModelCall it built instead of a copy of the one it received: %w", ErrConfig)

// baseHandler is the innermost handler of turn ts: it sends each request of the turn. It numbers
// the request, runs the call's hooks around it, streams it to the turn's sink if it holds the
// sink's claim, adds its usage to the turn's meter (not a hook, so no middleware can remove it),
// and checks the response, below middleware, so a retry middleware sees the fault.
func baseHandler(ts *turnState, usedIDs map[string]bool) ModelHandler {
	return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
		if call.turn != ts {
			return ModelResponse{}, errForeignCall
		}
		if call.Model == nil {
			return ModelResponse{}, fmt.Errorf("agent: a ModelCall reached the model handler with a nil Model: %w", ErrConfig)
		}
		call.attempt = int(ts.attempts.Add(1))
		ran := 0 // hooks whose Before returned nil
		var err error
		for _, h := range call.hooks {
			if h.Before != nil {
				if err = h.Before(ctx, call); err != nil {
					break
				}
			}
			ran++
		}
		var (
			resp      ModelResponse
			discarded Usage
		)
		if err == nil {
			resp, discarded, err = ts.send(ctx, call)
			if err == nil {
				resp, err = checkResponse(resp, usedIDs)
			}
			ts.sink.done(call.attempt, resp, err)
			ts.meter.add(resp.Usage)
			ts.meter.add(discarded)
		}
		for _, h := range call.hooks[:ran] {
			if h.After != nil {
				h.After(ctx, call, ModelAttempt{Response: resp, Discarded: discarded, Err: err})
			}
		}
		return resp, err
	}
}

// send makes the request call describes. It streams it to the turn's sink when the request can
// claim the sink, and otherwise drains it: middleware sees the assembled response either way. It
// returns the response, the discarded usage the stream's Finish reported, and on error the usage
// the stream reported before failing.
func (ts *turnState) send(ctx context.Context, call ModelCall) (ModelResponse, Usage, error) {
	n := call.attempt
	claimed := ts.sink.claim(n)
	s, err := call.Model.Stream(ctx, call.Request)
	if err != nil {
		return ModelResponse{}, Usage{}, err
	}
	var fin Finish
	msg, u, err := s.drain(func(ev Event) {
		if f, ok := ev.(Finish); ok {
			fin = f
		}
		if claimed {
			ts.sink.forward(n, ev)
		}
	})
	discarded := fin.Discarded.billable()
	if err != nil {
		return ModelResponse{Usage: u}, discarded, err
	}
	info, described := ModelInfoOf(call.Model)
	return ModelResponse{
		Message:   msg,
		Usage:     u,
		Finish:    fin.Reason,
		RawFinish: fin.Raw,
		origin:    &responseOrigin{attempt: n, req: call.Request, info: info, described: described},
	}, discarded, nil
}

// checkResponse checks a response the way Stream.Message checks a stream's: its usage has no
// negative count, its finish reason is one the run can record as an answer (an empty one is
// FinishStop), and its tool calls carry ids the run has not used (see checkToolUseIDs).
func checkResponse(resp ModelResponse, usedIDs map[string]bool) (ModelResponse, error) {
	if err := resp.Usage.Validate(); err != nil {
		return ModelResponse{}, err
	}
	switch resp.Finish {
	case "":
		resp.Finish = FinishStop
	case FinishStop:
	case FinishToolUse:
		if len(resp.Message.toolUses()) == 0 {
			return ModelResponse{Usage: resp.Usage}, fmt.Errorf("finish reason %q with no tool call: %w", FinishToolUse, ErrStreamProtocol)
		}
	case FinishLength:
		return ModelResponse{Usage: resp.Usage}, ErrOutputTruncated
	case FinishFiltered:
		return ModelResponse{Usage: resp.Usage}, ErrOutputFiltered
	default:
		return ModelResponse{Usage: resp.Usage}, fmt.Errorf("finish reason %q is not one of the neutral reasons: %w", cutName(string(resp.Finish)), ErrStreamProtocol)
	}
	if err := checkToolUseIDs(resp.Message, usedIDs); err != nil {
		return ModelResponse{Usage: resp.Usage}, err
	}
	return resp, nil
}

// turnSink is a streaming turn's live token feed. Requests claim it exclusively: the first request
// that starts while no other holds it streams its events live, and any other request of the turn
// runs without streaming. A request that fails releases the claim, and the next request to claim
// it first emits TurnRestarted if the failed one streamed anything. Once the chain has returned
// its response, the sink is closed, so a request still running (a hedge loser, a request a
// middleware left behind) never reaches the caller, and if the response is not exactly the one the
// claimer streamed (a hedge target that was not the claimer won, a middleware built or changed
// it), the caller gets TurnRestarted and then the response replayed as events. Middleware needs no
// sink code for any of this.
type turnSink struct {
	mu        sync.Mutex
	seq       int
	fire      func(AgentEvent)
	holder    int            // the number of the request holding the claim; 0 for none
	streamed  bool           // events were forwarded since the turn started or last restarted
	delivered *ModelResponse // the response the holder's request completed with
	closed    bool
}

// claim gives request n the claim if no request holds it, reporting whether it did.
func (s *turnSink) claim(n int) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.holder != 0 {
		return false
	}
	s.holder = n
	if s.streamed {
		s.streamed = false
		s.fire(TurnRestarted{Seq: s.seq})
	}
	return true
}

// forward streams ev to the caller if request n holds the claim and the turn is still open.
func (s *turnSink) forward(n int, ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.holder != n {
		return
	}
	s.streamed = true
	s.fire(ModelEvent{Event: ev})
}

// done records how request n ended: a holder that failed releases the claim, and one that
// succeeded keeps it with the response it streamed.
func (s *turnSink) done(n int, resp ModelResponse, err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holder != n {
		return
	}
	if err != nil {
		s.holder = 0
		return
	}
	s.delivered = &resp
}

// finish closes the sink on the response the turn returns, replaying it to the caller unless it is
// exactly what the claimer streamed.
func (s *turnSink) finish(resp ModelResponse) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.delivered != nil && sameResponse(*s.delivered, resp) {
		return
	}
	if s.streamed {
		s.streamed = false
		s.fire(TurnRestarted{Seq: s.seq})
	}
	for _, e := range emitsFor(resp.Message, Finish{Reason: resp.Finish, Raw: resp.RawFinish, Usage: resp.Usage}) {
		s.fire(ModelEvent{Event: e.Event})
	}
}

// close closes the sink on a turn that failed: nothing more reaches the caller from it.
func (s *turnSink) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// sameResponse reports whether a and b are the same response of the same request: what a request
// streamed is what the turn records only if the middleware above it returned that response
// unchanged.
func sameResponse(a, b ModelResponse) bool {
	return a.origin != nil && a.origin == b.origin && a.Usage == b.Usage && a.Finish == b.Finish &&
		a.RawFinish == b.RawFinish && reflect.DeepEqual(a.Message, b.Message)
}
