package audit

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/bide-ai/bide/agent"
)

// ReceiptIdentity is what VerifyIdentity establishes about a certificate-bound receipt.
type ReceiptIdentity struct {
	Approver string
	// Identifier is the verified canonical identifier of the leaf (the identity the policy maps the
	// approver id to).
	Identifier string
	// DisplayName is the leaf's subject common name, if it has one. It is the CA's label for a
	// person, not part of the identity: it is never compared or counted.
	DisplayName string
	// LeafHash and AnchorKey are the SHA-256 of the leaf's DER and of the trust anchor's
	// SubjectPublicKeyInfo, in hex; Chain is the hash of each certificate of the accepted path, leaf first.
	LeafHash, AnchorKey string
	Chain               []string
	NotBefore, NotAfter time.Time
	// DecidedAt is the approver's signed time; AnchoredAt is A, the time the decision became provable,
	// from TimeSource.
	DecidedAt, AnchoredAt time.Time
	TimeSource            string
	// Revocation is "not checked": VerifyIdentity does not look at revocation status, so the result is
	// never a verdict that the decision counts.
	Revocation string
}

// VerifyIdentity checks a certificate-bound receipt on its own: the anchoring proof holds under
// logKey and fixes A (AnchorTime), and the chain, the identity and the signature are accepted by ci at
// A (VerifyCertificate). It does not check revocation status and it reads no journal, so a nil error
// is the identity check at A, not the gate's verdict. The time is the log signer's clock.
func (r DecisionReceipt) VerifyIdentity(ci *CertIdentity, logKey Verifier) (ReceiptIdentity, error) {
	if ci == nil {
		return ReceiptIdentity{}, fmt.Errorf("audit: receipt: no certificate policy: %w", agent.ErrConfig)
	}
	a, err := r.AnchorTime(logKey)
	if err != nil {
		return ReceiptIdentity{}, err
	}
	if err := r.VerifyCertificate(ci, a); err != nil {
		return ReceiptIdentity{}, err
	}
	decided, err := agent.ParseTimestamp(r.DecidedAt)
	if err != nil {
		return ReceiptIdentity{}, fmt.Errorf("audit: receipt decided_at: %v: %w", err, ErrNotVerified)
	}
	ac, err := ci.VerifyChain(r.Approver, r.Cert, decided, a)
	if err != nil {
		return ReceiptIdentity{}, fmt.Errorf("audit: receipt: %w (%w)", err, ErrNotVerified)
	}
	out := ReceiptIdentity{
		Approver: r.Approver, Identifier: ac.Identity, DisplayName: ac.Leaf.Subject.CommonName,
		LeafHash: hex.EncodeToString(ac.LeafHash[:]), AnchorKey: hex.EncodeToString(ac.Anchor[:]),
		NotBefore: ac.Leaf.NotBefore.UTC(), NotAfter: ac.Leaf.NotAfter.UTC(),
		DecidedAt: decided, AnchoredAt: a, TimeSource: "log-signer clock", Revocation: "not checked",
	}
	for _, c := range ac.Chain {
		h := agent.CertLeafHash(c.Raw)
		out.Chain = append(out.Chain, hex.EncodeToString(h[:]))
	}
	return out, nil
}
