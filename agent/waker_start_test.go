package agent

import (
	"context"
	"testing"
	"time"
)

// After cancelling Start's context, a caller can wait for the loop to stop: the channel Start
// returns closes only once a resume that was in flight has returned, so the store it writes to
// can be closed safely afterwards.
func TestMemWaker_StartReportsWhenStopped(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var finished bool
	w := NewMemWaker(func(context.Context, string) error {
		close(entered)
		<-release // a resume still writing its journal
		finished = true
		return nil
	})
	w.Schedule(context.Background(), Wake{RunID: "r1", Name: "nap", FireAt: time.Now().Add(-time.Second)})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := w.Start(ctx, time.Millisecond, nil)
	<-entered
	cancel()
	select {
	case <-stopped:
		t.Fatal("Start reported stopped while a resume was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Start never reported stopped")
	}
	if !finished {
		t.Fatal("stopped closed before the in-flight resume returned")
	}
}
