package plan

// Adversarial review of the flow notifier (notify-flow, 3f6cfbbd). Tests named TestReview_ fail on
// the branch and reproduce a finding; tests named TestReviewHeld_ pass and record what held up.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// reviewSink blocks its first call until release is closed and fails it; later calls succeed. It
// records the keys of the calls that succeeded.
type reviewSink struct {
	mu      sync.Mutex
	n       int
	block   bool
	failAll bool
	release chan struct{}
	ok      []string
}

func (s *reviewSink) Notify(ctx context.Context, n agent.Notification) error {
	s.mu.Lock()
	s.n++
	first := s.n == 1
	s.mu.Unlock()
	if s.failAll {
		return errors.New("sink down")
	}
	if first && s.block {
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return agent.ErrNotifyPermanent
	}
	s.mu.Lock()
	s.ok = append(s.ok, n.Key)
	s.mu.Unlock()
	return nil
}

func (s *reviewSink) delivered(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.ok {
		if k == key {
			return true
		}
	}
	return false
}

// twoGates is a flow a -> b with a single-approver gate on each node.
func twoGates(t *testing.T, sink agent.Notifier, trail agent.NotifyTrail, opts ...agent.NotifyOption) *Flow[int, int] {
	t.Helper()
	b := New[int, int]("two-gates").WithNotifier(sink, trail, opts...)
	a := b.Step("a", func(_ context.Context, n int) (int, error) { return n + 1, nil }, Approval(agent.SingleApproval()))
	bb := b.Step("b", func(_ context.Context, n int) (int, error) { return n * 10, nil }, Approval(agent.SingleApproval()))
	b.Edge(a, bb)
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func pollFor(d time.Duration, cond func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// FL1. A flow gate's event is current until the run ends: current() looks for
// ToolResultStep("node:a"), which a flow never writes (its gate records approval:<key> and the
// node's result is node:a). So a gate whose first delivery failed is delivered as "waiting for
// approval" after it was approved and its node ran, while the run waits at the next gate.
func TestReview_FlowAPassedGateIsNotDeliveredAsPending(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	sink := &reviewSink{block: true, release: make(chan struct{})}
	f := twoGates(t, sink, agent.NewMemNotifyTrail(), agent.WithNotifyRetry(5*time.Millisecond, 5*time.Millisecond))
	defer func() { _ = f.FlushNotifications(ctx) }()

	if _, err := f.Run(ctx, j, "r1", 1); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatalf("first Run = %v", err)
	}
	if !pollFor(time.Second, func() bool { sink.mu.Lock(); defer sink.mu.Unlock(); return sink.n == 1 }) {
		t.Fatal("setup: gate a was not sent")
	}
	if err := agent.Approve(ctx, j, "r1", "node:a", true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, j, "r1", 1); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatalf("second Run = %v; want the pause at gate b", err)
	}
	close(sink.release) // gate a's first send fails now; its retry follows
	gateA := agent.ApprovalKey("r1", "node:a", 0)
	if pollFor(500*time.Millisecond, func() bool { return sink.delivered(gateA) }) {
		t.Fatal("gate a was delivered as waiting for approval after it was approved and its node had run")
	}
}

// FL2. The sweep reports a flow gate as pending while its approval-pending record stands and no
// ToolResultStep exists, which for a flow is always. A run that passed gate a long ago and now
// waits at gate b is reported at both.
func TestReview_FlowSweepReportsOnlyTheGateThatIsWaiting(t *testing.T) {
	ctx := context.Background()
	j := agenttest.MemJournal()
	f := twoGates(t, &reviewSink{}, agent.NewMemNotifyTrail())
	if _, err := f.Run(ctx, j, "r1", 1); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatal(err)
	}
	if err := agent.Approve(ctx, j, "r1", "node:a", true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, j, "r1", 1); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatal(err)
	}
	_ = f.FlushNotifications(ctx)

	sink := &reviewSink{}
	if err := agent.NotifyPending(ctx, j, sink, agent.NewMemNotifyTrail()); err != nil {
		t.Fatal(err)
	}
	if sink.delivered(agent.ApprovalKey("r1", "node:a", 0)) {
		t.Fatalf("the sweep reported gate a, which passed before the run reached gate b: %v", sink.ok)
	}
}

