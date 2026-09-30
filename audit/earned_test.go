package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/bide-ai/bide/agent"
)

func mustSign(t *testing.T, g Grant, s Signer) SignedGrant {
	t.Helper()
	sg, err := SignGrant(g, s)
	if err != nil {
		t.Fatalf("SignGrant %s: %v", g.ID, err)
	}
	return sg
}

// TestEarnedAuthority_Ladder walks the control loop: baseline, promotion on a clean streak up to
// the ceiling, cap at the top, reset on anomaly, and confirms every earned grant is a child of the
// root that verifies within the ceiling.
func TestEarnedAuthority_Ladder(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	verifier := func(string) (Verifier, bool) { return Ed25519Verifier{Pub: pub}, true }

	ctx := context.Background()
	root := mustSign(t, Grant{ID: "root", Issuer: "corp", Subject: "desk", NotAfterUnix: 5000, Scope: map[string]string{"limit": "10", "desk": "EQ-US"}}, signer)

	assertWithinCeiling := func(ea *EarnedAuthority) {
		t.Helper()
		if ea.Grant().Grant.ParentRef != root.Grant.Digest() {
			t.Fatalf("earned grant is not a child of root")
		}
		if err := VerifyDelegationChain([]SignedGrant{root, ea.Grant()}, verifier, EarnedRules); err != nil {
			t.Fatalf("earned grant at limit %d did not verify within the ceiling: %v", ea.Limit(), err)
		}
		if g := ea.Grant().Grant; g.NotAfterUnix != root.Grant.NotAfterUnix || g.Scope["desk"] != "EQ-US" {
			t.Fatalf("earned grant does not carry the root's expiry and constraints: %+v", g)
		}
	}

	ea, err := NewEarnedAuthority(ctx, []int{2, 5, 10}, 3, root, signer, "exec", agent.NewMemStore(), "ledger")
	if err != nil {
		t.Fatalf("NewEarnedAuthority: %v", err)
	}
	if ea.Tier() != 0 || ea.Limit() != 2 {
		t.Fatalf("baseline should be tier 0 limit 2, got tier %d limit %d", ea.Tier(), ea.Limit())
	}
	assertWithinCeiling(ea)

	// Two compliant actions: no promotion yet.
	for i := 0; i < 2; i++ {
		if p, _ := ea.RecordCompliant(ctx); p {
			t.Fatalf("promoted too early at action %d", i)
		}
	}
	// Third crosses the threshold: promote to limit 5.
	if p, _ := ea.RecordCompliant(ctx); !p || ea.Limit() != 5 {
		t.Fatalf("expected promotion to limit 5, got promoted=%v limit=%d", p, ea.Limit())
	}
	assertWithinCeiling(ea)

	// Three more: promote to the ceiling rung, limit 10.
	ea.RecordCompliant(ctx)
	ea.RecordCompliant(ctx)
	if p, _ := ea.RecordCompliant(ctx); !p || ea.Limit() != 10 {
		t.Fatalf("expected promotion to the ceiling limit 10, got promoted=%v limit=%d", p, ea.Limit())
	}
	assertWithinCeiling(ea)

	// Further clean actions cannot exceed the top rung.
	for i := 0; i < 6; i++ {
		if p, _ := ea.RecordCompliant(ctx); p {
			t.Fatalf("promoted past the ceiling")
		}
	}
	if ea.Limit() != 10 {
		t.Fatalf("limit should stay at the ceiling 10, got %d", ea.Limit())
	}

	// An anomaly resets to baseline immediately.
	if c, _ := ea.FlagAnomaly(ctx); !c || ea.Tier() != 0 || ea.Limit() != 2 {
		t.Fatalf("anomaly should reset to baseline, got changed=%v tier=%d limit=%d", c, ea.Tier(), ea.Limit())
	}
	assertWithinCeiling(ea)
}

// TestEarnedAuthority_CeilingEnforced confirms the controller refuses a ladder that would let an
// earned grant exceed the root grant.
func TestEarnedAuthority_CeilingEnforced(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	root := mustSign(t, Grant{ID: "root", Subject: "desk", Scope: map[string]string{"limit": "10"}}, signer)

	if _, err := NewEarnedAuthority(context.Background(), []int{2, 20}, 3, root, signer, "exec", agent.NewMemStore(), "ledger"); err == nil {
		t.Fatal("expected an error: a ladder rung (20) exceeds the root ceiling (10)")
	}
}

// earnedFixture is a controller over ledger run "ledger" that has issued a baseline grant, promoted
// it (high), and demoted it again, with the log key that signs ledger heads.
type earnedFixture struct {
	store          agent.Durable
	logPub         ed25519.PublicKey
	logPriv        ed25519.PrivateKey
	high, demoted  SignedGrant
	head2, head3   SignedTreeHead // the ledger heads while high was current, and after the demotion
	highInAgentRun SignedTreeHead // a head of run "agent-run", whose only leaf is high
}

