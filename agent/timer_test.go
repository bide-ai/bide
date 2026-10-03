package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// sleepModel calls the "wait" tool on the first turn, then answers once it sees the tool result.
type sleepModel struct{}

func (sleepModel) Stream(_ context.Context, req Request) (*Stream, error) {
	hasToolResult := false
	for _, m := range req.Messages {
		if m.Role == RoleTool {
			hasToolResult = true
		}
	}
	ch := make(chan Emit, 2)
	if !hasToolResult {
		ch <- Emit{Event: ToolCallDelta{Index: 0, ID: "w", Name: "wait", ArgsFragment: []byte(`{}`)}}
		ch <- Emit{Event: Finish{Reason: "tool_use"}}
	} else {
		ch <- Emit{Event: TextDelta{Text: "done"}}
		ch <- Emit{Event: Finish{Reason: "stop"}}
	}
	close(ch)
	return NewStream(ch), nil
}

// waitTool sleeps one hour (durably) then reports it waited. ReadOnly, so it is retry-safe.
func waitTool() Tool {
	return MustFunc("wait", "wait an hour", func(ctx context.Context, _ struct{}) (string, error) {
		if err := Sleep(ctx, "w1", time.Hour); err != nil {
			return "", err
		}
		return "waited", nil
	}, WithSafety(Safety{ReadOnly: true}))
}

// TestSleep_PausesAndResumes confirms a durable timer pauses the run, keeps the same wake time
// across resumes (at-most-once), stays paused before it is due, and resumes after.
func TestSleep_PausesAndResumes(t *testing.T) {
	var clk int64 = 1000 // seconds since epoch, controllable
	now := func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) }
	ctx := contextWithClock(context.Background(), now)

	a := mustNew(sleepModel{}, memJournal(), WithTools(waitTool()))

	// First run: the wait tool sleeps, so the run pauses with *Sleeping at now+1h.
	_, err := a.Run(ctx, "r1", UserText("go"))
	var slp *TimerPending
	if !errors.As(err, &slp) {
		t.Fatalf("expected *Sleeping, got %v", err)
	}
	wake := time.Unix(1000+3600, 0)
	if !slp.FireAt.Equal(wake) {
		t.Fatalf("wake time = %s, want %s", slp.FireAt, wake)
	}

	// Resume 30 minutes in: still before the wake time, and the wake time did NOT move (at-most-once).
	atomic.StoreInt64(&clk, 1000+1800)
	_, err = a.Run(ctx, "r1", UserText("go"))
	if !errors.As(err, &slp) {
		t.Fatalf("should still be sleeping at +30m, got %v", err)
	}
	if !slp.FireAt.Equal(wake) {
		t.Fatalf("wake time drifted to %s on resume; it must stay %s", slp.FireAt, wake)
	}

	// At the wake time: the run resumes and completes.
	atomic.StoreInt64(&clk, 1000+3600)
	res, err := a.Run(ctx, "r1", UserText("go"))
	if err != nil {
		t.Fatalf("run should resume once due, got %v", err)
	}
	msg := res.Message
	if msg.Text() != "done" {
		t.Fatalf("resumed run should finish, got %q", msg.Text())
	}
}

// TestMemWaker_FiresDueRun confirms the reference waker resumes a sleeping run once its timer is due.
func TestMemWaker_FiresDueRun(t *testing.T) {
	var clk int64 = 1000
	now := func() time.Time { return time.Unix(atomic.LoadInt64(&clk), 0) }
	a := mustNew(sleepModel{}, memJournal(), WithTools(waitTool()))

	var w *MemWaker
	w = NewMemWaker(func(ctx context.Context, runID string) error {
		rctx := contextWithWaker(contextWithClock(ctx, now), w) // resumed run can reschedule if it sleeps again
		_, err := a.Run(rctx, runID, UserText("go"))
		return err
	})
	ctx := contextWithWaker(contextWithClock(context.Background(), now), w)

	// First run pauses and registers a wake with the waker.
	if _, err := a.Run(ctx, "r1", UserText("go")); !errorsIsSleeping(err) {
		t.Fatalf("expected the run to sleep, got %v", err)
	}

	// Not due yet: Fire resumes nothing.
	if n, err := w.Fire(context.Background(), now()); n != 0 || err != nil {
		t.Fatalf("nothing should fire before the wake time, fired %d err %v", n, err)
	}

	// Advance past the wake time and Fire: the run resumes to completion.
	atomic.StoreInt64(&clk, 1000+3600)
	n, err := w.Fire(context.Background(), now())
	if err != nil || n != 1 {
		t.Fatalf("waker should resume exactly one run, fired %d err %v", n, err)
	}
	// The resumed run completed: a replay now returns the final answer with no pause.
	res, err := a.Run(context.Background(), "r1", UserText("go"), WithClock(now))
	var msg Message
	if res != nil {
		msg = res.Message
	}
	if err != nil || msg.Text() != "done" {
		t.Fatalf("run should be complete after the waker fired it, got %q err %v", msg.Text(), err)
	}
}

func errorsIsSleeping(err error) bool {
	var slp *TimerPending
	return errors.As(err, &slp)
}
