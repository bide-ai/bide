package audit_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

const (
	authAt   = 1_700_000_015_000 // after the placement (ms)
	liftAtMs = 1_700_000_020_000
)

// authRig is a trail rig with a hold in force and a second key, the authorizer's.
type authRig struct {
	*trailRig
	authz  audit.Signer
	authV  audit.Verifier
	holdID string
}

func newAuthRig(t *testing.T) *authRig {
	t.Helper()
	r := &authRig{trailRig: newTrailRig(t), holdID: "h1"}
	r.authz = p11Signers(t)["ecdsa-p256"]
	r.authV = p11Verifier(t, r.authz)
	if err := r.placeHold(r.holdID, "gov"); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *authRig) authorize(validFor time.Duration) audit.LiftAuthorization {
	r.t.Helper()
	a, err := audit.NewLiftAuthorization(context.Background(), r.trail, r.v, r.authz, "test", r.holdID, "matter closed", "LEGAL-9", "", time.UnixMilli(authAt), validFor)
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

func (r *authRig) lift(opts ...audit.LiftOption) error {
	return audit.LiftLegalHold(context.Background(), r.trail, r.store, r.signer, "test", nil, r.holdID, "matter closed", "LEGAL-9", "counsel", time.UnixMilli(liftAtMs), opts...)
}

func (r *authRig) inForce() bool {
	hs, err := r.store.Holds(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return len(hs) == 1
}

func (r *authRig) verify(require bool, pin ...audit.Verifier) (audit.RedactionReport, error) {
	r.t.Helper()
	leaves, head := r.signedTrail()
	return audit.VerifyRedactions(audit.RedactionVerifyInput{Deployment: "test", Trail: leaves, Heads: []audit.SignedTreeHead{head}, Verifier: r.v,
		Journals: r.journals(), LiftAuthorizers: pin, RequireLiftAuthorization: require})
}

func TestLiftAuthorization_AuthorizedLiftVerifiesOffline(t *testing.T) {
	r := newAuthRig(t)
	if err := r.lift(audit.WithLiftAuthorization(r.authorize(time.Hour)), audit.WithLiftAuthorizers(r.authV), audit.RequireLiftAuthorization()); err != nil {
		t.Fatal(err)
	}
	if r.inForce() {
		t.Fatal("the hold is still in force after an authorized lift")
	}
	rep, err := r.verify(true, r.authV)
	if err != nil || !rep.OK() {
		t.Fatalf("VerifyRedactions = %+v, %v; want OK", rep, err)
	}
	recs, _ := audit.LoadRedactionTrail(context.Background(), r.trail)
	if last := recs[len(recs)-1]; last.Authorization == nil || last.Authorization.KeyID != audit.KeyID(string(r.authz.Alg()), r.authz.PublicKey()) {
		t.Fatalf("the lift record does not embed the authorization: %+v", last)
	}
}

func TestLiftAuthorization_RequiredAndMissingIsRefused(t *testing.T) {
	r := newAuthRig(t)
	before, _ := r.trail.Load(context.Background(), audit.RedactionTrailName)
	if err := r.lift(audit.RequireLiftAuthorization()); !errors.Is(err, audit.ErrLiftUnauthorized) {
		t.Fatalf("lift = %v; want ErrLiftUnauthorized", err)
	}
	after, _ := r.trail.Load(context.Background(), audit.RedactionTrailName)
	if !r.inForce() || len(after) != len(before) {
		t.Fatal("a refused lift changed the hold or the trail")
	}
}

func TestLiftAuthorization_RefusedWhenItDoesNotCover(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		prep func(r *authRig) audit.LiftAuthorization
		opts []audit.LiftOption
	}{
		"the lifter's own key": {prep: func(r *authRig) audit.LiftAuthorization {
			r.authz, r.authV = r.signer, r.v
			// NewLiftAuthorization refuses the trail's key, so build it as another key and swap in the lifter's.
			a, err := audit.NewLiftAuthorization(ctx, r.trail, r.v, p11Signers(r.t)["ecdsa-p256"], "test", r.holdID, "x", "y", "", time.UnixMilli(authAt), time.Hour)
			if err != nil {
				r.t.Fatal(err)
			}
			a.Alg, a.PublicKey, a.KeyID = string(r.signer.Alg()), r.signer.PublicKey(), audit.KeyID(string(r.signer.Alg()), r.signer.PublicKey())
			return resign(r.t, a, r.signer)
		}},
		"expired at the lift": {prep: func(r *authRig) audit.LiftAuthorization { return r.authorize(time.Second) }},
		"another deployment": {prep: func(r *authRig) audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.Deployment = "other"
			return resign(r.t, a, r.authz)
		}},
		"another hold": {prep: func(r *authRig) audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.HoldID = "h2"
			return resign(r.t, a, r.authz)
		}},
		"another placement": {prep: func(r *authRig) audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.PlacedPosition++
			return resign(r.t, a, r.authz)
		}},
		"another placement leaf": {prep: func(r *authRig) audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.PlacedLeaf = strings.Repeat("0", len(a.PlacedLeaf))
			return resign(r.t, a, r.authz)
		}},
		"another matter": {prep: func(r *authRig) audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.HoldMatter = "a different matter"
			return resign(r.t, a, r.authz)
		}},
		"other runs": {prep: func(r *authRig) audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.HoldRuns = []string{"gov", "extra"}
			return resign(r.t, a, r.authz)
		}},
		"another tree setting": {prep: func(r *authRig) audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.HoldTree = !a.HoldTree
			return resign(r.t, a, r.authz)
		}},
		"signed before the placement": {prep: func(r *authRig) audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.TrailSize = a.PlacedPosition
			return resign(r.t, a, r.authz)
		}},
		"a key ID that is not the key's": {prep: func(r *authRig) audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.KeyID = "ecdsa-p256:00"
			return resign(r.t, a, r.authz)
		}},
		"a tampered signature": {prep: func(r *authRig) audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.Signature[0] ^= 1
			return a
		}},
		"a tampered field": {prep: func(r *authRig) audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.NotAfterMs += 1_000_000
			return a
		}},
		"another lifter key": {prep: func(r *authRig) audit.LiftAuthorization {
			a, err := audit.NewLiftAuthorization(ctx, r.trail, r.v, r.authz, "test", r.holdID, "x", "y", "ed25519:00", time.UnixMilli(authAt), time.Hour)
			if err != nil {
				r.t.Fatal(err)
			}
			return a
		}},
		"an authorizer that is not pinned": {prep: func(r *authRig) audit.LiftAuthorization { return r.authorize(time.Hour) },
			opts: []audit.LiftOption{audit.WithLiftAuthorizers(p11Verifier(t, p11Signers(t)["ml-dsa-65"]))}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newAuthRig(t)
			a := tc.prep(r)
			before, _ := r.trail.Load(ctx, audit.RedactionTrailName)
			err := r.lift(append([]audit.LiftOption{audit.WithLiftAuthorization(a)}, tc.opts...)...)
			if !errors.Is(err, audit.ErrLiftUnauthorized) {
				t.Fatalf("lift = %v; want ErrLiftUnauthorized", err)
			}
			after, _ := r.trail.Load(ctx, audit.RedactionTrailName)
			if !r.inForce() || len(after) != len(before) {
				t.Fatal("a refused lift changed the hold or the trail")
			}
		})
	}
}

