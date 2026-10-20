package approvecmd

import (
	"bytes"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/bide-ai/bide/internal/safefile"
	"github.com/bide-ai/bide/internal/term"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
	"github.com/bide-ai/bide/internal/policyfile"
)

// A certificate-bound decision (docs/design/approver-identity.md, "bide approve") is signed over the
// bide.approval.v4 bytes, which bind the SHA-256 of the approver's leaf certificate, and travels with the
// chain. -cert names the chain, leaf first, as a PEM file; the key is the one -key names (a key file, or an
// HSM or KMS key for bide-hsm). Everything below is checked before anything is signed, and no failure falls
// back to a bare-key (v3) decision: a certificate-bound approver's bare signature never counts, and a
// decision that looks like it was made cannot be the one that was.

const (
	// maxChainFile bounds what is read from a -cert file: the longest chain a decision may carry
	// (agent.MaxDecisionCertChain certificates of agent.MaxDecisionCertDER bytes each) in PEM, and slack.
	maxChainFile = agent.MaxDecisionCertChain * agent.MaxDecisionCertDER * 2
	// defaultCertMinLeft is the least validity a certificate must have left to be used: a decision made
	// seconds before expiry is provable only after the certificate expired.
	defaultCertMinLeft = 60 * time.Second
)

// certChain is the approver's certificate chain, leaf first.
type certChain struct {
	der  [][]byte
	leaf *x509.Certificate
}

// readChain reads a leaf-first PEM chain from path. The file is bounded, holds CERTIFICATE blocks only (a
// private key block in it is refused by name, never printed, so a key file given by mistake is not read),
// has no data outside the blocks, and holds at most agent.MaxDecisionCertChain certificates, each no
// larger than agent.MaxDecisionCertDER. Every certificate must parse.
func readChain(path string) (*certChain, error) {
	b, err := safefile.Read(path, maxChainFile, nil)
	if err != nil {
		var refused *safefile.RefusedError
		if errors.As(err, &refused) && refused.Kind == safefile.TooLarge {
			return nil, refuse("-cert is larger than %d bytes: it is not a certificate chain", maxChainFile)
		}
		return nil, refuse("-cert: %s", safe(unwrapPath(err).Error()))
	}
	var cc certChain
	rest := b
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			if strings.Contains(blk.Type, "PRIVATE KEY") {
				return nil, refuse("-cert holds a private key block (%s): give the certificate chain only, and keep the private key out of it", safe(blk.Type))
			}
			return nil, refuse("-cert holds a %s block: want CERTIFICATE blocks only, leaf first", safe(blk.Type))
		}
		if len(cc.der) == agent.MaxDecisionCertChain {
			return nil, refuse("-cert holds more than %d certificates: a decision carries at most that many", agent.MaxDecisionCertChain)
		}
		if len(blk.Bytes) > agent.MaxDecisionCertDER {
			return nil, refuse("-cert: certificate %d is larger than %d bytes", len(cc.der)+1, agent.MaxDecisionCertDER)
		}
		cc.der = append(cc.der, bytes.Clone(blk.Bytes))
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, refuse("-cert holds data that is not a PEM certificate block; want the chain only, leaf first")
	}
	if len(cc.der) == 0 {
		return nil, refuse("-cert holds no certificate")
	}
	for i, der := range cc.der {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, refuse("-cert: certificate %d does not parse", i+1)
		}
		if i == 0 {
			cc.leaf = cert
		}
	}
	return &cc, nil
}

// checkKeyIsLeaf refuses a signing key that is not the leaf certificate's key. It compares the key itself
// (scheme and public key bytes), before anything is signed: a signature that would not verify is not the
// check, since a decision signed by the wrong key is a decision made under the wrong identity.
func (cc *certChain) checkKeyIsLeaf(sg audit.Signer) error {
	alg, pub, err := audit.CertDecisionKey(cc.leaf)
	if err != nil {
		return refuse("the certificate's key cannot sign a decision: %s", safe(err.Error()))
	}
	if sg.Alg() != alg || !bytes.Equal(sg.PublicKey(), pub) {
		return refuse("the key you gave (%s) is not the key of the certificate's leaf (%s): sign with the key the certificate was issued for", keyName(sg), audit.KeyID(string(alg), pub))
	}
	return nil
}

