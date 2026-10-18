package agent_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
)

func TestNewTombstone_ReadsBack(t *testing.T) {
	rec, err := agent.JournalEntry("tool:x", agent.Record{Kind: agent.StepToolResult, Result: []byte(`"x"`)})
	if err != nil {
		t.Fatal(err)
	}
	tomb, err := agent.NewTombstone("tool:x", rec, time.UnixMilli(1_700_000_000_123))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"redacted":{"leaf_hash":"` + hex.EncodeToString(agent.LeafHash(rec)) + `","at_ms":1700000000123,"name":"tool:x"}}`
	if string(tomb) != want {
		t.Fatalf("NewTombstone = %s, want %s", tomb, want)
	}
	if h, ok := agent.TombstoneLeafHash(tomb); !ok || !bytes.Equal(h, agent.LeafHash(rec)) {
		t.Fatalf("TombstoneLeafHash = %x, %v", h, ok)
	}
	if err := agent.CheckRedaction("r", "tool:x", rec, tomb); err != nil {
		t.Fatalf("CheckRedaction of a well-formed redaction = %v", err)
	}
	if _, err := agent.NewTombstone("tool:x", tomb, time.Now()); !errors.Is(err, agent.ErrRedactMismatch) {
		t.Errorf("NewTombstone of a tombstone = %v, want ErrRedactMismatch", err)
	}
	if _, err := agent.NewTombstone("tool:x", rec, time.UnixMilli(-1)); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("NewTombstone before the epoch = %v, want ErrConfig", err)
	}
	if _, err := agent.NewTombstone("", rec, time.Now()); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("NewTombstone with no name = %v, want ErrConfig", err)
	}
	if n, ok := agent.TombstoneName(tomb); !ok || n != "tool:x" {
		t.Errorf("TombstoneName = %q, %v", n, ok)
	}
	// A tombstone with a changed hash, extra member, upper-case hex, or short hash is not one.
	for _, bad := range []string{
		strings.Replace(want, hex.EncodeToString(agent.LeafHash(rec))[:2], "00", 1),
		strings.Replace(want, `,"at_ms"`, `,"x":1,"at_ms"`, 1),
		strings.ToUpper(want[:strings.Index(want, `,"at_ms"`)]) + want[strings.Index(want, `,"at_ms"`):],
		`{"redacted":{"leaf_hash":"ab","at_ms":1}}`,
	} {
		if err := agent.CheckRedaction("r", "tool:x", rec, []byte(bad)); !errors.Is(err, agent.ErrRedactMismatch) {
			t.Errorf("CheckRedaction(%s) = %v, want ErrRedactMismatch", bad, err)
		}
	}
}

func TestRedactionProtected_Names(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind agent.StepKind
		want bool
	}{
		{"@journal", agent.StepHeader, true},
		{"run:complete", agent.StepValue, false}, // an end marker is read by its name: its content may go
		{"run:aborted", agent.StepValue, false},
		{"run:cancelled", agent.StepValue, false},
		{"run:cancel-requested", agent.StepValue, true},
		{"run:limits:1", agent.StepValue, true},
		{"run:start", agent.StepValue, false},
		{"attempt:tool:a", agent.StepAttempt, true},
		{"attempt:not-started:c:m", agent.StepNotStarted, true},
		{"tool:a", agent.StepSagaFail, true},
		{"tool:a", agent.StepToolResult, false},
		{"approval-tally:a", agent.StepValue, true},
		{"approval-identity:a", agent.StepValue, true},
		{"approval-status:h", agent.StepValue, true},
		{"approval-exec-hold:a", agent.StepValue, true},
		{"approval-deadline:a", agent.StepValue, true},
		{"audit:policy", agent.StepValue, true},
		{"flow:digest", agent.StepValue, true},
		{"@saga/compensate/c", agent.StepValue, true},
		{"@saga/args/c", agent.StepValue, true},
		{"approval:a", agent.StepApproval, false}, // protected by the selector, not by the store
		{"@llm/1", agent.StepModel, false},
		{"signal:x", agent.StepSignal, false},
	} {
		if got := agent.RedactionProtected(tc.name, tc.kind) != ""; got != tc.want {
			t.Errorf("RedactionProtected(%q, %q) protected = %v, want %v", tc.name, tc.kind, got, tc.want)
		}
	}
}

// A real run, redacted: the readers that need a run's end and its place keep working, the content
// is gone from the stored bytes, and the run cannot be driven again.
func TestRedact_RealRun(t *testing.T) {
	ctx := context.Background()
	store := agent.NewMemStore()
	j := agenttest.MustJournal(store)
	a := agenttest.MustNew(agenttest.NewScriptedModel(agenttest.TextTurn("your account 4111-1111 is fine")), j)
	if _, err := a.Run(ctx, "r", agent.UserText("my name is Alice Example, account 4111-1111")); err != nil {
		t.Fatal(err)
	}
	before, err := agent.Status(ctx, j, "r")
	if err != nil || before.State != agent.RunCompleted {
		t.Fatalf("Status = %+v, %v", before, err)
	}
	plan, err := agent.PlanRedaction(ctx, j, "r", agent.RedactionSelector{Payloads: true})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Terminal || len(plan.Records) < 2 {
		t.Fatalf("plan = %+v; want a finished run with run:start and a model turn selected", plan)
	}
	for _, k := range plan.Kept {
		// The one record the preset lists is the end marker, so its content is not left silently.
		if k.Name != "run:complete" || !strings.Contains(k.Reason, "end marker not selected") {
			t.Errorf("the payload preset lists %q as kept (%s): it should not list records it did not choose", k.Name, k.Reason)
		}
	}
	if _, err := agent.Redact(ctx, j, plan, time.Now()); err != nil {
		t.Fatalf("Redact: %v", err)
	}

	// No content is left in any stored entry.
	for e, err := range store.Load(ctx, "r", -1) {
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(e.Data, []byte("4111")) || bytes.Contains(e.Data, []byte("Alice")) {
			t.Errorf("entry %q still holds the content: %s", e.Name, e.Data)
		}
	}
	// Status is unchanged: the end marker is not redacted by the preset.
	after, err := agent.Status(ctx, j, "r")
	if err != nil || after.State != agent.RunCompleted || after.Records != before.Records {
		t.Errorf("Status after = %+v, %v; want %+v", after, err, before)
	}
	// Recover does not pick the run up.
	n, err := agent.Recover(ctx, j, func(ctx context.Context, runID string, _ agent.RunStart) error {
		t.Errorf("Recover resumed redacted run %s", runID)
		return nil
	})
	if err != nil || n != 0 {
		t.Errorf("Recover = %d, %v; want 0", n, err)
	}
	// The run is over for good: it cannot be driven again.
	if _, err := a.Run(ctx, "r", agent.UserText("again")); !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "redacted") {
		t.Errorf("Run over a redacted run = %v, want ErrConfig naming the redaction", err)
	}
	// History reads, naming every redacted record.
	hist, err := j.History(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	redacted := 0
	for _, r := range hist {
		if r.Redacted {
			redacted++
		}
	}
	if redacted != len(plan.Records) {
		t.Errorf("History reports %d redacted records, want %d", redacted, len(plan.Records))
	}
}

func TestRedact_RefusesAStoreWithoutTheCapability(t *testing.T) {
	ctx := context.Background()
	s := plainStore{agent.NewMemStore()}
	j := agenttest.MustJournal(s)
	plan := agent.RedactionPlan{RunID: "r"}
	_, err := agent.Redact(ctx, j, plan, time.Now())
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "cannot redact") {
		t.Fatalf("Redact over a store without Redactor = %v, want ErrConfig", err)
	}
}

// plainStore hides everything but the Store methods of a MemStore.
type plainStore struct{ inner *agent.MemStore }

func (p plainStore) Insert(ctx context.Context, r, n string, d []byte) (agent.Entry, bool, error) {
	return p.inner.Insert(ctx, r, n, d)
}
func (p plainStore) Get(ctx context.Context, r, n string) (agent.Entry, bool, error) {
	return p.inner.Get(ctx, r, n)
}
func (p plainStore) Load(ctx context.Context, r string, after int64) iter.Seq2[agent.Entry, error] {
	return p.inner.Load(ctx, r, after)
}

// A plan is for the records it was made from: executed against a journal whose record under the
// same name holds other bytes, it replaces nothing.
func TestRedact_RefusesAPlanMadeFromOtherRecords(t *testing.T) {
	ctx := context.Background()
	build := func(content string) (*agent.MemStore, *agent.Journal) {
		s := agent.NewMemStore()
		j := agenttest.MustJournal(s)
		if _, err := j.Step(ctx, "r", "step", func(context.Context) (string, error) { return content, nil }); err != nil {
			t.Fatal(err)
		}
		b, err := agent.JournalEntry("run:complete", agent.Record{Kind: agent.StepValue, Result: []byte(`"ok"`)})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Insert(ctx, "r", "run:complete", b); err != nil {
			t.Fatal(err)
		}
		return s, j
	}
	_, planned := build("first")
	other, otherJ := build("second")
	plan, err := agent.PlanRedaction(ctx, planned, "r", agent.RedactionSelector{Names: []string{"step"}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := agent.Redact(ctx, otherJ, plan, time.Now())
	if !errors.Is(err, agent.ErrRedactChanged) || len(res.Redacted) != 0 {
		t.Fatalf("Redact of a plan made from other bytes = %+v, %v; want ErrRedactChanged and nothing replaced", res, err)
	}
	if e, _, _ := other.Get(ctx, "r", "step"); bytes.Contains(e.Data, []byte(`"redacted"`)) {
		t.Fatalf("the record was replaced")
	}
}

// A tombstone that names a record sits only in that record's row.
func TestTombstone_NameMustMatchItsRow(t *testing.T) {
	ctx := context.Background()
	s := agent.NewMemStore()
	j := agenttest.MustJournal(s)
	b, err := agent.JournalEntry("a", agent.Record{Kind: agent.StepValue, Result: []byte(`1`)})
	if err != nil {
		t.Fatal(err)
	}
	tomb, err := agent.NewTombstone("a", b, time.UnixMilli(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Step(ctx, "r", "seed", func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "r", "a", tomb); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "r", "b", tomb); err != nil { // a's tombstone copied under b
		t.Fatal(err)
	}
	if r, ok, err := j.Get(ctx, "r", "a"); err != nil || !ok || !r.Redacted || r.Name != "a" {
		t.Fatalf("Get(a) = %+v, %v, %v; want the redacted record", r, ok, err)
	}
	if _, _, err := j.Get(ctx, "r", "b"); !errors.Is(err, agent.ErrStorage) {
		t.Fatalf("Get(b) of a tombstone naming a = %v, want ErrStorage", err)
	}
	// A tombstone that names no record (the reserved form without it) still reads.
	legacy := []byte(`{"redacted":{"leaf_hash":"` + hex.EncodeToString(agent.LeafHash(b)) + `","at_ms":1}}`)
	if _, _, err := s.Insert(ctx, "r", "c", legacy); err != nil {
		t.Fatal(err)
	}
	if r, ok, err := j.Get(ctx, "r", "c"); err != nil || !ok || !r.Redacted {
		t.Fatalf("Get(c) of a tombstone without a name = %+v, %v, %v", r, ok, err)
	}
}

// Only the exact bytes NewTombstone writes are a tombstone: a parser that takes the first of two
// duplicate members, or reads a member name without regard to case, would read another leaf hash.
func TestTombstone_OnlyTheCanonicalFormReads(t *testing.T) {
	leaf := strings.Repeat("ab", 32)
	good := `{"redacted":{"leaf_hash":"` + leaf + `","at_ms":5,"name":"x"}}`
	if _, ok := agent.TombstoneLeafHash([]byte(good)); !ok {
		t.Fatalf("the canonical form does not read")
	}
	for name, bad := range map[string]string{
		"duplicate leaf_hash":  `{"redacted":{"leaf_hash":"00","leaf_hash":"` + leaf + `","at_ms":5}}`,
		"duplicate at_ms":      `{"redacted":{"leaf_hash":"` + leaf + `","at_ms":5,"at_ms":6}}`,
		"upper-case member":    `{"redacted":{"LEAF_HASH":"` + leaf + `","at_ms":5}}`,
		"upper-case hex":       `{"redacted":{"leaf_hash":"` + strings.ToUpper(leaf) + `","at_ms":5}}`,
		"trailing space":       good + " ",
		"trailing newline":     good + "\n",
		"leading space":        " " + good,
		"space after colon":    `{"redacted": {"leaf_hash":"` + leaf + `","at_ms":5}}`,
		"missing at_ms":        `{"redacted":{"leaf_hash":"` + leaf + `"}}`,
		"at_ms past year 9999": `{"redacted":{"leaf_hash":"` + leaf + `","at_ms":253402300800000}}`,
		"at_ms max int64":      `{"redacted":{"leaf_hash":"` + leaf + `","at_ms":9223372036854775807}}`,
		"at_ms leading zero":   `{"redacted":{"leaf_hash":"` + leaf + `","at_ms":05}}`,
		"at_ms negative":       `{"redacted":{"leaf_hash":"` + leaf + `","at_ms":-1}}`,
		"at_ms float":          `{"redacted":{"leaf_hash":"` + leaf + `","at_ms":5.0}}`,
		"extra member":         `{"redacted":{"leaf_hash":"` + leaf + `","at_ms":5,"x":1}}`,
		"name before at_ms":    `{"redacted":{"leaf_hash":"` + leaf + `","name":"x","at_ms":5}}`,
		"empty name":           `{"redacted":{"leaf_hash":"` + leaf + `","at_ms":5,"name":""}}`,
		"short hash":           `{"redacted":{"leaf_hash":"ab","at_ms":5}}`,
		"second top member":    `{"redacted":{"leaf_hash":"` + leaf + `","at_ms":5},"x":1}`,
	} {
		if _, ok := agent.TombstoneLeafHash([]byte(bad)); ok {
			t.Errorf("%s: %s reads as a tombstone", name, bad)
		}
		rec, _ := agent.JournalEntry("x", agent.Record{Kind: agent.StepValue, Result: []byte(`1`)})
		if err := agent.CheckRedaction("r", "x", rec, []byte(bad)); err == nil {
			t.Errorf("%s: CheckRedaction accepted it", name)
		}
	}
}

func TestRedactionAncestorsAndSession(t *testing.T) {
	sub := agent.SubRunID(agent.SubRunID("root", "a"), "b")
	if got := agent.RedactionAncestors(sub); len(got) != 2 || got[0] != agent.SubRunID("root", "a") || got[1] != "root" {
		t.Errorf("RedactionAncestors(%q) = %v", sub, got)
	}
	if got := agent.RedactionAncestors("root"); len(got) != 0 {
		t.Errorf("a root run has ancestors %v", got)
	}
	for id, want := range map[string]bool{"root": false, sub: false, "s>@session": true, "s>@turn/1": true, agent.SubRunID("s>@turn/1", "c"): true} {
		if got := agent.RedactionSession(id); got != want {
			t.Errorf("RedactionSession(%q) = %v, want %v", id, got, want)
		}
	}
}
