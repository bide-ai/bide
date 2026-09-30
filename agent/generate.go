package agent

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
)

// modelChain is a model call chain: middleware (first = outermost) around a model handler that
// sends each request. An agent run builds one for all its turns (Agent.modelChain), and CallModel
// one per call.
type modelChain struct{ h ModelHandler }

// newModelChain returns the chain of mws around its own model handler. Every handler in it gets
// its calls clipped (see clipped).
func newModelChain(mws []Middleware) *modelChain {
	c := &modelChain{}
	h := clipped(c.send)
	for i := len(mws) - 1; i >= 0; i-- {
		h = clipped(mws[i](h))
	}
	c.h = h
	return c
}

// modelChain returns the agent's middleware chain, for one run.
func (a *Agent) modelChain() *modelChain { return newModelChain(a.mw) }

// newTurnSink returns the sink of streaming turn seq, which forwards to fire.
func newTurnSink(seq int, fire func(AgentEvent)) *turnSink { return &turnSink{seq: seq, fire: fire} }

// turnState is one model turn's state, shared by every request the turn sends. Middleware cannot
// reach it: a ModelCall carries it unexported, and the chain's model handler accepts only a call
// that carries a turn of its own chain.
type turnState struct {
	chain    *modelChain     // the chain the turn's calls go through
	attempts atomic.Int64    // requests numbered so far
	meter    *spendMeter     // every request's usage, for the run's budget and Result.Spend
	journal  Durable         // the run's journal, for middleware that records a step (WithRetrieval); nil outside a run
	sink     *turnSink       // the live token stream (Agent.Stream); nil when not streaming
	usedIDs  map[string]bool // the tool-use IDs already in the run's conversation (see checkToolUseIDs)
	finished atomic.Bool     // the chain has returned: the turn sends no new request

	answerMu sync.Mutex
	answers  map[any]func(context.Context, ModelResponse) // run once with the turn's answer (ModelCall.OnAnswer)
	order    []any                                        // the keys of answers, in registration order
}

// onAnswer registers fn under key, unless key is registered already. It reports false once the
// turn is over.
func (ts *turnState) onAnswer(key any, fn func(context.Context, ModelResponse)) bool {
	ts.answerMu.Lock()
	defer ts.answerMu.Unlock()
	if ts.finished.Load() {
		return false
	}
	if _, ok := ts.answers[key]; ok {
		return true
	}
	if ts.answers == nil {
		ts.answers = map[any]func(context.Context, ModelResponse){}
	}
	ts.answers[key] = fn
	ts.order = append(ts.order, key)
	return true
}

// finish ends the turn: no request starts after it, and no answer function registers.
func (ts *turnState) finish() {
	ts.answerMu.Lock()
	defer ts.answerMu.Unlock()
	ts.finished.Store(true)
}

// answer runs each registered answer function once with resp, the turn's answer.
func (ts *turnState) answer(ctx context.Context, resp ModelResponse) {
	ts.answerMu.Lock()
	fns := make([]func(context.Context, ModelResponse), len(ts.order))
	for i, k := range ts.order {
		fns[i] = ts.answers[k]
	}
	ts.answerMu.Unlock()
	for _, fn := range fns {
		fn(ctx, resp)
	}
}

// call sends call through the chain as turn ts, and checks the response the chain returns.
func (c *modelChain) call(ctx context.Context, call ModelCall, ts *turnState) (ModelResponse, error) {
	ts.chain = c
	call.turn = ts
	resp, err := c.h(ctx, call)
	ts.finish()
	if err == nil {
		// The response the chain returns is checked again, since a middleware can return one it did
		// not get from the handler it wraps (a fallback, a cache), and that response is what the
		// run records.
		resp, err = checkResponse(resp, ts.usedIDs)
	}
	if err != nil {
		ts.sink.close()
		return ModelResponse{}, err
	}
	ts.sink.finish(resp)
	ts.answer(ctx, resp)
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

// errTurnOver is the model handler's refusal of a request of a turn that is over: its chain has
// returned, so nothing would record the request (a middleware kept the call and sent it later).
var errTurnOver = fmt.Errorf("agent: a ModelCall of a model turn that is over reached the model handler: its call already returned, so nothing would record the request: %w", ErrConfig)

// send is the chain's model handler: it sends one request of the call's turn. It numbers the
// request, runs the call's hooks around it, streams it to the turn's sink if it holds the sink's
// claim, adds its usage to the turn's meter (not a hook, so no middleware can remove it), and
// checks the response, below middleware, so a retry middleware sees the fault.
func (c *modelChain) send(ctx context.Context, call ModelCall) (ModelResponse, error) {
	ts := call.turn
	if ts == nil || ts.chain != c {
		return ModelResponse{}, errForeignCall
	}
	if call.Model == nil {
		return ModelResponse{}, fmt.Errorf("agent: a ModelCall reached the model handler with a nil Model: %w", ErrConfig)
	}
	// The request is in flight from here until its usage is metered, so a run that ends waits for
	// it (spendMeter.wait). Counting it before the check makes the two race-free: a request that
	// passes the check is counted before the turn's end is observed.
	ts.meter.begin()
	defer ts.meter.end()
	if ts.finished.Load() {
		return ModelResponse{}, errTurnOver
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
			resp, err = checkResponse(resp, ts.usedIDs)
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
	if s.replayed {
		// A replayed turn answers as the model its record names, not as the replaying Model.
		info, described = ModelInfo{}, s.recorded != nil
		if described {
			info = *s.recorded
		}
	}
	return ModelResponse{
		Message:   msg,
		Usage:     u,
		Finish:    fin.Reason,
		RawFinish: fin.Raw,
		origin:    responseOrigin{attempt: n, msgs: call.Request.Messages, tools: call.Request.Tools, info: info, described: described},
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
		if !hasToolUse(resp.Message) {
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
	holder    int           // the number of the request holding the claim; 0 for none
	streamed  bool          // events were forwarded since the turn started or last restarted
	delivered ModelResponse // the response the holder's request completed with, if it succeeded
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
	s.delivered = resp
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
	if sameResponse(s.delivered, resp) {
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
	return a.origin.attempt != 0 && a.origin.attempt == b.origin.attempt && a.Usage == b.Usage && a.Finish == b.Finish &&
		a.RawFinish == b.RawFinish && reflect.DeepEqual(a.Message, b.Message)
}

// hasToolUse reports whether m calls a tool.
func hasToolUse(m Message) bool {
	for _, p := range m.Parts {
		if _, ok := p.(ToolUse); ok {
			return true
		}
	}
	return false
}