// checkWindow refuses a certificate that is not valid at at, or that has less than minLeft of validity
// left.
func (cc *certChain) checkWindow(at time.Time, minLeft time.Duration) error {
	l := cc.leaf
	switch {
	case at.Before(l.NotBefore):
		return refuse("the certificate is not valid yet (from %s, now %s)", l.NotBefore.UTC().Format(time.RFC3339), at.UTC().Format(time.RFC3339))
	case !at.Before(l.NotAfter):
		return refuse("the certificate has expired (at %s, now %s): ask the CA for a new one", l.NotAfter.UTC().Format(time.RFC3339), at.UTC().Format(time.RFC3339))
	case l.NotAfter.Sub(at) < minLeft:
		return refuse("the certificate expires at %s, in %s: less than the %s a decision needs left (-cert-min-left); the decision would become provable only after it expired", l.NotAfter.UTC().Format(time.RFC3339), l.NotAfter.Sub(at).Round(time.Second), minLeft)
	}
	return nil
}

// certDecision is the prepared certificate-bound decision of one invocation: the chain, the policy it is
// checked against, and the identity it will be signed as.
type certDecision struct {
	chain  *certChain
	policy *audit.CertIdentity // nil only under -unverified
}

// loadCert reads and checks everything about the certificate that needs neither the key nor the call: the
// chain file, the policy, the validity now and, with a policy, the chain's acceptance for -as. It runs
// before the key is opened (an HSM login, a billed KMS call), so a bad file costs neither. It returns nil
// when -cert is not given. checkSigner then holds the key against the leaf.
func (c *cmd) loadCert() (*certDecision, error) {
	if c.f.cert == "" {
		return nil, nil
	}
	cc, err := readChain(c.f.cert)
	if err != nil {
		return nil, err
	}
	cd := &certDecision{chain: cc}
	if c.f.identityPolicy != "" {
		if cd.policy, err = policyfile.Read(c.f.identityPolicy); err != nil {
			return nil, refuse("%s", safe(err.Error()))
		}
	}
	if err := cd.check(c.f.as, now(), c.f.certMinLeft); err != nil {
		return nil, err
	}
	return cd, nil
}

// check runs the checks that depend on the time at: the validity window and, with a policy, the chain, the
// identity and the extended key usage as the gate's rule applies them at at (decided = provable = at).
func (cd *certDecision) check(as string, at time.Time, minLeft time.Duration) error {
	if err := cd.chain.checkWindow(at, minLeft); err != nil {
		return err
	}
	if cd.policy == nil {
		return nil
	}
	if _, err := cd.policy.VerifyChain(as, cd.chain.der, at, at); err != nil {
		var ce *audit.CertError
		if errors.As(err, &ce) {
			return refuse("the certificate is not accepted for %s by the policy: %s", safe(as), safe(ce.Detail))
		}
		return refuse("the certificate is not accepted for %s by the policy: %s", safe(as), safe(err.Error()))
	}
	return nil
}

// identity is the canonical identifier the policy derives from the leaf, for display; "" without a policy.
func (cd *certDecision) identity(as string, at time.Time) string {
	if cd.policy == nil {
		return ""
	}
	ac, err := cd.policy.VerifyChain(as, cd.chain.der, at, at)
	if err != nil {
		return ""
	}
	return ac.Identity
}

// sign signs the v4 decision bytes of the call as as, and returns the decision with the chain.
func (cd *certDecision) sign(sg audit.Signer, s agent.ApprovalSubject, run, call, as string, approved bool, at time.Time) (agent.Decision, []byte, error) {
	leafHash := agent.CertLeafHash(cd.chain.der[0])
	msg := agent.ApprovalDecisionBytesCert(s, as, approved, at, leafHash)
	sig, err := sg.Sign(msg)
	if err != nil {
		return agent.Decision{}, nil, fmt.Errorf("sign the decision (nothing was recorded): %w", err)
	}
	return agent.Decision{RunID: run, ToolUseID: call, ApproverID: as, Approved: approved, DecidedAt: at, Alg: sg.Alg(), Signature: sig, Cert: cloneChain(cd.chain.der)}, msg, nil
}