func resign(t *testing.T, a audit.LiftAuthorization, s audit.Signer) audit.LiftAuthorization {
	t.Helper()
	b, err := audit.SignLiftAuthorizationForTest(a, s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// An authorization is bound to one placement: after the hold is lifted and placed again under the
// same ID, the old file does not cover the new placement.
func TestLiftAuthorization_NotReusableAfterAReplacement(t *testing.T) {
	ctx := context.Background()
	r := newAuthRig(t)
	a := r.authorize(time.Hour)
	if err := r.lift(audit.WithLiftAuthorization(a)); err != nil {
		t.Fatal(err)
	}
	if err := r.placeHold(r.holdID, "gov"); err != nil {
		t.Fatal(err)
	}
	if err := r.lift(audit.WithLiftAuthorization(a)); !errors.Is(err, audit.ErrLiftUnauthorized) {
		t.Fatalf("reused authorization: lift = %v; want ErrLiftUnauthorized", err)
	}
	if hs, _ := r.store.Holds(ctx); len(hs) != 1 {
		t.Fatal("the re-placed hold was lifted with the old authorization")
	}
}

// The verifier checks what is embedded, with no help from the lift code: records written directly are
// judged the same.
func TestLiftAuthorization_VerifierFindsWhatTheLiftWouldRefuse(t *testing.T) {
	ctx := context.Background()
	hold := func(r *authRig) audit.RedactionRecord {
		return audit.RedactionRecord{Phase: audit.RedactionHoldLifted, Reason: "x", Ticket: "y", Executor: "z", AtMs: liftAtMs, Deployment: "test",
			HoldID: r.holdID, HoldMatter: "litigation 7", HoldRuns: []string{"gov"}, HoldTree: true}
	}
	for name, tc := range map[string]struct {
		build   func(r *authRig) *audit.LiftAuthorization
		require bool
		pin     bool
		want    string // "" for none
	}{
		"none, not required": {build: func(*authRig) *audit.LiftAuthorization { return nil }, want: ""},
		"none, required":     {build: func(*authRig) *audit.LiftAuthorization { return nil }, require: true, pin: true, want: "LIFT_UNAUTHORIZED"},
		"valid, required":    {build: func(r *authRig) *audit.LiftAuthorization { a := r.authorize(time.Hour); return &a }, require: true, pin: true, want: ""},
		"valid, not pinned":  {build: func(r *authRig) *audit.LiftAuthorization { a := r.authorize(time.Hour); return &a }, want: ""},
		"bad signature, not required": {build: func(r *authRig) *audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.Signature[0] ^= 1
			return &a
		}, want: "LIFT_UNAUTHORIZED"},
		"expired": {build: func(r *authRig) *audit.LiftAuthorization { a := r.authorize(time.Second); return &a }, want: "LIFT_UNAUTHORIZED"},
		"the lifter's key": {build: func(r *authRig) *audit.LiftAuthorization {
			a := r.authorize(time.Hour)
			a.Alg, a.PublicKey, a.KeyID = string(r.signer.Alg()), r.signer.PublicKey(), audit.KeyID(string(r.signer.Alg()), r.signer.PublicKey())
			b := resign(r.t, a, r.signer)
			return &b
		}, want: "LIFT_UNAUTHORIZED"},
		"not a pinned authorizer": {build: func(r *authRig) *audit.LiftAuthorization {
			other := p11Signers(r.t)["ml-dsa-65"]
			a := r.authorize(time.Hour)
			a.Alg, a.PublicKey, a.KeyID = string(other.Alg()), other.PublicKey(), audit.KeyID(string(other.Alg()), other.PublicKey())
			b := resign(r.t, a, other)
			return &b
		}, pin: true, want: "LIFT_UNAUTHORIZED"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newAuthRig(t)
			rec := hold(r)
			rec.Authorization = tc.build(r)
			if err := audit.AppendRedactionRecord(ctx, r.trail, rec, r.signer); err != nil {
				t.Fatal(err)
			}
			var pin []audit.Verifier
			if tc.pin {
				pin = []audit.Verifier{r.authV}
			}
			rep, _ := r.verify(tc.require, pin...)
			got := ""
			for _, f := range rep.Findings {
				if f.Kind == audit.FindingLiftUnauthorized {
					got = string(f.Kind)
				}
			}
			if got != tc.want {
				t.Fatalf("findings = %s; want %q", kinds(rep), tc.want)
			}
		})
	}
}

