package agent

import (
	"os"
	"context"
	"errors"
	"testing"
	"time"
)

func redactAll(t *testing.T, j *Journal, run string, sel RedactionSelector) RedactionPlan {
	t.Helper()
	ctx := context.Background()
	plan, err := PlanRedaction(ctx, j, run, sel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Redact(ctx, j, plan, time.Now()); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	return plan
}

// BREAK1: an aborted saga re-walks its rollback; redact the compensation memo and see if the
// compensator fires again.
func TestBreak_SagaCompensationMemoRedacted(t *testing.T) {
	ctx := context.Background()
	l := newLedger()
	j := memJournal()
	a := mustNew(&scriptModel{turns: [][]Emit{toolTurn("a1", "A", `{}`), toolTurn("a2", "boom", `{}`)}}, j,
		WithTools(l.write("A"), failTool("boom")))
	_, err0 := a.Run(ctx, "r", UserText("go"), WithSaga())
	var ab0 *SagaAborted
	if !errors.As(err0, &ab0) {
		t.Fatal("want abort")
	}
	t.Logf("first: comp=%v uncomp=%v unknown=%v", ab0.Compensated, ab0.Uncompensated, ab0.UnknownOutcome)
	recs, _ := j.History(ctx, "r")
	for _, r := range recs {
		t.Logf("%d %s %s", len(r.Name), r.Name, r.Kind)
	}
	plan := redactAll(t, j, "r", RedactionSelector{Prefixes: []string{"@saga/", "tool:", "@llm/", "run:start"}})
	for _, p := range plan.Records {
		t.Logf("redacted %s (%s)", p.Name, p.Kind)
	}
	for _, k := range plan.Kept {
		t.Logf("kept %s: %s", k.Name, k.Reason)
	}
	a2 := mustNew(&scriptModel{turns: [][]Emit{toolTurn("a1", "A", `{}`), toolTurn("a2", "boom", `{}`)}}, j,
		WithTools(l.write("A"), failTool("boom")))
	_, err := a2.Run(ctx, "r", UserText("go"), WithSaga())
	t.Logf("re-drive err: %v", err)
	if l.undoCount["A"] != 1 {
		t.Errorf("compensator for A ran %d times after redaction + re-drive, want 1", l.undoCount["A"])
	}
	var ab *SagaAborted
	if errors.As(err, &ab) {
		t.Logf("second: comp=%v uncomp=%v unknown=%v", ab.Compensated, ab.Uncompensated, ab.UnknownOutcome)
		if len(ab.Compensated) != len(ab0.Compensated) || len(ab.Uncompensated) != len(ab0.Uncompensated) {
			t.Errorf("SagaAborted changed after redaction: comp %v->%v uncomp %v->%v", ab0.Compensated, ab.Compensated, ab0.Uncompensated, ab.Uncompensated)
		}
	}
}

func finishedRun(t *testing.T) (*MemStore, *Journal) {
	t.Helper()
	ctx := context.Background()
	st := NewMemStore()
	j := mustJournal(st)
	a := mustNew(&scriptModel{turns: [][]Emit{textTurn("secret answer")}}, j)
	if _, err := a.Run(ctx, "r", UserText("secret input")); err != nil {
		t.Fatal(err)
	}
	return st, j
}

// BREAK2: redact a tombstone with a tombstone of the tombstone bytes: the leaf hash no longer matches the original.
func TestBreak_DoubleRedaction(t *testing.T) {
	ctx := context.Background()
	st, j := finishedRun(t)
	e, _, _ := st.Get(ctx, "r", "@llm/0")
	orig := LeafHash(e.Data)
	tomb, _ := NewTombstone("@llm/0", e.Data, time.Now())
	if ok, err := st.Redact(ctx, "r", "@llm/0", e.Data, tomb); !ok || err != nil {
		t.Fatal(ok, err)
	}
	// hand-built tombstone over the tombstone
	t2 := []byte(`{"redacted":{"leaf_hash":"` + hexOf(LeafHash(tomb)) + `","at_ms":1}}`)
	ok, err := st.Redact(ctx, "r", "@llm/0", tomb, t2)
	e2, _, _ := st.Get(ctx, "r", "@llm/0")
	h, _ := TombstoneLeafHash(e2.Data)
	if ok || err == nil || !bytesEq(h, orig) {
		t.Errorf("double redaction: replaced=%v err=%v; stored leaf hash now %x, original %x", ok, err, h, orig)
	}
	_ = j
}

func hexOf(b []byte) string { return encodeHex(b) }

func bytesEq(a, b []byte) bool { return string(a) == string(b) }
func encodeHex(b []byte) string {
	const d = "0123456789abcdef"
	o := make([]byte, 0, 2*len(b))
	for _, c := range b {
		o = append(o, d[c>>4], d[c&15])
	}
	return string(o)
}

func TestBreak_NonCanonicalTombstone(t *testing.T) {
	ctx := context.Background()
	_, _ = ctx, t
	st, _ := finishedRun(t)
	e, _, _ := st.Get(ctx, "r", "@llm/0")
	h := encodeHex(LeafHash(e.Data))
	for _, tb := range []string{
		`{"redacted": {"leaf_hash":"` + h + `","at_ms":1}}`,
		`{"redacted":{"LEAF_HASH":"` + h + `","at_ms":1}}`,
		`{"redacted":{"leaf_hash":"` + h + `"}}`,
		`{"redacted":{"leaf_hash":"` + h + `","at_ms":1}} `,
		`{"redacted":{"leaf_hash":"` + h + `","at_ms":9223372036854775807}}`,
		`{"redacted":{"leaf_hash":"` + h + `","leaf_hash":"` + h + `","at_ms":1}}`,
		`{"redacted":{"leaf_hash":"00","leaf_hash":"` + h + `","at_ms":1}}`,
		`{"redacted":{"leaf_hash":"` + h + `","at_ms":1.0}}`,
		`{"redacted":{"leaf_hash":"` + h + `","at_ms":1},"redacted":null}`,
	} {
		if err := CheckRedaction("r", "@llm/0", e.Data, []byte(tb)); err == nil {
			t.Errorf("accepted non-canonical tombstone %s", tb)
		}
	}
}

type failOnce struct {
	*MemStore
	name string
	done bool
}

func (f *failOnce) Insert(ctx context.Context, run, name string, data []byte) (Entry, bool, error) {
	if name == f.name && !f.done {
		f.done = true
		return Entry{}, false, errors.New("injected crash")
	}
	return f.MemStore.Insert(ctx, run, name, data)
}

// BREAK3: a sub-run finished but its parent has not recorded the call: redact the child, resume the parent.
func TestBreak_RedactChildOfLiveParent(t *testing.T) {
	ctx := context.Background()
	ms := NewMemStore()
	fs := &failOnce{MemStore: ms, name: "tool:p1"}
	j := mustJournal(fs)
	child := mustNew(&scriptModel{turns: [][]Emit{textTurn("child answer with PII")}}, j)
	mk := func() *Agent {
		turns := [][]Emit{toolTurn("p1", "kid", `{"task":"x"}`), textTurn("parent done")}
		if fs.done {
			turns = turns[1:]
		}
		return mustNew(&scriptModel{turns: turns}, j,
			WithTools(MustSubAgent("kid", "", child)))
	}
	_, err := mk().Run(ctx, "root", UserText("go"))
	t.Logf("first run: %v", err)
	kids := []string{}
	for id, err := range ms.Runs(ctx, RunFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		kids = append(kids, id)
	}
	t.Logf("runs: %v", kids)
	for _, id := range kids {
		if id == "root" || os.Getenv("NOREDACT") != "" {
			continue
		}
		jr := mustJournal(ms)
		plan, err := PlanRedaction(ctx, jr, id, RedactionSelector{Payloads: true})
		if err != nil || !plan.Terminal {
			t.Logf("child %s plan terminal=%v err=%v", id, plan.Terminal, err)
			continue
		}
		if _, err := Redact(ctx, jr, plan, time.Now()); err != nil {
			t.Fatal(err)
		}
		t.Logf("redacted child %s (%d records) while root has no end marker", id, len(plan.Records))
	}
	res, err := mk().Run(ctx, "root", UserText("go"))
	t.Logf("resume of live parent: %v %v", res, err)
	if r, ok, _ := j.Get(ctx, "root", "tool:p1"); ok {
		t.Logf("parent's tool:p1 result: %s", r.Result)
	}
	if err != nil {
		t.Errorf("parent cannot resume after its finished child was redacted: %v", err)
	}
}

// BREAK4: a tombstone in place of an attempt marker (what the Postgres redactor role can write with
// plain SQL, or anyone with row access) keeps the Merkle leaf, and the engine re-fires the effect.
func TestBreak_TombstonedAttemptMarkerRefiresEffect(t *testing.T) {
	ctx := context.Background()
	ms := NewMemStore()
	fs := &failOnce{MemStore: ms, name: "tool:c1"}
	j := mustJournal(fs)
	fired := 0
	pay := MustFunc("pay", "", func(context.Context, struct{}) (struct{}, error) { fired++; return struct{}{}, nil })
	mk := func(turns [][]Emit) *Agent { return mustNew(&scriptModel{turns: turns}, j, WithTools(pay)) }
	_, err := mk([][]Emit{toolTurn("c1", "pay", `{}`), textTurn("done")}).Run(ctx, "r", UserText("go"))
	t.Logf("first: %v fired=%d", err, fired)
	// attacker replaces the attempt marker with a tombstone that keeps its leaf hash
	r := ms.runs["r"]
	i := r.byName["attempt:tool:c1"]
	tomb, _ := NewTombstone("attempt:tool:c1", r.entries[i].Data, time.Now())
	r.entries[i].Data = tomb
	_, err = mk([][]Emit{textTurn("done")}).Run(ctx, "r", UserText("go"))
	t.Logf("resume: %v fired=%d", err, fired)
	if fired != 1 {
		t.Errorf("effect fired %d times: a tombstoned attempt marker was read as 'never attempted'", fired)
	}
}

func TestBreak_StatusOfTombstonedEndMarker(t *testing.T) {
	ctx := context.Background()
	st, j := finishedRun(t)
	r := st.runs["r"]
	i := r.byName["run:complete"]
	tomb, _ := NewTombstone("run:complete", r.entries[i].Data, time.Now())
	r.entries[i].Data = tomb
	s, err := Status(ctx, j, "r")
	t.Logf("Status with run:complete tombstoned: %+v err=%v", s, err)
	if err == nil && s.State != RunCompleted {
		t.Errorf("Status = %v for a run whose end marker is a tombstone; want an error (corruption), not a state", s.State)
	}
	n, err := Recover(ctx, j, func(context.Context, string, RunStart) error { t.Error("Recover re-drove it"); return nil })
	t.Logf("Recover: %d %v", n, err)
}
