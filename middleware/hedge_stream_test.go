package middleware_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// lateStreamer is a primary that starts streaming at once, then keeps producing deltas after
// the hedge race is over, without honoring cancellation promptly (as a provider stream with
// buffered body data can). Its later deltas arrive after a faster backup has won and the run
// has finished.
type lateStreamer struct{}

func (lateStreamer) Stream(context.Context, agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit)
	go func() {
		defer close(ch)
		ch <- agent.Emit{Event: agent.TextDelta{Text: "primary-1 "}}
		for _, d := range []string{"primary-2 ", "primary-3"} {
			time.Sleep(40 * time.Millisecond)
			ch <- agent.Emit{Event: agent.TextDelta{Text: d}}
		}
		ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	}()
	return agent.NewStream(ch), nil
}

// A hedged, streamed run must show the caller exactly the response it records, and a losing
// target must never deliver events after the race: here the primary loses to a faster backup,
// then keeps producing deltas after the run has finished and its event stream has closed.
func TestHedge_StreamShowsOnlyTheWinner(t *testing.T) {
	backup := &stubModel{text: "backup", delay: 5 * time.Millisecond}
	a := agent.New(lateStreamer{}, agent.NewMemStore()).Use(middleware.Hedge(0, backup))

	as := a.Stream(context.Background(), "r1", "hi")
	var streamed strings.Builder
	for ev := range as.Events() {
		if me, ok := ev.(agent.ModelEvent); ok {
			if d, ok := me.Event.(agent.TextDelta); ok {
				streamed.WriteString(d.Text)
			}
		}
	}
	final, err := as.Final()
	if err != nil {
		t.Fatalf("Final: %v", err)
	}

	// Give the losing primary time to produce its late deltas. Before the fix, its next delta
	// was sent on the closed event channel, which panics and takes down the process.
	time.Sleep(150 * time.Millisecond)

	var recorded strings.Builder
	for _, p := range final.Parts {
		if tx, ok := p.(agent.Text); ok {
			recorded.WriteString(tx.Text)
		}
	}
	if recorded.String() != "backup" {
		t.Fatalf("recorded answer = %q, want the backup's %q (it answered first)", recorded.String(), "backup")
	}
	if streamed.String() != recorded.String() {
		t.Fatalf("the caller was streamed %q but the run recorded %q", streamed.String(), recorded.String())
	}
}

// earlyStreamer streams one delta at once, then waits and honors cancellation, so it never
// sends late. It isolates the second problem: text streamed from a target that then loses.
type earlyStreamer struct{}

func (earlyStreamer) Stream(ctx context.Context, _ agent.Request) (*agent.Stream, error) {
	ch := make(chan agent.Emit)
	go func() {
		defer close(ch)
		select {
		case ch <- agent.Emit{Event: agent.TextDelta{Text: "primary-partial"}}:
		case <-ctx.Done():
			return
		}
		<-ctx.Done() // slow: it never finishes before the backup wins
	}()
	return agent.NewStream(ch), nil
}

// The caller must never be streamed text from a target that loses the race: what it saw would
// not be what the run recorded.
func TestHedge_StreamMatchesRecordedAnswer(t *testing.T) {
	backup := &stubModel{text: "backup", delay: 20 * time.Millisecond}
	a := agent.New(earlyStreamer{}, agent.NewMemStore()).Use(middleware.Hedge(0, backup))

	as := a.Stream(context.Background(), "r1", "hi")
	var streamed strings.Builder
	for ev := range as.Events() {
		if me, ok := ev.(agent.ModelEvent); ok {
			if d, ok := me.Event.(agent.TextDelta); ok {
				streamed.WriteString(d.Text)
			}
		}
	}
	final, err := as.Final()
	if err != nil {
		t.Fatalf("Final: %v", err)
	}
	var recorded strings.Builder
	for _, p := range final.Parts {
		if tx, ok := p.(agent.Text); ok {
			recorded.WriteString(tx.Text)
		}
	}
	if streamed.String() != recorded.String() {
		t.Fatalf("the caller was streamed %q but the run recorded %q", streamed.String(), recorded.String())
	}
}
