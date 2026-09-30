package agent

import (
	"context"
	"sync"
)

// generate runs one model call through the middleware chain. usedIDs holds the tool-use IDs
// already in the run's conversation; a turn that reuses one is rejected (see checkToolUseIDs).
// The innermost handler checks each model response, below middleware, so a retry middleware
// sees the fault, and the check still holds when a middleware trims the history it sends. The
// response the chain returns is checked again, since a middleware can return one it did not get
// from the handler it wraps (a fallback, a cache), and that response is what the run records.
//
// The innermost handler sends every request of the call: it goes to the model WithModel set, if
// any, and runs the ModelCallHooks middleware installed around it. meter receives every
// request's usage, so the run can record what the call spent beyond the response it returns.
func (a *Agent) generate(ctx context.Context, req Request, usedIDs map[string]bool, meter *spendMeter) (Message, Usage, error) {
	ctx = inModelCall(ctx, meter)
	h := ModelHandler(func(ctx context.Context, req Request) (Message, Usage, error) {
		hooks := modelHooks(ctx)
		for _, hk := range hooks {
			if hk.Before != nil {
				if err := hk.Before(ctx); err != nil {
					return Message{}, Usage{}, err
				}
			}
		}
		msg, u, err := a.send(ctx, modelFor(ctx, a.model), req)
		for _, hk := range hooks {
			if hk.After != nil {
				hk.After(u)
			}
		}
		if err != nil {
			return msg, u, err
		}
		if err := checkToolUseIDs(msg, usedIDs); err != nil {
			return Message{}, u, err
		}
		return msg, u, nil
	})
	for i := len(a.mw) - 1; i >= 0; i-- {
		h = a.mw[i](h)
	}
	msg, u, err := h(ctx, req)
	if err != nil {
		return msg, u, err
	}
	if err := u.Validate(); err != nil {
		return Message{}, Usage{}, err
	}
	if err := checkToolUseIDs(msg, usedIDs); err != nil {
		return Message{}, u, err
	}
	return msg, u, nil
}

// send makes one model request. When a token sink is installed (Agent.Stream), it streams the
// call and forwards deltas as they arrive while still assembling the message for the journal;
// otherwise it takes the plain blocking drain. Middleware wraps this either way and sees the
// assembled message and usage: streaming stays below it.
func (a *Agent) send(ctx context.Context, m Model, req Request) (Message, Usage, error) {
	sink := modelSink(ctx)
	if sink == nil {
		return Generate(ctx, m, req)
	}
	sink(attemptStart{}) // a new attempt: any deltas an earlier one streamed are discarded
	s, err := m.Stream(ctx, req)
	if err != nil {
		return Message{}, Usage{}, err
	}
	return s.drain(sink)
}

// attemptStart is sent to a run's model sink when a model attempt starts delivering a
// response: the model handler starting a call, or EmitMessage. The sink does not forward it; it
// marks where the previous attempt, if any, ended.
type attemptStart struct{}

func (attemptStart) event() {}

// turnSink returns the model sink for turn seq. It forwards each model Event as a ModelEvent.
// On an attemptStart that follows an attempt which streamed deltas, it emits TurnRestarted,
// since those deltas are not part of the response the turn records: a middleware such as Retry
// is calling the model again, or delivering a response from elsewhere.
func turnSink(seq int, fire func(AgentEvent)) func(Event) {
	var (
		mu       sync.Mutex
		streamed bool // the current attempt has forwarded a delta
	)
	return func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := ev.(attemptStart); ok {
			if streamed {
				streamed = false
				fire(TurnRestarted{Seq: seq})
			}
			return
		}
		streamed = true
		fire(ModelEvent{Event: ev})
	}
}

// EmitMessage delivers a model call's response, m and its usage u, to sink as the events a
// model would have streamed for it, ending with a Finish that carries u. It does nothing when
// sink is nil. See DetachModelSink.
func EmitMessage(sink func(Event), m Message, u Usage) {
	if sink == nil {
		return
	}
	sink(attemptStart{}) // m replaces whatever an earlier attempt streamed
	for _, e := range emitsFor(m, u) {
		sink(e.Event)
	}
}
