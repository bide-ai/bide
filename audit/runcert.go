package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"slices"

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
//     member of the AUDITOR's approved allowlist (passed to VerifyRun; the certificate carries no
//     allowlist of its own). This is the COMPLETENESS-bearing property: it commits WHICH policies
//     were used, via the policy-used absence commitment (a PolicyUsedKeys tree head). The used set is
//     exactly the sorted distinct keys that tree commits to, so a verifier recomputes its root from
//     the disclosed set and confirms the signed head commits to it; that closes the "some other
//     policy was quietly used" gap that disclosing only positive leaves would leave open.
//   - policies-convergence-certified: for each used policy digest, an anchored convergence
//     certificate leaf exists in the same signed tree as the policy leaf and links to its digest.
//     The certificate is a producer claim; a rigorous verifier cross-checks it against the external
//     oracle (bide-audit verify-run -checker), exactly as verify-convergence does, so a
//     certificate that overstates convergence is caught outside this package.
//
// Binding. The used-policy head is bound to this run and to this journal, not merely signed by
// the right key: its signed encoding commits to Kind TreePolicyUsed, to RunID, and to the journal
// tree (Size and Root) whose records the used set was projected from. VerifyRun requires that
// journal tree to be exactly STH (the certificate's signed journal head, itself a journal head of
// RunID). So a used-policy head from another run, from a different point in this run, or from a
// tool-use key set cannot stand in, and the used set is the projection of the same records the
// convergence bundles are proven against.
//
// What the certificate proves, stated precisely: it proves properties of the GOVERNED, COMMITTED
// boundary of the run up to STH.Size records: which policies ran, that each is on the approved
// allowlist, and that each has an anchored convergence certificate that (with the oracle) is
// confirmed convergent. Composing the two properties yields "every governed state in the run was
// produced by an approved, oracle-certified-convergent policy," so the enforced invariant held
// throughout the governed boundary. It does NOT prove the model's judgment was correct, that
// ungoverned side effects were appropriate, or the runtime-refinement claim (the replay
// differential check covers the events this run took, not all inputs). It covers the journal up to
// STH.Size; that STH is the run's final head is a fact the anchor log supplies, not the certificate.
type RunCertificate struct {
	// Format is RunCertificateFormat.
	Format string `json:"format"`

	// RunID is the run this certificate is about. It must equal STH.RunID and
	// UsedPolicyAbsence.RunID, both signed.
	RunID string `json:"run_id"`

	// Properties names the asserted properties, so a reader sees the scope without decoding the
	// bindings. v1: exactly "only-approved-policies", "policies-convergence-certified".
	Properties []string `json:"properties"`

	// UsedPolicies is the sorted, distinct set of policy digests exercised by governed actions in the
	// run, recomputed from the committed leaves (PoliciesUsed). It is disclosed in full because the
	// only-approved-policies property is completeness-bearing: the verifier recomputes the key-set
	// root over this set and confirms UsedPolicyAbsence commits to it, so the set cannot omit a
	// policy that was in fact used.
	UsedPolicies []string `json:"used_policies"`

	// UsedPolicyAbsence is the signed commitment to the run's used-policy key set: a PolicyUsedKeys
	// tree head (NewAbsenceTreeHead) whose Journal is STH's tree. Its Size is the number of distinct
	// used policies and its Root is the RFC 6962 root over their sorted keys.
	UsedPolicyAbsence SignedTreeHead `json:"used_policy_absence"`

	// Convergence carries, per used policy digest and in UsedPolicies order, the anchored evidence
	// that the policy is certified convergent: the policy-leaf ProofBundle and the convergence-leaf
	// ProofBundle, both bound to the run STH below. The oracle cross-check (does the policy actually
	// converge) is performed by the verifier with a checker, as verify-convergence does; these
	// bundles establish the cryptographic root (anchored, digest-linked, same tree) that the oracle
	// check hangs off.
	Convergence []PolicyConvergence `json:"convergence"`

	// STH is the run's signed journal head: the commitment the policy and convergence bundles are
	// proven against and the journal tree the used-policy set was projected from.
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
	// issued for a run that exercised a disallowed policy. It is not written into the certificate:
	// the verifier supplies its own allowlist.
	ApprovedPolicies []string
}

