package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// ErrInvalidStop marks a stop record that is malformed, or that no policy in force authorises.
var ErrInvalidStop = errors.New("agent: invalid stop record")

// ErrStopRejected is returned when a record was written but the control run's fold does not accept it
// (a policy transition raced it, or its signature does not verify under the policy in force at its
// position): it has no effect.
var ErrStopRejected = errors.New("agent: stop record not accepted")

// StopApprover is an enrolled approver in a StopPolicyDoc: the signature scheme and the public key.
type StopApprover struct {
	Alg Alg    `json:"alg"`
	Key string `json:"key"` // hex of the public key
}

// StopPolicyDoc is a stop policy as the control run carries it in a policy transition: the enrolled
// approvers with their public keys, the roles, and the lift rule. Its digest (StopPolicy.Digest)
// names the policy.
type StopPolicyDoc struct {
	// DeploymentID names the deployment: a random value chosen at genesis (generate one per
	// deployment, never share it between staging and production) that every signed record carries and
	// every policy of the chain repeats, so a record signed for one deployment is rejected in another.
	DeploymentID   string                  `json:"deployment_id"`
	Approvers      map[string]StopApprover `json:"approvers"`
	Setters        []string                `json:"setters"`
	Lifters        []string                `json:"lifters"`
	LiftQuorum     int                     `json:"lift_quorum"`
	LiftNeedsOther bool                    `json:"lift_needs_other"`
}

// StopVerifierFunc builds a verifier from a scheme and a public key, for the approvers of a policy a
// transition names (audit.NewVerifier has this form). A control without one cannot adopt a transition.
type StopVerifierFunc func(alg Alg, publicKey []byte) (ApproverVerifier, error)

// StopPolicy is who may set and lift stops in audited mode: the enrolled setters and lifters (by
// approver id), how to verify an id's signatures, and the lift rule. Setting needs one setter's
// signature. Lifting needs LiftQuorum distinct lifters, and, with LiftNeedsOther, at least one who is
// not the setter, so the person who stopped cannot alone resume.
//
// A policy a process loads from its own configuration is its trust root: the control run's policy
// transitions (RotatePolicy) carry later policies, each approved by a quorum of the one before, and
// the fold judges every record under the policy in force at its position, so a rotation never drops a
// stop. A verifier must expose its public key (a PublicKey() []byte method, as audit's verifiers do),
// which is what a transition carries.
type StopPolicy struct {
	// DeploymentID is the deployment's identifier (see StopPolicyDoc); required.
	DeploymentID string
	Setters      []string
	Lifters      []string
	// Verifiers resolves an enrolled id to its verifier (the same verifiers an approval gate uses).
	Verifiers ApproverVerifierFor
	// LiftQuorum is how many distinct lifters a lift needs (at least 1; the recommended value is 2).
	LiftQuorum int
	// LiftNeedsOther requires that one of the lifters is not the stop's setter.
	LiftNeedsOther bool
}

// Validate reports whether the policy is well formed and can be satisfied: a verifier for every
// enrolled id that exposes its public key, one seat per signing key (two ids never share a key
// identity), at least LiftQuorum lifters, and, with LiftNeedsOther, a lifter other than the setter of
// any stop. An unsatisfiable policy is refused when it is loaded, and again whenever the control uses
// it, since a Verifiers function can change after it was given.
func (p StopPolicy) Validate() error {
	_, err := p.doc()
	return err
}