func TestLiftAuthorization_RequiringNeedsKeys(t *testing.T) {
	r := newAuthRig(t)
	leaves, head := r.signedTrail()
	_, err := audit.VerifyRedactions(audit.RedactionVerifyInput{Deployment: "test", Trail: leaves, Heads: []audit.SignedTreeHead{head}, Verifier: r.v, RequireLiftAuthorization: true})
	if !errors.Is(err, agent.ErrConfig) || !strings.Contains(err.Error(), "authorizers' keys") {
		t.Fatalf("err = %v; want ErrConfig naming the keys", err)
	}
}

func TestNewLiftAuthorization_Refusals(t *testing.T) {
	ctx := context.Background()
	r := newAuthRig(t)
	if _, err := audit.NewLiftAuthorization(ctx, r.trail, r.v, r.signer, "test", r.holdID, "x", "y", "", time.UnixMilli(authAt), time.Hour); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("the trail's own key as authorizer = %v; want ErrConfig", err)
	}
	if _, err := audit.NewLiftAuthorization(ctx, r.trail, r.v, r.authz, "test", "nope", "x", "y", "", time.UnixMilli(authAt), time.Hour); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("a hold the trail does not show = %v; want ErrConfig", err)
	}
	if _, err := audit.NewLiftAuthorization(ctx, r.trail, r.v, r.authz, "test", r.holdID, "x", "y", "", time.UnixMilli(authAt), 0); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("a zero validity = %v; want ErrConfig", err)
	}
	if _, err := audit.NewLiftAuthorization(ctx, r.trail, r.v, r.authz, "other-deployment", r.holdID, "x", "y", "", time.UnixMilli(authAt), time.Hour); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("another deployment's trail = %v; want ErrConfig (no such hold there)", err)
	}
}

