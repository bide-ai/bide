package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	agent "github.com/dayna/go-agents"
)

// Grant is a signed statement by a principal (Issuer) authorizing an actor (Subject) to act within
// Scope until NotAfter. It is the non-repudiable authority behind a governed action: the action's
// agent.Identity.AuthorityRef is a grant's Digest, and the grant is signed by the issuer's own key
// (distinct from the log's tree-head key), so an auditor attributes the authorization to the
// principal, not merely to the operator that runs the log.
//
// ParentRef links a grant to the one it was attenuated from (empty for a root grant), forming a
// hash-linked delegation chain: a holder of a grant may mint a strictly narrower sub-grant for a
// sub-agent without returning to the root issuer (capability attenuation), and the chain back to
// the root is provable. Scope semantics are domain-defined; the chain verifier takes an Attenuator
// that decides what "narrower" means (a helper for numeric limits is provided).
type Grant struct {
	ID        string            `json:"id"`
	Issuer    string            `json:"issuer"`
	Subject   string            `json:"subject"`
	Scope     map[string]string `json:"scope,omitempty"`
	NotAfter  int64             `json:"not_after,omitempty"`
	ParentRef string            `json:"parent_ref,omitempty"`
}

const grantDigestPrefix = "goagents-grant-v1\n"

// Bytes is the canonical serialization that is signed and digested. json.Marshal sorts map keys and
// emits struct fields in declaration order, so it is deterministic in-ecosystem.
func (g Grant) Bytes() []byte {
	b, _ := json.Marshal(g)
	return b
}

// Digest is a stable, domain-separated SHA-256 over the grant, used as agent.Identity.AuthorityRef
// and as the anchoring key. It is recomputable by any verifier from the disclosed grant.
func (g Grant) Digest() string {
	h := sha256.New()
	h.Write([]byte(grantDigestPrefix))
	h.Write(g.Bytes())
	return hex.EncodeToString(h.Sum(nil))
}

// Expired reports whether the grant is past NotAfter at time now (unix seconds). A zero NotAfter
// never expires. The caller supplies the clock, so verification stays deterministic.
func (g Grant) Expired(now int64) bool { return g.NotAfter != 0 && now > g.NotAfter }

// SignedGrant is a Grant plus the issuer's signature over its canonical bytes.
type SignedGrant struct {
	Grant Grant  `json:"grant"`
	Alg   string `json:"alg"`
	Sig   []byte `json:"sig"`
}

// SignGrant signs a grant with the issuer's key (Ed25519, ML-DSA, or hybrid, per the Signer).
func SignGrant(g Grant, s Signer) (SignedGrant, error) {
	sig, err := s.Sign(g.Bytes())
	if err != nil {
		return SignedGrant{}, fmt.Errorf("audit: sign grant %q: %w", g.ID, err)
	}
	return SignedGrant{Grant: g, Alg: s.Alg(), Sig: sig}, nil
}

// Verify checks the issuer's signature over the grant. The verifier is the issuer's public key,
// resolved by the caller's PKI; this is non-repudiation of the authorization, separate from the
// log's tree-head signature.
func (sg SignedGrant) Verify(v Verifier) (bool, error) {
	if sg.Alg != v.Alg() {
		return false, fmt.Errorf("audit: grant alg %q does not match verifier alg %q", sg.Alg, v.Alg())
	}
	return v.Verify(sg.Grant.Bytes(), sg.Sig), nil
}

func grantLeafName(digest string) string { return "audit:grant:" + digest }

// RecordGrant commits a signed grant as a dedicated journal leaf keyed by its digest (idempotent per
// (runID, digest)), so it is covered by the same signed tree head and inclusion proofs as the
// actions taken under it. A governed action whose Identity.AuthorityRef equals the grant's digest
// then links to an anchored, issuer-signed authority.
func RecordGrant(ctx context.Context, store agent.Durable, runID string, sg SignedGrant) (agent.Record, error) {
	content, err := json.Marshal(sg)
	if err != nil {
		return agent.Record{}, fmt.Errorf("audit: marshal signed grant: %w", err)
	}
	return store.Do(ctx, runID, grantLeafName(sg.Grant.Digest()), func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: content}, nil
	})
}

// ProveGrant builds a ProofBundle proving the grant with this digest was committed in the tree sth
// signs. Pair it with an action's bundle whose Identity.AuthorityRef matches the digest to show the
// action ran under an in-log, issuer-signed authority.
func ProveGrant(ctx context.Context, store agent.Durable, runID, digest string, sth SignedTreeHead) (ProofBundle, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return ProofBundle{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	name := grantLeafName(digest)
	idx := -1
	for i, r := range recs {
		if r.Kind == agent.StepValue && r.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ProofBundle{}, fmt.Errorf("audit: no grant leaf for digest %q in run %s", digest, runID)
	}
	return ProveRecord(ctx, store, runID, idx, sth)
}

// Attenuator reports whether child's authority is within parent's (domain-specific "narrower").
type Attenuator func(parent, child Grant) bool

// AttenuatesNumericScope requires child.Scope[key] <= parent.Scope[key] parsed as integers, the
// common "lower the limit" narrowing. Missing or unparseable values fail closed (not an
// attenuation), so a sub-grant cannot widen authority by dropping or corrupting a bound.
func AttenuatesNumericScope(key string) Attenuator {
	return func(parent, child Grant) bool {
		pv, perr := strconv.Atoi(parent.Scope[key])
		cv, cerr := strconv.Atoi(child.Scope[key])
		if perr != nil || cerr != nil {
			return false
		}
		return cv <= pv
	}
}

// VerifyDelegationChain verifies a delegation chain ordered root-first (index 0) to leaf-last:
//  1. each grant's signature under its issuer's verifier (resolved via issuerVerifier),
//  2. the root has no ParentRef; every other grant's ParentRef equals the previous grant's Digest,
//  3. every non-root grant is an attenuation of its parent (if atten is non-nil).
//
// It establishes that the leaf's authority descends, unbroken and never widened, from a root grant
// each hop's issuer signed. Expiry is left to the caller (grant.Expired) since it needs a clock.
func VerifyDelegationChain(chain []SignedGrant, issuerVerifier func(issuer string) (Verifier, bool), atten Attenuator) (bool, error) {
	if len(chain) == 0 {
		return false, fmt.Errorf("audit: empty delegation chain")
	}
	for i, sg := range chain {
		v, ok := issuerVerifier(sg.Grant.Issuer)
		if !ok {
			return false, fmt.Errorf("audit: no verifier for issuer %q (grant %q)", sg.Grant.Issuer, sg.Grant.ID)
		}
		if ok2, err := sg.Verify(v); err != nil || !ok2 {
			return false, fmt.Errorf("audit: grant %q signature invalid (err=%v)", sg.Grant.ID, err)
		}
		if i == 0 {
			if sg.Grant.ParentRef != "" {
				return false, fmt.Errorf("audit: root grant %q carries a parent_ref", sg.Grant.ID)
			}
			continue
		}
		parent := chain[i-1].Grant
		if sg.Grant.ParentRef != parent.Digest() {
			return false, fmt.Errorf("audit: grant %q parent_ref does not link to %q", sg.Grant.ID, parent.ID)
		}
		if atten != nil && !atten(parent, sg.Grant) {
			return false, fmt.Errorf("audit: grant %q is not an attenuation of %q", sg.Grant.ID, parent.ID)
		}
	}
	return true, nil
}