func (p StopPolicy) doc() (StopPolicyDoc, error) {
	if p.Verifiers == nil || len(p.Setters) == 0 || len(p.Lifters) == 0 {
		return StopPolicyDoc{}, fmt.Errorf("stop policy: setters, lifters and verifiers are required: %w", ErrConfig)
	}
	if p.LiftQuorum < 1 {
		return StopPolicyDoc{}, fmt.Errorf("stop policy: the lift quorum is %d, want at least 1: %w", p.LiftQuorum, ErrConfig)
	}
	if err := checkStopText("stop policy deployment ID", p.DeploymentID, maxStopName); err != nil || p.DeploymentID == "" {
		return StopPolicyDoc{}, fmt.Errorf("stop policy: a deployment ID is required (a random value, one per deployment): %w", ErrConfig)
	}
	d := StopPolicyDoc{DeploymentID: p.DeploymentID, Approvers: map[string]StopApprover{}, Setters: uniq(p.Setters), Lifters: uniq(p.Lifters), LiftQuorum: p.LiftQuorum, LiftNeedsOther: p.LiftNeedsOther}
	seen := map[string]string{} // key identity -> approver id
	for _, id := range slices.Concat(d.Setters, d.Lifters) {
		if _, done := d.Approvers[id]; done {
			continue
		}
		if err := checkStopText("stop policy approver id", id, maxStopName); err != nil || id == "" {
			return StopPolicyDoc{}, fmt.Errorf("stop policy: approver id %q: %w", id, ErrConfig)
		}
		v, ok := p.Verifiers(id)
		if !ok || v == nil {
			return StopPolicyDoc{}, fmt.Errorf("stop policy: no verifier for approver %q: %w", id, ErrConfig)
		}
		pk, ok := v.(interface{ PublicKey() []byte })
		if !ok || len(pk.PublicKey()) == 0 {
			return StopPolicyDoc{}, fmt.Errorf("stop policy: the verifier of %q does not expose its public key: %w", id, ErrConfig)
		}
		keys := v.KeyIDs()
		if len(keys) == 0 {
			return StopPolicyDoc{}, fmt.Errorf("stop policy: the verifier of %q reports no key identity: %w", id, ErrConfig)
		}
		for _, k := range keys {
			if k == "" {
				return StopPolicyDoc{}, fmt.Errorf("stop policy: the verifier of %q reports an empty key identity: %w", id, ErrConfig)
			}
			if o, dup := seen[k]; dup && o != id {
				return StopPolicyDoc{}, fmt.Errorf("stop policy: approvers %q and %q share a signing key: %w", o, id, ErrConfig)
			}
			seen[k] = id
		}
		d.Approvers[id] = StopApprover{Alg: v.Alg(), Key: hex.EncodeToString(pk.PublicKey())}
	}
	if len(d.Lifters) < p.LiftQuorum {
		return StopPolicyDoc{}, fmt.Errorf("stop policy: %d lifters enrolled, the quorum is %d, so no stop could be lifted: %w", len(d.Lifters), p.LiftQuorum, ErrConfig)
	}
	if p.LiftNeedsOther && len(d.Lifters) == 1 && slices.Contains(d.Setters, d.Lifters[0]) {
		return StopPolicyDoc{}, fmt.Errorf("stop policy: the only lifter %q is also a setter, so a stop they set could not be lifted by someone else: %w", d.Lifters[0], ErrConfig)
	}
	return d, nil
}

// Doc returns the policy as a transition carries it; it fails as Validate does.
func (p StopPolicy) Doc() (StopPolicyDoc, error) { return p.doc() }

// PolicyFromDoc builds the policy a transition names, with verifiers made by nv, and validates it.
func PolicyFromDoc(d StopPolicyDoc, nv StopVerifierFunc) (StopPolicy, error) {
	if nv == nil {
		return StopPolicy{}, fmt.Errorf("stop policy: no verifier constructor: %w", ErrConfig)
	}
	vs := map[string]ApproverVerifier{}
	for id, a := range d.Approvers {
		pub, err := hex.DecodeString(a.Key)
		if err != nil {
			return StopPolicy{}, fmt.Errorf("stop policy: approver %q key is not hex: %w", id, ErrConfig)
		}
		v, err := nv(a.Alg, pub)
		if err != nil || v == nil {
			return StopPolicy{}, fmt.Errorf("stop policy: approver %q: no verifier for %s key: %w", id, a.Alg, ErrConfig)
		}
		vs[id] = v
	}
	p := StopPolicy{DeploymentID: d.DeploymentID, Setters: d.Setters, Lifters: d.Lifters, LiftQuorum: d.LiftQuorum, LiftNeedsOther: d.LiftNeedsOther,
		Verifiers: func(id string) (ApproverVerifier, bool) { v, ok := vs[id]; return v, ok }}
	for _, id := range slices.Concat(d.Setters, d.Lifters) {
		if _, ok := vs[id]; !ok {
			return StopPolicy{}, fmt.Errorf("stop policy: enrolled approver %q has no key: %w", id, ErrConfig)
		}
	}
	got, err := p.doc()
	if err != nil {
		return StopPolicy{}, err
	}
	if StopPolicyDigest(got) != StopPolicyDigest(d) {
		return StopPolicy{}, fmt.Errorf("stop policy: the document does not match the verifiers built from it: %w", ErrConfig)
	}
	return p, nil
}

func uniq(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// StopPolicyDigest returns the hex SHA-256 of the canonical bytes of a policy document.
func StopPolicyDigest(d StopPolicyDoc) string {
	d.Setters, d.Lifters = uniq(d.Setters), uniq(d.Lifters)
	b, _ := json.Marshal(d) // maps marshal with sorted keys
	sum := sha256.Sum256(append([]byte("bide-stop-policy/2\n"), b...))
	return hex.EncodeToString(sum[:])
}

// Digest returns the digest of the policy's document; "" for a policy that does not validate.
func (p StopPolicy) Digest() string {
	d, err := p.doc()
	if err != nil {
		return ""
	}
	return StopPolicyDigest(d)
}

// verify reports whether r carries a valid signature by an id enrolled in role ("setter" or "lifter").
func (p StopPolicy) verify(r StopRecord, role string) bool {
	if p.Verifiers == nil || !r.Signed() || r.Deployment != p.DeploymentID {
		return false
	}
	ids := p.Setters
	if role == "lifter" {
		ids = p.Lifters
	}
	if !slices.Contains(ids, r.By) {
		return false
	}
	v, ok := p.Verifiers(r.By)
	if !ok || v == nil || v.Alg() != r.Alg {
		return false
	}
	sig, err := hex.DecodeString(r.Signature)
	if err != nil {
		return false
	}
	return v.Verify(StopSigningBytes(r), sig)
}
