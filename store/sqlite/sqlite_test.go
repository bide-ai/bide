package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	agent "github.com/blackwell-systems/bide"
)

// --- minimal test doubles (mirrors the ones in the agent package) ---

type scriptModel struct {
	turns [][]agent.Emit
	i     int
}

func (m *scriptModel) Stream(context.Context, agent.Request) (*agent.Stream, error) {
	turn := m.turns[m.i]
	m.i++
	ch := make(chan agent.Emit, len(turn))
	for _, e := range turn {
		ch <- e
	}
	close(ch)
	return agent.NewStream(ch), nil
}

func toolTurn(id, name, args string) []agent.Emit {
	return []agent.Emit{
		{Event: agent.ToolCallDelta{Index: 0, ID: id, Name: name, ArgsFragment: json.RawMessage(args)}},
		{Event: agent.Finish{Reason: "tool_use"}},
	}
}
func textTurn(s string) []agent.Emit {
	return []agent.Emit{{Event: agent.TextDelta{Text: s}}, {Event: agent.Finish{Reason: "stop"}}}
}
func errTurn(err error) []agent.Emit { return []agent.Emit{{Err: err}} }

type countingTool struct {
	calls *int
}

func (t *countingTool) Name() string                { return "lookup" }
func (t *countingTool) Description() string         { return "" }
func (t *countingTool) Safety() agent.Safety        { return agent.Safety{ReadOnly: true} }
func (t *countingTool) ArgsSchema() json.RawMessage { return nil }
func (t *countingTool) Call(context.Context, json.RawMessage) (json.RawMessage, error) {
	*t.calls++
	return json.RawMessage(`{"ok":true}`), nil
}

// The moat, persisted: crash mid-run, REOPEN the DB file (a fresh process), resume, and
// the completed tool must not re-run.
func TestSQLite_DurableResumeAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	var calls int
	tool := &countingTool{calls: &calls}
	ctx := context.Background()

	// First process: tool runs, result persisted, then the model "crashes".
	store1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	crashy := &scriptModel{turns: [][]agent.Emit{toolTurn("c1", "lookup", `{"q":"x"}`), errTurn(errors.New("boom"))}}
	if _, err := agent.New(crashy, store1, tool).Run(ctx, "r1", "hi"); err == nil {
		t.Fatal("expected crash on first attempt")
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times before crash, want 1", calls)
	}
	store1.Close() // process exits

	// Second process: reopen the SAME file and resume.
	store2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	recovered := &scriptModel{turns: [][]agent.Emit{textTurn("final")}}
	out, err := agent.New(recovered, store2, tool).Run(ctx, "r1", "hi")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if textOf(out) != "final" {
		t.Fatalf("resumed answer = %q, want final", textOf(out))
	}
	if calls != 1 {
		t.Fatalf("tool re-ran after reopen: %d times, want 1 (durability not persisted)", calls)
	}
}

// Do memoizes by (runID, name); History reads them back in order.
func TestSQLite_DoMemoizesAndHistory(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	var runs int
	mk := func() (agent.Record, error) {
		runs++
		return agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`1`)}, nil
	}
	if _, err := store.Do(ctx, "r1", "step", func(context.Context) (agent.Record, error) { return mk() }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Do(ctx, "r1", "step", func(context.Context) (agent.Record, error) { return mk() }); err != nil {
		t.Fatal(err)
	}
	if runs != 1 {
		t.Fatalf("fn ran %d times, want 1 (not memoized)", runs)
	}
	h, err := store.History(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 || h[0].Name != "step" {
		t.Fatalf("history = %+v, want one step named 'step'", h)
	}
}

// Recover works on the sqlite store now that it implements agent.Lister: crash a run
// mid-flight (a persisted tool step but no completion marker), reopen the file in a fresh
// process, and Recover must enumerate the in-flight run and re-drive it to completion
// without the old "needs a store that implements Lister" error.
func TestSQLite_RecoverReDrivesInFlightRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	var calls int
	tool := &countingTool{calls: &calls}
	ctx := context.Background()

	// First process: the tool step is persisted, then the model crashes before the run
	// completes. The journal now holds an in-flight run (no run:complete marker).
	store1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	crashy := &scriptModel{turns: [][]agent.Emit{toolTurn("c1", "lookup", `{"q":"x"}`), errTurn(errors.New("boom"))}}
	if _, err := agent.New(crashy, store1, tool).Run(ctx, "r1", "hi"); err == nil {
		t.Fatal("expected crash on first attempt")
	}
	store1.Close() // process exits

	// Second process: reopen the SAME file. Recover enumerates runs via Lister, finds r1
	// incomplete, and re-drives it through resume, which finishes the run.
	store2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	var resumed []string
	resume := func(ctx context.Context, runID string) error {
		resumed = append(resumed, runID)
		recovered := &scriptModel{turns: [][]agent.Emit{textTurn("final")}}
		_, err := agent.New(recovered, store2, tool).Run(ctx, runID, "hi")
		return err
	}
	n, err := agent.Recover(ctx, store2, resume)
	if err != nil {
		t.Fatalf("Recover on sqlite store: %v", err)
	}
	if n != 1 {
		t.Fatalf("Recover re-drove %d runs, want 1", n)
	}
	if len(resumed) != 1 || resumed[0] != "r1" {
		t.Fatalf("resumed = %v, want [r1]", resumed)
	}
	if calls != 1 {
		t.Fatalf("tool ran %d times, want 1 (completed step re-ran on recovery)", calls)
	}

	// The re-driven run is now complete: a second Recover finds nothing to do.
	n2, err := agent.Recover(ctx, store2, resume)
	if err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second Recover re-drove %d runs, want 0 (run already complete)", n2)
	}
}

func textOf(m agent.Message) string {
	for _, p := range m.Parts {
		if t, ok := p.(agent.Text); ok {
			return t.Text
		}
	}
	return ""
}
