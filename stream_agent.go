package agent

import (
	"context"
	"encoding/json"
	"iter"
)

// AgentEvent is a lifecycle event emitted by Agent.Stream as the run loop advances:
// turn boundaries, live model token deltas, and tool start/finish. It is the
// SEMANTIC-layer companion to the model's byte-level Event — a UI ranges over these
// to render progress while the durable loop runs underneath.
//
// The terminal answer and any error are NOT events: they come from AgentStream.Final
// (mirroring how the model Stream yields Events but returns the assembled Message
// separately). A run that pauses for approval or halts on an unsafe resume emits the
// relevant lifecycle event and then surfaces the *PendingApproval / *ResumeHalt via
// Final, exactly as Run returns it.
type AgentEvent interface{ agentEvent() }

// TurnStarted marks the beginning of a fresh model turn (Seq is the model-call
// sequence number within the run). Not emitted for turns replayed from the journal.
type TurnStarted struct{ Seq int }

func (TurnStarted) agentEvent() {}

// ModelEvent forwards one live model stream Event (TextDelta, ReasoningDelta,
// ToolCallDelta, Finish) from the current turn — the token-by-token feed. Emitted
// only for a FRESH model call; on durable replay the turn is reused from the journal
// and produces no deltas (an AssistantTurn with Replayed=true is emitted instead).
type ModelEvent struct{ Event Event }

func (ModelEvent) agentEvent() {}

// AssistantTurn is the fully-assembled assistant message for a turn. Replayed is true
// when it was reconstructed from the journal on resume rather than produced by a live
// model call (in which case no ModelEvent deltas preceded it).
type AssistantTurn struct {
	Message  Message
	Replayed bool
}

func (AssistantTurn) agentEvent() {}

// ToolStarted fires when a tool call begins executing (after any approval gate and,
// for non-idempotent tools, after the durable attempt marker is written).
type ToolStarted struct {
	ToolUseID string
	Name      string
	Args      json.RawMessage
}

func (ToolStarted) agentEvent() {}

// ToolCompleted carries a tool call's result. Emitted for live executions, for a
// human-denied call, and for each result reconstructed from the journal on resume.
type ToolCompleted struct {
	ToolUseID string
	Name      string
	Result    json.RawMessage
	IsError   bool
}

func (ToolCompleted) agentEvent() {}

// ApprovalRequired fires immediately before the run pauses for a human decision on a
// tool that RequiresApproval. The run then returns *PendingApproval from Final; record
// a decision (Durable.RecordApproval) and re-invoke to continue.
type ApprovalRequired struct {
	ToolUseID string
	Name      string
	Args      json.RawMessage
}

func (ApprovalRequired) agentEvent() {}

// Finished carries the terminal assistant answer — the same Message that Run returns
// and that Final reports. The event stream closes after this.
type Finished struct{ Final Message }

func (Finished) agentEvent() {}

// AgentStream is a live view of a running agent: range Events for progress, then call
// Final for the terminal answer (or error). It is the streaming counterpart of Run,
// the same way the model Stream is the counterpart of Generate — Run is literally
// Stream(...).Final().
//
// A Stream is consumed once. Either range Events fully (Final then returns
// immediately) or call Final directly (it drains and discards events); calling Final
// after breaking out of Events early drains whatever remains. Cancel ctx to abandon a
// run without draining — pending emits then unblock on ctx.Done rather than leaking.
type AgentStream struct {
	ch     chan AgentEvent
	result chan agentResult // buffered(1); the run goroutine's return value
	res    *agentResult     // memoized after first read
}

type agentResult struct {
	msg Message
	err error
}

// Events returns a range-over-func iterator over lifecycle events until the run ends.
// Per-event errors do not occur here (a mid-stream model error aborts the run and is
// reported by Final); the iterator simply ends when the run stops producing events.
func (as *AgentStream) Events() iter.Seq[AgentEvent] {
	return func(yield func(AgentEvent) bool) {
		for e := range as.ch {
			if !yield(e) {
				return
			}
		}
	}
}

// Final drains any un-consumed events and returns the run's terminal message and
// error (including *PendingApproval / *ResumeHalt, matching Run). Safe to call after
// fully or partially ranging Events, or on its own.
func (as *AgentStream) Final() (Message, error) {
	for range as.ch { // drain remaining events so the run goroutine can finish
	}
	if as.res == nil {
		r := <-as.result
		as.res = &r
	}
	return as.res.msg, as.res.err
}

// Stream drives the agent like Run but returns a live AgentStream: token deltas, turn
// boundaries, and tool start/finish arrive as events while the durable loop runs.
// Resume, approval, and side-effect safety are identical to Run — Stream and Run share
// one loop; Run is Stream(...).Final().
func (a *Agent) Stream(ctx context.Context, runID, input string) *AgentStream {
	return a.stream(ctx, runID, input, false)
}

// StreamSaga is the streaming counterpart of RunSaga (transactional run with reverse-
// order compensation on failure).
func (a *Agent) StreamSaga(ctx context.Context, runID, input string) *AgentStream {
	return a.stream(ctx, runID, input, true)
}

func (a *Agent) stream(ctx context.Context, runID, input string, saga bool) *AgentStream {
	as := &AgentStream{ch: make(chan AgentEvent), result: make(chan agentResult, 1)}
	emit := func(e AgentEvent) {
		select {
		case as.ch <- e:
		case <-ctx.Done():
		}
	}
	go func() {
		defer close(as.ch)
		var msg Message
		var err error
		if saga {
			msg, err = a.runSaga(ctx, runID, input, emit)
		} else {
			msg, _, _, err = a.run(ctx, runID, []Message{UserText(input)}, false, emit)
		}
		as.result <- agentResult{msg: msg, err: err}
	}()
	return as
}
