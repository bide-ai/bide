package plan

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

type planSink struct {
	mu    sync.Mutex
	calls []agent.Notification
}

func (s *planSink) Notify(_ context.Context, n agent.Notification) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, n)
	return nil
}

func (s *planSink) snapshot() []agent.Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]agent.Notification(nil), s.calls...)
}

// A flow run that stops at a gate tells the notifier once, however often the run is driven into the
// same gate again, and names the node, the policy and the run.
func TestFlowNotifier_AGateIsNotifiedOnce(t *testing.T) {
	ctx := context.Background()
	_, vf := approvers("ops", "finance", "risk")
	trail, sink := agent.NewMemNotifyTrail(), &planSink{}
	j := agenttest.MemJournal()
	refunds := 0
	b := New[string, string]("refunds").WithApproverVerifiers(vf).WithNotifier(sink, trail)
	pay := b.Step("refund", func(_ context.Context, o string) (string, error) { refunds++; return o, nil },
		Approval(&agent.ApprovalPolicy{Need: 2, Approvers: []string{"ops", "finance", "risk"}}))
	_ = pay
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := f.Run(ctx, j, "r1", "order-42"); !errors.As(err, new(*agent.ApprovalPending)) {
			t.Fatalf("Run = %v", err)
		}
		if err := f.FlushNotifications(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("notified %d times over 3 drives, want 1", len(got))
	}
	n := got[0]
	if n.Kind != agent.NotificationApprovalPending || n.RunID != "r1" || n.Approval == nil || n.Approval.ToolUseID != "node:refund" || n.Approval.ToolName != "refund" || n.Approval.Need != 2 || n.Approval.Of != 3 {
		t.Fatalf("notification = %+v / %+v", n, n.Approval)
	}
	if refunds != 0 {
		t.Fatal("the gated node ran")
	}
}

// A node whose failed attempt may have taken effect halts the next drive; the halt is notified.
func TestFlowNotifier_AHaltIsNotified(t *testing.T) {
	ctx := context.Background()
	trail, sink := agent.NewMemNotifyTrail(), &planSink{}
	j := agenttest.MemJournal()
	b := New[string, string]("pay").WithNotifier(sink, trail)
	calls := 0
	b.Step("charge", func(context.Context, string) (string, error) { calls++; return "", errors.New("connection reset") })
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Run(ctx, j, "r1", "x"); err == nil || errors.As(err, new(*agent.OutcomeUnknown)) {
		t.Fatalf("first Run = %v; want the body's error", err)
	}
	if err := f.FlushNotifications(ctx); err != nil || len(sink.snapshot()) != 0 {
		t.Fatalf("a plain error was notified: %v, %+v", err, sink.snapshot())
	}
	for range 2 {
		if _, err := f.Run(ctx, j, "r1", "x"); !errors.As(err, new(*agent.OutcomeUnknown)) {
			t.Fatalf("Run = %v; want a halt", err)
		}
		if err := f.FlushNotifications(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := sink.snapshot()
	if len(got) != 1 || got[0].Kind != agent.NotificationOutcomeUnknown || got[0].Halt == nil || got[0].Halt.Op.Kind != agent.OpStep || got[0].Halt.Op.ID != "node:charge" {
		t.Fatalf("notifications = %+v", got)
	}
	if calls != 1 {
		t.Fatalf("the body ran %d times, want 1", calls)
	}
}

// One flow run against two journals notifies each: the notifier is bound to the journal it reads.
func TestFlowNotifier_EachJournalHasItsOwn(t *testing.T) {
	ctx := context.Background()
	trail, sink := agent.NewMemNotifyTrail(), &planSink{}
	b := New[int, int]("g").WithNotifier(sink, trail)
	b.Step("n", func(_ context.Context, n int) (int, error) { return n, nil }, Approval(agent.SingleApproval()))
	f, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{"a", "b"} {
		if _, err := f.Run(ctx, agenttest.MemJournal(), run, 1); !errors.As(err, new(*agent.ApprovalPending)) {
			t.Fatal(err)
		}
	}
	if err := f.FlushNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	if got := sink.snapshot(); len(got) != 2 {
		t.Fatalf("%d notifications, want one per run: %+v", len(got), got)
	}
}

func TestFlowNotifier_BuildRefusesABadBinding(t *testing.T) {
	for name, mk := range map[string]func(*Builder[int, int]) *Builder[int, int]{
		"nil notifier": func(b *Builder[int, int]) *Builder[int, int] { return b.WithNotifier(nil, agent.NewMemNotifyTrail()) },
		"nil trail":    func(b *Builder[int, int]) *Builder[int, int] { return b.WithNotifier(&planSink{}, nil) },
		"bad option": func(b *Builder[int, int]) *Builder[int, int] {
			return b.WithNotifier(&planSink{}, agent.NewMemNotifyTrail(), agent.WithNotifyDeadline(0))
		},
	} {
		b := mk(New[int, int]("g"))
		b.Step("n", func(_ context.Context, n int) (int, error) { return n, nil })
		if _, err := b.Build(); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), `"g"`) {
			t.Errorf("%s: Build = %v; want an ErrConfig naming the flow", name, err)
		}
	}
}

// The binding is runtime only: it changes neither the digest nor a flow without one.
func TestFlowNotifier_IsNotPartOfTheTopology(t *testing.T) {
	build := func(withNotifier bool) *Flow[int, int] {
		b := New[int, int]("g")
		if withNotifier {
			b.WithNotifier(&planSink{}, agent.NewMemNotifyTrail())
		}
		b.Step("n", func(_ context.Context, n int) (int, error) { return n, nil })
		f, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if build(true).Digest() != build(false).Digest() {
		t.Fatal("WithNotifier changed the flow's digest")
	}
	if err := build(false).FlushNotifications(context.Background()); err != nil {
		t.Fatalf("Flush on a flow with no notifier: %v", err)
	}
}

func TestFlowNotifier_LoadedFlow(t *testing.T) {
	ctx := context.Background()
	trail, sink := agent.NewMemNotifyTrail(), &planSink{}
	_, vf := approvers("ops", "finance")
	cfg := strings.Replace(approvalNodeConfig, "APPROVAL", `{"need": 2, "approvers": ["ops", "finance"]}`, 1)
	flow, err := Load[int, int]([]byte(cfg), approvalRegistry(t), WithLoadedApproverVerifiers(vf), WithLoadedNotifier(sink, trail))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := flow.Run(ctx, agenttest.MemJournal(), "r", 1); !errors.As(err, new(*agent.ApprovalPending)) {
		t.Fatal(err)
	}
	if err := flow.FlushNotifications(ctx); err != nil || len(sink.snapshot()) != 1 {
		t.Fatalf("%v %+v", err, sink.snapshot())
	}
}
