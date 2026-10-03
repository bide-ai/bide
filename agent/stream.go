package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"sync"
)

// AgentEvent is a lifecycle event emitted by Agent.Stream as the run loop advances:
// turn boundaries, live model token deltas, and tool start/finish. It is the
// SEMANTIC-layer companion to the model's byte-level Event — a UI ranges over these
// to render progress while the durable loop runs underneath.
//
// The terminal answer and any error are NOT events: they come from AgentStream.Final
// (mirroring how the model Stream yields Events but returns the assembled Message
// separately). A run that pauses for approval or halts on an unsafe resume emits the
// relevant lifecycle event and then surfaces the *ApprovalPending / *OutcomeUnknown via
// Final, exactly as Run returns it.
type AgentEvent interface{ agentEvent() }

// TurnStarted marks the beginning of a fresh model turn (Seq is the model-call
// sequence number within the run). Not emitted for turns replayed from the journal.
type TurnStarted struct {
	Seq int `json:"seq"`
}

func (TurnStarted) agentEvent() {}

// ModelEvent forwards one live model stream Event (TextDelta, ReasoningDelta,
// ToolCallDelta, Finish) from the current turn — the token-by-token feed. Emitted
// only for a FRESH model call; on durable replay the turn is reused from the journal
// and produces no deltas (an AssistantTurn with Replayed=true is emitted instead). When the
// turn's model call starts over (see TurnRestarted), the deltas before it are not part of the
// recorded turn.
type ModelEvent struct {
	Event Event `json:"event"`
}

func (ModelEvent) agentEvent() {}

// TurnRestarted fires when the model call for turn Seq starts over after an attempt that had
// already streamed ModelEvent deltas: a middleware such as Retry called the model again after
// that attempt failed, or delivered a different response in its place. The deltas received
// since TurnStarted{Seq} (or the previous TurnRestarted) belong to the discarded attempt and
// are not part of the recorded turn, so a consumer rendering the turn should clear them. Not
// emitted when the discarded attempt streamed nothing.
type TurnRestarted struct {
	Seq int `json:"seq"`
}

func (TurnRestarted) agentEvent() {}

// AssistantTurn is the fully-assembled assistant message for a turn. Replayed is true
// when it was reconstructed from the journal on resume rather than produced by a live
// model call (in which case no ModelEvent deltas preceded it).
type AssistantTurn struct {
	Message  Message `json:"message"`
	Replayed bool    `json:"replayed"`
}

func (AssistantTurn) agentEvent() {}

// ToolStarted fires when a tool call begins executing: after any approval gate and, for
// non-idempotent tools, after the durable attempt marker is written, immediately before the tool
// is called. A call that does not start (cancelled after its attempt marker and before the call,
// and recorded as not started) emits neither ToolStarted nor ToolCompleted.
type ToolStarted struct {
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
}

func (ToolStarted) agentEvent() {}

// ToolCompleted carries a tool call's result. Emitted for live executions, for a
// human-denied call, and for each result reconstructed from the journal on resume.
type ToolCompleted struct {
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	Result    json.RawMessage `json:"result"`
	IsError   bool            `json:"is_error"`
}

func (ToolCompleted) agentEvent() {}

// ApprovalRequired fires immediately before the run pauses for a human decision on a
// tool that requires approval. The run then returns *ApprovalPending from Final; record
// a decision (Approve, or SubmitDecision for an m-of-n gate) and re-invoke to continue.
type ApprovalRequired struct {
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
	// Quorum is the running tally when the tool has an m-of-n approval policy (ToolSpec.Approval), so a
	// streaming UI can show progress ("1 of 2 approved") without waiting for Final. Nil for
	// a 1-of-1 gate.
	Quorum *ApprovalTally `json:"quorum,omitempty"`
}

func (ApprovalRequired) agentEvent() {}

// Finished carries the terminal assistant answer — the same Message that Run returns
// and that Final reports. The event stream closes after this.
type Finished struct {
	Final Message `json:"final"`
}

func (Finished) agentEvent() {}

