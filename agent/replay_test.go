package agent

import (
	"context"
	"reflect"
	"testing"
)

// TestReplayEvents_MatchesLiveStream: the journal projection reproduces the SAME semantic
// events (assistant turns + completed tool calls) the live stream emitted — the projection
// is not inventing a different history. Live events carry Replayed=false; the durable
// projection is inherently a replay (Replayed=true), so we normalize that one flag.
func TestReplayEvents_MatchesLiveStream(t *testing.T) {
	ctx := context.Background()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	m := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), textTurn("final")}}
	store := NewMemStore()

	// Collect the semantic subset from a live run.
	var live []AgentEvent
	for e := range New(m, store, tool).Stream(ctx, "run", "hi").Events() {
		switch ev := e.(type) {
		case AssistantTurn:
			ev.Replayed = true // normalize: the projection reads back as a replay
			live = append(live, ev)
		case ToolCompleted:
			live = append(live, ev)
		}
	}

	proj, err := ReplayEvents(ctx, store, "run")
	if err != nil {
		t.Fatalf("ReplayEvents: %v", err)
	}
	if !reflect.DeepEqual(live, proj) {
		t.Fatalf("projection != live semantic events:\n live=%v\n proj=%v", kinds(live), kinds(proj))
	}
}

// TestReplayEvents_AppendOnlyAcrossCrash is the durability heart: a crash mid-run leaves a
// PREFIX of the events in the durable journal; resuming appends the rest. The projected
// event trail after the crash is therefore an append-only prefix of the trail after resume —
// so a commitment over it grows monotonically and never rewrites history across a crash. It
// is also deterministic: recomputing from the same journal yields the identical sequence
// (unlike a live in-memory event log, which is lost on crash).
func TestReplayEvents_AppendOnlyAcrossCrash(t *testing.T) {
	ctx := context.Background()
	var calls int
	tool := &countingTool{name: "lookup", safety: Safety{ReadOnly: true}, calls: &calls}
	store := NewMemStore()

	// Crash on the second model turn, after the tool call + result are journaled.
	crashy := &scriptModel{turns: [][]Emit{toolTurn("c1", "lookup", `{"q":"x"}`), errTurn(errCrash)}}
	if _, err := New(crashy, store, tool).Run(ctx, "run", "hi"); err == nil {
		t.Fatal("expected the injected crash to fail the run")
	}

	prefix, err := ReplayEvents(ctx, store, "run")
	if err != nil {
		t.Fatalf("ReplayEvents (post-crash): %v", err)
	}
	prefixAgain, _ := ReplayEvents(ctx, store, "run")
	if !reflect.DeepEqual(prefix, prefixAgain) {
		t.Fatal("projection is not deterministic over the same journal")
	}

	// Resume with a healed model: the tool call + result replay from the journal, only the
	// final turn is produced live.
	recovered := &scriptModel{turns: [][]Emit{textTurn("final")}}
	if _, err := New(recovered, store, tool).Run(ctx, "run", "hi"); err != nil {
		t.Fatalf("resume: %v", err)
	}

	full, err := ReplayEvents(ctx, store, "run")
	if err != nil {
		t.Fatalf("ReplayEvents (post-resume): %v", err)
	}
	if len(full) <= len(prefix) {
		t.Fatalf("resume did not append events: prefix=%d full=%d", len(prefix), len(full))
	}
	for i := range prefix {
		if !reflect.DeepEqual(prefix[i], full[i]) {
			t.Fatalf("event %d changed across resume — not append-only:\n was=%v\n now=%v", i, prefix[i], full[i])
		}
	}
}
