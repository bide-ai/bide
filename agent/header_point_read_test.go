package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bide-ai/bide/agent"
)

// A run whose first entry is not the header, but which holds a header later (the interleaving
// insertHeader's comment names: a non-Journal writer's entry lands between a Journal's empty read
// and its header Insert). History refuses it; the doc says every read does. Step's point read of
// "@journal" accepts it, marks the run good, and runs the step's effect into a run no reader of
// History will ever replay.
func TestPointReadAcceptsANonFirstHeader(t *testing.T) {
	ctx := context.Background()
	m := agent.NewMemStore()
	// A header this version writes, taken from another run.
	jj, _ := agent.NewJournal(m)
	if _, err := jj.Step(ctx, "donor", "x", func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	h, ok, err := m.Get(ctx, "donor", "@journal")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, _, err := m.Insert(ctx, "r", "legacy", []byte(`{"name":"legacy","kind":"value","salt":"AAAA"}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Insert(ctx, "r", "@journal", h.Data); err != nil {
		t.Fatal(err)
	}
	var v *agent.JournalVersionError
	// A point read of a record the run holds: the record is found, then the run is checked.
	if cold, _ := agent.NewJournal(m); cold != nil {
		if _, _, err := cold.Get(ctx, "r", "legacy"); !errors.As(err, &v) {
			t.Fatalf("Get of a record in a run whose header is not first = %v; want a *JournalVersionError", err)
		}
	}
	j, _ := agent.NewJournal(m)
	fired := 0
	_, stepErr := j.Step(ctx, "r", "pay", func(context.Context) (string, error) { fired++; return "paid", nil })
	_, histErr := j.History(ctx, "r")
	t.Logf("Step = %v (fired %d); History = %v", stepErr, fired, histErr)
	if errors.As(histErr, &v) && fired > 0 {
		t.Fatal("Step fired an effect into a run that History refuses as unversioned")
	}
}
