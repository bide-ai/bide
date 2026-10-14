package audit_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// autoAnchored builds a trail the way a notifier does with an AuditedEventStore beneath it, and
// returns it with the heads the anchor log received, in arrival order.
func autoAnchored(t *testing.T) (*trailBuilder, []audit.SignedTreeHead, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	mem := audit.NewMemEventStore()
	log := audit.NewMemAnchorLog()
	s, err := audit.NewAuditedEventStore(mem, audit.Ed25519Signer{Priv: priv}, log)
	if err != nil {
		t.Fatal(err)
	}
	s.WithClock(func() int64 { return at(30).UnixNano() })
	b := &trailBuilder{t: t, es: mem, tr: audit.NewNotifyTrail(s)}
	b.complete(agent.ApprovalKey("r1", "c1", 0), agent.NotificationApprovalPending, 0)
	b.complete(agent.ApprovalKey("r1", "c2", 0), agent.NotificationApprovalPending, 5)
	var heads []audit.SignedTreeHead
	for _, e := range log.Entries() {
		heads = append(heads, e.STH)
	}
	if len(heads) != 6 {
		t.Fatalf("%d heads for 6 records", len(heads))
	}
	return b, heads, pub, priv
}

func verifyWith(b *trailBuilder, x audit.NotifyTrailExport, heads []audit.SignedTreeHead, pub ed25519.PublicKey, mod func(*audit.NotifyVerifyInput)) (audit.NotifyReport, error) {
	in := audit.NotifyVerifyInput{Trails: []audit.NotifyTrailExport{x}, Heads: heads, Verifier: audit.Ed25519Verifier{Pub: pub}, Now: at(60)}
	if mod != nil {
		mod(&in)
	}
	return audit.VerifyNotifications(in)
}

// The heads an anchor log holds for a trail, one per size, verify it without -unanchored: every one
// is checked, the newest is the attestation, and nothing is left unattested.
func TestVerifyNotifications_AnAutoAnchoredTrailVerifiesAgainstEveryHead(t *testing.T) {
	b, heads, pub, _ := autoAnchored(t)
	rep, err := verifyWith(b, b.export(), heads, pub, nil)
	tr := rep.Trails[0]
	if err != nil || !tr.Anchored || tr.Heads != 6 || tr.HeadSize != 6 || tr.Tail != 0 || !tr.TailSince.IsZero() || tr.TailAge != 0 {
		t.Fatalf("%v %+v", err, tr)
	}
	// Order and repetition of the heads do not matter.
	shuffled := append(slices.Clone(heads), heads[1], heads[1])
	slices.Reverse(shuffled)
	if rep, err = verifyWith(b, b.export(), shuffled, pub, nil); err != nil || rep.Trails[0].HeadSize != 6 {
		t.Fatalf("shuffled heads: %v %+v", err, rep.Trails[0])
	}
}

// A record rewritten, reordered or deleted after it was anchored contradicts the heads that covered
// it, however the heads are chosen, and a record deleted from the end is caught by the newest.
func TestVerifyNotifications_AutoAnchoredHeadsCatchTampering(t *testing.T) {
	b, heads, pub, _ := autoAnchored(t)
	good := b.export()
	cp := func() audit.NotifyTrailExport {
		x := good
		x.Leaves = slices.Clone(good.Leaves)
		return x
	}
	cases := map[string]func(*audit.NotifyTrailExport){
		"deleted a record":     func(x *audit.NotifyTrailExport) { x.Leaves = slices.Delete(x.Leaves, 2, 3) },
		"deleted the last":     func(x *audit.NotifyTrailExport) { x.Leaves = x.Leaves[:5] },
		"truncated to nothing": func(x *audit.NotifyTrailExport) { x.Leaves = nil },
		"reordered":            func(x *audit.NotifyTrailExport) { x.Leaves[0], x.Leaves[3] = x.Leaves[3], x.Leaves[0] },
		"changed a record": func(x *audit.NotifyTrailExport) {
			x.Leaves[2] = bytes.Replace(x.Leaves[2], []byte("200"), []byte("201"), 1)
		},
	}
	for name, mod := range cases {
		x := cp()
		mod(&x)
		if _, err := verifyWith(b, x, heads, pub, nil); err == nil {
			t.Errorf("%s: a tampered trail verified against the full set of heads", name)
		}
		// Only the head of size 3 is kept: a change to its first three records still shows.
		if name == "changed a record" || name == "reordered" {
			if _, err := verifyWith(b, x, heads[2:3], pub, nil); err == nil {
				t.Errorf("%s: not caught by the early head alone", name)
			}
		}
	}

	// A head signed by another key is a violation, not skipped in favour of the genuine ones.
	_, other, _ := ed25519.GenerateKey(nil)
	forged, err := audit.SignNotifyTrail(context.Background(), b.es, trailName, audit.Ed25519Signer{Priv: other}, at(30))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := verifyWith(b, good, append(slices.Clone(heads), forged), pub, nil)
	if err == nil || rep.Trails[0].Anchored || !strings.Contains(strings.Join(rep.Trails[0].Violations, "\n"), "does not verify") {
		t.Fatalf("a forged head among genuine ones: %v %+v", err, rep.Trails[0])
	}
}

// Records after the newest head are read and reported with how many there are and how long the
// oldest has gone unattested; MaxTailAge makes a long gap a failure.
func TestVerifyNotifications_ReportsTheUnattestedGapAndItsAge(t *testing.T) {
	b, heads, pub, _ := autoAnchored(t)
	x := b.export()
	rep, err := verifyWith(b, x, heads[:3], pub, nil) // the anchor stopped after the third record
	tr := rep.Trails[0]
	if err != nil || !tr.Anchored || tr.HeadSize != 3 || tr.Tail != 3 {
		t.Fatalf("%v %+v", err, tr)
	}
	// Record 3 is the first unattested one; it is dated at(1+5) = 6s after t0 (see complete).
	if !tr.TailSince.Equal(at(6)) || tr.TailAge != 54*time.Second {
		t.Fatalf("tail since %v age %v, want %v and 54s", tr.TailSince, tr.TailAge, at(6))
	}
	js, _ := json.Marshal(tr)
	if !strings.Contains(string(js), `"tail":3`) || !strings.Contains(string(js), `"tail_age_ns":54000000000`) || !strings.Contains(string(js), `"tail_since"`) {
		t.Fatalf("the gap is not in the JSON report: %s", js)
	}

	if _, err := verifyWith(b, x, heads[:3], pub, func(in *audit.NotifyVerifyInput) { in.MaxTailAge = 2 * time.Minute }); err != nil {
		t.Fatalf("a gap within MaxTailAge failed: %v", err)
	}
	rep, err = verifyWith(b, x, heads[:3], pub, func(in *audit.NotifyVerifyInput) { in.MaxTailAge = 30 * time.Second })
	if err == nil || !strings.Contains(strings.Join(rep.Problems, "\n"), "unattested for 54s") {
		t.Fatalf("a gap past MaxTailAge: %v %v", err, rep.Problems)
	}
	// A trail attested to its end has no gap to bound.
	if _, err := verifyWith(b, x, heads, pub, func(in *audit.NotifyVerifyInput) { in.MaxTailAge = time.Nanosecond }); err != nil {
		t.Fatalf("a fully attested trail failed MaxTailAge: %v", err)
	}
}
