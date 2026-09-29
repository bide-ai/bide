package middleware

import (
	"context"
	"errors"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Hedge races the model call against one or more backup models and returns the FIRST
// successful response, cancelling the rest. It cuts tail latency (a slow primary no longer
// blocks the turn, you take whichever target answers first) and adds provider-outage failover
// (a down or throttled primary does not stall the run). It is safe here in a way it is not in a
// bare service: the agent loop journals only the winning response, at most once, so the durable
// record stays exactly-once and replays deterministically regardless of which target won the
// race.
//
// Scheduling:
//   - The primary (the wrapped model, i.e. the one passed to agent.New) fires immediately.
//   - If delay > 0, the backups fire only after the primary has been outstanding for delay, so
//     you pay for a backup generation only when the primary is actually slow. A delay of 0 fires
//     every target at once (pure redundancy, always pays for all of them).
//   - If a target returns an ERROR while backups are still pending, the backups fire immediately
//     (fast failover), rather than waiting out the remaining delay on a fast failure.
//
// The first target to return a nil-error result wins; every other in-flight call is cancelled via
// a derived context. If every target fails, Hedge returns the joined error. With no backups it is
// a pass-through, so it is safe to wire unconditionally and add backups later.
//
// Every target goes through the middleware installed inside Hedge (after it in Use) and the
// agent's checks on each model response, backups included. Every request a target sends counts
// wherever the counting middleware sits: a RateLimit takes a token per request (1 + the backups
// launched), and Cost and the run's token budget count each target's usage as spend, the
// winner's as the answer's.
//
// Hedge does not wait for the losers: it returns as soon as a target wins, and a loser runs until
// its model honors the cancellation. A model that ignores it keeps a loser, and the middleware
// inside Hedge on its path, running after the turn, or the run, has ended. Its usage then reaches
// Cost when it ends, and the run's budget only if it ends before the run's next model turn.
//
// Streaming: with backups, a streaming caller (Agent.Stream) receives the winning response's
// events once the winner is chosen, not token by token. The targets race without access to the
// caller's stream, so a target that loses can never show the caller text the run does not
// record, or send anything after the run has ended. With no backups, streaming is live as usual.
//
// Hedge only races the model generation. It does not duplicate tool calls or any other side
// effect: those run in the agent loop, above this middleware, under the at-most-once journal and
// the Safety layer. Note it hedges latency and availability, not correctness: taking the first
// answer is not the same as requiring agreement across models (that is a governance-quorum
// concern, not this).
//
//	// Primary is Anthropic; if it is quiet for 800ms, also try OpenAI and take the first.
//	a := agent.New(anthropicModel, store, tools...).
//	    Use(middleware.Hedge(800*time.Millisecond, openaiModel))
func Hedge(delay time.Duration, backups ...agent.Model) agent.Middleware {
	return func(next agent.ModelHandler) agent.ModelHandler {
		// One handler per target. Every target goes through `next`, the rest of the chain: the
		// primary as is, each backup with agent.WithModel directing the agent's model handler to
		// it, so the middleware inside Hedge and the agent's checks on each response apply to
		// every target alike. Outside an agent, where nothing below routes by model, a backup is
		// called directly.
		handlers := make([]agent.ModelHandler, 0, 1+len(backups))
		handlers = append(handlers, next)
		for _, b := range backups {
			b := b
			handlers = append(handlers, func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
				if tctx, ok := agent.WithModel(ctx, b); ok {
					return next(tctx, req)
				}
				s, err := b.Stream(ctx, req)
				if err != nil {
					return agent.Message{}, agent.Usage{}, err
				}
				return s.Message()
			})
		}

		return func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
			if len(handlers) == 1 { // no backups: plain pass-through, streaming live
				return next(ctx, req)
			}

			// The targets race without the caller's token sink: only the winner's response is
			// delivered to it, once chosen (see the Streaming note above).
			ctx, sink := agent.DetachModelSink(ctx)
			hctx, cancel := context.WithCancel(ctx)
			defer cancel() // returning cancels every loser still in flight

			type result struct {
				msg agent.Message
				u   agent.Usage
				err error
			}
			// Buffered so a losing goroutine finishing after we return never blocks on send.
			results := make(chan result, len(handlers))
			launched := 0
			launch := func(i int) {
				go func() {
					m, u, e := handlers[i](hctx, req)
					results <- result{m, u, e}
				}()
			}

			launch(0) // primary immediately
			launched = 1

			launchRemaining := func() {
				for launched < len(handlers) {
					launch(launched)
					launched++
				}
			}

			var timerC <-chan time.Time
			if delay <= 0 {
				launchRemaining()
			} else {
				t := time.NewTimer(delay)
				defer t.Stop()
				timerC = t.C
			}

			var errs []error
			for {
				select {
				case <-ctx.Done():
					return agent.Message{}, agent.Usage{}, ctx.Err()
				case <-timerC:
					launchRemaining()
					timerC = nil
				case r := <-results:
					if r.err == nil {
						agent.EmitMessage(sink, r.msg, r.u) // the caller sees exactly what the run records
						return r.msg, r.u, nil              // first success wins; defer cancel() kills the rest
					}
					errs = append(errs, r.err)
					// A target failed: bring the backups forward now instead of waiting out delay.
					if launched < len(handlers) {
						launchRemaining()
						timerC = nil
					}
					if len(errs) == launched && launched == len(handlers) {
						return agent.Message{}, agent.Usage{}, errors.Join(errs...) // all targets failed
					}
				}
			}
		}
	}
}
