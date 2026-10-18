package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/bide-ai/bide/audit"
)

// The trail key is a hybrid (Ed25519 + ML-DSA); the "second person" signs with plain Ed25519 under the
// hybrid's own Ed25519 key. It is the same secret under another encoding.
func TestReview2_AuthorizerIsTheTrailKeysEd25519Half(t *testing.T) {
	signers := p11Signers(t)
	hy := signers["hybrid"].(audit.HybridSigner)
	tr := newTrailRig(t)
	tr.signer, tr.v = hy, p11Verifier(t, hy) // the trail is signed with the hybrid key
	tr.options.Signer, tr.options.Verifier = hy, tr.v
	r := &authRig{trailRig: tr, holdID: "h1"}
	r.authz, r.authV = hy.Ed, p11Verifier(t, hy.Ed)
	if err := r.placeHold("h1", "gov"); err != nil {
		t.Fatal(err)
	}
	half := hy.Ed
	a, err := audit.NewLiftAuthorization(context.Background(), r.trail, r.v, half, "test", "h1", "closed", "L-9", "", time.UnixMilli(authAt), time.Hour)
	if err == nil {
		if lerr := r.lift(audit.WithLiftAuthorization(a), audit.RequireLiftAuthorization()); lerr == nil {
			t.Fatalf("one key holder lifted the hold alone: the authorizer is the trail key's Ed25519 half")
		}
		return
	}
	t.Logf("refused at authorization: %v", err)
}

// The authorization's expiry is judged against the lift's now, which the caller supplies and the record
// carries (the CLI passes time.Now). A caller that backdates it uses an expired authorization.
func TestReview2_ExpiredAuthorizationAcceptedWithABackdatedNow(t *testing.T) {
	r := newAuthRig(t)
	a := r.authorize(time.Minute) // valid until authAt + 1 minute
	late := audit.LiftLegalHold(context.Background(), r.trail, r.store, r.signer, "test", nil, r.holdID, "matter closed", "LEGAL-9", "counsel", time.UnixMilli(authAt).Add(time.Hour), audit.WithLiftAuthorization(a), audit.RequireLiftAuthorization())
	if late == nil {
		t.Fatal("(control) an hour after the window the authorization should be refused")
	}
	if err := audit.LiftLegalHold(context.Background(), r.trail, r.store, r.signer, "test", nil, r.holdID, "matter closed", "LEGAL-9", "counsel", time.UnixMilli(authAt+1000), audit.WithLiftAuthorization(a), audit.RequireLiftAuthorization()); err == nil {
		t.Log("a lift stamped inside the window passes whatever the real time is (the trail does not bound the lift's at_ms)")
	}
}
