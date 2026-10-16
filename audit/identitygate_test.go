package audit_test

import (
	"os"
	"testing"

	"github.com/bide-ai/bide/internal/identitygate"
)

// The tests of certificate-bound gates run with the gate switch on; until the last change of the
// feature turns it on by default, a gate refuses such an approver (agent TestGateRefusesCertificateBoundApprovers).
func TestMain(m *testing.M) {
	identitygate.Enabled.Store(true)
	os.Exit(m.Run())
}
