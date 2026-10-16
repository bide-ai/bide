package audit

import (
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/bide-ai/bide/agent"
)

// execTiers prepares p for the execution-time status check of ac and returns the function that gives each
// certificate of the chain the age of its tier: the certificate at position 0 (issued by the leaf's
// issuing CA) is judged by ExecStatusAge, every later one by the ExecStatusAgeFor of its issuer. A
// per-issuer MaxStatusAgeFor does not apply (the execution ages replace the counting ones). reason is
// non-empty when the check cannot be made: no ExecStatusAge, or a CA tier with no ExecStatusAgeFor.
func (c *CertApprover) execTiers(ac *ApproverCert, p *StatusParams) (perCert func(int, *x509.Certificate, StatusParams) StatusParams, reason, detail string) {
	if c.ExecStatusAge <= 0 {
		return nil, agent.ReasonStatusUnknown, "the verifier declares no ExecStatusAge, so status cannot be judged fresh enough to execute"
	}
	for i := 1; i+1 < len(ac.Chain); i++ {
		k := SPKIHashOf(ac.Chain[i+1])
		if c.ExecStatusAgeFor[k] <= 0 {
			return nil, agent.ReasonStatusUnknown, fmt.Sprintf("the verifier declares no ExecStatusAgeFor for the CA with key %s at position %d above the leaf's issuer, so its status cannot be judged fresh enough to execute", hex.EncodeToString(k[:4]), i)
		}
	}
	if p.Now.IsZero() {
		p.Now = time.Now()
	}
	p.CountLive = true
	return func(i int, issuer *x509.Certificate, q StatusParams) StatusParams {
		q.MaxStatusAgeFor = nil
		if i == 0 {
			q.MaxStatusAge = c.ExecStatusAge
		} else {
			q.MaxStatusAge = c.ExecStatusAgeFor[SPKIHashOf(issuer)]
		}
		return q
	}, "", ""
}

// ExecLatencyBound is the published revocation latency of the execution-time re-check for one CA tier: the
// longest time from the CA recording a revocation to a gate refusing to execute on it, publishDelay (the
// time the CA takes to publish a CRL after the revocation; AD CS stamps a revocation at publication, so
// publish right after revoking) plus period (the CRL or delta period) plus overlap plus execAge. It is a
// bound on when the gate can know, not on the instant between the check and the call. With a daily CRL the
// number is a day; for the offline root's CRL it is the ceremony delay and the root's period, days to a
// week: it is not hidden, and never to be read as minutes.
func ExecLatencyBound(publishDelay, period, overlap, execAge time.Duration) time.Duration {
	return publishDelay + period + overlap + execAge
}
