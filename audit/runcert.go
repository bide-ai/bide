package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"

	"github.com/bide-ai/bide/agent"
)

// A RunCertificate is a proof-carrying run: one portable, offline-checkable artifact asserting
// behavioral-property compliance over a WHOLE run, discharged from the audit package's existing
// primitives rather than from new cryptography. It composes what already exists (RFC 6962 inclusion
// proofs, signed tree heads, the policy-used absence commitment, anchored policy and convergence
// leaves) into one per-run statement plus a verifier, so a compliance reviewer checks a single
// bundle against one out-of-band public key instead of assembling the proofs by hand.
//
// v1 asserts these properties, each dischargeable from a committed leaf:
//
//   - only-approved-policies: every policy digest exercised by a governed action in the run is a
//     member of a caller-supplied approved allowlist. This is the COMPLETENESS-bearing property: it
//     commits WHICH policies were used, via the policy-used absence commitment (AbsenceRoot over
//     PolicyUsedKey). The used set is exactly the sorted distinct keys the absence tree commits to,
//     so a verifier recomputes that root from the disclosed set and confirms the signed absence STH
//     commits to it; that closes the "some other policy was quietly used" gap that disclosing only
//     positive leaves would leave open.
//   - policies-convergence-certified: for each used policy digest, an anchored convergence
//     certificate leaf exists in the same signed tree as the policy leaf and links to its digest.
//     The certificate is a producer claim; a rigorous verifier cross-checks it against the external
//     oracle (bide-audit verify-run -checker), exactly as verify-convergence does, so a
//     certificate that overstates convergence is caught outside this package.
//
// What the certificate proves, stated precisely: it proves properties of the GOVERNED, COMMITTED
// boundary of the run: which policies ran, that each is on the approved allowlist, and that each has
// an anchored convergence certificate that (with the oracle) is confirmed convergent. Composing the
// two properties yields "every governed state in the run was produced by an approved,
// oracle-certified-convergent policy," so the enforced invariant held throughout the governed
// boundary. It does NOT prove the model's judgment was correct, that ungoverned side effects were
// appropriate, or the runtime-refinement claim (the replay differential check covers the events this
// run took, not all inputs). The used-policy completeness rests entirely on the absence-root key-set
// commitment described above.
type RunCertificate struct {
	// RunID is the run this certificate is about.
	RunID string `json:"run_id"`

	// Properties names the asserted properties, so a reader sees the scope without decoding the
	// bindings. v1: "only-approved-policies", "policies-convergence-certified".
	Properties []string `json:"properties"`

	// ApprovedPolicies is the caller-supplied allowlist of policy digests the run was permitted to
	// exercise. The certificate binds to it so an auditor sees exactly which allowlist was asserted.
	ApprovedPolicies []string `json:"approved_policies"`

	// UsedPolicies is the sorted, distinct set of policy digests exercised by governed actions in the
	// run, recomputed from the committed leaves (PoliciesUsed). It is disclosed in full because the
	// only-approved-policies property is completeness-bearing: the verifier recomputes the absence
	// root over this set and confirms UsedPolicyAbsence.STH commits to it, so the set cannot omit a
	// policy that was in fact used.
	UsedPolicies []string `json:"used_policies"`

	// UsedPolicyAbsence is the signed commitment to the run's used-policy key set: an STH over
	// AbsenceRoot(records, PolicyUsedKey). Its Size is the number of distinct used policies and its
	// Root is the RFC 6962 root over their sorted keys, so recomputing that root from UsedPolicies and
	// comparing binds the disclosed set to what the run actually committed. See SignAbsenceRoot.
	UsedPolicyAbsence SignedTreeHead `json:"used_policy_absence"`

	// Convergence carries, per used policy digest, the anchored evidence that the policy is certified
	// convergent: the policy-leaf ProofBundle and the convergence-leaf ProofBundle, both bound to the
	// run STH below. The oracle cross-check (does the policy actually converge) is performed by the
	// verifier with a checker, as verify-convergence does; these bundles establish the cryptographic
	// root (anchored, digest-linked, same tree) that the oracle check hangs off.
	Convergence []PolicyConvergence `json:"convergence"`

	// STH is the run's signed tree head: the {Size, Root, Timestamp} commitment the policy and
	// convergence bundles are proven against. It is the same tree the whole run's journal commits.
	STH SignedTreeHead `json:"sth"`
}

