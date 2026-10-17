package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// Redactor checks agent.Redactor on a store that implements it: that a redaction replaces one
// entry's bytes with a tombstone and nothing else (position, name and every other entry
// unchanged), and that it refuses, writing nothing, a run that is not over, a run with a live
// lease, a protected record, a tombstone that does not keep the bytes' leaf hash, an entry that is
// not the one the caller read, and an entry the run does not hold. Run calls it for a store that
// implements agent.Redactor.
func Redactor(t *testing.T, open func(t *testing.T) agent.Store) {
	t.Run("Replaces", func(t *testing.T) { redactReplaces(t, open(t)) })
	t.Run("Idempotent", func(t *testing.T) { redactIdempotent(t, open(t)) })
	t.Run("Refusals", func(t *testing.T) { redactRefusals(t, open(t)) })
	t.Run("DoubleRedaction", func(t *testing.T) { redactTwice(t, open(t)) })
	t.Run("SubRun", func(t *testing.T) { redactSubRun(t, open(t)) })
	t.Run("Session", func(t *testing.T) { redactSession(t, open(t)) })
	t.Run("Lease", func(t *testing.T) { redactLease(t, open(t)) })
	t.Run("Context", func(t *testing.T) { redactContext(t, open(t)) })
	t.Run("ConcurrentSameEntry", func(t *testing.T) { redactConcurrent(t, open) })
	t.Run("ConcurrentWithWriters", func(t *testing.T) { redactWithWriters(t, open) })
	t.Run("RunPlan", func(t *testing.T) { redactRunPlan(t, open(t)) })
}

func redactor(t *testing.T, s agent.Store) agent.Redactor {
	t.Helper()
	r, ok := agent.Capability[agent.Redactor](s)
	if !ok {
		t.Fatalf("%T does not implement agent.Redactor", s)
	}
	return r
}

// put stores a record of kind named name with result, and returns the bytes stored.
func put(t *testing.T, s agent.Store, id, name string, kind agent.StepKind, result string) []byte {
	t.Helper()
	b, err := agent.JournalEntry(name, agent.Record{Kind: kind, Result: json.RawMessage(result)})
	if err != nil {
		t.Fatal(err)
	}
	e, ok, err := s.Insert(context.Background(), id, name, b)
	if err != nil || !ok {
		t.Fatalf("Insert %q = %v, %v", name, ok, err)
	}
	return e.Data
}

// redactRun builds a run holding a header, a payload tool result, a payload step and, if over is
// set, an end marker. It returns the run ID and the bytes of the tool result.
func redactRun(t *testing.T, s agent.Store, over bool) (string, []byte) {
	t.Helper()
	id := runID(t)
	j := journal(t, s)
	if _, err := j.Step(context.Background(), id, "before", func(context.Context) (string, error) { return "kept", nil }); err != nil {
		t.Fatal(err)
	}
	tool := put(t, s, id, "tool:a", agent.StepToolResult, `{"ssn":"123-45-6789"}`)
	put(t, s, id, "tool:b", agent.StepToolResult, `{"name":"Alice"}`)
	if over {
		put(t, s, id, "run:complete", agent.StepValue, `"done"`)
	}
	return id, tool
}

