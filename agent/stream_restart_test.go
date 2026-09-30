package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// renderTurns ranges a stream the way a UI does: it appends each text delta and clears the
// turn's text on TurnRestarted. It returns the rendered text and the TurnRestarted events.
func renderTurns(t *testing.T, as *AgentStream) (string, []TurnRestarted) {
	t.Helper()
	var text strings.Builder
	var restarts []TurnRestarted
	for ev := range as.Events() {
		switch e := ev.(type) {
		case ModelEvent:
			if d, ok := e.Event.(TextDelta); ok {
				text.WriteString(d.Text)
			}
		case TurnRestarted:
			restarts = append(restarts, e)
			text.Reset()
		}
	}
	return text.String(), restarts
}

// retryOnceMW calls next again when the first call fails, as middleware.Retry does.
func retryOnceMW(next ModelHandler) ModelHandler {
	return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
		resp, err := next(ctx, call)
		if err != nil {
			return next(ctx, call)
		}
		return resp, err
	}
}

// A model attempt that streams part of its answer and then fails, and is retried by
// middleware, must not leave its partial text in what the consumer renders: the consumer
// would show "partial complete" while the run records "complete".
func TestStream_RetriedTurnMarksDiscardedDeltas(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{
		{{Event: TextDelta{Text: "partial "}}, {Err: errors.New("connection reset")}},
		textTurn("complete"),
	}}
	as := New(m, NewMemStore()).Use(retryOnceMW).Stream(context.Background(), "r", "go")
	rendered, restarts := renderTurns(t, as)
	final, err := as.Final()
	if err != nil {
		t.Fatalf("Final: %v", err)
	}
	if rendered != final.Text() {
		t.Fatalf("the consumer rendered %q but the run recorded %q", rendered, final.Text())
	}
	if len(restarts) != 1 || restarts[0].Seq != 0 {
		t.Fatalf("restarts = %+v, want one TurnRestarted{Seq: 0}", restarts)
	}
}

// The same holds when a middleware replaces a failed live attempt with a response from
// elsewhere: the agent replays the response, after a restart, with no sink code in the
// middleware.
func TestStream_FallbackResponseMarksDiscardedDeltas(t *testing.T) {
	m := &scriptModel{turns: [][]Emit{
		{{Event: TextDelta{Text: "partial "}}, {Err: errors.New("connection reset")}},
	}}
	fallback := func(next ModelHandler) ModelHandler {
		return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
			if resp, err := next(ctx, call); err == nil {
				return resp, nil
			}
			return ModelResponse{Message: Message{Role: RoleAssistant, Parts: []Part{Text{Text: "fallback"}}}}, nil
		}
	}
	as := New(m, NewMemStore()).Use(fallback).Stream(context.Background(), "r", "go")
	rendered, restarts := renderTurns(t, as)
	final, err := as.Final()
	if err != nil {
		t.Fatalf("Final: %v", err)
	}
	if rendered != final.Text() {
		t.Fatalf("the consumer rendered %q but the run recorded %q", rendered, final.Text())
	}
	if len(restarts) != 1 {
		t.Fatalf("restarts = %+v, want one", restarts)
	}
}

// retryTwiceMW calls next up to three times in all while it fails.
func retryTwiceMW(next ModelHandler) ModelHandler {
	return func(ctx context.Context, call ModelCall) (ModelResponse, error) {
		resp, err := next(ctx, call)
		for i := 0; i < 2 && err != nil; i++ {
			resp, err = next(ctx, call)
		}
		return resp, err
	}
}

// A retried attempt that streamed nothing leaves nothing to discard, so no TurnRestarted is
// emitted for it; and each turn's restart carries that turn's Seq.
func TestStream_RestartOnlyAfterStreamedDeltas(t *testing.T) {
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{
		errTurn(errors.New("refused")), // turn 0, first attempt: fails before streaming
		toolTurn("c1", "lookup", `{}`),
		{{Event: TextDelta{Text: "half"}}, {Err: errors.New("reset")}}, // turn 1: streams, then fails
		errTurn(errors.New("refused")),                                 // turn 1, second attempt: fails before streaming
		textTurn("done"),
	}}
	as := New(m, NewMemStore(), tool).Use(retryTwiceMW).Stream(context.Background(), "r", "go")
	rendered, restarts := renderTurns(t, as)
	final, err := as.Final()
	if err != nil {
		t.Fatalf("Final: %v", err)
	}
	if len(restarts) != 1 || restarts[0].Seq != 1 {
		t.Fatalf("restarts = %+v, want one TurnRestarted{Seq: 1}", restarts)
	}
	if rendered != final.Text() {
		t.Fatalf("the consumer rendered %q but the run recorded %q", rendered, final.Text())
	}
}
