package plan

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

// Safety opt-in on the plan surface: a node marked ReadOnly/Idempotent RE-RUNS its
// body on an ambiguous mid-node crash (its body ran, its result was lost) instead of
// halting, while an unmarked non-idempotent node keeps the conservative
// *agent.OutcomeUnknown. A Tool node auto-derives its Safety from the wrapped
// agent.Tool.Safety(). These tests reuse the crashFlowStore DST harness from
// flow_dst_test.go (errCrash, crashFlowStore, runFlowOnce style) for crash injection.

// readCounter is a read-only-style body: it reads a shared value and returns it
// without mutating anything, so re-running it on resume yields the SAME value. The
// call counter proves how many times the body ran; safety of the effect is that the
// returned value is stable across repeats.
func readCounter(reads *int, value int) func(context.Context, int) (int, error) {
	return func(context.Context, int) (int, error) {
		*reads++          // observe the body ran; NOT a state mutation the flow depends on
		return value, nil // a read: same value every time, safe to repeat
	}
}

// buildReadFlow builds a single-node flow whose entry is a read marked by opt (for
// example plan.ReadOnly()). The terminal produces the read value as a string so the
// flow output is observable across a crash and resume.
func buildReadFlow(reads *int, value int, opt NodeOption) (*Flow[int, int], error) {
	b := New[int, int]("read-flow")
	b.Step("read", readCounter(reads, value), opt)
	return b.Build()
}

// buildUnmarkedReadFlow is buildReadFlow with NO Safety option: the default
// conservative node that must still halt on the ambiguous crash.
func buildUnmarkedReadFlow(reads *int, value int) (*Flow[int, int], error) {
	b := New[int, int]("read-flow")
	b.Step("read", readCounter(reads, value))
	return b.Build()
}

// findEntryResultCrash sweeps crash points to find the one that lands on the entry
// node's RESULT write: the body ran (reads incremented) but its result record was
// lost (a node that is not retry-safe also leaves its attempt marker persisted). That is the
// exact ambiguous-crash window this feature changes. It returns the shared journal
// primed to that state and the reads count at the crash.
func findEntryResultCrash(t *testing.T, build func(reads *int) (*Flow[int, int], error)) (*agent.Journal, int) {
	t.Helper()
	for crashAt := 1; crashAt <= 32; crashAt++ {
		reads := 0
		mem := agent.NewMemStore()
		j := agenttest.MustJournal(mem)
		store := agenttest.MustJournal(&crashFlowStore{inner: j, crashAt: crashAt})
		flow, err := build(&reads)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		_, err = flow.Run(context.Background(), store, "safety", 0)
		if !errors.Is(err, errCrash) {
			continue
		}
		// The crash landed in the window iff the body ran but the journal holds no
		// node:read result. A node that is not retry-safe also holds its attempt marker
		// then (its claim precedes the body); a retry-safe node writes none.
		recs, hErr := j.History(context.Background(), "safety")
		if hErr != nil {
			t.Fatalf("History: %v", hErr)
		}
		if !hasRecord(recs, "node:read") && reads >= 1 {
			return j, reads
		}
	}
	t.Fatal("no crash point produced the entry-result ambiguous window")
	return nil, 0
}

// TestSafety_ReadOnly_RerunsAndCompletes proves a ReadOnly node re-runs its body on
// the ambiguous-crash window and the flow COMPLETES (no OutcomeUnknown), and that the
// re-run is safe: the read returns the same value both times, so the completed
// output is correct.
func TestSafety_ReadOnly_RerunsAndCompletes(t *testing.T) {
	mem, readsAtCrash := findEntryResultCrash(t, func(reads *int) (*Flow[int, int], error) {
		return buildReadFlow(reads, 42, ReadOnly())
	})

	// Resume with no further crash. The node is ReadOnly, so runNode re-runs the body
	// rather than returning *agent.OutcomeUnknown.
	reads := readsAtCrash
	flow, err := buildReadFlow(&reads, 42, ReadOnly())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := flow.Run(context.Background(), mem, "safety", 0)
	var halt *agent.OutcomeUnknown
	if errors.As(err, &halt) {
		t.Fatalf("ReadOnly node halted at %q; want re-run and completion", halt.Op.ID)
	}
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if out != 42 {
		t.Fatalf("output = %d, want 42 (the read returns the same value on re-run)", out)
	}
	if reads < 2 {
		t.Fatalf("body ran %d times total, want >= 2 (a re-run happened on resume)", reads)
	}
}