// runCertProperties is the fixed v1 property list a certificate asserts.
var runCertProperties = []string{"only-approved-policies", "policies-convergence-certified"}

// CertifyRun assembles a RunCertificate for runID from its committed leaves, against the run's signed
// journal head sth. It confirms sth is a journal head of runID that matches the journal, recomputes
// the used-policy set from exactly the sth.Size records sth commits to (PoliciesUsed), confirms every
// used policy is in spec.ApprovedPolicies (refusing to certify a run that used a disallowed policy),
// signs the used-policy key-set head bound to sth's tree with priv, and for each used policy
// assembles the anchored policy-leaf and convergence-leaf proof bundles against sth. It fails if a
// used policy has no anchored policy leaf or no anchored convergence leaf, so the certificate can
// only be issued for a run whose governed policies are fully anchored and certified.
//
// priv is the same signing key that produced sth (the log's tree-head key): the used-policy head is
// a second commitment over the SAME run, signed the same way, so a verifier checks both under one
// out-of-band public key. timestamp stamps the used-policy head.
func CertifyRun(ctx context.Context, store agent.Durable, runID string, sth SignedTreeHead, spec RunCertSpec, priv ed25519.PrivateKey, timestamp int64) (RunCertificate, error) {
	all, err := store.History(ctx, runID)
	if err != nil {
		return RunCertificate{}, fmt.Errorf("audit: load journal %s: %w", runID, err)
	}
	recs, err := journalPrefix(runID, all, sth.TreeHead)
	if err != nil {
		return RunCertificate{}, fmt.Errorf("audit: certify run %s: %w", runID, err)
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

	// The used-policy set is the key set of the absence commitment. Sign that commitment, bound to
	// sth's journal tree, so the disclosed UsedPolicies can be bound to what the run committed.
	absSTH, err := SignAbsenceRoot(recs, PolicyUsedKeys, sth.TreeHead, priv, timestamp)
	if err != nil {
		return RunCertificate{}, fmt.Errorf("audit: certify run %s: %w", runID, err)
	}

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
		Format:            RunCertificateFormat,
		RunID:             runID,
		Properties:        append([]string(nil), runCertProperties...),
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
	OK bool `json:"ok"`
	// OnlyApprovedPolicies is true when every used policy is in the allowlist AND the disclosed
	// used-policy set is bound to this run's journal by the signed used-policy head (completeness).
	OnlyApprovedPolicies bool `json:"only_approved_policies"`
	// ConvergenceCertified is true when every used policy has an anchored policy leaf and convergence
	// leaf, authentic and in the same signed tree, with linking digests. It does NOT include the
	// oracle cross-check, which the caller performs separately (the CLI does, with -checker).
	ConvergenceCertified bool `json:"convergence_certified"`
	// Reasons carries human-readable detail for any property that failed (empty on full success).
	Reasons []string `json:"reasons,omitempty"`
	// Policies carries, when ConvergenceCertified holds, what the verification read from each used
	// policy's leaves, in UsedPolicies order, so a caller that cross-checks a policy (the CLI's
	// -checker runs the external oracle on it) checks exactly the bytes that were verified.
	Policies []VerifiedPolicy `json:"policies,omitempty"`
}

// VerifiedPolicy is one used policy as VerifyRun read it from its anchored leaves: the policy
// bytes from the policy leaf and the serialized convergence certificate from the convergence leaf.
type VerifiedPolicy struct {
	Digest      string          `json:"digest"`
	Policy      string          `json:"policy"`
	Certificate json.RawMessage `json:"certificate"`
}

// VerifyRun checks a RunCertificate's claims against the disclosed proofs, the auditor's own
// approved allowlist, and the out-of-band public key pub, entirely offline. It does NOT trust the
// certificate; it re-derives each property from the bundles it carries:
//
//  0. the certificate itself: Properties is exactly the v1 list, and STH is an authentic journal
//     head of RunID.
//  1. only-approved-policies (completeness-bearing): the used-policy head is authentic under pub, is
//     a TreePolicyUsed head of RunID whose source journal is exactly STH's tree, and its Root equals
//     the RFC 6962 root recomputed from the disclosed UsedPolicies (in the sorted, distinct order the
//     tree commits; any other order, a duplicate, or a missing or extra digest changes the root), so
//     the disclosed set IS what this run's journal committed; and every used policy is
//     a member of approved. A policy that was actually used cannot be dropped from UsedPolicies
//     without changing the root, and a head for another run or journal cannot be swapped in.
//  2. policies-convergence-certified: for every used policy there is exactly one Convergence entry,
//     in UsedPolicies order and none for an unused digest, whose policy-leaf and convergence-leaf
//     bundles both verify under pub, are in the same tree as the run STH, and whose leaves link by
//     digest. The leaves are read with UnmarshalStrict, so a leaf that reads differently to a person
//     than to encoding/json does not verify. The oracle cross-check of the certificate's convergence
//     claim is left to the caller (see the CLI's -checker), on the contents returned in Policies.
//
// A false OK with populated Reasons means a well-formed-but-invalid certificate; an error means a
// bundle could not be canonicalized or a leaf could not be read (a malformed artifact), or the
// certificate or one of its bundles is not of the format this version reads (ErrFormat). It does
// not check the heads' timestamps; EvidencePackage.Verify and bide-audit verify-run do (see
// CheckTimestamp).
func VerifyRun(cert RunCertificate, approved []string, pub ed25519.PublicKey) (RunVerification, error) {
	res := RunVerification{}
	if err := formatOf(cert, cert.Format); err != nil {
		return res, err
	}
	fail := func(ok *bool, format string, args ...any) {
		*ok = false
		res.Reasons = append(res.Reasons, fmt.Sprintf(format, args...))
	}

	// The run STH anchors both properties.
	runSTH := true
	if !slices.Equal(cert.Properties, runCertProperties) {
		fail(&runSTH, "properties %q are not the v1 set %q", cert.Properties, runCertProperties)
	}
	if !cert.STH.Verify(pub) {
		fail(&runSTH, "run STH is not authentic under this key")
	}
	if cert.STH.Kind != TreeJournal || cert.STH.RunID != cert.RunID {
		fail(&runSTH, "run STH is a %q head of run %q, not the journal of run %q", cert.STH.Kind, cert.STH.RunID, cert.RunID)
	}

	// Property 1: only-approved-policies (completeness-bearing).
	onlyApproved := runSTH
	abs := cert.UsedPolicyAbsence
	if !abs.Verify(pub) {
		fail(&onlyApproved, "used-policy head is not authentic under this key")
	}
	if abs.Kind != TreePolicyUsed || abs.RunID != cert.RunID {
		fail(&onlyApproved, "used-policy head is a %q head of run %q, not the used-policy set of run %q", abs.Kind, abs.RunID, cert.RunID)
	}
	if abs.Journal == nil || abs.Journal.Size != cert.STH.Size || !bytes.Equal(abs.Journal.Root, cert.STH.Root) {
		fail(&onlyApproved, "used-policy head was not projected from the certificate's journal tree")
	}
	if !bytes.Equal(abs.Root, usedPolicyAbsenceRoot(cert.UsedPolicies)) {
		fail(&onlyApproved, "used-policy set does not match the signed root (the disclosed set is not what the run committed)")
	}
	allow := make(map[string]struct{}, len(approved))
	for _, d := range approved {
		allow[d] = struct{}{}
	}
	for _, d := range cert.UsedPolicies {
		if _, ok := allow[d]; !ok {
			fail(&onlyApproved, "policy %q was used but is not in the approved set", d)
		}
	}
	res.OnlyApprovedPolicies = onlyApproved

	// Property 2: policies-convergence-certified.
	certified := runSTH
	if len(cert.Convergence) != len(cert.UsedPolicies) {
		fail(&certified, "%d convergence entries for %d used policies", len(cert.Convergence), len(cert.UsedPolicies))
	}
	var policies []VerifiedPolicy
	for i, d := range cert.UsedPolicies {
		if i >= len(cert.Convergence) || cert.Convergence[i].Digest != d {
			fail(&certified, "no convergence evidence for used policy %q", d)
			continue
		}
		vp, ok, reason, err := verifyPolicyConvergence(cert.Convergence[i], cert.STH, pub)
		if err != nil {
			return RunVerification{}, err
		}
		if !ok {
			fail(&certified, "policy %q: %s", d, reason)
		}
		policies = append(policies, vp)
	}
	res.ConvergenceCertified = certified
	if certified {
		res.Policies = policies
	}

	res.OK = res.OnlyApprovedPolicies && res.ConvergenceCertified
	return res, nil
}

// verifyPolicyConvergence checks one policy's anchored evidence against the run STH and pub: both
// bundles authentic, both in the same tree as the run STH, and the leaves link to the used digest.
// The oracle cross-check is intentionally not done here (it needs an external binary); the CLI adds
// it. The leaves are read with UnmarshalStrict, so what is verified is what the leaf shows a reader.
// Returns (what was read, ok, reason-if-not-ok, error-if-malformed).
func verifyPolicyConvergence(pc PolicyConvergence, runSTH SignedTreeHead, pub ed25519.PublicKey) (VerifiedPolicy, bool, string, error) {
	okP, err := pc.PolicyLeaf.Verify(pub)
	if err != nil {
		return VerifiedPolicy{}, false, "", err
	}
	if !okP {
		return VerifiedPolicy{}, false, "policy-leaf bundle did not verify under this key", nil
	}
	okC, err := pc.Certificate.Verify(pub)
	if err != nil {
		return VerifiedPolicy{}, false, "", err
	}
	if !okC {
		return VerifiedPolicy{}, false, "convergence-leaf bundle did not verify under this key", nil
	}
	if !pc.PolicyLeaf.STH.SameTree(runSTH.TreeHead) || !pc.Certificate.STH.SameTree(runSTH.TreeHead) {
		return VerifiedPolicy{}, false, "policy or convergence leaf is not in the same signed tree as the run STH", nil
	}
	if pl := pc.PolicyLeaf.Record; pl.Kind != agent.StepValue || pl.Name != policyLeafName(pc.Digest) {
		return VerifiedPolicy{}, false, fmt.Sprintf("policy bundle proves record %q, not the policy leaf for %q", pl.Name, pc.Digest), nil
	}
	if cl := pc.Certificate.Record; cl.Kind != agent.StepValue || cl.Name != convergenceLeafName(pc.Digest) {
		return VerifiedPolicy{}, false, fmt.Sprintf("convergence bundle proves record %q, not the convergence leaf for %q", cl.Name, pc.Digest), nil
	}
	var polC PolicyContent
	if err := UnmarshalStrict(pc.PolicyLeaf.Record.Result, &polC); err != nil {
		return VerifiedPolicy{}, false, "", fmt.Errorf("audit: policy leaf is not a policy content leaf: %w", err)
	}
	var convC ConvergenceContent
	if err := UnmarshalStrict(pc.Certificate.Record.Result, &convC); err != nil {
		return VerifiedPolicy{}, false, "", fmt.Errorf("audit: convergence leaf is not a convergence content leaf: %w", err)
	}
	if polC.Digest != pc.Digest {
		return VerifiedPolicy{}, false, fmt.Sprintf("policy leaf digest %q does not match", polC.Digest), nil
	}
	if convC.Digest != pc.Digest {
		return VerifiedPolicy{}, false, fmt.Sprintf("convergence certificate digest %q does not match", convC.Digest), nil
	}
	return VerifiedPolicy{Digest: pc.Digest, Policy: polC.Policy, Certificate: convC.Certificate}, true, "", nil
}

// usedPolicyAbsenceRoot recomputes the RFC 6962 key-set root over the used-policy keys, using the
// same PolicyUsedKeyFor key form the run committed. It mirrors what AbsenceRoot(records,
// PolicyUsedKeys) computes, but from the disclosed digest list alone, in the order given: it equals
// the signed root only for the sorted, distinct list the run committed (PolicyUsedKeyFor is a fixed
// prefix, so key order is digest order). A verifier holding only the certificate binds the list to
// the signed root with it.
func usedPolicyAbsenceRoot(usedDigests []string) []byte {
	keys := make([]string, len(usedDigests))
	for i, d := range usedDigests {
		keys[i] = PolicyUsedKeyFor(d)
	}
	return merkleRoot(keyLeaves(keys))
}