func cloneChain(ch [][]byte) [][]byte {
	out := make([][]byte, len(ch))
	for i := range ch {
		out[i] = bytes.Clone(ch[i])
	}
	return out
}

// verifySigned checks the signature just made under the leaf's own key, over the v4 bytes: a defence in
// depth after checkKeyIsLeaf (a signer whose Sign uses another key than its PublicKey reports) and the
// check that SubmitDecision's own, which does not verify a decision that carries a chain, leaves undone.
func (cd *certDecision) verifySigned(d agent.Decision, msg []byte) error {
	alg, pub, err := audit.CertDecisionKey(cd.chain.leaf)
	if err != nil {
		return err
	}
	v, err := audit.NewVerifier(alg, pub)
	if err != nil {
		return err
	}
	if d.Alg != alg || !v.Verify(msg, d.Signature) {
		return errors.New("the signature does not verify under the certificate's key: the key you gave does not sign for this certificate; nothing was recorded")
	}
	return nil
}

// boundApprovers are the approver ids the journal says are certificate-bound on this gate (the pause
// note's identity fields), and whether it says anything: a gate whose note carries none is read as bare
// keys, which a note written by an older process also looks like.
func (g *gateOf) boundApprovers() (ids []string, known bool) {
	if g.gate.Identity == nil || !g.gate.Identity.Bound {
		return nil, false
	}
	return g.gate.Identity.Approvers, true
}

// refuseBoundWithoutCert refuses a bare-key decision by an approver the journal says is
// certificate-bound: the gate would record it and never count it, since a bare signature is not a
// certificate-bound approver's decision.
func (g *gateOf) refuseBoundWithoutCert(as string) error {
	ids, known := g.boundApprovers()
	if known && slices.Contains(ids, as) {
		return refuse("%s is a certificate-bound approver of this gate: its decision carries your certificate chain and is signed over bide.approval.v4. Give -cert (and -identity-policy); a bare-key decision would be recorded and never counted", safe(as))
	}
	return nil
}

// refuseCertForBare refuses a certificate-bound decision by an approver the journal says is a bare-key
// approver of a gate that has certificate-bound ones: the gate counts a bare-key approver's decision under
// its key only, so a v4 decision would be recorded and never counted.
func (g *gateOf) refuseCertForBare(as string) error {
	ids, known := g.boundApprovers()
	if known && !slices.Contains(ids, as) {
		return refuse("%s is a bare-key approver of this gate (its certificate-bound approvers are: %s): a decision carrying a certificate would be recorded and never counted. Drop -cert", safe(as), safe(strings.Join(ids, ", ")))
	}
	return nil
}

// show prints the certificate rows of the confirmation: what is shown is checked, not the certificate's
// own words. The name the policy derives is the identity the gate would count the decision as; a
// certificate name is the CA's text and is not shown.
func (cd *certDecision) show(row func(k, v string), st term.Style, as string, at time.Time) {
	l := cd.chain.leaf
	row("cert", fmt.Sprintf("%d certificate(s), leaf SHA-256 %s, valid %s to %s", len(cd.chain.der), st.Bold(hex.EncodeToString(agentLeafHash(cd.chain.der[0]))),
		l.NotBefore.UTC().Format(time.RFC3339), l.NotAfter.UTC().Format(time.RFC3339)))
	if id := cd.identity(as, at); id != "" {
		row("identity", fmt.Sprintf("%s %s", st.Bold(safe(id)), st.Dim("(accepted for "+safe(as)+" by the policy now)")))
	} else {
		row("", st.BoldFail("UNVERIFIED")+" "+st.Warn("the chain is not checked against a policy (-unverified): if the gate does not accept it, the decision is recorded and never counts"))
	}
}

func agentLeafHash(der []byte) []byte {
	h := agent.CertLeafHash(der)
	return h[:]
}

