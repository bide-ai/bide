package agent

import (
	"context"
	"errors"
	"testing"
)

// cutModel's response stops partway through the turn, as when a connection closes cleanly
// mid-stream: some text arrives, then the stream ends with no Finish.
type cutModel struct{}

func (cutModel) Stream(context.Context, Request) (*Stream, error) {
	ch := make(chan Emit, 1)
	ch <- Emit{Event: TextDelta{Text: "Your refund is appr"}}
	close(ch)
	return NewStream(ch), nil
}

// A response cut off partway is not the model's answer. The run must fail with
// ErrIncompleteResponse, stay incomplete, and journal nothing for the turn, so a retry asks
// the model again instead of the truncated text becoming the run's recorded answer.
func TestIncompleteResponse_IsNotTheAnswer(t *testing.T) {
	store := NewMemStore()
	msg, err := New(cutModel{}, store).Run(context.Background(), "r1", "status of my refund?")
	if !errors.Is(err, ErrIncompleteResponse) || !errors.Is(err, ErrModel) {
		t.Errorf("run = %q, %v; want ErrIncompleteResponse (an ErrModel)", msg.Text(), err)
	}
	if complete, _ := IsComplete(context.Background(), store, "r1"); complete {
		t.Errorf("the run was marked complete with a truncated answer")
	}
	recs, _ := store.History(context.Background(), "r1")
	for _, r := range recs {
		if r.Kind == StepModel {
			t.Errorf("the truncated turn was journaled: %q", r.Message.Text())
		}
	}
}

// An error ends the stream: a consumer that keeps ranging after it sees that one error and
// nothing more, not a second ErrIncompleteResponse for the Finish that never came.
func TestStreamError_IsTerminal(t *testing.T) {
	boom := errors.New("boom")
	ch := make(chan Emit, 3)
	ch <- Emit{Event: TextDelta{Text: "a"}}
	ch <- Emit{Err: boom}
	ch <- Emit{Event: TextDelta{Text: "after"}}
	close(ch)
	var errs []error
	var events int
	for ev, err := range NewStream(ch).Events() {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_ = ev
		events++
	}
	if len(errs) != 1 || !errors.Is(errs[0], boom) || events != 1 {
		t.Fatalf("got events=%d errs=%v; want 1 event, then only boom", events, errs)
	}
}
