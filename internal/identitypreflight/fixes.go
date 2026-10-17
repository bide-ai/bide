package identitypreflight

import "github.com/bide-ai/bide/agent"

// chainFix is what to do about a chain the gate would refuse, by the gate's reason; the precise cause is in
// the Detail the gate gives, which the check prints beside it.
func chainFix(reason string) string {
	switch reason {
	case agent.ReasonNoCert:
		return "give -cert the approver's chain as PEM, leaf first"
	case agent.ReasonCertChain:
		return "the file bide approve -cert signs with must carry the leaf and every pinned intermediate, leaf first, and the chain must end at an anchor of the policy; compare the issuer of each certificate with the policy's anchors and intermediates"
	case agent.ReasonCertIdentity:
		return "the leaf must carry exactly one subject alternative name of the policy's id_kind, equal to the identifier the policy's identity map gives this approver id; reissue the certificate, or correct the identity map through the bank's change process"
	case agent.ReasonCertUsage:
		return "compare the leaf with the policy's eku and policy_oid, its key usage (digitalSignature, optionally contentCommitment) and its key type (Ed25519, ECDSA P-256 or ML-DSA-65); reissue from a certificate template that sets them"
	case agent.ReasonCertWindow:
		return "the certificate, or a CA above it, is outside its validity at the check time, or the leaf lives longer than max_leaf_lifetime plus not_before_backdate: renew it, or check the clock of this host"
	}
	return "the gate would refuse this chain; the detail names why"
}

// statusFix is what to do about a status verdict the gate would refuse.
func statusFix(reason string) string {
	switch reason {
	case agent.ReasonRevoked:
		return "a certificate of the chain is revoked (or refused by the status policy): the approver needs a new certificate; if the revocation is a mistake, the CA must lift it and publish a new CRL"
	case agent.ReasonStatusStale:
		return "the newest CRL is older than the declared ages allow: the CA must publish within its declared period and the fetch job must be running; change a declared age only when your CA's real period justifies it"
	case agent.ReasonStatusUnknown:
		return "no usable CRL covers a certificate of the chain: fetch the CRLs for every issuer in the chain (see the crl.* lines), and declare ExecStatusAgeFor for every CA above the leaf's issuer"
	}
	return "the gate would refuse on status; the detail names why"
}
