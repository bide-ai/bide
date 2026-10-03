package govern_test

import (
	"testing"

	"github.com/blackwell-systems/gsm"

	"github.com/bide-ai/bide/govern"
)

// Since gsm v0.12.0, Build can pass its own WFC and CC checks and still return no
// machine: the verified table oracle it runs in-process did not certify the tables
// (Report.OracleDisagreement). Such a machine is not certified, so its certificate
// must not say it converges.
func TestCertifyConvergence_OracleDisagreementDoesNotConverge(t *testing.T) {
	rep := &gsm.Report{Name: "m", StateCount: 4, WFC: true, CC: true,
		OracleDisagreement: "gsm: the verified table oracle rejects the machine's tables", Assurance: gsm.AssuranceNone}
	if c := govern.CertifyConvergence(rep, "d"); c.Converges {
		t.Fatalf("certificate says Converges for a machine the oracle gate refused: %+v", c)
	}
}

// A machine Build returned (Assurance set) converges.
func TestCertifyConvergence_CertifiedMachineConverges(t *testing.T) {
	r := gsm.NewRegistry("ok")
	b := r.Bool("b")
	r.On("set").Does(gsm.Raise(b)).Add()
	_, rep, err := r.Build()
	if err != nil {
		t.Fatal(err)
	}
	if rep.Assurance == gsm.AssuranceNone {
		t.Fatalf("Build returned a machine with Assurance none: %+v", rep)
	}
	if c := govern.CertifyConvergence(rep, "d"); !c.Converges {
		t.Fatalf("certificate does not say Converges for a certified machine: %+v", c)
	}
}
