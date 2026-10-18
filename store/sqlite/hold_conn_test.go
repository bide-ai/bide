package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

func putRec(t *testing.T, s *Store, run, name string, kind agent.StepKind, result string) []byte {
	t.Helper()
	b, err := agent.JournalEntry(name, agent.Record{Kind: kind, Result: []byte(result)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(context.Background(), run, name, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func redactOne(s *Store, run, name string, cur []byte) (bool, error) {
	tomb, err := agent.NewTombstone(name, cur, time.Now())
	if err != nil {
		return false, err
	}
	return s.Redact(context.Background(), run, name, cur, tomb)
}

// Once PlaceHold has returned, no erasure that starts afterwards replaces a record of a covered run,
// whichever connection or process it comes through.
func TestHold_IsHonouredAcrossConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "j.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	const n = 200
	run := "stress"
	putRec(t, a, run, "run:complete", agent.StepValue, `"ok"`)
	cur := make([][]byte, n)
	for i := range cur {
		cur[i] = putRec(t, a, run, fmt.Sprintf("tool:%d", i), agent.StepToolResult, fmt.Sprintf(`{"pii":%d}`, i))
	}
	var placed atomic.Bool
	var violations, erased atomic.Int32
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range cur {
			before := placed.Load() // the hold was already placed when this erasure began
			done, err := redactOne(a, run, fmt.Sprintf("tool:%d", i), cur[i])
			if done {
				erased.Add(1)
			}
			if done && before {
				violations.Add(1)
			}
			_ = err
		}
	}()
	go func() {
		defer wg.Done()
		for erased.Load() < 60 {
			runtime.Gosched()
		}
		if err := b.PlaceHold(ctx, agent.Hold{ID: "h", Matter: "m", Runs: []string{run}}); err != nil {
			t.Error(err)
		}
		placed.Store(true)
	}()
	wg.Wait()
	t.Logf("%d of %d erasures went through before the hold landed", erased.Load(), n)
	if v := violations.Load(); v != 0 {
		t.Fatalf("%d erasure(s) that began after PlaceHold returned replaced a record", v)
	}
}

// A hold on a run that does not exist yet covers it when it is created, at any depth below with Tree,
// and a lift of one hold leaves another hold of the same matter in force.
func TestHold_TreeDeepFutureAndPartialLift(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	deep := agent.SubRunID(agent.SubRunID(agent.SubRunID("root", "c1"), "c2"), "c3")
	if err := s.PlaceHold(ctx, agent.Hold{ID: "tree", Matter: "M", Runs: []string{"root"}, Tree: true}); err != nil { // root does not exist yet
		t.Fatal(err)
	}
	if err := s.PlaceHold(ctx, agent.Hold{ID: "flat", Matter: "M", Runs: []string{"other", deep}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"root", "other", deep, agent.SubRunID("root", "late")} { // created after the hold
		putRec(t, s, id, "run:complete", agent.StepValue, `"ok"`)
	}
	rootTool := putRec(t, s, "root", "tool:x", agent.StepToolResult, `1`)
	otherTool := putRec(t, s, "other", "tool:x", agent.StepToolResult, `1`)
	deepTool := putRec(t, s, deep, "tool:x", agent.StepToolResult, `1`)
	lateID := agent.SubRunID("root", "late")
	lateTool := putRec(t, s, lateID, "tool:x", agent.StepToolResult, `1`)
	for name, c := range map[string]struct {
		run string
		cur []byte
	}{"root": {"root", rootTool}, "other": {"other", otherTool}, "deep": {deep, deepTool}, "late": {lateID, lateTool}} {
		if done, err := redactOne(s, c.run, "tool:x", c.cur); done || err == nil {
			t.Errorf("%s: erased under a hold: %v %v", name, done, err)
		}
	}
	// Lift the flat hold: "other" is free, "deep" is still under the tree hold, "root" too.
	if ok, err := s.LiftHold(ctx, "flat"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if done, err := redactOne(s, "other", "tool:x", otherTool); !done || err != nil {
		t.Errorf("other after the lift: %v %v", done, err)
	}
	if done, err := redactOne(s, deep, "tool:x", deepTool); done || err == nil {
		t.Errorf("deep after lifting the flat hold: erased under the tree hold")
	}
}