func newEarnedFixture(t *testing.T) earnedFixture {
	t.Helper()
	ctx := context.Background()
	var f earnedFixture
	f.logPub, f.logPriv, _ = ed25519.GenerateKey(rand.Reader)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer := Ed25519Signer{Priv: priv}
	root := mustSign(t, Grant{ID: "root", Issuer: "corp", Subject: "desk", Scope: map[string]string{"limit": "10"}}, signer)
	f.store = agent.NewMemStore()
	ea, err := NewEarnedAuthority(ctx, []int{2, 5, 10}, 1, root, signer, "exec", f.store, "ledger")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ea.RecordCompliant(ctx); err != nil {
		t.Fatal(err)
	}
	f.high = ea.Grant()
	// The agent run records the grant it acts under, so its last leaf is high too.
	if _, err := RecordGrant(ctx, f.store, "agent-run", f.high); err != nil {
		t.Fatal(err)
	}
	f.highInAgentRun = f.sign(t, "agent-run")
	f.head2 = f.sign(t, "ledger")
	if _, err := ea.FlagAnomaly(ctx); err != nil {
		t.Fatal(err)
	}
	f.demoted = ea.Grant()
	f.head3 = f.sign(t, "ledger")
	return f
}

func (f earnedFixture) sign(t *testing.T, run string) SignedTreeHead {
	t.Helper()
	th, err := NewTreeHead(context.Background(), f.store, run, 1)
	if err != nil {
		t.Fatal(err)
	}
	return signTH(t, th, f.logPriv)
}

// A superseded grant that is the last leaf of some OTHER run signed by the same log key is not
// the current grant of the ledger.
func TestEarnedAuthority_ProofOverOtherRunIsNotCurrent(t *testing.T) {
	ctx := context.Background()
	f := newEarnedFixture(t)
	pb, err := ProveCurrentGrant(ctx, f.store, "agent-run", f.highInAgentRun, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyCurrentGrant(f.high, "ledger", pb, nil, edV(f.logPub)); err == nil {
		t.Fatalf("superseded grant accepted as current via a proof over run %q (err=%v)", pb.Leaf.RunID, err)
	}
	cur, err := ProveCurrentGrant(ctx, f.store, "ledger", f.head3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyCurrentGrant(f.demoted, "ledger", cur, nil, edV(f.logPub)); err != nil {
		t.Fatalf("the current grant did not verify against the ledger: %v", err)
	}
}

// A verifier that has seen a ledger head cannot be shown an older head as current, and accepts a
// newer head only when it extends the one it saw.
func TestEarnedAuthority_StaleHeadIsNotCurrent(t *testing.T) {
	ctx := context.Background()
	f := newEarnedFixture(t)
	stale, err := ProveCurrentGrant(ctx, f.store, "ledger", f.head2, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Without a last-seen head the stale proof shows only that high was current then.
	if err := VerifyCurrentGrant(f.high, "ledger", stale, nil, edV(f.logPub)); err != nil {
		t.Fatalf("the grant current at head 2 did not verify there: %v", err)
	}
	for _, bad := range []int{f.head3.Size, -1} {
		if _, err := ProveCurrentGrant(ctx, f.store, "ledger", f.head2, bad); err == nil {
			t.Fatalf("ProveCurrentGrant proved consistency from last-seen size %d to size %d", bad, f.head2.Size)
		}
	}
	// A verifier that saw head 3 rejects head 2, whatever consistency proof comes with it.
	for _, c := range []Consistency{
		stale.Consistency,
		{First: f.head3.Size, Size: f.head2.Size},
		{First: f.head3.Size, Size: f.head3.Size},
	} {
		p := stale
		p.Consistency = c
		if err := VerifyCurrentGrant(f.high, "ledger", p, &f.head3, edV(f.logPub)); err == nil {
			t.Fatalf("a head older than the last-seen head verified as current (consistency %+v)", c)
		}
	}
	// A verifier that saw head 2 accepts head 3, which extends it, and the demoted grant is current.
	cur, err := ProveCurrentGrant(ctx, f.store, "ledger", f.head3, f.head2.Size)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyCurrentGrant(f.demoted, "ledger", cur, &f.head2, edV(f.logPub)); err != nil {
		t.Fatalf("a head extending the last-seen head did not verify: %v", err)
	}
	if err := VerifyCurrentGrant(f.demoted, "ledger", cur, &f.head3, edV(f.logPub)); err == nil {
		t.Fatal("a consistency proof from the wrong last-seen size verified")
	}
	// The last-seen head must itself be an authentic journal head of the ledger run.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	forged := signTH(t, f.head2.TreeHead, otherPriv)
	events := f.head2.TreeHead
	events.Kind = TreeEvents
	eventsHead := signTH(t, events, f.logPriv)
	// A run holding the same records as the ledger has the same roots under another run id.
	recs, _ := f.store.History(ctx, "ledger")
	for _, r := range recs[:f.head2.Size] {
		if _, err := f.store.Do(ctx, "copy", r.Name, func(context.Context) (agent.Record, error) { return r, nil }); err != nil {
			t.Fatal(err)
		}
	}
	copyHead := f.sign(t, "copy")
	for name, seen := range map[string]SignedTreeHead{"wrong key": forged, "events kind": eventsHead, "other run": copyHead} {
		if err := VerifyCurrentGrant(f.demoted, "ledger", cur, &seen, edV(f.logPub)); err == nil {
			t.Fatalf("last-seen head (%s) accepted", name)
		}
	}
}
