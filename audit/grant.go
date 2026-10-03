package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/bide-ai/bide/agent"
)

// Grant is a signed statement by a principal (Issuer) authorizing an actor (Subject) to act within
// Scope until NotAfterUnix. It is the non-repudiable authority behind a governed action: the action's
// agent.Identity.AuthorityRef is a grant's Digest, and the grant is signed by the issuer's own key
// (distinct from the log's tree-head key), so an auditor attributes the authorization to the
// principal, not merely to the operator that runs the log.
//
// ParentRef links a grant to the one it was attenuated from (empty for a root grant), forming a
// hash-linked delegation chain: a holder of a grant may mint a strictly narrower sub-grant for a
// sub-agent without returning to the root issuer (capability attenuation), and the chain back to
// the root is provable. Scope entries are constraints whose meaning is domain-defined; the chain
// verifier requires a child to keep every one of its parent's, unchanged or narrowed by a ScopeRule
// the verifier supplies (NumericAtMost is provided for limits). See CheckAttenuation.
type Grant struct {
	ID      string            `json:"id"`              // unique identifier for this grant
	Issuer  string            `json:"issuer"`          // principal that authorized the grant (resolves to the signing key)
	Subject string            `json:"subject"`         // actor the grant authorizes to act
	Scope   map[string]string `json:"scope,omitempty"` // domain-defined constraints (e.g. limits); a child keeps every one, equal or narrower
	// NotAfterUnix is the expiry in Unix seconds; zero never expires, and only a grant whose parent
	// never expires may be zero.
	NotAfterUnix int64  `json:"not_after_unix,omitempty"`
	ParentRef    string `json:"parent_ref,omitempty"` // Digest of the grant this was attenuated from; empty for a root grant
}

// GrantFormat tags the canonical bytes of a grant (Grant.Bytes), which are both signed and
// digested. v2 names the expiry not_after_unix and tags the signed bytes as well as the digest.
const GrantFormat = "bide.audit.grant.v2"

// Bytes is the canonical serialization that is signed and digested: "bide.audit.grant.v2\n"
// followed by the grant's JSON. json.Marshal sorts map keys and emits struct fields in declaration
// order, so it is deterministic in-ecosystem. It is injective only over valid UTF-8 (JSON rewrites
// invalid bytes to U+FFFD), so SignGrant refuses, and SignedGrant.Verify rejects, a grant with
// invalid UTF-8 in any string.
func (g Grant) Bytes() []byte {
	b, _ := json.Marshal(g) // a Grant always marshals: strings, a string map, and integers
	return append([]byte(GrantFormat+"\n"), b...)
}

// Digest is a stable SHA-256 over the grant's canonical bytes (Bytes), used as
// agent.Identity.AuthorityRef and as the anchoring key. It is recomputable by any verifier from the
// disclosed grant.
func (g Grant) Digest() string {
	sum := sha256.Sum256(g.Bytes())
	return hex.EncodeToString(sum[:])
}

// Expired reports whether the grant is past NotAfterUnix at nowUnix (Unix seconds). A zero
// NotAfterUnix never expires. The caller supplies the clock, so verification stays deterministic.
func (g Grant) Expired(nowUnix int64) bool { return g.NotAfterUnix != 0 && nowUnix > g.NotAfterUnix }

// SignedGrant is a Grant plus the issuer's signature over its canonical bytes.
type SignedGrant struct {
	Grant Grant  `json:"grant"` // the grant whose canonical bytes are signed
	Alg   Alg    `json:"alg"`   // signature scheme the issuer used (see signing.go)
	Sig   []byte `json:"sig"`   // issuer's signature over Grant.Bytes()
}