// A second person is a second secret: a hybrid key whose Ed25519 half is the trail's Ed25519 key is the
// same key holder, however the algorithm is named. The check compares by component, at creation, at the
// lift, in the pinned list and in the verifier.
func TestLiftAuthorization_SharedComponentIsNotASecondPerson(t *testing.T) {
	ctx := context.Background()
	r := newAuthRig(t)
	ed := r.signer.(audit.Ed25519Signer)
	hybrid := audit.HybridSigner{Ed: ed, ML: p11Signers(t)["ml-dsa-65"].(audit.MLDSASigner)}
	hv := p11Verifier(t, hybrid)
	// The authorizer is a hybrid holding the trail's Ed25519 key.
	if _, err := audit.NewLiftAuthorization(ctx, r.trail, r.v, hybrid, "test", r.holdID, "x", "y", "", time.UnixMilli(authAt), time.Hour); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("a hybrid authorizer holding the trail's Ed25519 key = %v; want ErrConfig", err)
	}
	// Pinning such a key is refused, by the lift and by the verifier.
	if err := r.lift(audit.WithLiftAuthorizers(hv)); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("lift with a pinned authorizer that shares a component = %v; want ErrConfig", err)
	}
	leaves, head := r.signedTrail()
	if _, err := audit.VerifyRedactions(audit.RedactionVerifyInput{Deployment: "test", Trail: leaves, Heads: []audit.SignedTreeHead{head}, Verifier: r.v, LiftAuthorizers: []audit.Verifier{hv}}); !errors.Is(err, agent.ErrConfig) {
		t.Errorf("verify with such a pinned authorizer = %v; want ErrConfig", err)
	}
	// A forged authorization signed by that hybrid is refused by the lift and found by the verifier.
	a := r.authorize(time.Hour)
	a.Alg, a.PublicKey, a.KeyID = string(hybrid.Alg()), hybrid.PublicKey(), audit.KeyID(string(hybrid.Alg()), hybrid.PublicKey())
	a = resign(t, a, hybrid)
	if err := r.lift(audit.WithLiftAuthorization(a)); !errors.Is(err, audit.ErrLiftUnauthorized) {
		t.Fatalf("lift with a hybrid that shares the trail's Ed25519 half = %v; want ErrLiftUnauthorized", err)
	}
	rec := audit.RedactionRecord{Phase: audit.RedactionHoldLifted, Reason: "x", Ticket: "y", Executor: "z", AtMs: liftAtMs, Deployment: "test",
		HoldID: r.holdID, HoldMatter: "litigation 7", HoldRuns: []string{"gov"}, HoldTree: true, Authorization: &a}
	if err := audit.AppendRedactionRecord(ctx, r.trail, rec, r.signer); err != nil {
		t.Fatal(err)
	}
	if rep, _ := r.verify(false); !strings.Contains(kinds(rep), "LIFT_UNAUTHORIZED") {
		t.Errorf("the verifier accepted a hybrid that shares the trail key's Ed25519 half: %s", kinds(rep))
	}
}

// The other direction: the trail is signed with a hybrid key, the authorizer is its Ed25519 half.
func TestLiftAuthorization_AuthorizerIsAHalfOfTheTrailsHybridKey(t *testing.T) {
	ctx := context.Background()
	r := newTrailRig(t)
	ed := p11Signers(t)["ed25519"].(audit.Ed25519Signer)
	hybrid := audit.HybridSigner{Ed: ed, ML: p11Signers(t)["ml-dsa-65"].(audit.MLDSASigner)}
	r.signer, r.v = hybrid, p11Verifier(t, hybrid)
	if err := r.placeHold("h1", "gov"); err != nil {
		t.Fatal(err)
	}
	if _, err := audit.NewLiftAuthorization(ctx, r.trail, r.v, ed, "test", "h1", "x", "y", "", time.UnixMilli(authAt), time.Hour); !errors.Is(err, agent.ErrConfig) {
		t.Fatalf("the Ed25519 half of the trail's hybrid key as authorizer = %v; want ErrConfig", err)
	}
}
