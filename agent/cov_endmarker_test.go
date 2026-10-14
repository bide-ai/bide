package agent

import (
	"context"
	"errors"
	"testing"
)

// corruptEntry stores bytes that do not decode as run r's entry name, after the run's header.
func corruptEntry(t *testing.T, s Store, j *Journal, r, name string) {
	t.Helper()
	if err := j.ensureHeader(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(context.Background(), r, name, []byte(`{"name":`)); err != nil {
		t.Fatal(err)
	}
}

// The first end marker in journal order is a run's end whichever writer asks: writeEnd reports an
// earlier marker over its own, firstEndOf picks the lowest position, and a marker of another kind
// than a value is no end.
func TestEndMarkers_FirstInJournalOrderWins(t *testing.T) {
	ctx := context.Background()
	j := memJournal()
	if got, err := writeEnd(ctx, j, "r", runCompleteStep, Record{Kind: StepValue, Result: []byte(`"done"`)}, []string{runCancelledStep}); err != nil || got.name != runCompleteStep {
		t.Fatalf("first writeEnd = %+v, %v", got, err)
	}
	got, err := writeEnd(ctx, j, "r", runCancelledStep, Record{Kind: StepValue, Result: []byte(`{"reason":"late"}`)}, []string{runCompleteStep})
	if err != nil || got.name != runCompleteStep || string(got.rec.Result) != `"done"` {
		t.Fatalf("later writeEnd = %+v, %v; want the earlier complete marker reported", got, err)
	}
	end, ok, err := firstEndOf(ctx, j, "r", runCancelledStep, runCompleteStep)
	if err != nil || !ok || end.name != runCompleteStep {
		t.Fatalf("firstEndOf = %+v, %v, %v; want complete", end, ok, err)
	}
	if c, err := cancelledFirst(ctx, j, "r"); err != nil || c {
		t.Fatalf("cancelledFirst = %v, %v; want false (complete came first)", c, err)
	}
	// A record of another kind under an end marker's name is no end of the run.
	j2 := memJournal()
	writeRaw(t, j2, "r", runCompleteStep, Record{Kind: StepApproval, ToolUseID: "x"})
	if _, ok, err := firstEndOf(ctx, j2, "r", runCompleteStep); err != nil || ok {
		t.Fatalf("firstEndOf over a non-value record = %v, %v; want none", ok, err)
	}
}

// A marker that cannot be read or decoded is an error, never a guess about which end came first.
func TestEndMarkers_StoreFaults(t *testing.T) {
	ctx := context.Background()
	t.Run("read-back fails", func(t *testing.T) {
		j := mustJournal(covGetFail{NewMemStore(), runCancelledStep})
		if _, err := writeEnd(ctx, j, "r", runCompleteStep, Record{Kind: StepValue}, []string{runCancelledStep}); !errors.Is(err, ErrStorage) {
			t.Fatalf("writeEnd = %v; want ErrStorage", err)
		}
	})
	t.Run("other marker corrupt", func(t *testing.T) {
		s := NewMemStore()
		j := mustJournal(s)
		corruptEntry(t, s, j, "r", runCancelledStep)
		if _, err := writeEnd(ctx, j, "r", runCompleteStep, Record{Kind: StepValue}, []string{runCancelledStep}); !errors.Is(err, ErrStorage) {
			t.Fatalf("writeEnd = %v; want ErrStorage", err)
		}
		if _, _, err := firstEndOf(ctx, j, "r", runCancelledStep); !errors.Is(err, ErrStorage) {
			t.Fatalf("firstEndOf = %v; want ErrStorage", err)
		}
	})
	t.Run("firstEndOf read fails", func(t *testing.T) {
		j := mustJournal(covGetFail{NewMemStore(), runCompleteStep})
		if _, _, err := firstEndOf(ctx, j, "r", runCompleteStep); !errors.Is(err, ErrStorage) {
			t.Fatalf("firstEndOf = %v; want ErrStorage", err)
		}
	})
	t.Run("cancelledFirst read fails", func(t *testing.T) {
		s := NewMemStore()
		writeRaw(t, mustJournal(s), "r", runCancelledStep, Record{Kind: StepValue})
		j := mustJournal(covGetFail{s, runCompleteStep})
		if _, err := cancelledFirst(ctx, j, "r"); !errors.Is(err, ErrStorage) {
			t.Fatalf("cancelledFirst = %v; want ErrStorage", err)
		}
	})
	t.Run("marker cannot be written", func(t *testing.T) {
		j := mustJournal(covInsertFail{NewMemStore(), runCompleteStep})
		if _, err := writeEnd(ctx, j, "r", runCompleteStep, Record{Kind: StepValue}, nil); !errors.Is(err, ErrStorage) {
			t.Fatalf("writeEnd = %v; want ErrStorage", err)
		}
	})
	t.Run("unencodable marker", func(t *testing.T) {
		if _, _, err := memJournal().putEntry(ctx, "r", runCompleteStep, Record{Kind: StepValue, Result: []byte(`{bad`)}); !errors.Is(err, ErrStorage) {
			t.Fatalf("putEntry = %v; want ErrStorage", err)
		}
	})
}

// A drive that finds its run over reports the end as the run's: the recorded answer of a completed
// run, ErrRunCancelled for a cancelled one (and for none, which only a cancelled read asks about),
// and ErrConfig for a saga's finished abort driven as a plain run.
func TestEndVerdict(t *testing.T) {
	ctx := context.Background()
	mk := func(name string, result string) *Agent {
		j := memJournal()
		if name != "" {
			writeRaw(t, j, "r", name, Record{Kind: StepValue, Result: []byte(result)})
		}
		return mustNew(&scriptModel{}, j)
	}
	if _, err := mk("", "").endVerdict(ctx, "r"); !errors.Is(err, ErrRunCancelled) {
		t.Errorf("no end marker = %v; want ErrRunCancelled", err)
	}
	if _, err := mk(runCancelledStep, `{"reason":"x"}`).endVerdict(ctx, "r"); !errors.Is(err, ErrRunCancelled) {
		t.Errorf("cancelled = %v; want ErrRunCancelled", err)
	}
	if _, err := mk(runAbortedStep, `"boom"`).endVerdict(ctx, "r"); !errors.Is(err, ErrConfig) {
		t.Errorf("aborted = %v; want ErrConfig", err)
	}
	a := mustNew(&scriptModel{}, mustJournal(loadFails{NewMemStore()}))
	if _, err := a.endVerdict(ctx, "r"); !errors.Is(err, ErrStorage) {
		t.Errorf("unreadable history = %v; want ErrStorage", err)
	}
}

// cancelTrip names why a saga was cancelled.
func TestCancelTripError(t *testing.T) {
	if got := (&cancelTrip{reason: "stop it"}).Error(); got != "saga cancelled: stop it" {
		t.Errorf("Error = %q", got)
	}
	if got := (&sagaTrip{toolName: "pay", cause: errors.New("declined")}).Error(); got != `saga step "pay" failed: declined` {
		t.Errorf("sagaTrip.Error = %q", got)
	}
}