// SignGrant signs a grant with the issuer's key (Ed25519, ML-DSA, or hybrid, per the Signer).
func SignGrant(g Grant, s Signer) (SignedGrant, error) {
	if err := checkSigner(s); err != nil {
		return SignedGrant{}, fmt.Errorf("audit: sign grant %q: %w", g.ID, err)
	}
	if err := checkUTF8(g); err != nil {
		return SignedGrant{}, fmt.Errorf("audit: sign grant %q: %w", g.ID, err)
	}
	sig, err := s.Sign(g.Bytes())
	if err != nil {
		return SignedGrant{}, fmt.Errorf("audit: sign grant %q: %w", g.ID, err)
	}
	return SignedGrant{Grant: g, Alg: s.Alg(), Sig: sig}, nil
}

// Verify returns nil if the issuer's signature over the grant verifies under v. The verifier is
// the issuer's public key, resolved by the caller's PKI; this is non-repudiation of the
// authorization, separate from the log's tree-head signature. A grant signed under another scheme
// than v's, or whose signature does not verify, is an error wrapping ErrNotVerified; a grant with
// invalid UTF-8 in a string, ErrMalformed.
func (sg SignedGrant) Verify(v Verifier) error {
	if v == nil {
		return fmt.Errorf("audit: no verifier for grant %q: %w", sg.Grant.ID, agent.ErrConfig)
	}
	if err := checkUTF8(sg.Grant); err != nil {
		return fmt.Errorf("audit: grant %q: %w (%w)", sg.Grant.ID, err, ErrMalformed)
	}
	if sg.Alg != v.Alg() {
		return notVerified("audit: grant %q is signed under %q, not the verifier's %q", sg.Grant.ID, sg.Alg, v.Alg())
	}
	if !v.Verify(sg.Grant.Bytes(), sg.Sig) {
		return notVerified("audit: grant %q signature does not verify under its issuer's key", sg.Grant.ID)
	}
	return nil
}

// grantLeafPrefix starts the journal name of every anchored grant leaf.
const grantLeafPrefix = "audit:grant:"

func grantLeafName(digest string) string { return grantLeafPrefix + digest }

// RecordGrant commits a signed grant as a dedicated journal leaf keyed by its digest (idempotent per
// (runID, digest)), so it is covered by the same signed tree head and inclusion proofs as the
// actions taken under it. A governed action whose Identity.AuthorityRef equals the grant's digest
// then links to an anchored, issuer-signed authority.
func RecordGrant(ctx context.Context, store *agent.Journal, runID string, sg SignedGrant) (agent.Record, error) {
	content, err := json.Marshal(sg)
	if err != nil {
		return agent.Record{}, fmt.Errorf("audit: marshal signed grant: %w", err)
	}
	return doRecord(ctx, store, runID, grantLeafName(sg.Grant.Digest()), func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: content}, nil
	})
}