// localIntake is the write-time check of a certificate-bound decision that bide approve makes itself,
// in the form SubmitDecision's check takes (agent.CertIntakeVerifier): the record carries the chain that
// was prepared, the policy (when there is one) accepts it for the approver now, and the signature verifies
// over the v4 bytes under the leaf's key. It judges no revocation status: the command has no CRLs, and the
// gate decides that when it counts.
type localIntake struct {
	cd *certDecision
	as string
}

func (localIntake) Alg() agent.Alg                  { return agent.AlgCertificate }
func (localIntake) Verify(message, sig []byte) bool { return false }
func (l localIntake) KeyIDs() []string              { return []string{"ident:" + l.as} }
func (l localIntake) VerifyCertDecision(cd agent.CertDecision) agent.CertCheck {
	return l.VerifyCertIntake(cd)
}

func (l localIntake) VerifyCertIntake(cd agent.CertDecision) agent.CertCheck {
	r := cd.Record
	if r.Approver() != l.as {
		return agent.CertCheck{Reason: agent.ReasonNotEligible, Detail: "the decision is not by the approver this command decides as"}
	}
	chain := r.ApproverCert()
	if len(chain) != len(l.cd.chain.der) || !slices.EqualFunc(chain, l.cd.chain.der, bytes.Equal) {
		return agent.CertCheck{Reason: agent.ReasonCertChain, Detail: "the decision does not carry the chain this command prepared"}
	}
	at, err := agent.ParseTimestamp(r.DecidedAt())
	if err != nil {
		return agent.CertCheck{Reason: agent.ReasonDecidedAt, Detail: "decided_at: " + err.Error()}
	}
	if l.cd.policy != nil {
		if _, err := l.cd.policy.VerifyChain(l.as, chain, at, now()); err != nil {
			var ce *audit.CertError
			if errors.As(err, &ce) {
				return agent.CertCheck{Reason: ce.Reason, Detail: ce.Detail}
			}
			return agent.CertCheck{Reason: agent.ReasonCertChain, Detail: err.Error()}
		}
	}
	alg, pub, err := audit.CertDecisionKey(l.cd.chain.leaf)
	if err != nil {
		return agent.CertCheck{Reason: agent.ReasonCertUsage, Detail: err.Error()}
	}
	v, err := audit.NewVerifier(alg, pub)
	if err != nil {
		return agent.CertCheck{Reason: agent.ReasonCertUsage, Detail: err.Error()}
	}
	if r.ApproverAlg() != alg || !v.Verify(agent.ApprovalDecisionBytesCert(cd.Subject, r.Approver(), r.Approved, at, agent.CertLeafHash(chain[0])), r.Signature()) {
		return agent.CertCheck{Reason: agent.ReasonBadSig, Detail: "the signature does not verify over the bide.approval.v4 bytes of this call under the certificate's key"}
	}
	return agent.CertCheck{Identity: l.cd.identity(l.as, at), Anchored: now()}
}

// verifyReceipt checks a certificate-bound receipt an endpoint returned against the decision just sent:
// it carries the chain that was sent, and its signature verifies over the v4 bytes of the call under the
// leaf's key.
func (cd *certDecision) verifyReceipt(rc audit.DecisionReceipt, s agent.ApprovalSubject) error {
	if len(rc.Cert) != len(cd.chain.der) || !slices.EqualFunc(rc.Cert, cd.chain.der, bytes.Equal) {
		return errors.New("the receipt does not carry the chain that was sent")
	}
	at, err := agent.ParseTimestamp(rc.DecidedAt)
	if err != nil {
		return fmt.Errorf("the receipt's decided_at: %w", err)
	}
	alg, pub, err := audit.CertDecisionKey(cd.chain.leaf)
	if err != nil {
		return err
	}
	v, err := audit.NewVerifier(alg, pub)
	if err != nil {
		return err
	}
	if rc.Alg != alg || !v.Verify(agent.ApprovalDecisionBytesCert(s, rc.Approver, rc.Approved, at, agent.CertLeafHash(rc.Cert[0])), rc.Signature) {
		return errors.New("the receipt's signature does not verify over the bide.approval.v4 bytes of this call under your certificate's key")
	}
	return nil
}
