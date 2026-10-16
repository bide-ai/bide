package agent

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/bide-ai/bide/internal/identitygate"
)

// The tests of certificate-bound approvers run with the gate switch on; until the last change of the
// feature turns it on by default, a gate refuses such an approver (TestGateRefusesCertificateBoundApprovers).
func TestMain(m *testing.M) {
	identitygate.Enabled.Store(true)
	os.Exit(m.Run())
}

// Until the last change of the feature turns the switch on, a gate refuses a certificate-bound approver
// at configuration (build and every evaluation): no deployment reaches a half-built gate. The counting
// rule itself (TallyApprovals) is not a gate and is unaffected.
func TestGateRefusesCertificateBoundApprovers(t *testing.T) {
	was := identitygate.Enabled.Load()
	defer identitygate.Enabled.Store(was)
	pol := ApprovalPolicy{Need: 1, Approvers: []string{"a"}}
	v := &fakeCertVerifier{id: "a", check: func(CertDecision) CertCheck { return CertCheck{Identity: "a", KeyHash: [32]byte{1}} }}
	vf := certResolver(map[string]ApproverVerifier{"a": v})
	identitygate.Enabled.Store(false)
	err := pol.ValidateKeys(vf)
	if err == nil || !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "certificate-bound") {
		t.Fatalf("switch off: %v", err)
	}
	if err := gatePolicyError(&pol, vf); !errors.Is(err, ErrConfig) {
		t.Fatalf("gatePolicyError, switch off: %v", err)
	}
	if got, _ := TallyApprovals([]Record{certRec("approval:c1:a:00", "a")}, timesSubject, pol, vf); got.Approved != 0 {
		t.Fatalf("a refused approver counted: %+v", got)
	}
	identitygate.Enabled.Store(true)
	if err := pol.ValidateKeys(vf); err != nil {
		t.Fatalf("switch on: %v", err)
	}
	if got, _ := TallyApprovals([]Record{certRec("approval:c1:a:00", "a")}, timesSubject, pol, vf); got.Approved != 1 {
		t.Fatalf("switch on: %+v", got)
	}
}
