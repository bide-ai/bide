package audit

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
)

// anchorTrail hands its caller the answer of the goroutine that published the head. The trail's
// slot must be free by then: a caller that has its answer may publish the next head at once, and
// must not find the slot still held by a goroutine that only has its deferred release left to run
// (it would wait for it, or, with a short timeout, give up on a head it could have published).
func TestAnchorTrail_TheSlotIsFreeWhenTheAnswerIsReturned(t *testing.T) {
	old := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(old)
	_, priv, _ := ed25519.GenerateKey(nil)
	inner := NewMemEventStore()
	a, err := NewAuditedEventStore(inner, Ed25519Signer{Priv: priv}, NewMemAnchorLog())
	if err != nil {
		t.Fatal(err)
	}
	trail := NewNotifyTrail(inner) // appends to the inner store: nothing publishes behind this test's back
	ctx := context.Background()
	at := time.Now()
	for i := range 20000 {
		rec := agent.NotifyRecord{Kind: agent.NotifyClaimed, Key: fmt.Sprintf("k%d", i), RunID: "r", At: at, Holder: "p"}
		if err := trail.Append(ctx, "notify:r", i, rec); err != nil {
			t.Fatal(err)
		}
		if err := a.anchorTrail(ctx, "notify:r", true); err != nil {
			t.Fatalf("head %d: %v", i, err)
		}
		if n := len(a.trailOf("notify:r").slot); n != 0 {
			t.Fatalf("head %d: the trail's slot is still held when anchorTrail returned", i)
		}
	}
}
