package middleware_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/middleware"
)

// stubModel is a Model that waits `delay`, then either errors or returns a one-token answer. It
// records whether it was ever entered (started) and whether its context was cancelled before it
// could respond (cancelled), so tests can assert launch scheduling and loser cancellation.
type stubModel struct {
	text      string
	delay     time.Duration
	err       error
	started   int32
	cancelled int32
}

func (m *stubModel) Stream(ctx context.Context, _ agent.Request) (*agent.Stream, error) {
	atomic.StoreInt32(&m.started, 1)
	select {
	case <-time.After(m.delay):
	case <-ctx.Done():
		atomic.StoreInt32(&m.cancelled, 1)
		return nil, ctx.Err()
	}
	if m.err != nil {
		return nil, m.err
	}
	ch := make(chan agent.Emit, 2)
	ch <- agent.Emit{Event: agent.TextDelta{Text: m.text}}
	ch <- agent.Emit{Event: agent.Finish{Reason: "stop"}}
	close(ch)
	return agent.NewStream(ch), nil
}

func asHandler(m agent.Model) agent.ModelHandler {
	return func(ctx context.Context, req agent.Request) (agent.Message, agent.Usage, error) {
		s, err := m.Stream(ctx, req)
		if err != nil {
			return agent.Message{}, agent.Usage{}, err
		}
		return s.Message()
	}
}

func eventually(t *testing.T, cond func() bool, why string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within 1s: %s", why)
}

// TestHedge_FirstSuccessWins confirms a slow primary loses to a fast backup once the delay elapses,
// the backup's answer is returned, and the losing primary is cancelled.
func TestHedge_FirstSuccessWins(t *testing.T) {
	primary := &stubModel{text: "primary", delay: 300 * time.Millisecond}
	backup := &stubModel{text: "backup", delay: 10 * time.Millisecond}

	h := middleware.Hedge(20*time.Millisecond, backup)(asHandler(primary))
	msg, _, err := h(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Hedge: %v", err)
	}
	if msg.Text() != "backup" {
		t.Fatalf("expected the fast backup to win, got %q", msg.Text())
	}
	eventually(t, func() bool { return atomic.LoadInt32(&primary.cancelled) == 1 },
		"the losing primary should be cancelled after the backup wins")
}

// TestHedge_DelayHonored confirms a primary that answers before the delay wins and the backup is
// never even launched (no wasted generation).
func TestHedge_DelayHonored(t *testing.T) {
	primary := &stubModel{text: "primary", delay: 10 * time.Millisecond}
	backup := &stubModel{text: "backup", delay: 10 * time.Millisecond}

	h := middleware.Hedge(200*time.Millisecond, backup)(asHandler(primary))
	msg, _, err := h(context.Background(), agent.Request{})
	if err != nil {
		t.Fatalf("Hedge: %v", err)
	}
	if msg.Text() != "primary" {
		t.Fatalf("expected the primary to win before the delay, got %q", msg.Text())
	}
	if atomic.LoadInt32(&backup.started) != 0 {
		t.Fatalf("backup should not have launched: primary answered before the hedge delay")
	}
}

// TestHedge_FastFailover confirms a primary that errors brings the backups forward immediately,
// rather than waiting out the (long) hedge delay.
func TestHedge_FastFailover(t *testing.T) {
	primary := &stubModel{delay: 5 * time.Millisecond, err: errors.New("primary down")}
	backup := &stubModel{text: "backup", delay: 10 * time.Millisecond}

	start := time.Now()
	h := middleware.Hedge(2*time.Second, backup)(asHandler(primary)) // long delay, must be short-circuited
	msg, _, err := h(context.Background(), agent.Request{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Hedge: %v", err)
	}
	if msg.Text() != "backup" {
		t.Fatalf("expected failover to the backup, got %q", msg.Text())
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("failover waited out the delay (%v); it should fire backups on the primary error", elapsed)
	}
}

// TestHedge_AllFail confirms that when every target fails, the joined error carries all causes.
func TestHedge_AllFail(t *testing.T) {
	e1, e2 := errors.New("primary boom"), errors.New("backup boom")
	primary := &stubModel{delay: 2 * time.Millisecond, err: e1}
	backup := &stubModel{delay: 2 * time.Millisecond, err: e2}

	h := middleware.Hedge(0, backup)(asHandler(primary))
	_, _, err := h(context.Background(), agent.Request{})
	if err == nil {
		t.Fatal("expected an error when all targets fail")
	}
	if !errors.Is(err, e1) || !errors.Is(err, e2) {
		t.Fatalf("joined error should carry both causes, got %v", err)
	}
}

// TestHedge_NoBackups confirms Hedge is a pass-through with no backups (safe to wire always).
func TestHedge_NoBackups(t *testing.T) {
	primary := &stubModel{text: "solo", delay: 1 * time.Millisecond}
	h := middleware.Hedge(50 * time.Millisecond)(asHandler(primary))
	msg, _, err := h(context.Background(), agent.Request{})
	if err != nil || msg.Text() != "solo" {
		t.Fatalf("pass-through failed: msg=%q err=%v", msg.Text(), err)
	}
}
