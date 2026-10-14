package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var covHalt = HaltRef{RunID: "r", Op: OpRef{Kind: OpTool, ID: "c1"}, Cause: HaltCrashed}

// toolResultRecorded reports whether run r holds a result for call c1.
func toolResultRecorded(t *testing.T, j *Journal) bool {
	t.Helper()
	_, ok, err := j.Get(context.Background(), "r", ToolResultStep("c1"))
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// A resolution whose outcome cannot be encoded, or whose evidence is given twice or cannot be
// encoded, is ErrConfig and records nothing: the halt stands, so a corrected call can resolve it.
func TestResolveHalt_RefusesUnrecordableOutcomes(t *testing.T) {
	ctx := context.Background()
	s := markerAt(t, time.Now().UnixMilli())
	j := mustJournal(s)
	for name, c := range map[string]struct {
		out  Outcome
		opts []ResolveOption
	}{
		"result not encodable":              {Outcome{Result: make(chan int)}, nil},
		"evidence in outcome not encodable": {Outcome{Result: "ok", Evidence: make(chan int)}, nil},
		"evidence option not encodable":     {Outcome{Result: "ok"}, []ResolveOption{WithEvidence(make(chan int))}},
		"evidence given twice":              {Outcome{Result: "ok", Evidence: "a"}, []ResolveOption{WithEvidence("b")}},
	} {
		err := ResolveHalt(ctx, j, covHalt, c.out, append(c.opts, WithoutLiveDriverCheck())...)
		if !errors.Is(err, ErrConfig) {
			t.Errorf("%s: ResolveHalt = %v; want ErrConfig", name, err)
		}
		if toolResultRecorded(t, j) {
			t.Fatalf("%s: a refused resolution recorded a result", name)
		}
	}
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "charged"}, WithoutLiveDriverCheck()); err != nil {
		t.Fatalf("the corrected resolution = %v", err)
	}
}

// covLeaseErr is a store whose leases cannot be taken.
type covLeaseErr struct{ *MemStore }

func (covLeaseErr) AcquireLease(context.Context, string, string, time.Duration) (bool, error) {
	return false, errCovBlip
}

// A store that cannot say who holds the run's lease, or cannot list its history, is a storage
// error: the resolution does not guess that no driver is live.
func TestResolveHalt_StoreFaults(t *testing.T) {
	ctx := context.Background()
	if err := ResolveHalt(ctx, mustJournal(covLeaseErr{markerAt(t, time.Now().UnixMilli())}), covHalt, Outcome{Result: "x"}); !errors.Is(err, ErrStorage) {
		t.Errorf("lease fault = %v; want ErrStorage", err)
	}
	if err := ResolveHalt(ctx, mustJournal(loadFails{markerAt(t, time.Now().UnixMilli())}), covHalt, Outcome{Result: "x"}, WithoutLiveDriverCheck()); !errors.Is(err, ErrStorage) {
		t.Errorf("history fault = %v; want ErrStorage", err)
	}
	// The claim on the next attempt cannot be written: the resolution reports it and records nothing.
	next := nextAttemptStep(toolAttemptStep("c1"))
	j := mustJournal(covInsertFail{markerAt(t, time.Now().UnixMilli()), next})
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "x"}); err == nil || errors.Is(err, ErrConfig) {
		t.Errorf("claim fault = %v; want a store error", err)
	}
	if toolResultRecorded(t, j) {
		t.Error("a resolution recorded a result though it could not claim the next attempt")
	}
}

