package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// ReportWritten says whether this call's Insert stored the record. Of concurrent identical
// submissions through separate Journals over one store, exactly one sees true; a resubmission, and a
// call refused before it writes, see false.
func TestSubmitDecision_ReportWritten(t *testing.T) {
	ctx := context.Background()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	vf := fakeVerifiers("a")
	store := NewMemStore()
	j := mustJournal(store)
	var charged int
	if _, err := mofnRun(j, "r", true, pol, vf, &charged); err == nil {
		t.Fatal("run did not pause")
	}
	sig := fakeSign("a", ApprovalDecisionBytes(subjectOf(t, j, "r", "c1"), "a", true, testDecidedAt))
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: sig}

	const n = 8
	wrote := make([]bool, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			// Each call has a Journal of its own over the store, as separate handlers of one process
			// (or processes sharing a database) do.
			if err := SubmitDecision(ctx, mustJournal(store), d, WithDecisionCheck(vf), ReportWritten(&wrote[i])); err != nil {
				t.Errorf("call %d: %v", i, err)
			}
		})
	}
	close(start)
	wg.Wait()
	got := 0
	for _, w := range wrote {
		if w {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("%d of %d concurrent identical submissions report they wrote, want exactly 1: %v", got, n, wrote)
	}

	var again bool
	if err := SubmitDecision(ctx, j, d, ReportWritten(&again)); err != nil || again {
		t.Fatalf("a resubmission: %v, wrote %v", err, again)
	}
	bad := d
	bad.Signature = []byte("forged")
	var refused = true
	if err := SubmitDecision(ctx, j, bad, WithDecisionCheck(vf), ReportWritten(&refused)); !errors.Is(err, ErrInvalidApproval) || refused {
		t.Fatalf("a refused decision: %v, wrote %v", err, refused)
	}
	if err := SubmitDecision(ctx, j, d, ReportWritten(nil)); !errors.Is(err, ErrConfig) {
		t.Fatalf("a nil pointer: %v", err)
	}
}

// A record another writer stored first under the same name is the journal's: this call did not write.
func TestSubmitDecision_ReportWrittenWhenInsertLoses(t *testing.T) {
	ctx := context.Background()
	pol := &ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	vf := fakeVerifiers("a")
	inner := NewMemStore()
	j := mustJournal(inner)
	var charged int
	if _, err := mofnRun(j, "r", true, pol, vf, &charged); err == nil {
		t.Fatal("run did not pause")
	}
	sig := fakeSign("a", ApprovalDecisionBytes(subjectOf(t, j, "r", "c1"), "a", true, testDecidedAt))
	d := Decision{RunID: "r", ToolUseID: "c1", ApproverID: "a", Approved: true, DecidedAt: testDecidedAt, Alg: fakeAlg, Signature: sig}
	name := DecisionStep(d)
	rival := Record{Name: name, Kind: StepApproval, ToolUseID: "c1", Approved: true, ApproverSignature: &ApproverSignature{
		Approver: "a", ApproverAlg: fakeAlg, Signature: sig, DecidedAt: FormatTimestamp(d.DecidedAt), RecordedAt: "2026-10-13T11:00:00.000Z"}}
	data, err := JournalEntry(name, rival)
	if err != nil {
		t.Fatal(err)
	}
	// The rival's Insert lands between this call's read of the step and its own Insert.
	var wrote bool
	racy := mustJournal(&insertRival{Store: inner, name: name, data: data})
	if err := SubmitDecision(ctx, racy, d, ReportWritten(&wrote)); err != nil || wrote {
		t.Fatalf("SubmitDecision = %v, wrote %v; want nil, false", err, wrote)
	}
}

// insertRival stores a rival record under name just before the first Insert of that name.
type insertRival struct {
	Store
	name string
	data []byte
	once sync.Once
}

func (s *insertRival) Unwrap() Store { return s.Store }

func (s *insertRival) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == s.name {
		s.once.Do(func() { s.Store.Insert(ctx, runID, name, s.data) })
	}
	return s.Store.Insert(ctx, runID, name, data)
}