// TestSafety_Idempotent_RerunsAndCompletes is the same proof for plan.Idempotent(),
// the other retry-safe classification.
func TestSafety_Idempotent_RerunsAndCompletes(t *testing.T) {
	mem, readsAtCrash := findEntryResultCrash(t, func(reads *int) (*Flow[int, int], error) {
		return buildReadFlow(reads, 7, Idempotent())
	})
	reads := readsAtCrash
	flow, err := buildReadFlow(&reads, 7, Idempotent())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := flow.Run(context.Background(), mem, "safety", 0)
	var halt *agent.OutcomeUnknown
	if errors.As(err, &halt) {
		t.Fatalf("Idempotent node halted at %q; want re-run and completion", halt.Op.ID)
	}
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if out != 7 {
		t.Fatalf("output = %d, want 7", out)
	}
}

// TestSafety_Default_StillHalts is the regression guard: an unmarked (non-idempotent)
// node hitting the SAME ambiguous-crash window still returns *agent.OutcomeUnknown and does
// NOT re-run the body, preserving the surface's safe-by-default behavior.
func TestSafety_Default_StillHalts(t *testing.T) {
	mem, readsAtCrash := findEntryResultCrash(t, func(reads *int) (*Flow[int, int], error) {
		return buildUnmarkedReadFlow(reads, 42)
	})
	reads := readsAtCrash
	flow, err := buildUnmarkedReadFlow(&reads, 42)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	_, err = flow.Run(context.Background(), mem, "safety", 0)
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("unmarked node did not halt: err = %v; want *agent.OutcomeUnknown", err)
	}
	if halt.Op.ID != "node:read" || halt.Op.Kind != agent.OpStep {
		t.Fatalf("halt named %v, want step %q", halt.Op, "node:read")
	}
	if reads != readsAtCrash {
		t.Fatalf("unmarked node re-ran its body on resume (reads %d -> %d); it must halt without re-running", readsAtCrash, reads)
	}
}

// readOnlyTool and nonIdempotentTool are minimal agent.Tool implementations whose
// only distinguishing property is Safety(): one ReadOnly, one zero (non-idempotent).
// A shared calls counter proves how many times Call ran across a crash and resume.
type safetyTool struct {
	safety agent.Safety
	calls  *int
	value  int
}

// Spec describes the tool to the agent (see agent.Tool).
func (s safetyTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "safety-tool", Description: "test tool", Input: json.RawMessage(`{"type":"object"}`), Safety: s.safety}
}

func (s safetyTool) Call(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	*s.calls++
	return json.Marshal(s.value)
}

// buildToolFlow builds a one-node flow whose entry is a Tool wrapping t, so the
// node's Safety is auto-derived from t.Safety().
func buildToolFlow(t agent.Tool) (*Flow[int, int], error) {
	b := New[int, int]("tool-flow")
	b.Tool[int, int]("call", t)
	return b.Build()
}

// findToolResultCrash is findEntryResultCrash for the Tool flow: it sweeps for the
// crash landing on the "call" node's result write.
func findToolResultCrash(t *testing.T, tool safetyTool) *agent.Journal {
	t.Helper()
	for crashAt := 1; crashAt <= 32; crashAt++ {
		mem := agent.NewMemStore()
		j := agenttest.MustJournal(mem)
		store := agenttest.MustJournal(&crashFlowStore{inner: j, crashAt: crashAt})
		flow, err := buildToolFlow(tool)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		_, err = flow.Run(context.Background(), store, "tool", 0)
		if !errors.Is(err, errCrash) {
			continue
		}
		recs, hErr := j.History(context.Background(), "tool")
		if hErr != nil {
			t.Fatalf("History: %v", hErr)
		}
		if !hasRecord(recs, "node:call") && *tool.calls >= 1 {
			return j
		}
	}
	t.Fatal("no crash point produced the tool-result ambiguous window")
	return nil
}