// FL3. The guide says to call FlushNotifications before the process exits. It returns nil when
// every delivery failed: the failed event is left to a backoff no process will run, and the caller
// is not told. For a flow app with no RecoverLoop or NotifyPending, that is a silent drop.
func TestReview_FlowFlushReportsUndeliveredEvents(t *testing.T) {
	ctx := context.Background()
	f := twoGates(t, &reviewSink{failAll: true}, agent.NewMemNotifyTrail())
	if _, err := f.Run(ctx, agenttest.MemJournal(), "r1", 1); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatal(err)
	}
	if err := f.FlushNotifications(ctx); err == nil {
		t.Fatal("FlushNotifications returned nil although the gate's delivery failed and nothing will retry it")
	}
}

// FL4. The flow keeps one notifier (an engine and, until flushed, a goroutine) per *agent.Journal
// value, forever. An app that wraps one store in a new Journal per request grows the map without
// bound, and the per-journal engines over the same store are separate deliverers of the same
// events.
func TestReview_FlowNotifiersPerJournalAreBounded(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	f := twoGates(t, &reviewSink{}, agent.NewMemNotifyTrail())
	for i := range 50 {
		j, err := agent.NewJournal(store)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Run(ctx, j, "r"+strings.Repeat("x", i), 1)
	}
	_ = f.FlushNotifications(ctx)
	f.core.notify.mu.Lock()
	n := len(f.core.notify.per)
	f.core.notify.mu.Unlock()
	if n > 1 {
		t.Fatalf("one store wrapped in 50 Journals left %d notifiers held by the flow", n)
	}
}

// Held: a FlushNotifications whose context ends while a delivery hangs returns the context's
// error; the first-error loop does not lose it.
func TestReviewHeld_FlowFlushReturnsTheDeadline(t *testing.T) {
	ctx := context.Background()
	sink := &reviewSink{block: true, release: make(chan struct{})}
	defer close(sink.release)
	f := twoGates(t, sink, agent.NewMemNotifyTrail())
	// Two journals, so the loop runs over two notifiers.
	for _, run := range []string{"a", "b"} {
		_, _ = f.Run(ctx, agenttest.MemJournal(), run, 1)
	}
	fctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := f.FlushNotifications(fctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Flush = %v, want the deadline", err)
	}
}

// Held: two processes (two Flows built separately) driving one run over one store and one trail,
// concurrently and repeatedly, deliver the gate once.
func TestReviewHeld_FlowExactlyOnceAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	store, trail, sink := agent.NewMemStore(), agent.NewMemNotifyTrail(), &reviewSink{}
	p1, p2 := twoGates(t, sink, trail), twoGates(t, sink, trail)
	var wg sync.WaitGroup
	for _, f := range []*Flow[int, int]{p1, p2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, _ := agent.NewJournal(store)
			for range 5 {
				_, _ = f.Run(ctx, j, "r1", 1)
			}
			_ = f.FlushNotifications(ctx)
		}()
	}
	wg.Wait()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.ok) != 1 {
		t.Fatalf("%d deliveries, want 1: %v", len(sink.ok), sink.ok)
	}
}

// Held: a loaded gated flow's digest does not change with the notifier.
func TestReviewHeld_LoadedFlowDigestIgnoresTheNotifier(t *testing.T) {
	_, vf := approvers("ops", "finance")
	cfg := strings.Replace(approvalNodeConfig, "APPROVAL", `{"need": 2, "approvers": ["ops", "finance"]}`, 1)
	with, err := Load[int, int]([]byte(cfg), approvalRegistry(t), WithLoadedApproverVerifiers(vf), WithLoadedNotifier(&reviewSink{}, agent.NewMemNotifyTrail()))
	if err != nil {
		t.Fatal(err)
	}
	without, err := Load[int, int]([]byte(cfg), approvalRegistry(t), WithLoadedApproverVerifiers(vf))
	if err != nil {
		t.Fatal(err)
	}
	if with.Digest() != without.Digest() {
		t.Fatal("the notifier changed a loaded flow's digest")
	}
}
