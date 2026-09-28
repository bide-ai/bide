package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// A reconciler that resolves a halt before the provider's record has settled would re-fire
// the effect. WithMinHaltAge refuses to resolve a halt younger than the grace period,
// measured from the attempt marker, so early resolution cannot happen even by mistake.
func TestResolveHalt_MinHaltAge(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seed := func() *MemStore {
		s := NewMemStore()
		// Attempt marker stamped at base, no result: an unknown-outcome halt.
		_, _ = s.Do(ctx, "r", "attempt:c1", func(context.Context) (Record, error) {
			return Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: base.UnixMilli()}, nil
		})
		return s
	}

	// Too soon: 30s after the attempt, 60s grace -> *HaltTooYoung, nothing recorded.
	s := seed()
	err := ResolveHalt(ctx, s, "r", "c1", "charged", false,
		WithMinHaltAge(60*time.Second), WithNow(func() time.Time { return base.Add(30 * time.Second) }))
	var young *HaltTooYoung
	if !errors.As(err, &young) {
		t.Fatalf("err = %v, want *HaltTooYoung", err)
	}
	if h, _ := s.History(ctx, "r"); hasResult(h, "c1") {
		t.Fatal("a too-young halt must not record a result")
	}

	// Past the grace: 90s after the attempt -> resolves and records the result.
	s = seed()
	if err := ResolveHalt(ctx, s, "r", "c1", "charged", false,
		WithMinHaltAge(60*time.Second), WithNow(func() time.Time { return base.Add(90 * time.Second) })); err != nil {
		t.Fatalf("resolve after grace: %v", err)
	}
	if h, _ := s.History(ctx, "r"); !hasResult(h, "c1") {
		t.Fatal("resolve after grace should record the result")
	}

	// No attempt timestamp to measure against: fail closed rather than resolve blind.
	bare := NewMemStore()
	if err := ResolveHalt(ctx, bare, "r", "c1", "x", false, WithMinHaltAge(time.Second)); err == nil {
		t.Fatal("min-halt-age with no attempt marker should error, not resolve")
	}
}

// A reconciled resolution is journaled distinctly (Reconciled + Evidence, signed with the
// record), so a later reader can tell a reconciled outcome from a clean one and re-check
// what the reconciler read. A plain resolve carries no such annotation.
func TestResolveHalt_Evidence(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()

	ev := map[string]string{"message_id": "msg_123", "source": "provider log"}
	if err := ResolveHalt(ctx, s, "r", "c1", "sent", false, WithEvidence(ev)); err != nil {
		t.Fatalf("reconciled resolve: %v", err)
	}
	rec := resultFor(t, s, "r", "c1")
	if !rec.Reconciled {
		t.Fatal("WithEvidence should mark the result Reconciled")
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Evidence, &got); err != nil || got["message_id"] != "msg_123" {
		t.Fatalf("evidence = %s (err %v), want the reconciler's record", rec.Evidence, err)
	}

	// A plain resolve is a clean outcome: no reconciliation annotation.
	if err := ResolveHalt(ctx, s, "r", "c2", "sent", false); err != nil {
		t.Fatalf("plain resolve: %v", err)
	}
	rec2 := resultFor(t, s, "r", "c2")
	if rec2.Reconciled || rec2.Evidence != nil {
		t.Fatalf("plain resolve must not be reconciled (got Reconciled=%v Evidence=%s)", rec2.Reconciled, rec2.Evidence)
	}
}

// The halt surfaces when the effect was attempted, so a reconciler can honor a grace period.
func TestResumeHalt_AttemptedAt(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	at := time.Date(2026, 2, 2, 3, 4, 5, 0, time.UTC)
	asst := Message{Role: RoleAssistant, Parts: []Part{ToolUse{ID: "c1", Name: "charge", Args: json.RawMessage(`{}`)}}}
	_, _ = store.Do(ctx, "r", "@llm/0", func(context.Context) (Record, error) {
		return Record{Kind: StepModel, Message: &asst}, nil
	})
	_, _ = store.Do(ctx, "r", "attempt:c1", func(context.Context) (Record, error) {
		return Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: at.UnixMilli()}, nil
	})

	var calls int
	write := &countingTool{name: "charge", safety: Safety{}, calls: &calls} // not retry-safe
	_, err := New(&scriptModel{}, store, write).Run(ctx, "r", "hi")

	var halt *ResumeHalt
	if !errors.As(err, &halt) {
		t.Fatalf("err = %v, want *ResumeHalt", err)
	}
	if !halt.AttemptedAt.Equal(at) {
		t.Fatalf("halt.AttemptedAt = %v, want %v", halt.AttemptedAt, at)
	}
}

func hasResult(recs []Record, id string) bool {
	for _, r := range recs {
		if r.Kind == StepToolResult && r.ToolUseID == id {
			return true
		}
	}
	return false
}

func resultFor(t *testing.T, s Durable, runID, id string) Record {
	t.Helper()
	recs, err := s.History(context.Background(), runID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	for _, r := range recs {
		if r.Kind == StepToolResult && r.ToolUseID == id {
			return r
		}
	}
	t.Fatalf("no tool result for %s", id)
	return Record{}
}