// PolicyConvergence bundles the anchored evidence for one used policy: its anchored policy leaf and
// its anchored convergence-certificate leaf, both proven against the run STH. The verifier confirms
// both bundles are authentic and in the same tree, the convergence certificate certifies the policy
// leaf's digest, and the policy leaf's bytes hash to that digest (done in the CLI, independent of
// gsm), then optionally runs the oracle on the disclosed policy bytes.
type PolicyConvergence struct {
	Digest      string      `json:"digest"`      // the used policy digest this evidence is for
	PolicyLeaf  ProofBundle `json:"policy_leaf"` // proof the policy leaf is anchored in the run STH
	Certificate ProofBundle `json:"certificate"` // proof the convergence-certificate leaf is anchored in the same tree
}

// RunCertSpec carries the caller's assertion inputs to CertifyRun. v1 needs only the approved
// allowlist; it is a struct so future properties (authority roots, quorum thresholds) can be added
// without changing the signature.
type RunCertSpec struct {
	// ApprovedPolicies is the allowlist of policy digests the run was permitted to exercise. Every
	// policy the run actually used must be a member, or CertifyRun fails: a certificate cannot be
	// issued for a run that exercised a disallowed policy.
	ApprovedPolicies []string
}

// runCertProperties is the fixed v1 property list a certificate asserts.
var runCertProperties = []string{"only-approved-policies", "policies-convergence-certified"}

// CertifyRun assembles a RunCertificate for runID from its committed leaves, against the run's signed
// tree head sth. It recomputes the used-policy set from the journal (PoliciesUsed), confirms every
// used policy is in spec.ApprovedPolicies (refusing to certify a run that used a disallowed policy),
// signs the used-policy absence commitment with priv, and for each used policy assembles the anchored
// policy-leaf and convergence-leaf proof bundles against sth. It fails if a used policy has no
// anchored policy leaf or no anchored convergence leaf, so the certificate can only be issued for a
// run whose governed policies are fully anchored and certified.
//
// priv is the same signing key that produced sth (the log's tree-head key): the used-policy absence
// STH is a second commitment over the SAME run, signed the same way, so a verifier checks both under
// one out-of-band public key. timestamp stamps the absence STH.
func CertifyRun(ctx context.Context, store agent.Durable, runID string, sth SignedTreeHead, spec RunCertSpec, priv ed25519.PrivateKey, timestamp int64) (RunCertificate, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return RunCertificate{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}

	used := PoliciesUsed(recs)
	approved := make(map[string]struct{}, len(spec.ApprovedPolicies))
	for _, d := range spec.ApprovedPolicies {
		approved[d] = struct{}{}
	}
	for _, d := range used {
		if _, ok := approved[d]; !ok {
			return RunCertificate{}, fmt.Errorf("audit: cannot certify run %s: policy %q was used but is not in the approved set", runID, d)
		}
	}

	// The used-policy set is the key set of the absence commitment. Sign that commitment so the
	// disclosed UsedPolicies can be bound to what the run actually committed (completeness).
	absSTH := SignAbsenceRoot(recs, PolicyUsedKey, priv, timestamp)

	conv := make([]PolicyConvergence, 0, len(used))
	for _, d := range used {
		pb, err := ProvePolicy(ctx, store, runID, d, sth)
		if err != nil {
			return RunCertificate{}, fmt.Errorf("audit: certify run %s: %w", runID, err)
		}
		cb, err := ProveConvergence(ctx, store, runID, d, sth)
		if err != nil {
			return RunCertificate{}, fmt.Errorf("audit: certify run %s: %w", runID, err)
		}
		conv = append(conv, PolicyConvergence{Digest: d, PolicyLeaf: pb, Certificate: cb})
	}

	return RunCertificate{
		RunID:             runID,
		Properties:        append([]string(nil), runCertProperties...),
		ApprovedPolicies:  append([]string(nil), spec.ApprovedPolicies...),
		UsedPolicies:      used,
		UsedPolicyAbsence: absSTH,
		Convergence:       conv,
		STH:               sth,
	}, nil
}

// runCertLeafName is the reserved journal name for a run certificate leaf, keyed by runID so the
// certificate a run makes about itself is anchored in the same tree as the actions it summarizes.
func runCertLeafName(runID string) string { return "audit:runcert:" + runID }

// RecordRunCertificate anchors a RunCertificate as a dedicated journal leaf (idempotent per runID),
// so the certificate is itself covered by a signed tree head and inclusion proofs, mirroring
// RecordPolicy / RecordConvergence. Anchoring the certificate lets a later verifier prove the
// certificate was committed in the run, not produced after the fact out of band.
//
// Note the ordering: the certificate commits to a run STH taken at some size; anchoring it grows the
// journal past that size. That is intended: the run STH inside the certificate is the tree the
// policy/convergence bundles are proven against, and a fresh STH taken after RecordRunCertificate
// commits the certificate leaf itself (ProveRunCertificate proves it against that later STH).
func RecordRunCertificate(ctx context.Context, store agent.Durable, runID string, cert RunCertificate) (agent.Record, error) {
	content, err := json.Marshal(cert)
	if err != nil {
		return agent.Record{}, fmt.Errorf("audit: marshal run certificate: %w", err)
	}
	return store.Do(ctx, runID, runCertLeafName(runID), func(context.Context) (agent.Record, error) {
		return agent.Record{Kind: agent.StepValue, Result: content}, nil
	})
}