// TestSafety_Tool_AutoDerivesReadOnly proves a Tool node whose wrapped agent.Tool is
// ReadOnly re-runs on the ambiguous crash and completes, without any explicit option:
// the node's Safety was auto-derived from agent.Tool.Safety().
func TestSafety_Tool_AutoDerivesReadOnly(t *testing.T) {
	calls := 0
	tool := safetyTool{safety: agent.Safety{ReadOnly: true}, calls: &calls, value: 99}
	mem := findToolResultCrash(t, tool)

	flow, err := buildToolFlow(tool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := flow.Run(context.Background(), mem, "tool", 0)
	var halt *agent.OutcomeUnknown
	if errors.As(err, &halt) {
		t.Fatalf("ReadOnly tool halted at %q; want auto-derived re-run and completion", halt.Op.ID)
	}
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if out != 99 {
		t.Fatalf("output = %d, want 99", out)
	}
	if calls < 2 {
		t.Fatalf("tool Call ran %d times, want >= 2 (a re-run happened on resume)", calls)
	}
}

// TestSafety_Tool_AutoDerivesNonIdempotentHalts proves a Tool node whose wrapped
// agent.Tool declares no Safety (zero value: non-idempotent) still HALTS on the same
// ambiguous crash, since its auto-derived Safety is not retry-safe.
func TestSafety_Tool_AutoDerivesNonIdempotentHalts(t *testing.T) {
	calls := 0
	tool := safetyTool{safety: agent.Safety{}, calls: &calls, value: 99}
	mem := findToolResultCrash(t, tool)
	callsAtCrash := calls

	flow, err := buildToolFlow(tool)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	_, err = flow.Run(context.Background(), mem, "tool", 0)
	var halt *agent.OutcomeUnknown
	if !errors.As(err, &halt) {
		t.Fatalf("non-idempotent tool did not halt: err = %v; want *agent.OutcomeUnknown", err)
	}
	if halt.Op.ID != "node:call" || halt.Op.Kind != agent.OpStep {
		t.Fatalf("halt named %v, want step %q", halt.Op, "node:call")
	}
	if calls != callsAtCrash {
		t.Fatalf("non-idempotent tool re-ran on resume (calls %d -> %d); it must halt", callsAtCrash, calls)
	}
}

// TestSafety_ExplicitOptionOverridesToolSafety proves an explicit plan.ReadOnly()
// option on a Tool overrides the wrapped tool's own (non-idempotent) Safety: the node
// re-runs and completes despite the tool declaring no Safety.
func TestSafety_ExplicitOptionOverridesToolSafety(t *testing.T) {
	calls := 0
	tool := safetyTool{safety: agent.Safety{}, calls: &calls, value: 5}
	// Sweep with the override in place so the built flow carries ReadOnly.
	var mem *agent.Journal
	for crashAt := 1; crashAt <= 32; crashAt++ {
		calls = 0
		m := agent.NewMemStore()
		j := agenttest.MustJournal(m)
		store := agenttest.MustJournal(&crashFlowStore{inner: j, crashAt: crashAt})
		b := New[int, int]("tool-flow")
		b.Tool[int, int]("call", tool, ReadOnly())
		flow, err := b.Build()
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		_, err = flow.Run(context.Background(), store, "override", 0)
		if !errors.Is(err, errCrash) {
			continue
		}
		recs, _ := j.History(context.Background(), "override")
		if !hasRecord(recs, "node:call") && calls >= 1 {
			mem = j
			break
		}
	}
	if mem == nil {
		t.Fatal("no crash point produced the ambiguous window for the override case")
	}

	b := New[int, int]("tool-flow")
	b.Tool[int, int]("call", tool, ReadOnly())
	flow, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out, err := flow.Run(context.Background(), mem, "override", 0)
	var halt *agent.OutcomeUnknown
	if errors.As(err, &halt) {
		t.Fatalf("explicit ReadOnly override halted at %q; want re-run and completion", halt.Op.ID)
	}
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if out != 5 {
		t.Fatalf("output = %d, want 5", out)
	}
}

// TestSafety_NotInDigest documents the Digest decision: Safety is a runtime resume
// property, not part of the wired topology, so two flows identical in shape but
// differing only in a node's Safety produce the SAME Digest. A flow's identity is
// its shape.
func TestSafety_NotInDigest(t *testing.T) {
	reads := 0
	plain, err := buildUnmarkedReadFlow(&reads, 1)
	if err != nil {
		t.Fatalf("Build plain: %v", err)
	}
	safe, err := buildReadFlow(&reads, 1, ReadOnly())
	if err != nil {
		t.Fatalf("Build safe: %v", err)
	}
	if plain.Digest() != safe.Digest() {
		t.Fatalf("Safety changed the Digest: plain %s != readonly %s; Safety must not be part of topology identity", plain.Digest(), safe.Digest())
	}
}

// hasRecord reports whether recs holds a record named name.
func hasRecord(recs []agent.Record, name string) bool {
	for _, r := range recs {
		if r.Name == name {
			return true
		}
	}
	return false
}