// A driver that holds the run's lease, or the claim on the next attempt, may be running the effect:
// the resolution is refused with *HaltInFlight saying which, and records nothing.
func TestResolveHalt_InFlightDriver(t *testing.T) {
	ctx := context.Background()
	t.Run("lease held", func(t *testing.T) {
		s := markerAt(t, time.Now().UnixMilli())
		if won, err := s.AcquireLease(ctx, "r", "driver", time.Minute); err != nil || !won {
			t.Fatal(won, err)
		}
		j := mustJournal(s)
		err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "x"})
		var hf *HaltInFlight
		if !errors.As(err, &hf) || hf.Attempt != "" || hf.RootRunID != "r" {
			t.Fatalf("ResolveHalt = %v; want *HaltInFlight on the lease", err)
		}
		if !strings.Contains(hf.Error(), "lease") || toolResultRecorded(t, j) {
			t.Errorf("message %q; result recorded: %v", hf.Error(), toolResultRecorded(t, j))
		}
	})
	t.Run("next attempt claimed", func(t *testing.T) {
		// Another driver claims the next attempt between the resolution's read and its own claim.
		j := mustJournal(covRacer{markerAt(t, time.Now().UnixMilli()), nextAttemptStep(toolAttemptStep("c1"))})
		err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "x"})
		var hf *HaltInFlight
		if !errors.As(err, &hf) || hf.Attempt == "" {
			t.Fatalf("ResolveHalt = %v; want *HaltInFlight on the claim", err)
		}
		if !strings.Contains(hf.Error(), "claim") || toolResultRecorded(t, j) {
			t.Errorf("message %q; result recorded: %v", hf.Error(), toolResultRecorded(t, j))
		}
	})
}

// covRacer stores another driver's attempt marker under name just before an Insert of name.
type covRacer struct {
	*MemStore
	name string
}

func (s covRacer) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	if name == s.name {
		other, err := JournalEntry(name, Record{Kind: StepAttempt, ToolUseID: "c1", AttemptedAt: time.Now().UnixMilli(), claim: "other"})
		if err != nil {
			return Entry{}, false, err
		}
		if _, _, err := s.MemStore.Insert(ctx, runID, name, other); err != nil {
			return Entry{}, false, err
		}
	}
	return s.MemStore.Insert(ctx, runID, name, data)
}

// A resolution of an operation that already has another outcome reports the one that stands
// (ErrAlreadyResolved), and one made before the halt is old enough reports its age.
func TestResolveHalt_ErrorsDescribeWhatStands(t *testing.T) {
	ctx := context.Background()
	j := mustJournal(markerAt(t, time.Now().UnixMilli()))
	if err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "charged"}, WithoutLiveDriverCheck()); err != nil {
		t.Fatal(err)
	}
	err := ResolveHalt(ctx, j, covHalt, Outcome{Result: "refunded"}, WithoutLiveDriverCheck())
	var ar *HaltAlreadyResolved
	if !errors.As(err, &ar) || !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second resolution = %v; want *HaltAlreadyResolved", err)
	}
	if !strings.Contains(ar.Error(), `"charged"`) || !strings.Contains(ar.Error(), "c1") {
		t.Errorf("message %q does not name the standing outcome and the call", ar.Error())
	}

	young := mustJournal(markerAt(t, time.Now().UnixMilli()))
	err = ResolveHalt(ctx, young, covHalt, Outcome{Result: "x"}, WithoutLiveDriverCheck(), WithMinHaltAge(time.Hour))
	var ty *HaltTooYoung
	if !errors.As(err, &ty) || ty.Min != time.Hour || ty.Age < 0 || ty.Age >= time.Hour {
		t.Fatalf("young resolution = %v; want *HaltTooYoung", err)
	}
	if !strings.Contains(ty.Error(), "c1") || !strings.Contains(ty.Error(), "1h0m0s") {
		t.Errorf("message %q does not name the call and the minimum", ty.Error())
	}
}

// The approval pause names its run, call and, for a re-approval, why.
func TestApprovalPending_ErrorNamesTheCall(t *testing.T) {
	p := &ApprovalPending{RunRef: RunRef{RunID: "r9", RootRunID: "r9"}, ToolUseID: "c4", ToolName: "pay"}
	for _, w := range []string{"r9", "c4", `"pay"`, "SubmitDecision"} {
		if !strings.Contains(p.Error(), w) {
			t.Errorf("%q lacks %q", p.Error(), w)
		}
	}
	p.Reason = PendingReapproval
	if !strings.Contains(p.Error(), string(PendingReapproval)) || strings.Contains(p.Error(), "SubmitDecision") {
		t.Errorf("re-approval message %q", p.Error())
	}
}
