package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// The remembered claims of one marker key are a set: remembering adds, the resume gate takes only
// the live marker's own id, and a claim takes them all.
func TestClaimMemo_IsASetPerKey(t *testing.T) {
	ctx := context.Background()
	j := newJournal(NewMemStore())
	if err := j.ensureHeader(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	key := stepAttemptStep("s")
	k := flightKey{j.id, "r", key}
	pendingClaims.remember(k, "b1")
	pendingClaims.remember(k, "a1")
	pendingClaims.remember(k, "a1") // twice: held once
	marker := Record{Kind: StepAttempt, ToolUseID: "s", claim: "a1"}
	if !j.retryNotStarted(ctx, "r", key, marker) {
		t.Fatal("the resume gate did not record the live marker's remembered claim as not started")
	}
	if j.retryNotStarted(ctx, "r", key, marker) {
		t.Fatal("the live marker's id was still remembered after the resume gate took it")
	}
	if got := pendingClaims.takeAll(k); !slices.Equal(got, []string{"b1"}) {
		t.Fatalf("after the resume gate took a1, the key remembers %v; want [b1] (another claim's id stays)", got)
	}
	if got := pendingClaims.takeAll(k); len(got) != 0 {
		t.Fatalf("takeAll left %v", got)
	}
}

// Past maxPendingClaims ids, whole keys are dropped, oldest first, and the count stays exact.
func TestClaimMemo_DropsWholeKeysPastTheBound(t *testing.T) {
	c := &claimMemo{m: map[flightKey]map[string]struct{}{}}
	first := flightKey{nil, "r", "first"}
	c.remember(first, "x")
	c.remember(first, "y")
	for i := range maxPendingClaims - 1 {
		c.remember(flightKey{nil, "r", string(rune('a'+i%26)) + string(rune(i))}, "id")
	}
	if _, ok := c.m[first]; ok || c.n != maxPendingClaims-1 {
		t.Fatalf("held %d ids, first key kept = %v; want the oldest key dropped whole and %d ids", c.n, ok, maxPendingClaims-1)
	}
	total := 0
	for _, ids := range c.m {
		total += len(ids)
	}
	if total != c.n {
		t.Fatalf("count %d, ids held %d", c.n, total)
	}
}

// failNames fails, not committed, every Insert whose name contains one of its substrings.
type failNames struct {
	Store
	subs []string
}

func (f failNames) Insert(ctx context.Context, runID, name string, data []byte) (Entry, bool, error) {
	for _, s := range f.subs {
		if strings.Contains(name, s) {
			return Entry{}, false, errors.New("injected store fault")
		}
	}
	return f.Store.Insert(ctx, runID, name, data)
}

// A claim writes the not-started record of every id remembered for its key, one at a time: one
// that cannot be written is remembered again and does not stop the others, and the claim then
// goes on with a fresh id of its own.
func TestClaimMemo_ClaimRetriesEveryRememberedID(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	j := newJournal(failNames{mem, []string{"aaaa"}})
	if err := j.ensureHeader(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	key := stepAttemptStep("s")
	k := flightKey{j.id, "r", key}
	pendingClaims.remember(k, "aaaa")
	pendingClaims.remember(k, "bbbb")
	won, _, err := j.claim(ctx, "r", key, Record{Kind: StepAttempt, ToolUseID: "s", AttemptedAt: 1})
	if err != nil || !won {
		t.Fatalf("claim = %v, %v; want a won claim under a fresh id (no remembered marker was written)", won, err)
	}
	if rec, ok, err := j.Get(ctx, "r", notStartedStep(key, "bbbb")); err != nil || !ok || rec.Kind != StepNotStarted {
		t.Fatalf("bbbb's not-started record = %v, %v; want it written though aaaa's failed", ok, err)
	}
	if got := pendingClaims.takeAll(k); !slices.Equal(got, []string{"aaaa"}) {
		t.Fatalf("remembered after the claim: %v; want [aaaa], whose record could not be written", got)
	}
}

func TestNextAttemptStep(t *testing.T) {
	for _, base := range []string{toolAttemptStep("c:1"), stepAttemptStep("charge")} {
		for gen := range 12 {
			if got, want := nextAttemptStep(retryAttemptStep(base, gen)), retryAttemptStep(base, gen+1); got != want {
				t.Errorf("nextAttemptStep(%q) = %q, want %q", retryAttemptStep(base, gen), got, want)
			}
		}
	}
}
