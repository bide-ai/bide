package audit

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
)

// LiftAuthorizationFormat is the format of a LiftAuthorization.
const LiftAuthorizationFormat = "bide.audit.redaction-authorization.v1"

const liftAuthorizationTag = "bide.audit.redaction-authorization.v1\x00"

// liftAuthorizationKind is LiftAuthorization.Kind: what the authorizer authorizes.
const liftAuthorizationKind = "hold_lift"

// ErrLiftUnauthorized is a lift of a legal hold that needs a second person's signed authorization and
// has none that checks out: it is refused, and the hold stays in force.
var ErrLiftUnauthorized = fmt.Errorf("legal hold lift refused: it needs the signed authorization of a second person: %w", agent.ErrConfig)

// A LiftAuthorization is a second person's signed consent to lift one placement of one legal hold. The
// authorizer signs it with a key of their own, which is not the key that signs the redaction trail
// (the lifter's), and the lift's trail record embeds it, so a verifier checks it offline without the
// file. It is bound to the hold as it stood: the placement record's position and leaf hash, so a file
// cannot be reused against a re-placed hold with the same ID, against another deployment, or after
// NotAfterMs.
type LiftAuthorization struct {
	Format string `json:"format"`
	Kind   string `json:"kind"`
	// Deployment is the deployment whose trail the hold is in.
	Deployment string `json:"deployment"`
	// HoldID, HoldMatter, HoldRuns and HoldTree describe the hold as the authorizer saw it.
	HoldID     string   `json:"hold_id"`
	HoldMatter string   `json:"hold_matter"`
	HoldRuns   []string `json:"hold_runs"`
	HoldTree   bool     `json:"hold_tree,omitempty"`
	// PlacedPosition and PlacedLeaf are the trail position and the hex leaf hash of the hold_placed
	// record the authorizer saw in force; TrailSize is how many records the trail held then.
	PlacedPosition int    `json:"placed_position"`
	PlacedLeaf     string `json:"placed_leaf"`
	TrailSize      int    `json:"trail_size"`
	Reason         string `json:"reason"`
	Ticket         string `json:"ticket"`
	// LifterKeyID, when set, is the KeyID of the trail key that must sign the lift.
	LifterKeyID    string `json:"lifter_key_id,omitempty"`
	AuthorizedAtMs int64  `json:"authorized_at_ms"`
	NotAfterMs     int64  `json:"not_after_ms"`
	// Alg, PublicKey and KeyID identify the authorizer's key; Signature signs every other member.
	Alg       string `json:"alg"`
	PublicKey []byte `json:"public_key"`
	KeyID     string `json:"key_id"`
	Signature []byte `json:"signature"`
}

func (a LiftAuthorization) signedBytes() ([]byte, error) {
	a.Signature = nil
	b, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("audit: encode lift authorization: %w", err)
	}
	return append([]byte(liftAuthorizationTag), b...), nil
}

// LiftOption is an option of LiftLegalHold.
type LiftOption func(*liftConfig)

type liftConfig struct {
	auth        *LiftAuthorization
	authorizers []Verifier
	require     bool
}

// WithLiftAuthorization gives LiftLegalHold the second person's authorization to embed in the lift's
// record. It is checked first, against the placement in force, and a lift it does not cover is refused.
func WithLiftAuthorization(a LiftAuthorization) LiftOption {
	return func(c *liftConfig) { c.auth = &a }
}

// WithLiftAuthorizers pins the keys an authorization must be signed by. With none, any key but the
// lifter's is accepted, and the verifier (RedactionVerifyInput.LiftAuthorizers) is where the keys are
// pinned.
func WithLiftAuthorizers(v ...Verifier) LiftOption {
	return func(c *liftConfig) { c.authorizers = append(c.authorizers, v...) }
}

// RequireLiftAuthorization makes LiftLegalHold refuse a lift that has no valid authorization
// (ErrLiftUnauthorized).
func RequireLiftAuthorization() LiftOption {
	return func(c *liftConfig) { c.require = true }
}

