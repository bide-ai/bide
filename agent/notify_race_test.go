package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

// two processes deliver the same escalation step at once. The trail's append race and the
// claim must give one observation and one delivery.
func TestNotifyRace_TwoProcessesOneEscalationStep(t *testing.T) {
	for i := 0; i < 50; i++ {
		clk := routeClock()
		def, esc := &fakeSink{fail: 1000}, &fakeSink{}
		j, trail := memJournal(), NewMemNotifyTrail()
		opts := []NotifyOption{WithNotifyReceiver("boss", esc),
			WithNotifyEscalation(NotificationApprovalPending, EscalationStep{Receiver: "boss", Deadline: time.Minute})}
		a := testEngine(t, j, def, trail, clk, "p1", opts...)
		b := testEngine(t, j, def, trail, clk, "p2", opts...)
		ev := approvalEvent("alice")
		_ = a.roundRouted(context.Background(), ev) // observed, never acked
		clk.advance(10 * time.Minute)
		var wg sync.WaitGroup
		for _, e := range []*notifyEngine{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				base := ev
				base.receiver = ""
				_, _ = e.round(context.Background(), e.escalationOf(base, 1))
			}()
		}
		wg.Wait()
		n := 0
		for _, r := range trailOf(t, trail, "r", EscalationKey(ev.key, 1)) {
			if r.Kind == NotifyObserved {
				n++
			}
		}
		if n != 1 || esc.count() > 1 {
			t.Fatalf("iteration %d: %d observations of the step, %d deliveries", i, n, esc.count())
		}
	}
}

type inflightSink struct {
	fakeSink
	in, release chan struct{}
	once        sync.Once
}

func (g *inflightSink) Notify(ctx context.Context, n Notification) error {
	if n.Kind != NotificationResolved {
		g.once.Do(func() { close(g.in); <-g.release })
	}
	return g.fakeSink.Notify(ctx, n)
}

// an event is resolved while its delivery is in flight and a second process scans. The
// trail must stay a valid history (nothing follows a clearing or an acknowledgement) and the
// receiver must end with exactly one all-clear.
func TestNotifyRace_AllClearWhileDeliveryInFlight(t *testing.T) {
	ctx := context.Background()
	clk := resolvedClock()
	j, trail := memJournal(), NewMemNotifyTrail()
	sink := &inflightSink{in: make(chan struct{}), release: make(chan struct{})}
	a := testEngine(t, j, sink, trail, clk, "p1")
	b := testEngine(t, j, sink, trail, clk, "p2")
	ev := haltEvent()
	done := make(chan error, 1)
	go func() { _, err := a.round(ctx, ev); done <- err }()
	<-sink.in
	putResult(t, j, "r", "c1")
	clk.advance(time.Minute)
	_, _ = b.round(ctx, notifyEvent{key: scanKey("r"), scan: true, runID: "r"})
	_ = b.flush(ctx)
	close(sink.release)
	if err := <-done; err != nil {
		t.Logf("delivery round: %v", err)
	}
	clk.advance(time.Minute)
	for _, e := range []*notifyEngine{a, b} {
		_, _ = e.round(ctx, notifyEvent{key: scanKey("r"), scan: true, runID: "r"})
		_ = e.flush(ctx)
	}
	var kinds []NotifyRecordKind
	for _, r := range trailOf(t, trail, "r", ev.key) {
		kinds = append(kinds, r.Kind)
	}
	closed := 0
	for i, k := range kinds {
		if k == NotifyAcked || k == NotifyCleared {
			closed++
			if i != len(kinds)-1 {
				t.Errorf("records follow a %s: %v", k, kinds)
			}
		}
	}
	if closed > 1 {
		t.Errorf("event both acked and cleared: %v", kinds)
	}
	if n := len(resolvedCalls(&sink.fakeSink)); n != 1 {
		t.Errorf("%d all-clears delivered, want 1 (trail %v)", n, kinds)
	}
}