func tombstoneFor(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	tomb, err := agent.NewTombstone(name, data, time.UnixMilli(1_700_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	return tomb
}

// snapshot returns every entry of the run.
func snapshot(t *testing.T, s agent.Store, id string) []agent.Entry {
	t.Helper()
	var out []agent.Entry
	for e, err := range s.Load(context.Background(), id, -1) {
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func redactReplaces(t *testing.T, s agent.Store) {
	ctx := context.Background()
	id, tool := redactRun(t, s, true)
	before := snapshot(t, s, id)
	tomb := tombstoneFor(t, "tool:a", tool)
	done, err := redactor(t, s).Redact(ctx, id, "tool:a", tool, tomb)
	if err != nil || !done {
		t.Fatalf("Redact = %v, %v; want it replaced", done, err)
	}
	after := snapshot(t, s, id)
	if len(after) != len(before) {
		t.Fatalf("the run holds %d entries after the redaction, want %d", len(after), len(before))
	}
	for i := range before {
		if before[i].Seq != after[i].Seq || before[i].Name != after[i].Name {
			t.Errorf("entry %d is (%d, %q) after the redaction, was (%d, %q): a redaction keeps position and name", i, after[i].Seq, after[i].Name, before[i].Seq, before[i].Name)
		}
		if before[i].Name == "tool:a" {
			if !bytes.Equal(after[i].Data, tomb) {
				t.Errorf("tool:a holds %q after the redaction, want the tombstone %q", after[i].Data, tomb)
			}
		} else if !bytes.Equal(before[i].Data, after[i].Data) {
			t.Errorf("entry %q changed: a redaction replaces only the entry it names", before[i].Name)
		}
	}
	if e, ok, err := s.Get(ctx, id, "tool:a"); err != nil || !ok || !bytes.Equal(e.Data, tomb) {
		t.Errorf("Get(tool:a) = %q, %v, %v; want the tombstone", e.Data, ok, err)
	}
	// The journal reads the run and reports the record as redacted, under its name.
	hist, err := journal(t, s).History(ctx, id)
	if err != nil {
		t.Fatalf("History of a redacted run: %v", err)
	}
	var seen bool
	for _, r := range hist {
		if r.Name == "tool:a" {
			seen = r.Redacted && bytes.Equal(r.Raw(), tomb)
		}
	}
	if !seen {
		t.Errorf("History does not report tool:a as redacted with the tombstone as its stored bytes")
	}
	// A new write is still possible at the store (a straggler); the redacted run is over.
	if _, ok, err := s.Insert(ctx, id, "@spend-late/x", rawRecord(t, "@spend-late/x")); err != nil || !ok {
		t.Errorf("Insert after a redaction = %v, %v", ok, err)
	}
}

func redactIdempotent(t *testing.T, s agent.Store) {
	ctx := context.Background()
	id, tool := redactRun(t, s, true)
	r := redactor(t, s)
	first := tombstoneFor(t, "tool:a", tool)
	if done, err := r.Redact(ctx, id, "tool:a", tool, first); err != nil || !done {
		t.Fatalf("first Redact = %v, %v", done, err)
	}
	// The same call again, and the same call stamped at another time, replace nothing.
	other, err := agent.NewTombstone("tool:a", tool, time.UnixMilli(1_800_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	for _, tomb := range [][]byte{first, other} {
		done, err := r.Redact(ctx, id, "tool:a", tool, tomb)
		if err != nil || done {
			t.Errorf("repeated Redact = %v, %v; want false, nil", done, err)
		}
	}
	if e, _, _ := s.Get(ctx, id, "tool:a"); !bytes.Equal(e.Data, first) {
		t.Errorf("a repeated redaction changed the tombstone to %q", e.Data)
	}
}

func redactRefusals(t *testing.T, s agent.Store) {
	ctx := context.Background()
	r := redactor(t, s)
	expect := func(t *testing.T, id, name string, current, tomb []byte, want error) {
		t.Helper()
		before := snapshot(t, s, id)
		done, err := r.Redact(ctx, id, name, current, tomb)
		if done || !errors.Is(err, want) || !errors.Is(err, agent.ErrConfig) {
			t.Errorf("Redact = %v, %v; want a refusal wrapping %v and ErrConfig", done, err, want)
		}
		after := snapshot(t, s, id)
		if len(before) != len(after) {
			t.Fatalf("a refused redaction changed the number of entries")
		}
		for i := range before {
			if !bytes.Equal(before[i].Data, after[i].Data) {
				t.Errorf("a refused redaction changed entry %q", before[i].Name)
			}
		}
	}

	t.Run("NotOver", func(t *testing.T) {
		id, tool := redactRun(t, s, false)
		expect(t, id, "tool:a", tool, tombstoneFor(t, "tool:a", tool), agent.ErrRedactNotTerminal)
	})
	t.Run("Protected", func(t *testing.T) {
		id, _ := redactRun(t, s, true)
		for _, name := range []string{"@journal", "run:complete"} {
			e, ok, err := s.Get(ctx, id, name)
			if err != nil || !ok {
				t.Fatalf("Get %q = %v, %v", name, ok, err)
			}
			expect(t, id, name, e.Data, tombstoneFor(t, name, e.Data), agent.ErrRedactProtected)
		}
		att := put(t, s, id, "attempt:tool:a", agent.StepAttempt, `null`)
		expect(t, id, "attempt:tool:a", att, tombstoneFor(t, "attempt:tool:a", att), agent.ErrRedactProtected)
		fail := put(t, s, id, "tool:saga", agent.StepSagaFail, `"x"`)
		expect(t, id, "tool:saga", fail, tombstoneFor(t, "tool:saga", fail), agent.ErrRedactProtected)
		for _, saga := range []string{"@saga/compensate/c1", "@saga/args/c1"} {
			b := put(t, s, id, saga, agent.StepValue, `{}`)
			expect(t, id, saga, b, tombstoneFor(t, saga, b), agent.ErrRedactProtected)
		}
		tally := put(t, s, id, "approval-tally:x", agent.StepValue, `{}`)
		expect(t, id, "approval-tally:x", tally, tombstoneFor(t, "approval-tally:x", tally), agent.ErrRedactProtected)
	})
	t.Run("HashMismatch", func(t *testing.T) {
		id, tool := redactRun(t, s, true)
		other, _, _ := s.Get(ctx, id, "tool:b")
		expect(t, id, "tool:a", tool, tombstoneFor(t, "tool:a", other.Data), agent.ErrRedactMismatch) // another record's hash
		for _, bad := range []string{
			``, `{}`, `{"redacted":{}}`, `{"redacted":{"leaf_hash":"ab"}}`,
			`{"redacted":{"leaf_hash":"` + fmt.Sprintf("%x", agent.LeafHash(tool)) + `","extra":1}}`,
			`{"redacted":{"leaf_hash":"` + fmt.Sprintf("%X", agent.LeafHash(tool)) + `","at_ms":1}}`, // upper case
			`{"redacted":{"leaf_hash":"` + fmt.Sprintf("%x", agent.LeafHash(tool)) + `","at_ms":-1}}`,
		} {
			expect(t, id, "tool:a", tool, []byte(bad), agent.ErrRedactMismatch)
		}
	})
	t.Run("Name", func(t *testing.T) {
		id, tool := redactRun(t, s, true)
		// A tombstone that names another record, or none, is not a redaction of this entry.
		expect(t, id, "tool:a", tool, tombstoneFor(t, "tool:b", tool), agent.ErrRedactMismatch)
		legacy := fmt.Sprintf(`{"redacted":{"leaf_hash":"%x","at_ms":1}}`, agent.LeafHash(tool))
		expect(t, id, "tool:a", tool, []byte(legacy), agent.ErrRedactMismatch)
	})
	t.Run("Changed", func(t *testing.T) {
		id, _ := redactRun(t, s, true)
		// A well-formed redaction of bytes the entry does not hold: the caller's copy is stale.
		stale, err := agent.JournalEntry("tool:a", agent.Record{Kind: agent.StepToolResult, Result: json.RawMessage(`{"ssn":"other"}`)})
		if err != nil {
			t.Fatal(err)
		}
		expect(t, id, "tool:a", stale, tombstoneFor(t, "tool:a", stale), agent.ErrRedactChanged)
	})
	t.Run("NoEntry", func(t *testing.T) {
		id, _ := redactRun(t, s, true)
		ghost, err := agent.JournalEntry("tool:ghost", agent.Record{Kind: agent.StepToolResult, Result: json.RawMessage(`1`)})
		if err != nil {
			t.Fatal(err)
		}
		expect(t, id, "tool:ghost", ghost, tombstoneFor(t, "tool:ghost", ghost), agent.ErrRedactNoEntry)
		other := runID(t)
		expect2 := func() {
			done, err := r.Redact(ctx, other, "tool:ghost", ghost, tombstoneFor(t, "tool:ghost", ghost))
			if done || !errors.Is(err, agent.ErrRedactNoEntry) {
				t.Errorf("Redact in an unknown run = %v, %v; want ErrRedactNoEntry", done, err)
			}
		}
		expect2()
	})
	t.Run("Undecodable", func(t *testing.T) {
		id := runID(t)
		raw := []byte(`not a record`)
		if _, _, err := s.Insert(ctx, id, "tool:junk", raw); err != nil {
			t.Fatal(err)
		}
		put(t, s, id, "run:complete", agent.StepValue, `"done"`)
		done, err := r.Redact(ctx, id, "tool:junk", raw, tombstoneFor(t, "tool:junk", raw))
		if done || !errors.Is(err, agent.ErrRedactProtected) {
			t.Errorf("Redact of bytes that do not read as a record = %v, %v; want a refusal", done, err)
		}
	})
}

func redactLease(t *testing.T, s agent.Store) {
	leaser, ok := agent.Capability[agent.Leaser](s)
	if !ok {
		t.Skip("the store has no leases")
	}
	ctx := context.Background()
	id, tool := redactRun(t, s, true)
	tomb := tombstoneFor(t, "tool:a", tool)
	if got, err := leaser.AcquireLease(ctx, id, "driver", time.Minute); err != nil || !got {
		t.Fatalf("AcquireLease = %v, %v", got, err)
	}
	done, err := redactor(t, s).Redact(ctx, id, "tool:a", tool, tomb)
	if done || !errors.Is(err, agent.ErrRedactLeased) {
		t.Fatalf("Redact under a live lease = %v, %v; want ErrRedactLeased", done, err)
	}
	if e, _, _ := s.Get(ctx, id, "tool:a"); !bytes.Equal(e.Data, tool) {
		t.Fatalf("a redaction refused for a live lease changed the entry")
	}
	if err := leaser.ReleaseLease(ctx, id, "driver"); err != nil {
		t.Fatal(err)
	}
	if done, err := redactor(t, s).Redact(ctx, id, "tool:a", tool, tomb); err != nil || !done {
		t.Fatalf("Redact after the lease was released = %v, %v", done, err)
	}
}

func redactContext(t *testing.T, s agent.Store) {
	id, tool := redactRun(t, s, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done, err := redactor(t, s).Redact(ctx, id, "tool:a", tool, tombstoneFor(t, "tool:a", tool))
	if done || err == nil {
		t.Fatalf("Redact with a cancelled context = %v, %v; want an error", done, err)
	}
	if e, _, _ := s.Get(context.Background(), id, "tool:a"); !bytes.Equal(e.Data, tool) {
		t.Fatalf("a redaction with a cancelled context changed the entry")
	}
}

// redactConcurrent: many handles redact one entry at once; exactly one replaces it and none fails.
func redactConcurrent(t *testing.T, open func(*testing.T) agent.Store) {
	ctx := context.Background()
	hs := make([]agent.Store, handles)
	for i := range hs {
		hs[i] = open(t)
	}
	id, tool := redactRun(t, hs[0], true)
	tomb := tombstoneFor(t, "tool:a", tool)
	var won atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, writers)
	for g := range writers {
		wg.Go(func() {
			<-start
			done, err := redactor(t, hs[g%handles]).Redact(ctx, id, "tool:a", tool, tomb)
			if err != nil {
				errs <- err
				return
			}
			if done {
				won.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent Redact failed: %v", err)
	}
	if won.Load() != 1 {
		t.Errorf("%d concurrent redactions report they replaced the entry, want exactly 1", won.Load())
	}
	if e, _, _ := hs[1].Get(ctx, id, "tool:a"); !bytes.Equal(e.Data, tomb) {
		t.Errorf("the entry holds %q, want the tombstone", e.Data)
	}
}

// redactWithWriters: inserts into the run and readers of it run while an entry is redacted. No
// reader sees anything but the whole original or the tombstone, no insert is lost or changed, and
// the other entries are untouched.
func redactWithWriters(t *testing.T, open func(*testing.T) agent.Store) {
	ctx := context.Background()
	w, rd, rr := open(t), open(t), open(t)
	id, tool := redactRun(t, w, true)
	tomb := tombstoneFor(t, "tool:a", tool)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var inserted atomic.Int64
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			name := fmt.Sprintf("tool:w%d", i)
			if _, ok, err := w.Insert(ctx, id, name, rawRecord(t, name)); err != nil || !ok {
				t.Errorf("Insert %q = %v, %v", name, ok, err)
				return
			}
			inserted.Add(1)
		}
	})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			for e, err := range rd.Load(ctx, id, -1) {
				if err != nil {
					t.Errorf("Load: %v", err)
					return
				}
				if e.Name == "tool:a" && !bytes.Equal(e.Data, tool) && !bytes.Equal(e.Data, tomb) {
					t.Errorf("a reader saw tool:a as %q, neither the original nor the tombstone", e.Data)
					return
				}
			}
		}
	})
	for inserted.Load() < 3 {
		time.Sleep(time.Millisecond)
	}
	if done, err := redactor(t, rr).Redact(ctx, id, "tool:a", tool, tomb); err != nil || !done {
		t.Errorf("Redact beside writers = %v, %v", done, err)
	}
	n := inserted.Load()
	for inserted.Load() < n+3 {
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
	got := snapshot(t, w, id)
	var last int64 = -1
	for _, e := range got {
		if e.Seq <= last {
			t.Fatalf("positions are not increasing after a redaction beside writers: %d after %d", e.Seq, last)
		}
		last = e.Seq
	}
	if int64(len(got)) < n+5 {
		t.Errorf("the run holds %d entries, want at least the %d inserted plus the originals", len(got), n+5)
	}
}

// redactRunPlan: PlanRedaction and Redact over a Journal on the store.
func redactRunPlan(t *testing.T, s agent.Store) {
	ctx := context.Background()
	j := journal(t, s)
	id, tool := redactRun(t, s, true)
	put(t, s, id, "approval:call1", agent.StepApproval, `null`)
	plan, err := agent.PlanRedaction(ctx, j, id, agent.RedactionSelector{Payloads: true})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Terminal {
		t.Fatalf("a run with run:complete is planned as not over")
	}
	var names []string
	for _, r := range plan.Records {
		names = append(names, r.Name)
	}
	want := []string{"before", "tool:a", "tool:b"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("the payload plan selects %v, want %v (the approval decision is kept, the end marker and header are protected)", names, want)
	}
	if len(plan.Kept) != 1 || plan.Kept[0].Name != "approval:call1" {
		t.Errorf("the plan keeps %+v, want the approval decision listed as kept", plan.Kept)
	}
	res, err := agent.Redact(ctx, j, plan, time.UnixMilli(1_700_000_000_000))
	if err != nil || len(res.Redacted) != 3 || len(res.Already) != 0 {
		t.Fatalf("Redact = %+v, %v; want 3 records replaced", res, err)
	}
	if e, _, _ := s.Get(ctx, id, "tool:a"); bytes.Equal(e.Data, tool) {
		t.Fatalf("tool:a still holds its content")
	}
	// A second pass of the same plan finds everything done; planning again gives the same digest.
	res, err = agent.Redact(ctx, j, plan, time.UnixMilli(1_800_000_000_000))
	if err != nil || len(res.Redacted) != 0 || len(res.Already) != 3 {
		t.Fatalf("second Redact = %+v, %v; want 3 already redacted", res, err)
	}
	again, err := agent.PlanRedaction(ctx, j, id, agent.RedactionSelector{Payloads: true})
	if err != nil || again.Digest != plan.Digest {
		t.Fatalf("the plan of a redacted run has digest %q (%v), want the original %q", again.Digest, err, plan.Digest)
	}
	for _, r := range again.Records {
		if !r.AlreadyRedacted {
			t.Errorf("record %q is not reported as already redacted", r.Name)
		}
	}
	// IncludeApprovals selects the decision too.
	withApprovals, err := agent.PlanRedaction(ctx, j, id, agent.RedactionSelector{Payloads: true, IncludeApprovals: true})
	if err != nil || len(withApprovals.Records) != 4 {
		t.Fatalf("the plan with approvals selects %d records (%v), want 4", len(withApprovals.Records), err)
	}

	// A run that is not over: the plan says so and the store refuses.
	live, _ := redactRun(t, s, false)
	lp, err := agent.PlanRedaction(ctx, j, live, agent.RedactionSelector{Payloads: true})
	if err != nil || lp.Terminal {
		t.Fatalf("a live run is planned as over (%v)", err)
	}
	if _, err := agent.Redact(ctx, j, lp, time.Now()); !errors.Is(err, agent.ErrRedactNotTerminal) {
		t.Fatalf("Redact of a live run = %v, want ErrRedactNotTerminal", err)
	}
	// A plan whose record changed since it was made is refused.
	stale, _ := redactRun(t, s, true)
	sp, _ := agent.PlanRedaction(ctx, j, stale, agent.RedactionSelector{Names: []string{"tool:a"}})
	sp.Records[0].LeafHash = fmt.Sprintf("%x", agent.LeafHash([]byte("x")))
	if _, err := agent.Redact(ctx, j, sp, time.Now()); err == nil {
		t.Fatalf("Redact accepted a plan whose digest no longer matches its records")
	}
}

// A tombstone is never replaced by another: its leaf hash must stay the hash of the original record.
func redactTwice(t *testing.T, s agent.Store) {
	ctx := context.Background()
	id, tool := redactRun(t, s, true)
	r := redactor(t, s)
	first := tombstoneFor(t, "tool:a", tool)
	if done, err := r.Redact(ctx, id, "tool:a", tool, first); err != nil || !done {
		t.Fatalf("Redact = %v, %v", done, err)
	}
	// A hand-built tombstone that keeps the hash of the first tombstone, as the bytes it replaces.
	second := fmt.Appendf(nil, `{"redacted":{"leaf_hash":"%x","at_ms":1,"name":"tool:a"}}`, agent.LeafHash(first))
	done, err := r.Redact(ctx, id, "tool:a", first, second)
	if done || !errors.Is(err, agent.ErrRedactMismatch) {
		t.Fatalf("Redact of a tombstone = %v, %v; want ErrRedactMismatch", done, err)
	}
	if e, _, _ := s.Get(ctx, id, "tool:a"); !bytes.Equal(e.Data, first) {
		t.Fatalf("the tombstone was replaced by %q", e.Data)
	}
}

// A sub-agent's run is read by its parent when the parent replays the call, so it is redacted only
// once every run above it is over.
func redactSubRun(t *testing.T, s agent.Store) {
	ctx := context.Background()
	parent, _ := redactRun(t, s, false)
	child := agent.SubRunID(parent, "call1")
	childEnd := put(t, s, child, "run:complete", agent.StepValue, `"done"`)
	_ = childEnd
	payload := put(t, s, child, "tool:x", agent.StepToolResult, `{"pii":1}`)
	tomb := tombstoneFor(t, "tool:x", payload)
	r := redactor(t, s)
	done, err := r.Redact(ctx, child, "tool:x", payload, tomb)
	if done || !errors.Is(err, agent.ErrRedactParentLive) {
		t.Fatalf("Redact in a sub-run whose parent is live = %v, %v; want ErrRedactParentLive", done, err)
	}
	if e, _, _ := s.Get(ctx, child, "tool:x"); !bytes.Equal(e.Data, payload) {
		t.Fatalf("a refused redaction changed the sub-run")
	}
	plan, err := agent.PlanRedaction(ctx, journal(t, s), child, agent.RedactionSelector{Names: []string{"tool:x"}})
	if err == nil && plan.Refusal == "" {
		t.Errorf("the plan of a sub-run with a live parent has no Refusal")
	}
	// A grandchild is refused for the live run two levels up.
	grand := agent.SubRunID(child, "call2")
	put(t, s, grand, "run:complete", agent.StepValue, `"done"`)
	gp := put(t, s, grand, "tool:y", agent.StepToolResult, `{"pii":2}`)
	if done, err := r.Redact(ctx, grand, "tool:y", gp, tombstoneFor(t, "tool:y", gp)); done || !errors.Is(err, agent.ErrRedactParentLive) {
		t.Fatalf("Redact in a grandchild under a live root = %v, %v; want ErrRedactParentLive", done, err)
	}
	// Once the parent is over the child may be redacted.
	put(t, s, parent, "run:complete", agent.StepValue, `"done"`)
	if done, err := r.Redact(ctx, child, "tool:x", payload, tomb); err != nil || !done {
		t.Fatalf("Redact after the parent ended = %v, %v", done, err)
	}
}

// A session's runs are never redacted: the session reads its turns' runs and does not end.
func redactSession(t *testing.T, s agent.Store) {
	ctx := context.Background()
	sid := runID(t)
	turn := sid + ">@turn/1"
	put(t, s, turn, "run:complete", agent.StepValue, `"done"`)
	payload := put(t, s, turn, "tool:x", agent.StepToolResult, `{"pii":1}`)
	done, err := redactor(t, s).Redact(ctx, turn, "tool:x", payload, tombstoneFor(t, "tool:x", payload))
	if done || !errors.Is(err, agent.ErrRedactSession) {
		t.Fatalf("Redact in a session turn run = %v, %v; want ErrRedactSession", done, err)
	}
	sub := agent.SubRunID(turn, "c")
	put(t, s, sub, "run:complete", agent.StepValue, `"done"`)
	sp := put(t, s, sub, "tool:x", agent.StepToolResult, `{"pii":1}`)
	if done, err := redactor(t, s).Redact(ctx, sub, "tool:x", sp, tombstoneFor(t, "tool:x", sp)); done || !errors.Is(err, agent.ErrRedactSession) {
		t.Fatalf("Redact in a sub-run of a session turn = %v, %v; want ErrRedactSession", done, err)
	}
}