// ProveGrant builds a ProofBundle proving the grant with this digest was committed in the tree sth
// signs. Pair it with an action's bundle whose Identity.AuthorityRef matches the digest to show the
// action ran under an in-log, issuer-signed authority.
func ProveGrant(ctx context.Context, store *agent.Journal, runID, digest string, sth SignedTreeHead) (ProofBundle, error) {
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

// ScopeRule reports whether child, a changed value for one scope key, is within parent, the
// parent grant's value for that key.
type ScopeRule func(parent, child string) bool

// ScopeRules maps a scope key to the rule that decides whether a changed value narrows it. A key
// with no rule can only be carried to a child unchanged.
type ScopeRules map[string]ScopeRule

// NumericAtMost is the ScopeRule for an integer upper bound (a limit): the child's value, parsed as
// a base-10 integer, must be at most the parent's. Unparseable values fail closed.
func NumericAtMost(parent, child string) bool {
	pv, perr := strconv.ParseInt(parent, 10, 64)
	cv, cerr := strconv.ParseInt(child, 10, 64)
	return perr == nil && cerr == nil && cv <= pv
}

// CheckAttenuation returns nil if child is a valid delegation of parent, and otherwise an error
// naming the first rule it breaks:
//
//  1. child.ParentRef is parent's Digest (the hash link);
//  2. child.Issuer is parent.Subject (only the holder of a grant can delegate it);
//  3. child expires no later than parent: if parent.NotAfterUnix is set, child.NotAfterUnix is set
//     and at most parent.NotAfterUnix (a child of an expiring grant can never be non-expiring);
//  4. every parent scope key is in child, with the same value or, where rules has a ScopeRule for
//     that key, a value the rule accepts as narrower.
//
// Scope entries are constraints: each one restricts what the grant allows, so a child may add keys
// its parent lacks (adding a constraint narrows) but never drop or loosen one. A domain whose scope
// key grants rather than restricts must express it so that its ScopeRule decides narrowing (for
// example a comma-separated allowlist whose rule requires a subset).
func CheckAttenuation(parent, child Grant, rules ScopeRules) error {
	if child.ParentRef != parent.Digest() {
		return fmt.Errorf("audit: grant %q parent_ref does not link to %q", child.ID, parent.ID)
	}
	if child.Issuer != parent.Subject {
		return fmt.Errorf("audit: grant %q is issued by %q, but only %q (the subject of %q) can delegate it", child.ID, child.Issuer, parent.Subject, parent.ID)
	}
	if parent.NotAfterUnix != 0 && (child.NotAfterUnix == 0 || child.NotAfterUnix > parent.NotAfterUnix) {
		return fmt.Errorf("audit: grant %q expires at %d, after its parent %q (%d)", child.ID, child.NotAfterUnix, parent.ID, parent.NotAfterUnix)
	}
	keys := make([]string, 0, len(parent.Scope))
	for k := range parent.Scope {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		pv := parent.Scope[k]
		cv, ok := child.Scope[k]
		switch {
		case !ok:
			return fmt.Errorf("audit: grant %q drops scope %q=%q of its parent %q", child.ID, k, pv, parent.ID)
		case cv == pv:
		case rules[k] == nil:
			return fmt.Errorf("audit: grant %q changes scope %q from %q to %q, and no rule allows narrowing it", child.ID, k, pv, cv)
		case !rules[k](pv, cv):
			return fmt.Errorf("audit: grant %q widens scope %q from %q to %q", child.ID, k, pv, cv)
		}
	}
	return nil
}

// VerifyDelegationChain returns nil if chain, ordered root-first (index 0) to leaf-last, is valid:
//  1. each grant's signature verifies under its issuer's verifier (resolved via issuerVerifier),
//  2. the root has no ParentRef,
//  3. every other grant is a valid delegation of the previous one (CheckAttenuation with rules):
//     hash-linked, issued by the parent's subject, expiring no later, and keeping every scope
//     constraint equal or narrower.
//
// It establishes that the leaf's authority descends, unbroken and never widened, from the root
// grant, with each hop signed by the principal it names. The caller still checks that the root's
// Issuer is a principal it trusts to grant that authority (issuerVerifier resolves any issuer it
// knows), and that the leaf is unexpired at the time of use (Grant.Expired, which needs a clock).
// A chain that does not hold, including one with an issuer issuerVerifier does not know, is an
// error wrapping ErrNotVerified.
func VerifyDelegationChain(chain []SignedGrant, issuerVerifier func(issuer string) (Verifier, bool), rules ScopeRules) error {
	if len(chain) == 0 {
		return notVerified("audit: empty delegation chain")
	}
	if issuerVerifier == nil {
		return fmt.Errorf("audit: no issuer verifier resolver: %w", agent.ErrConfig)
	}
	for i, sg := range chain {
		v, ok := issuerVerifier(sg.Grant.Issuer)
		if !ok || v == nil {
			return notVerified("audit: no verifier for issuer %q (grant %q)", sg.Grant.Issuer, sg.Grant.ID)
		}
		if err := sg.Verify(v); err != nil {
			return err
		}
		if i == 0 {
			if sg.Grant.ParentRef != "" {
				return notVerified("audit: root grant %q carries a parent_ref", sg.Grant.ID)
			}
			continue
		}
		if err := CheckAttenuation(chain[i-1].Grant, sg.Grant, rules); err != nil {
			return fmt.Errorf("%w: %w", err, ErrNotVerified)
		}
	}
	return nil
}
