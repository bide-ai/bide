package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/capledger"
)

// The loop journals why it releases a call's slot (cap-release:<attempt>, in the run that made the call)
// before it releases it, so an audit can tell a release the loop wrote from one appended to the store.

// releaseNotes returns, for every release slot of the day ledger, the cap-release note of its attempt (nil
// when there is none), keyed by slot number.
func releaseNotes(t *testing.T, j *Journal) map[int]*capReleaseNote {
	t.Helper()
	l := newCapLedger(j, func() time.Time { return capNow }, 0)
	run := l.LedgerRun(capledger.Rule{Namespace: defaultCapNamespace, Name: "refund_total", Currency: "USD", Kind: capledger.Day}, "2026-10-22", capledger.KeyDigest(""))
	slots, err := l.Slots(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]*capReleaseNote{}
	for _, s := range slots {
		if s.Kind != "release" {
			continue
		}
		rec, ok, err := j.Get(context.Background(), s.Run, capReleaseStep(s.Attempt))
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			out[s.N] = nil
			continue
		}
		var n capReleaseNote
		if err := json.Unmarshal(rec.Result, &n); err != nil {
			t.Fatal(err)
		}
		out[s.N] = &n
	}
	return out
}

func TestCapsReleaseNote_ARefusalByALaterRuleLeavesItsReason(t *testing.T) {
	j := memJournal()
	tool := sumTool(Safety{}, capRuleDay, capRuleHour)
	if _, err := runSum(t, j, "r0", sumArgs(60, ""), tool); err != nil {
		t.Fatal(err)
	}
	if _, err := runSum(t, j, "r1", sumArgs(80, ""), tool); err != nil {
		t.Fatal(err)
	}
	notes := releaseNotes(t, j)
	if len(notes) != 1 {
		t.Fatalf("releases = %d, want the one the refusal made", len(notes))
	}
	for n, note := range notes {
		if note == nil || note.Why != capReleaseRefused || note.Format != capReleaseFormat || !strings.HasPrefix(note.Attempt, "r1/") {
			t.Fatalf("slot %d's note = %+v, want the cap refusal of r1's attempt", n, note)
		}
	}
}

// A note that cannot be written releases nothing: the slot stays counted, as when a release fails, and the
// void lets the reconcile pass release it.
func TestCapsReleaseNote_AFailedNoteReleasesNothing(t *testing.T) {
	os := &orderStore{Store: NewMemStore()}
	j := newJournal(os)
	os.failOn = func(name string, _ []byte) bool { return strings.HasPrefix(name, "cap-release:") }
	tool := sumTool(Safety{}, capRuleDay, capRuleHour)
	failures := capReleaseFailures.Load()
	if _, err := runSum(t, j, "r0", sumArgs(60, ""), tool); err != nil {
		t.Fatal(err)
	}
	if _, err := runSum(t, j, "r1", sumArgs(80, ""), tool); err != nil {
		t.Fatal(err)
	}
	if got := len(releaseNotes(t, j)); got != 0 {
		t.Fatalf("%d release(s) were written without their note", got)
	}
	if live := liveDay(t, j); len(live) != 2 {
		t.Fatalf("live = %+v, want both reservations still counted", live)
	}
	if capReleaseFailures.Load() != failures+1 {
		t.Fatalf("release failures = %d, want one more than %d", capReleaseFailures.Load(), failures)
	}
}

func TestCapReleaseWhy(t *testing.T) {
	if got := capReleaseWhy(&errCapRefused{rule: "r"}); got != capReleaseRefused {
		t.Fatalf("a cap refusal: %q", got)
	}
	if got := capReleaseWhy(fmt.Errorf("wrapped: %w", &errCapRefused{rule: "r"})); got != capReleaseRefused {
		t.Fatalf("a wrapped cap refusal: %q", got)
	}
	if got := capReleaseWhy(errors.New("middleware")); got != capReleaseNoCall {
		t.Fatalf("another error: %q", got)
	}
	if got := capReleaseWhy(nil); got != capReleaseNoCall {
		t.Fatalf("no error: %q", got)
	}
}

// Only the engine writes the name: a caller's Step and ClaimAttempt refuse it.
func TestCapsReleaseNote_CallersCannotWriteTheName(t *testing.T) {
	j := memJournal()
	if _, err := j.Step[string](context.Background(), "r", "cap-release:x", func(context.Context) (string, error) { return "x", nil }); !errors.Is(err, ErrConfig) {
		t.Fatalf("Step: %v, want ErrConfig", err)
	}
	if _, _, err := ClaimAttempt(context.Background(), j, "r", "cap-release:x", Record{Kind: StepAttempt, ToolUseID: "x"}); !errors.Is(err, ErrConfig) {
		t.Fatalf("ClaimAttempt: %v, want ErrConfig", err)
	}
}