// ReplayEvents returns the semantic lifecycle events implied by runID's DURABLE journal:
// the same AssistantTurn and ToolCompleted events Agent.Stream re-emits when it resumes from
// that journal, in persisted order. Because the sequence is a pure function of the recorded
// steps, it — and any commitment built over it (see audit.EventLogFromJournal) — is identical
// before and after a crash, which is what makes it a resume-stable audit artifact.
//
// It reconstructs from the journal alone, without re-running the model or tools. Only
// journaled facts are reproduced: assembled assistant turns (StepModel) and completed tool
// calls with their results (StepToolResult). Live-loop-only signals — token-level ModelEvent
// deltas, TurnStarted, TurnRestarted, ToolStarted, and the terminal Finished — are not
// journaled and so are not part of the durable projection; the durable content is the turns and
// tool results.
func ReplayEvents(ctx context.Context, store *Journal, runID string) ([]AgentEvent, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load history %s: %w (%w)", runID, err, ErrStorage)
	}
	evs, _ := ProjectEvents(recs)
	return evs, nil
}

// ProjectEvents returns the events ReplayEvents returns for a journal holding recs, and for each
// event the index in recs of the record it projects: each event comes from exactly one record.
// The audit package uses the index to salt a projected event from its record's salt.
func ProjectEvents(recs []Record) (events []AgentEvent, sources []int) {
	for i, r := range recs {
		switch r.Kind {
		case StepModel:
			if r.Message != nil {
				events = append(events, AssistantTurn{Message: *r.Message, Replayed: true})
				sources = append(sources, i)
			}
		case StepToolResult:
			name, _ := toolNameFor(recs, r.ToolUseID)
			events = append(events, ToolCompleted{ToolUseID: r.ToolUseID, Name: name, Result: r.Result, IsError: r.IsError})
			sources = append(sources, i)
		}
	}
	return events, sources
}

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
	res *Result // the Result RunMessage would return; nil for a stream from Stream or StreamSaga
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
// error (including *ApprovalPending / *OutcomeUnknown, matching Run). Safe to call after
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

// Result drains any un-consumed events and returns the run's Result and error, as RunMessage
// returns them: the Result is non-nil whenever the run ID is valid, whatever the error. For a
// stream from the transitional Stream or StreamSaga it returns a Result built from Final's
// message.
func (as *AgentStream) Result() (*Result, error) {
	msg, err := as.Final()
	if as.res.res != nil {
		return as.res.res, err
	}
	return &Result{Message: msg}, err
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
	var cfg runConfig
	cfg.saga = saga
	in := UserText(input)
	return a.startStream(ctx, func(emit func(AgentEvent)) agentResult {
		msg, _, _, err := a.drive(ctx, runID, &driveSpec{input: &in, cfg: cfg, emit: emit, strictSaga: !saga})
		return agentResult{msg: msg, err: err}
	})
}

// streamEntry is StreamMessage's body: RunMessage's, with events.
func (a *Agent) streamEntry(ctx context.Context, runID string, d *driveSpec, opts []RunOption) *AgentStream {
	return a.startStream(ctx, func(emit func(AgentEvent)) agentResult {
		d.emit = emit
		res, err := a.runEntry(ctx, runID, d, opts)
		r := agentResult{res: res, err: err}
		if res != nil {
			r.msg = res.Message
		}
		return r
	})
}

// startStream runs body on its own goroutine, handing it the emit function that feeds the
// stream's events, and returns the stream.
func (a *Agent) startStream(ctx context.Context, body func(emit func(AgentEvent)) agentResult) *AgentStream {
	as := &AgentStream{ch: make(chan AgentEvent), result: make(chan agentResult, 1)}
	// mu and closed let an event that arrives after the run has ended be dropped rather than
	// sent on the closed channel, which would panic and take down the process. That can happen
	// only through a goroutine outliving its call, such as a middleware that fans a model call
	// out and leaves a loser running; Middleware is a public extension point, so the stream does
	// not rely on every middleware getting that right.
	var (
		mu     sync.Mutex
		closed bool
	)
	emit := func(e AgentEvent) {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		select {
		case as.ch <- e:
		case <-ctx.Done():
		}
	}
	go func() {
		defer func() {
			mu.Lock()
			closed = true
			close(as.ch)
			mu.Unlock()
		}()
		as.result <- body(emit)
	}()
	return as
}
