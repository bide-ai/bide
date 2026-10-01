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
// agent's checks on each model response, backups included: a backup is the same call with its
// Model set to the backup, and nothing else changed. Every request a target sends counts wherever
// the counting middleware sits: a RateLimit takes a token per request (1 + the backups launched),
// and Cost and the run's token budget count each target's usage as spend. Only the turn's answer,
// the winner's response, counts as an answer, once, even with Cost inside Hedge (see
// agent.ModelCall.OnAnswer). Each request is numbered apart (agent.ModelCall.Attempt).
//
// Hedge does not wait for the losers: it returns as soon as a target wins, and a loser runs until
// its model honors the cancellation. Its usage reaches Cost when it ends. The run counts it with
// its next model turn, or, when the run ends first, waits for it (for at most two seconds, and not
// past the run's context) and journals it in a spend record of its own. A loser that has not
// reached the model when the turn ends is not sent. A model that ignores the cancellation for
// longer keeps a loser, and the middleware inside Hedge on its path, running after the run has
// ended, and its usage is not in the run's spend.
//
// Streaming: Hedge has no streaming code. The agent's model handler lets one request of a turn
// at a time stream to a streaming caller (Agent.Stream), the first to start: the others run
// without streaming. If that request's response wins, the caller has seen it live. If another
// target wins, the caller gets agent.TurnRestarted, which retracts what the first one streamed,
// and then the winner's response. Either way nothing reaches the caller after the turn is over,
// so a loser can never show the caller text the run does not record.
//
// Hedge only races the model generation. It does not duplicate tool calls or any other side
// effect: those run in the agent loop, above this middleware, under the at-most-once journal and
// the Safety layer. Note it hedges latency and availability, not correctness: taking the first
// answer is not the same as requiring agreement across models (that is a governance-quorum
// concern, not this).
//
//	// Primary is Anthropic; if it is quiet for 800ms, also try OpenAI and take the first.
//	a, err := agent.Build(anthropicModel, journal, agent.WithTools(tools...),
//		agent.WithMiddleware(middleware.Hedge(800*time.Millisecond, openaiModel)))
func Hedge(delay time.Duration, backups ...agent.Model) agent.Middleware {
	return func(next agent.ModelHandler) agent.ModelHandler {
		if len(backups) == 0 {
			return next // no backups: a plain pass-through
		}
		return func(ctx context.Context, call agent.ModelCall) (agent.ModelResponse, error) {
			// One call per target: the primary as it came, each backup the same call sent to it.
			targets := make([]agent.ModelCall, 1+len(backups))
			targets[0] = call
			for i, b := range backups {
				c := call
				c.Model = b
				targets[i+1] = c
			}

			hctx, cancel := context.WithCancel(ctx)
			defer cancel() // returning cancels every loser still in flight

			type result struct {
				resp agent.ModelResponse
				err  error
			}
			// Buffered so a losing goroutine finishing after we return never blocks on send.
			results := make(chan result, len(targets))
			launched := 0
			launch := func(i int) {
				go func() {
					r, e := next(hctx, targets[i])
					results <- result{r, e}
				}()
			}

			launch(0) // primary immediately
			launched = 1

			launchRemaining := func() {
				for launched < len(targets) {
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
					return agent.ModelResponse{}, ctx.Err()
				case <-timerC:
					launchRemaining()
					timerC = nil
				case r := <-results:
					if r.err == nil {
						return r.resp, nil // first success wins; defer cancel() kills the rest
					}
					errs = append(errs, r.err)
					// A target failed: bring the backups forward now instead of waiting out delay.
					if launched < len(targets) {
						launchRemaining()
						timerC = nil
					}
					if len(errs) == launched && launched == len(targets) {
						return agent.ModelResponse{}, errors.Join(errs...) // all targets failed
					}
				}
			}
		}
	}
}
