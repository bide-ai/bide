package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// streamFinishes runs a streamed run and returns the Finish events its model calls sent.
func streamFinishes(t *testing.T, a *Agent, runID string) ([]Finish, *AgentStream) {
	t.Helper()
	as := a.Stream(context.Background(), runID, "go")
	var out []Finish
	for ev := range as.Events() {
		if me, ok := ev.(ModelEvent); ok {
			if f, ok := me.Event.(Finish); ok {
				out = append(out, f)
			}
		}
	}
	return out, as
}

// A replayed run reports the same usage as the original, per turn in its journal and in total
// in its RunResult.
func TestReplay_ReportsRecordedUsage(t *testing.T) {
	ctx := context.Background()
	u1 := Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2, CacheWriteTokens: 1}
	u2 := Usage{InputTokens: 20, OutputTokens: 8, CacheWriteTokens: 3}

	rec := NewMemStore()
	var calls1 int
	tool1 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls1}
	m := &scriptModel{turns: [][]Emit{toolTurnWithUsage("c1", "lookup", `{"q":"x"}`, u1), textTurnWithUsage("final", u2)}}
	orig, err := New(m, rec, tool1).RunResult(ctx, "run", "go")
	if err != nil {
		t.Fatal(err)
	}

	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewMemStore()
	var calls2 int
	tool2 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls2}
	replayed, err := New(rm, fresh, tool2).RunResult(ctx, "run", "go")
	if err != nil {
		t.Fatalf("replay run: %v", err)
	}
	if replayed.Usage != orig.Usage || replayed.Turns != orig.Turns {
		t.Fatalf("replayed usage %+v over %d turns, want %+v over %d", replayed.Usage, replayed.Turns, orig.Usage, orig.Turns)
	}
	journalsEqual(t, rec, fresh, "run")
}

// A run stopped by WithTokenBudget stops at the same point on replay, with the same error and
// the same journal, rather than asking the replay model for a turn that was never recorded.
func TestReplay_TokenBudgetStopsAtSamePoint(t *testing.T) {
	ctx := context.Background()
	rec := NewMemStore()
	_, origErr := New(&meteredModel{u: turnUsage}, rec, lookupTool(func() {})).WithTokenBudget(100).Run(ctx, "run", "q")
	if !errors.Is(origErr, ErrBudgetExceeded) {
		t.Fatalf("setup: err = %v, want ErrBudgetExceeded", origErr)
	}

	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewMemStore()
	_, replayErr := New(rm, fresh, lookupTool(func() {})).WithTokenBudget(100).Run(ctx, "run", "q")
	if replayErr == nil || replayErr.Error() != origErr.Error() {
		t.Fatalf("replay err = %v, want %v", replayErr, origErr)
	}
	journalsEqual(t, rec, fresh, "run")
}

// Each replayed model call ends with the Finish a stream consumer saw live: the recorded usage,
// and the reason derived from the turn (see finishReason).
func TestReplay_StreamedFinishMatchesLive(t *testing.T) {
	ctx := context.Background()
	u1 := Usage{InputTokens: 10, OutputTokens: 5}
	u2 := Usage{InputTokens: 20, OutputTokens: 8, CacheReadTokens: 4}

	rec := NewMemStore()
	var calls1 int
	tool1 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls1}
	m := &scriptModel{turns: [][]Emit{toolTurnWithUsage("c1", "lookup", `{"q":"x"}`, u1), textTurnWithUsage("final", u2)}}
	live, as := streamFinishes(t, New(m, rec, tool1), "run")
	if _, err := as.Final(); err != nil {
		t.Fatal(err)
	}
	want := []Finish{{Reason: "tool_use", Usage: u1}, {Reason: "stop", Usage: u2}}
	if !reflect.DeepEqual(live, want) {
		t.Fatalf("setup: live finishes = %+v, want %+v", live, want)
	}

	rm, err := Replay(ctx, rec, "run")
	if err != nil {
		t.Fatal(err)
	}
	var calls2 int
	tool2 := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls2}
	replayed, as := streamFinishes(t, New(rm, NewMemStore(), tool2), "run")
	if _, err := as.Final(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, live) {
		t.Fatalf("replayed finishes = %+v, want %+v", replayed, live)
	}
}

func journalsEqual(t *testing.T, a, b Durable, runID string) {
	t.Helper()
	ra, err := a.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := b.History(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	// Salt is fresh random bytes per record (event-leaf.v2), so it never matches across runs.
	for i := range ra {
		ra[i].Salt = nil
	}
	for i := range rb {
		rb[i].Salt = nil
	}
	if !reflect.DeepEqual(ra, rb) {
		t.Fatalf("replayed journal differs:\n orig   %s\n replay %s", dumpRecords(ra), dumpRecords(rb))
	}
}

func dumpRecords(rs []Record) string {
	var out []byte
	for _, r := range rs {
		b, _ := EncodeRecord(r)
		out = append(append(out, b...), '\n')
	}
	return string(out)
}