// NewLiftAuthorization reads the redaction trail (its records verified under trailKey, the key that
// signs the trail) and returns authorizer's signed consent to lift the hold holdID as the trail shows
// it now. The authorization is valid until now + validFor. lifterKeyID, when not empty, names the trail
// key that must sign the lift. It is refused for a hold the trail does not show in force, for the
// trail's own key (the authorizer must be a second key), and for a validity that is not positive.
func NewLiftAuthorization(ctx context.Context, trail EventStore, trailKey Verifier, authorizer Signer, deployment, holdID, reason, ticket, lifterKeyID string, now time.Time, validFor time.Duration) (LiftAuthorization, error) {
	switch {
	case trail == nil || trailKey == nil || authorizer == nil:
		return LiftAuthorization{}, fmt.Errorf("audit: NewLiftAuthorization: a trail, the trail's key and an authorizer are required: %w", agent.ErrConfig)
	case strings.TrimSpace(deployment) == "" || strings.TrimSpace(holdID) == "" || strings.TrimSpace(reason) == "" || strings.TrimSpace(ticket) == "":
		return LiftAuthorization{}, fmt.Errorf("audit: NewLiftAuthorization: a deployment, a hold ID, a reason and a ticket are required: %w", agent.ErrConfig)
	case validFor <= 0 || now.IsZero():
		return LiftAuthorization{}, fmt.Errorf("audit: NewLiftAuthorization: a time and a positive validity are required: %w", agent.ErrConfig)
	case sameKey(string(authorizer.Alg()), authorizer.PublicKey(), string(trailKey.Alg()), trailKey.PublicKey()):
		return LiftAuthorization{}, fmt.Errorf("audit: NewLiftAuthorization: the authorizer's key is the redaction trail's key: a lift needs a second person's key: %w", agent.ErrConfig)
	}
	active, _, leaves, err := trailHoldsWith(ctx, trail, trailKey, deployment)
	if err != nil {
		return LiftAuthorization{}, err
	}
	placed, ok := active[holdID]
	if !ok {
		return LiftAuthorization{}, fmt.Errorf("audit: NewLiftAuthorization: the redaction trail shows no hold %q in force: %w", holdID, agent.ErrConfig)
	}
	a := LiftAuthorization{
		Format: LiftAuthorizationFormat, Kind: liftAuthorizationKind, Deployment: strings.TrimSpace(deployment),
		HoldID: placed.HoldID, HoldMatter: placed.HoldMatter, HoldRuns: slices.Clone(placed.HoldRuns), HoldTree: placed.HoldTree,
		PlacedPosition: placed.Position, PlacedLeaf: hex.EncodeToString(leafHash(leaves[placed.Position])), TrailSize: len(leaves),
		Reason: reason, Ticket: ticket, LifterKeyID: lifterKeyID,
		AuthorizedAtMs: now.UnixMilli(), NotAfterMs: now.Add(validFor).UnixMilli(),
		Alg: string(authorizer.Alg()), PublicKey: authorizer.PublicKey(), KeyID: KeyID(string(authorizer.Alg()), authorizer.PublicKey()),
	}
	m, err := a.signedBytes()
	if err != nil {
		return LiftAuthorization{}, err
	}
	if a.Signature, err = authorizer.Sign(m); err != nil {
		return LiftAuthorization{}, fmt.Errorf("audit: sign lift authorization: %w", err)
	}
	return a, nil
}

// sameKey reports whether two keys are one key holder: their verifiers' KeyIDs sets intersect. This is
// the rule approvals and the stop quorum use for "the same person" (Verifier.KeyIDs lists one ID per
// independent key component, so a hybrid key and its own Ed25519 or ML-DSA half share an ID): one
// rule repo-wide, and a key that does not read is never "the same" as anything.
func sameKey(algA string, pubA []byte, algB string, pubB []byte) bool {
	va, errA := NewVerifier(Alg(algA), pubA)
	vb, errB := NewVerifier(Alg(algB), pubB)
	if errA != nil || errB != nil {
		return false
	}
	return slices.ContainsFunc(va.KeyIDs(), func(id string) bool { return slices.Contains(vb.KeyIDs(), id) })
}