// ProveRunCertificate builds a ProofBundle proving the run certificate leaf for runID was committed
// in the tree sth signs. Because RecordRunCertificate grows the journal, sth here is a tree head
// taken AFTER the certificate was anchored (later than the run STH embedded in the certificate).
func ProveRunCertificate(ctx context.Context, store agent.Durable, runID string, sth SignedTreeHead) (ProofBundle, error) {
	recs, err := store.History(ctx, runID)
	if err != nil {
		return ProofBundle{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	name := runCertLeafName(runID)
	idx := -1
	for i, r := range recs {
		if r.Kind == agent.StepValue && r.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ProofBundle{}, fmt.Errorf("audit: no run certificate leaf for run %s", runID)
	}
	return ProveRecord(ctx, store, runID, idx, sth)
}

// RunVerification is the structured outcome of VerifyRun: the overall verdict plus per-property
// results, so a caller (or the CLI) can report exactly which property held and which failed.
type RunVerification struct {
	// OK is the overall verdict: true only when every asserted property held.
	OK bool
	// OnlyApprovedPolicies is true when every used policy is in the allowlist AND the disclosed
	// used-policy set is bound to the run's absence commitment (completeness).
	OnlyApprovedPolicies bool
	// ConvergenceCertified is true when every used policy has an anchored policy leaf and convergence
	// leaf, authentic and in the same signed tree, with linking digests. It does NOT include the
	// oracle cross-check, which the caller performs separately (the CLI does, with -checker).
	ConvergenceCertified bool
	// Reasons carries human-readable detail for any property that failed (empty on full success).
	Reasons []string
}

// VerifyRun checks a RunCertificate's claims against the disclosed proofs and the out-of-band public
// key pub, entirely offline. It does NOT trust the certificate; it re-derives each property from the
// bundles it carries:
//
//  1. only-approved-policies (completeness-bearing): the used-policy absence STH is authentic under
//     pub, its Root equals the RFC 6962 root recomputed from the disclosed UsedPolicies key set (so
//     the disclosed set IS what the run committed, not a set the producer chose), and every used
//     policy is a member of ApprovedPolicies. Recomputing the absence root is what gives the negative
//     teeth: a policy that was actually used cannot be dropped from UsedPolicies without changing the
//     root and breaking the signature check.
//  2. policies-convergence-certified: the run STH is authentic under pub; and for every used policy
//     there is a Convergence entry whose policy-leaf and convergence-leaf bundles both verify under
//     pub, are in the same tree as the run STH, and whose leaves link by digest (the policy leaf's
//     PolicyContent.Digest and the convergence leaf's ConvergenceContent.Digest both equal the used
//     digest). The oracle cross-check of the certificate's convergence claim is left to the caller
//     (see the CLI's -checker); VerifyRun establishes the anchored, digest-linked cryptographic root
//     it hangs off.
//
// A false OK with populated Reasons means a well-formed-but-invalid certificate; an error means a
// bundle could not be canonicalized (a malformed artifact).
func VerifyRun(cert RunCertificate, pub ed25519.PublicKey) (RunVerification, error) {
	res := RunVerification{}

	// Property 1: only-approved-policies (completeness-bearing).
	onlyApproved := true
	if !cert.UsedPolicyAbsence.Verify(pub) {
		onlyApproved = false
		res.Reasons = append(res.Reasons, "used-policy absence STH is not authentic under this key")
	}
	if cert.UsedPolicyAbsence.Size != len(cert.UsedPolicies) {
		onlyApproved = false
		res.Reasons = append(res.Reasons, fmt.Sprintf("absence STH size %d does not match %d disclosed used policies", cert.UsedPolicyAbsence.Size, len(cert.UsedPolicies)))
	}
	// Recompute the absence root over the disclosed used set and bind it to the signed root. The keys
	// committed by the absence tree are the PolicyUsedKey-namespaced digests, so we recompute with the
	// same key form the run committed (PolicyUsedKeyFor), sorted, and compare.
	if !bytes.Equal(cert.UsedPolicyAbsence.Root, usedPolicyAbsenceRoot(cert.UsedPolicies)) {
		onlyApproved = false
		res.Reasons = append(res.Reasons, "used-policy set does not match the signed absence root (the disclosed set is not what the run committed)")
	}
	approved := make(map[string]struct{}, len(cert.ApprovedPolicies))
	for _, d := range cert.ApprovedPolicies {
		approved[d] = struct{}{}
	}
	for _, d := range cert.UsedPolicies {
		if _, ok := approved[d]; !ok {
			onlyApproved = false
			res.Reasons = append(res.Reasons, fmt.Sprintf("policy %q was used but is not in the approved set", d))
		}
	}
	res.OnlyApprovedPolicies = onlyApproved

	// Property 2: policies-convergence-certified.
	certified := true
	if !cert.STH.Verify(pub) {
		certified = false
		res.Reasons = append(res.Reasons, "run STH is not authentic under this key")
	}
	byDigest := make(map[string]PolicyConvergence, len(cert.Convergence))
	for _, pc := range cert.Convergence {
		byDigest[pc.Digest] = pc
	}
	for _, d := range cert.UsedPolicies {
		pc, ok := byDigest[d]
		if !ok {
			certified = false
			res.Reasons = append(res.Reasons, fmt.Sprintf("no convergence evidence for used policy %q", d))
			continue
		}
		ok, reason, err := verifyPolicyConvergence(pc, cert.STH, pub)
		if err != nil {
			return RunVerification{}, err
		}
		if !ok {
			certified = false
			res.Reasons = append(res.Reasons, fmt.Sprintf("policy %q: %s", d, reason))
		}
	}
	res.ConvergenceCertified = certified

	res.OK = res.OnlyApprovedPolicies && res.ConvergenceCertified
	return res, nil
}

// verifyPolicyConvergence checks one policy's anchored evidence against the run STH and pub: both
// bundles authentic, both in the same tree as the run STH, and the leaves link to the used digest.
// The oracle cross-check is intentionally not done here (it needs an external binary); the CLI adds
// it. Returns (ok, reason-if-not-ok, error-if-malformed).
func verifyPolicyConvergence(pc PolicyConvergence, runSTH SignedTreeHead, pub ed25519.PublicKey) (bool, string, error) {
	okP, err := pc.PolicyLeaf.Verify(pub)
	if err != nil {
		return false, "", err
	}
	if !okP {
		return false, "policy-leaf bundle did not verify under this key", nil
	}
	okC, err := pc.Certificate.Verify(pub)
	if err != nil {
		return false, "", err
	}
	if !okC {
		return false, "convergence-leaf bundle did not verify under this key", nil
	}
	if !sameTree(pc.PolicyLeaf.STH, runSTH) || !sameTree(pc.Certificate.STH, runSTH) {
		return false, "policy or convergence leaf is not in the same signed tree as the run STH", nil
	}
	var polC PolicyContent
	if err := json.Unmarshal(pc.PolicyLeaf.Record.Result, &polC); err != nil {
		return false, "", fmt.Errorf("audit: policy leaf is not a policy content leaf: %w", err)
	}
	var convC ConvergenceContent
	if err := json.Unmarshal(pc.Certificate.Record.Result, &convC); err != nil {
		return false, "", fmt.Errorf("audit: convergence leaf is not a convergence content leaf: %w", err)
	}
	if polC.Digest != pc.Digest {
		return false, fmt.Sprintf("policy leaf digest %q does not match", polC.Digest), nil
	}
	if convC.Digest != pc.Digest {
		return false, fmt.Sprintf("convergence certificate digest %q does not match", convC.Digest), nil
	}
	return true, "", nil
}

// sameTree reports whether two signed tree heads commit to the same tree (same size and root).
func sameTree(a, b SignedTreeHead) bool {
	return a.Size == b.Size && bytes.Equal(a.Root, b.Root)
}

// usedPolicyAbsenceRoot recomputes the RFC 6962 absence root over the sorted, distinct used-policy
// keys, using the same PolicyUsedKeyFor key form the run committed. It mirrors what
// AbsenceRoot(records, PolicyUsedKey) computes, but from the disclosed digest set alone, so a
// verifier holding only the certificate can bind the set to the signed root.
func usedPolicyAbsenceRoot(usedDigests []string) []byte {
	keys := make([]string, len(usedDigests))
	for i, d := range usedDigests {
		keys[i] = PolicyUsedKeyFor(d)
	}
	// UsedPolicies is already sorted-distinct (PoliciesUsed), and PolicyUsedKeyFor is a stable prefix,
	// so sort order is preserved; keyLeaves + merkleRoot reproduce AbsenceRoot exactly.
	return merkleRoot(keyLeaves(keys))
}
