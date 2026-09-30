package agent

import (
	"context"
	"slices"
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