// checkAuthorizersApart refuses a pinned authorizer that shares a secret with the key that signs the
// trail: it could never authorize anything a second person should.
func checkAuthorizersApart(authorizers []Verifier, trailAlg string, trailPub []byte) error {
	for _, p := range authorizers {
		if sameKey(string(p.Alg()), p.PublicKey(), trailAlg, trailPub) {
			return fmt.Errorf("audit: an authorizer key (%s) is the redaction trail's key or shares a component with it: a second person needs a key of their own: %w", KeyID(string(p.Alg()), p.PublicKey()), agent.ErrConfig)
		}
	}
	return nil
}

// checkLiftAuthorization reports why a does not authorize the lift record lift of the placement placed
// (the hold_placed record in force, whose trail leaf is placedLeaf), or "" if it does. The lift was
// signed by the key lifterAlg/lifterPub. authorizers, when not empty, pins the keys a must be signed
// by.
func checkLiftAuthorization(a LiftAuthorization, lift, placed RedactionRecord, placedLeaf []byte, lifterAlg string, lifterPub []byte, authorizers []Verifier) string {
	v, err := NewVerifier(Alg(a.Alg), a.PublicKey)
	if err != nil {
		return "its authorizer key does not read"
	}
	m, err := a.signedBytes()
	if err != nil || len(a.Signature) == 0 || !v.Verify(m, a.Signature) {
		return "its signature does not verify under its authorizer key"
	}
	switch {
	case a.Format != LiftAuthorizationFormat || a.Kind != liftAuthorizationKind:
		return "it is not a hold lift authorization"
	case a.KeyID != KeyID(a.Alg, a.PublicKey):
		return "its key ID is not its key's"
	case a.Deployment != lift.Deployment:
		return fmt.Sprintf("it is for deployment %q, not %q", a.Deployment, lift.Deployment)
	case a.HoldID != lift.HoldID || a.HoldID != placed.HoldID:
		return fmt.Sprintf("it is for hold %q, not %q", a.HoldID, lift.HoldID)
	case a.HoldMatter != placed.HoldMatter || a.HoldTree != placed.HoldTree || !slices.Equal(a.HoldRuns, placed.HoldRuns):
		return "it describes a different hold than the one in force (matter, runs or tree setting)"
	case a.PlacedPosition != placed.Position || a.PlacedLeaf != hex.EncodeToString(leafHash(placedLeaf)):
		return "it is for another placement of the hold than the one in force (the placement's trail position or leaf differs)"
	case a.TrailSize <= a.PlacedPosition:
		return "it claims to have been signed before the placement it authorizes the lift of"
	case a.NotAfterMs < lift.AtMs:
		return fmt.Sprintf("it expired at %d, before the lift at %d", a.NotAfterMs, lift.AtMs)
	case sameKey(a.Alg, a.PublicKey, lifterAlg, lifterPub):
		return "its authorizer's key is the key that signed the lift: a lift needs a second person's key"
	case a.LifterKeyID != "" && a.LifterKeyID != KeyID(lifterAlg, lifterPub):
		return fmt.Sprintf("it names lifter key %s, and the lift is signed by %s", a.LifterKeyID, KeyID(lifterAlg, lifterPub))
	}
	if len(authorizers) > 0 && !slices.ContainsFunc(authorizers, func(p Verifier) bool {
		return sameKey(a.Alg, a.PublicKey, string(p.Alg()), p.PublicKey())
	}) {
		return fmt.Sprintf("its authorizer key %s is not one of the keys given for authorizers", a.KeyID)
	}
	return ""
}
