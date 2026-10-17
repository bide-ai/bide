package audit_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/agent/agenttest"
	"github.com/bide-ai/bide/audit"
)

type trailRig struct {
	t       *testing.T
	store   *agent.MemStore
	j       *agent.Journal
	trail   *audit.MemEventStore
	signer  audit.Signer
	v       audit.Verifier
	head    audit.SignedTreeHead
	plan    agent.RedactionPlan
	options audit.RedactionOptions
}

func newTrailRig(t *testing.T) *trailRig {
	t.Helper()
	ctx := context.Background()
	s, _ := p11GovernedRun(t)
	if _, _, err := s.Insert(ctx, "gov", "run:complete", mustEntry(t, "run:complete", agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"ok"`)})); err != nil {
		t.Fatal(err)
	}
	r := &trailRig{t: t, store: s, j: agenttest.MustJournal(s), trail: audit.NewMemEventStore()}
	r.signer = p11Signers(t)["ed25519"]
	r.v = p11Verifier(t, r.signer)
	th, err := audit.NewTreeHead(ctx, r.j, "gov", p11Now())
	if err != nil {
		t.Fatal(err)
	}
	if r.head, err = audit.SignTreeHead(th, r.signer); err != nil {
		t.Fatal(err)
	}
	if r.plan, err = agent.PlanRedaction(ctx, r.j, "gov", agent.RedactionSelector{Names: []string{"call:pay", "note"}}); err != nil {
		t.Fatal(err)
	}
	r.options = audit.RedactionOptions{Reason: "customer request", Ticket: "PRIV-42", Executor: "alice", Now: time.UnixMilli(1_700_000_000_000),
		Version: "test", Heads: []audit.SignedTreeHead{r.head}, Verifier: r.v, Signer: r.signer}
	return r
}

// signedTrail signs the trail as it stands and returns its leaves and head.
func (r *trailRig) signedTrail() ([][]byte, audit.SignedTreeHead) {
	r.t.Helper()
	h, err := audit.SignNotifyTrail(context.Background(), r.trail, audit.RedactionTrailName, r.signer, time.Unix(0, p11Now()))
	if err != nil {
		r.t.Fatal(err)
	}
	leaves, err := r.trail.Load(context.Background(), audit.RedactionTrailName)
	if err != nil {
		r.t.Fatal(err)
	}
	return leaves, h
}

func (r *trailRig) verify(leaves [][]byte, heads ...audit.SignedTreeHead) (audit.RedactionReport, error) {
	recs, err := r.j.History(context.Background(), "gov")
	if err != nil {
		r.t.Fatal(err)
	}
	return audit.VerifyRedactions(audit.RedactionVerifyInput{Trail: leaves, Heads: heads, Verifier: r.v, Journals: map[string][]agent.Record{"gov": recs}})
}

func kinds(rep audit.RedactionReport) string {
	var k []string
	for _, f := range rep.Findings {
		k = append(k, string(f.Kind))
	}
	return strings.Join(k, ",")
}

func TestRedactJournal_AccountableAndVerifies(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	res, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options)
	if err != nil || len(res.Redacted) != 2 {
		t.Fatalf("RedactJournal = %+v, %v", res, err)
	}
	recs, err := audit.LoadRedactionTrail(ctx, r.trail)
	if err != nil || len(recs) != 2 || recs[0].Phase != audit.RedactionBegin || recs[1].Phase != audit.RedactionEnd {
		t.Fatalf("trail = %+v, %v; want begin then end", recs, err)
	}
	b := recs[0]
	if b.Reason != "customer request" || b.Ticket != "PRIV-42" || b.Executor != "alice" || b.RunID != "gov" || b.HeadSize != r.head.Size || b.Uncovered || len(b.Records) != 2 {
		t.Errorf("begin record = %+v", b)
	}
	if recs[1].Redacted != 2 || recs[1].Error != "" {
		t.Errorf("end record = %+v", recs[1])
	}
	for _, rf := range b.Records {
		if strings.Contains(rf.Name+rf.LeafHash, "pii") || rf.LeafHash == "" {
			t.Errorf("ref %+v", rf)
		}
	}
	leaves, head := r.signedTrail()
	rep, err := r.verify(leaves, head)
	if err != nil || !rep.OK() || !rep.Anchored || rep.Redactions != 1 {
		t.Fatalf("VerifyRedactions = %+v, %v", rep, err)
	}
	// Running the same plan again finds everything done, and is recorded again.
	if res, err = audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil || len(res.Redacted) != 0 || len(res.Already) != 2 {
		t.Fatalf("second RedactJournal = %+v, %v", res, err)
	}
}

func TestRedactJournal_Refusals(t *testing.T) {
	ctx := context.Background()
	erased := func(r *trailRig) bool {
		recs, _ := r.j.History(ctx, "gov")
		for _, x := range recs {
			if x.Redacted {
				return true
			}
		}
		return false
	}
	t.Run("NoCoveringHead", func(t *testing.T) {
		r := newTrailRig(t)
		r.options.Heads = nil
		if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); !errors.Is(err, audit.ErrRedactNoHead) || !errors.Is(err, agent.ErrConfig) {
			t.Fatalf("err = %v, want ErrRedactNoHead", err)
		}
		if erased(r) {
			t.Error("a refused redaction erased a record")
		}
		if leaves, _ := r.trail.Load(ctx, audit.RedactionTrailName); len(leaves) != 0 {
			t.Error("a refused redaction wrote the trail")
		}
	})
	t.Run("HeadOfAnotherJournal", func(t *testing.T) {
		r := newTrailRig(t)
		// A head over a shorter prefix does not cover the run as it stands.
		n := r.head.Size - 1
		th := audit.TreeHead{Kind: audit.TreeJournal, RunID: "gov", Size: n, Root: rootOfPrefix(ctx, t, r.store, "gov", n), TimestampNanos: p11Now()}
		short, err := audit.SignTreeHead(th, r.signer)
		if err != nil {
			t.Fatal(err)
		}
		r.options.Heads = []audit.SignedTreeHead{short}
		if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); !errors.Is(err, audit.ErrRedactNoHead) {
			t.Fatalf("err = %v, want ErrRedactNoHead", err)
		}
		// A head the wrong key signed.
		other := p11Verifier(t, p11Signers(t)["ecdsa-p256"])
		r.options.Heads, r.options.Verifier = []audit.SignedTreeHead{r.head}, other
		if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); !errors.Is(err, audit.ErrRedactNoHead) {
			t.Fatalf("wrong key: err = %v, want ErrRedactNoHead", err)
		}
	})
	t.Run("AllowUncoveredIsRecorded", func(t *testing.T) {
		r := newTrailRig(t)
		r.options.Heads, r.options.AllowUncovered = nil, true
		if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil {
			t.Fatal(err)
		}
		leaves, head := r.signedTrail()
		rep, err := r.verify(leaves, head)
		if err == nil || kinds(rep) != "UNCOVERED" {
			t.Fatalf("report = %+v, %v; want the UNCOVERED finding", rep, err)
		}
	})
	t.Run("MissingFields", func(t *testing.T) {
		for _, mut := range []func(*audit.RedactionOptions){
			func(o *audit.RedactionOptions) { o.Reason = " " }, func(o *audit.RedactionOptions) { o.Ticket = "" },
			func(o *audit.RedactionOptions) { o.Executor = "" }, func(o *audit.RedactionOptions) { o.Now = time.Time{} },
		} {
			r := newTrailRig(t)
			mut(&r.options)
			if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); !errors.Is(err, agent.ErrConfig) || erased(r) {
				t.Fatalf("err = %v erased=%v", err, erased(r))
			}
		}
	})
	t.Run("RefusedPlan", func(t *testing.T) {
		r := newTrailRig(t)
		r.plan.Refusal = "a run above it is not over"
		if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); !errors.Is(err, agent.ErrConfig) || erased(r) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("TrailUnavailableErasesNothing", func(t *testing.T) {
		r := newTrailRig(t)
		_, err := audit.RedactJournal(ctx, r.j, failingTrail{r.trail}, r.plan, r.options)
		if !errors.Is(err, audit.ErrRedactNoTrail) || erased(r) {
			t.Fatalf("err = %v erased=%v; want ErrRedactNoTrail and nothing erased", err, erased(r))
		}
	})
	t.Run("EndRecordFailureKeepsTheErasureAndSaysSo", func(t *testing.T) {
		r := newTrailRig(t)
		ft := &failNth{EventStore: r.trail, failAt: 1}
		_, err := audit.RedactJournal(ctx, r.j, ft, r.plan, r.options)
		if err == nil || !strings.Contains(err.Error(), "end record was not written") || !erased(r) {
			t.Fatalf("err = %v erased=%v", err, erased(r))
		}
		leaves, head := r.signedTrail()
		if rep, _ := r.verify(leaves, head); !strings.Contains(kinds(rep), "INCOMPLETE") {
			t.Fatalf("report = %+v; want INCOMPLETE", rep)
		}
		// Running the plan again closes it.
		if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil {
			t.Fatal(err)
		}
	})
}

type failingTrail struct{ audit.EventStore }

func (failingTrail) Append(context.Context, string, int, []byte) error {
	return errors.New("disk full")
}

// failNth fails the Append at trail position failAt, once.
type failNth struct {
	audit.EventStore
	failAt int
	done   bool
}

func (f *failNth) Append(ctx context.Context, run string, seq int, leaf []byte) error {
	if seq == f.failAt && !f.done {
		f.done = true
		return errors.New("disk full")
	}
	return f.EventStore.Append(ctx, run, seq, leaf)
}

func TestVerifyRedactions_Findings(t *testing.T) {
	ctx := context.Background()
	setup := func() (*trailRig, [][]byte, audit.SignedTreeHead) {
		r := newTrailRig(t)
		if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil {
			t.Fatal(err)
		}
		leaves, head := r.signedTrail()
		return r, leaves, head
	}
	t.Run("Unanchored", func(t *testing.T) {
		r, leaves, _ := setup()
		rep, err := r.verify(leaves)
		if err == nil || kinds(rep) != "UNANCHORED" {
			t.Fatalf("report = %+v, %v", rep, err)
		}
		rep, err = audit.VerifyRedactions(audit.RedactionVerifyInput{Trail: leaves, Verifier: r.v, Unanchored: true, Journals: r.journals()})
		if err != nil || !rep.OK() {
			t.Fatalf("an allowed unanchored trail = %+v, %v", rep, err)
		}
	})
	t.Run("DeletedRecord", func(t *testing.T) {
		r, leaves, head := setup()
		rep, err := r.verify(leaves[:1], head)
		if err == nil || !strings.Contains(kinds(rep), "TRAIL_ALTERED") {
			t.Fatalf("report = %+v, %v; want TRAIL_ALTERED", rep, err)
		}
	})
	t.Run("ChangedRecord", func(t *testing.T) {
		r, leaves, head := setup()
		leaves = append([][]byte(nil), leaves...)
		leaves[0] = bytes.Replace(leaves[0], []byte("PRIV-42"), []byte("PRIV-43"), 1)
		rep, err := r.verify(leaves, head)
		if err == nil || !strings.Contains(kinds(rep), "TRAIL_ALTERED") {
			t.Fatalf("report = %+v, %v", rep, err)
		}
	})
	t.Run("UnattestedTail", func(t *testing.T) {
		r, leaves, head := setup()
		r.options.Now = time.UnixMilli(1_700_000_001_000)
		if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil {
			t.Fatal(err)
		}
		more, _ := r.trail.Load(ctx, audit.RedactionTrailName)
		rep, err := r.verify(more, head)
		_ = leaves
		if err == nil || kinds(rep) != "UNATTESTED_TAIL" {
			t.Fatalf("report = %+v, %v", rep, err)
		}
	})
	t.Run("UnaccountedTombstone", func(t *testing.T) {
		r := newTrailRig(t)
		// An erasure made outside the tool: the journal holds a tombstone, the trail is empty.
		plan, _ := agent.PlanRedaction(ctx, r.j, "gov", agent.RedactionSelector{Names: []string{"note"}})
		if _, err := agent.Redact(ctx, r.j, plan, time.Now()); err != nil {
			t.Fatal(err)
		}
		rep, err := audit.VerifyRedactions(audit.RedactionVerifyInput{Trail: nil, Verifier: r.v, Unanchored: true, Journals: r.journals()})
		if err == nil || kinds(rep) != "UNACCOUNTED" {
			t.Fatalf("report = %+v, %v; want UNACCOUNTED", rep, err)
		}
	})
	t.Run("RenamedTombstone", func(t *testing.T) {
		r, leaves, head := setup()
		recs, _ := r.j.History(ctx, "gov")
		for i := range recs {
			if recs[i].Redacted && recs[i].Name == "note" {
				raw := bytes.Replace(recs[i].Raw(), []byte(`"name":"note"`), []byte(`"name":"approval:x"`), 1)
				rec, err := agent.DecodeStoredRecord("gov", "note", raw)
				if err == nil {
					recs[i] = rec
				}
				// the row name stays "note"; a rename in an export shows as another name
				x, _ := audit.ExportJournal(ctx, r.j, "gov")
				x.Records[i] = raw
				exp, err := x.Journal()
				if err == nil {
					recs = exp
				}
			}
		}
		rep, err := audit.VerifyRedactions(audit.RedactionVerifyInput{Trail: leaves, Heads: []audit.SignedTreeHead{head}, Verifier: r.v, Journals: map[string][]agent.Record{"gov": recs}})
		if err == nil || !strings.Contains(kinds(rep), "NAME_MISMATCH") {
			t.Fatalf("report = %+v, %v; want NAME_MISMATCH", rep, err)
		}
	})
	t.Run("NotErased", func(t *testing.T) {
		r, leaves, head := setup()
		// A journal as it was before the erasure, under the trail that says it was erased.
		orig, _ := p11GovernedRun(t)
		orig.Insert(ctx, "gov", "run:complete", mustEntry(t, "run:complete", agent.Record{Kind: agent.StepValue, Result: json.RawMessage(`"ok"`)}))
		recs, _ := agenttest.MustJournal(orig).History(ctx, "gov")
		rep, err := audit.VerifyRedactions(audit.RedactionVerifyInput{Trail: leaves, Heads: []audit.SignedTreeHead{head}, Verifier: r.v, Journals: map[string][]agent.Record{"gov": recs}})
		if err == nil || !strings.Contains(kinds(rep), "NOT_ERASED") {
			t.Fatalf("report = %+v, %v; want NOT_ERASED", rep, err)
		}
	})
	t.Run("BeginWithoutEnd", func(t *testing.T) {
		r, leaves, _ := setup()
		short := leaves[:1]
		store := audit.NewMemEventStore()
		for i, l := range short {
			store.Append(ctx, audit.RedactionTrailName, i, l)
		}
		h, _ := audit.SignNotifyTrail(ctx, store, audit.RedactionTrailName, r.signer, time.Unix(0, p11Now()))
		rep, err := r.verify(short, h)
		if err == nil || !strings.Contains(kinds(rep), "INCOMPLETE") {
			t.Fatalf("report = %+v, %v", rep, err)
		}
	})
	t.Run("MalformedAndForeignRecords", func(t *testing.T) {
		r := newTrailRig(t)
		rep, err := audit.VerifyRedactions(audit.RedactionVerifyInput{Trail: [][]byte{[]byte("junk")}, Verifier: r.v, Unanchored: true})
		if err == nil || !strings.Contains(kinds(rep), "MALFORMED") {
			t.Fatalf("report = %+v, %v", rep, err)
		}
	})
}

func (r *trailRig) journals() map[string][]agent.Record {
	recs, err := r.j.History(context.Background(), "gov")
	if err != nil {
		r.t.Fatal(err)
	}
	return map[string][]agent.Record{"gov": recs}
}

// A Redactor given in the options does the replacing, while the journal still supplies the records:
// the arrangement of a Postgres erasing role that can read nothing.
func TestRedactJournal_UsesTheGivenRedactor(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	rd := &countingRedactor{Redactor: r.store}
	r.options.Redactor = rd
	if res, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil || len(res.Redacted) != 2 || rd.n != 2 {
		t.Fatalf("RedactJournal = %+v, %v; the given redactor was called %d times, want 2", res, err, rd.n)
	}
}

type countingRedactor struct {
	agent.Redactor
	n int
}

func (c *countingRedactor) Redact(ctx context.Context, run, name string, cur, tomb []byte) (bool, error) {
	c.n++
	return c.Redactor.Redact(ctx, run, name, cur, tomb)
}

// An erasure made by hand is read by a later plan as "already redacted". Listing it in a signed
// begin record must not account for it.
func TestVerifyRedactions_HandTombstoneIsNotLaunderedByALaterPlan(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	hand, err := agent.PlanRedaction(ctx, r.j, "gov", agent.RedactionSelector{Names: []string{"note"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Redact(ctx, r.j, hand, time.Now()); err != nil { // outside the tool: nothing is recorded
		t.Fatal(err)
	}
	plan, err := agent.PlanRedaction(ctx, r.j, "gov", agent.RedactionSelector{Names: []string{"call:pay", "note"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.RedactJournal(ctx, r.j, r.trail, plan, r.options); err != nil {
		t.Fatal(err)
	}
	leaves, head := r.signedTrail()
	rep, err := r.verify(leaves, head)
	if err == nil || kinds(rep) != "UNACCOUNTED" {
		t.Fatalf("report = %+v, %v; want only the hand tombstone UNACCOUNTED", rep, err)
	}
}

// A pass that stopped and was run again to the end is closed: the audit judges each plan by its
// last record, and says what failed earlier as a note.
func TestVerifyRedactions_AFinishedRetryClearsAnEarlierFailure(t *testing.T) {
	ctx := context.Background()
	t.Run("EndRecordMissingThenRetried", func(t *testing.T) {
		r := newTrailRig(t)
		ft := &failNth{EventStore: r.trail, failAt: 1}
		if _, err := audit.RedactJournal(ctx, r.j, ft, r.plan, r.options); err == nil {
			t.Fatal("the injected failure did not show")
		}
		r.options.Now = time.UnixMilli(1_700_000_001_000)
		plan, _ := agent.PlanRedaction(ctx, r.j, "gov", agent.RedactionSelector{Names: []string{"call:pay", "note"}})
		if _, err := audit.RedactJournal(ctx, r.j, r.trail, plan, r.options); err != nil {
			t.Fatal(err)
		}
		leaves, head := r.signedTrail()
		if rep, err := r.verify(leaves, head); err != nil || !rep.OK() {
			t.Fatalf("report = %+v, %v; the finished retry should close the pass", rep, err)
		}
	})
	t.Run("FailedThenSucceeded", func(t *testing.T) {
		r := newTrailRig(t)
		// A first pass that stops with an error recorded in its end record (a lease held).
		leaser := r.store
		if ok, err := leaser.AcquireLease(ctx, "gov", "driver", time.Minute); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err == nil {
			t.Fatal("a leased run was erased")
		}
		if err := leaser.ReleaseLease(ctx, "gov", "driver"); err != nil {
			t.Fatal(err)
		}
		leaves, head := r.signedTrail()
		if rep, err := r.verify(leaves, head); err == nil || !strings.Contains(kinds(rep), "INCOMPLETE") {
			t.Fatalf("while failed: report = %+v, %v; want INCOMPLETE", rep, err)
		}
		r.options.Now = time.UnixMilli(1_700_000_002_000)
		if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil {
			t.Fatal(err)
		}
		leaves, head = r.signedTrail()
		rep, err := r.verify(leaves, head)
		if err != nil || !rep.OK() || len(rep.Notes) != 1 {
			t.Fatalf("after the retry: report = %+v, %v; want OK with one note about the earlier failure", rep, err)
		}
	})
}

// A trail checked against no journal checked nothing.
func TestVerifyRedactions_NoJournalIsNotAPass(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil {
		t.Fatal(err)
	}
	leaves, head := r.signedTrail()
	rep, err := audit.VerifyRedactions(audit.RedactionVerifyInput{Trail: leaves, Heads: []audit.SignedTreeHead{head}, Verifier: r.v})
	if err == nil || kinds(rep) != "UNCHECKED" {
		t.Fatalf("report = %+v, %v; want UNCHECKED", rep, err)
	}
}

// A tombstone is stamped with the time of a pass that listed it.
func TestVerifyRedactions_TombstoneTimeIsChecked(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil {
		t.Fatal(err)
	}
	leaves, head := r.signedTrail()
	x, _ := audit.ExportJournal(ctx, r.j, "gov")
	for i, b := range x.Records {
		if n, ok := agent.TombstoneName(b); ok && n == "note" {
			x.Records[i] = bytes.Replace(b, []byte(`"at_ms":1700000000000`), []byte(`"at_ms":1600000000000`), 1)
		}
	}
	recs, err := x.Journal()
	if err != nil {
		t.Fatal(err)
	}
	rep, err := audit.VerifyRedactions(audit.RedactionVerifyInput{Trail: leaves, Heads: []audit.SignedTreeHead{head}, Verifier: r.v, Journals: map[string][]agent.Record{"gov": recs}})
	if err == nil || kinds(rep) != "TIME_MISMATCH" {
		t.Fatalf("report = %+v, %v; want TIME_MISMATCH", rep, err)
	}
}

// plant appends to the trail a begin and end pair that names a hand-made tombstone as erased by a
// pass, written by someone who can append to the event store and holds no key (or the wrong one).
func (r *trailRig) plant(signer audit.Signer, runID string) {
	r.t.Helper()
	ctx := context.Background()
	recs, _ := r.j.History(ctx, runID)
	var refs []audit.RedactedRef
	for i, x := range recs {
		if x.Redacted {
			h, _ := agent.TombstoneLeafHash(x.Raw())
			at, _ := agent.TombstoneMillis(x.Raw())
			_ = at
			refs = append(refs, audit.RedactedRef{Position: i, Name: x.Name, LeafHash: hex.EncodeToString(h)})
		}
	}
	b := audit.RedactionRecord{Phase: audit.RedactionBegin, PlanDigest: "planted", RunID: runID, Records: refs, Reason: "x", Ticket: "x", Executor: "x", AtMs: 1_700_000_000_000}
	e := audit.RedactionRecord{Phase: audit.RedactionEnd, PlanDigest: "planted", RunID: runID, Reason: "x", Ticket: "x", Executor: "x", AtMs: 1_700_000_000_000}
	for _, rec := range []audit.RedactionRecord{b, e} {
		if signer == nil {
			// no key at all: an unsigned record straight into the store
			leaf := unsignedLeaf(r.t, rec)
			leaves, _ := r.trail.Load(ctx, audit.RedactionTrailName)
			if err := r.trail.Append(ctx, audit.RedactionTrailName, len(leaves), leaf); err != nil {
				r.t.Fatal(err)
			}
			continue
		}
		if err := audit.AppendRedactionRecord(ctx, r.trail, rec, signer); err != nil {
			r.t.Fatal(err)
		}
	}
}

// A begin and end pair planted through the event store, with no key or the wrong one, is never
// attested: the tool refuses to sign a head over it, and the audit rejects it under any head.
func TestRedactionTrail_PlantedRecordsAreNeverAttested(t *testing.T) {
	ctx := context.Background()
	for name, signer := range map[string]audit.Signer{"no key": nil, "another key": p11Signers(t)["ecdsa-p256"]} {
		r := newTrailRig(t)
		// An erasure made by hand, then a planted record that claims a pass erased it.
		hand, _ := agent.PlanRedaction(ctx, r.j, "gov", agent.RedactionSelector{Names: []string{"note"}})
		if _, err := agent.Redact(ctx, r.j, hand, time.Now()); err != nil {
			t.Fatal(err)
		}
		r.plant(signer, "gov")
		_, err := audit.SignRedactionTrail(ctx, r.trail, r.signer, r.v, nil, time.Unix(0, p11Now()))
		if err == nil || !errors.Is(err, audit.ErrNotVerified) || !strings.Contains(err.Error(), "BAD_SIGNATURE") {
			t.Fatalf("%s: SignRedactionTrail = %v; want a refusal naming BAD_SIGNATURE", name, err)
		}
		// Even a head some other process signed over it does not attest it.
		leaves, head := r.signedTrail()
		rep, err := r.verify(leaves, head)
		if err == nil || !strings.Contains(kinds(rep), "BAD_SIGNATURE") {
			t.Fatalf("%s: report = %+v, %v; want BAD_SIGNATURE", name, rep, err)
		}
	}
}

// The tool signs a head over a trail it wrote, with its earlier heads checked.
func TestSignRedactionTrail_ChecksEarlierHeads(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil {
		t.Fatal(err)
	}
	h1, err := audit.SignRedactionTrail(ctx, r.trail, r.signer, r.v, nil, time.Unix(0, p11Now()))
	if err != nil {
		t.Fatal(err)
	}
	r.options.Now = time.UnixMilli(1_700_000_001_000)
	if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.SignRedactionTrail(ctx, r.trail, r.signer, r.v, []audit.SignedTreeHead{h1}, time.Unix(0, p11Now())); err != nil {
		t.Fatalf("with the earlier head: %v", err)
	}
	// A different trail (a typo in -trail-db starts a new one) does not match the earlier head.
	other := audit.NewMemEventStore()
	r2 := newTrailRig(t)
	r2.trail = other
	if _, err := audit.RedactJournal(ctx, r2.j, other, r2.plan, r2.options); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.SignRedactionTrail(ctx, other, r.signer, r.v, []audit.SignedTreeHead{h1}, time.Unix(0, p11Now())); err == nil || !strings.Contains(err.Error(), "TRAIL_ALTERED") {
		t.Fatalf("a trail the earlier head does not match: %v; want a refusal naming TRAIL_ALTERED", err)
	}
}

// A redaction that leaves events in the event store says so in the trail, and the audit reports it.
func TestRedactJournal_EventCopiesKeptIsRecordedAndReported(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	r.options.EventCopiesKept = 3
	if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil {
		t.Fatal(err)
	}
	leaves, head := r.signedTrail()
	rep, err := r.verify(leaves, head)
	if err == nil || kinds(rep) != "EVENT_COPIES_KEPT" {
		t.Fatalf("report = %+v, %v; want EVENT_COPIES_KEPT", rep, err)
	}
}

// A record signed by the wrong key is rejected, and so is a changed signed record.
func TestVerifyRedactions_RecordSignaturesAreChecked(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	other := p11Signers(t)["ecdsa-p256"]
	r.options.Signer = other
	if _, err := audit.RedactJournal(ctx, r.j, r.trail, r.plan, r.options); err != nil {
		t.Fatal(err)
	}
	leaves, head := r.signedTrail()
	if rep, err := r.verify(leaves, head); err == nil || !strings.Contains(kinds(rep), "BAD_SIGNATURE") {
		t.Fatalf("records signed by another key: %+v, %v", rep, err)
	}
	r2 := newTrailRig(t)
	if _, err := audit.RedactJournal(ctx, r2.j, r2.trail, r2.plan, r2.options); err != nil {
		t.Fatal(err)
	}
	leaves, _ = r2.signedTrail()
	leaves = append([][]byte(nil), leaves...)
	leaves[0] = bytes.Replace(leaves[0], []byte("PRIV-42"), []byte("PRIV-99"), 1)
	changed := audit.NewMemEventStore()
	for i, l := range leaves {
		if err := changed.Append(ctx, audit.RedactionTrailName, i, l); err != nil {
			t.Fatal(err)
		}
	}
	h, err := audit.SignNotifyTrail(ctx, changed, audit.RedactionTrailName, r2.signer, time.Unix(0, p11Now()))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := r2.verify(leaves, h)
	if err == nil || !strings.Contains(kinds(rep), "BAD_SIGNATURE") {
		t.Fatalf("a record changed after signing, under a head that matches the changed trail: %+v, %v", rep, err)
	}
}

// unsignedLeaf is an event leaf holding rec as the trail writes it, with no signature.
func unsignedLeaf(t *testing.T, rec audit.RedactionRecord) []byte {
	t.Helper()
	rec.Format = audit.RedactionRecordFormat
	inner, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	kind := "redaction_begin"
	if rec.Phase == audit.RedactionEnd {
		kind = "redaction_end"
	}
	salt := make([]byte, agent.SaltSize)
	b, err := json.Marshal(map[string]any{"kind": kind, "event": json.RawMessage(inner), "salt": salt})
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte("bide.audit.event-leaf.v3\x00"), b...)
}
