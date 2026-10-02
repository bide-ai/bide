package govern

import (
	"encoding/json"
	"fmt"

	gsm "github.com/blackwell-systems/gsm"
)

// ConfluenceCertificate is the machine-checkable evidence that a governed policy converges,
// carried out of gsm's build-time verification and into the SDK so it can be surfaced to a user
// and anchored in the audit trail. gsm.Registry.Build runs the normalization-confluence check
// EXHAUSTIVELY over the enumerated state space (not sampled): WFC proves every compensation chain
// terminates, and CC proves every independent event pair commutes to the same normal form. By
// Newman's lemma those two local facts give global confluence, so applying a set of events in ANY
// order reaches the same valid state. This certificate is that result made portable.
//
// Without it the guarantee lived only inside gsm's build step and was discarded at the Bide
// boundary; a run could prove which policy was used (audit) but not that the policy was provably
// convergent. Anchoring the certificate next to the policy leaf (see audit.RecordConvergence)
// closes that: an auditor verifies not merely "policy digest X was used" but "policy digest X is
// provably convergent, checked over N states".
type ConfluenceCertificate struct {
	Machine      string `json:"machine"`       // name of the gsm machine the certificate is about
	PolicyDigest string `json:"policy_digest"` // stable identifier of the policy this certificate certifies

	// Converges is the headline: WFC && CC. True means gsm's Build certified that every
	// interleaving of events reaches the same normal form, checked exhaustively at build time
	// (gsm v0.11.0 can certify a machine whose event guards or effects read another event's
	// writes when it does not converge; see docs/KNOWN-LIMITATIONS.md).
	Converges bool `json:"converges"`

	// WFC (well-founded compensation): every repair chain terminates; MaxRepairLen is the longest
	// one found. CC (compensation commutativity): independent event pairs commute.
	WFC          bool `json:"wfc"`
	CC           bool `json:"cc"`
	MaxRepairLen int  `json:"max_repair_len"`

	// The CC evidence: how many independent pairs were checked, split into those discharged by
	// footprint-disjointness and those checked by brute force over all valid states.
	PairsTotal    int `json:"pairs_total"`
	PairsDisjoint int `json:"pairs_disjoint"`
	PairsBrute    int `json:"pairs_brute"`

	// States is the size of the state space the check ran over exhaustively.
	States int `json:"states"`

	// CompensationFree marks the CRDT fragment. CRDT.v (sibling proof repo) establishes that
	// CRDTs are exactly the compensation-free fragment of this theory: operations that always
	// preserve every invariant, so normalization never needs to repair. MaxRepairLen == 0 means
	// no state in the enumerated space ever triggered a repair, so the machine sits in that
	// fragment: it is a CRDT, needs no coordinator, and its convergent state can merge with other
	// CRDT infrastructure. A positive MaxRepairLen means the policy uses compensation and is
	// therefore strictly more expressive than any CRDT (the reason to run it here at all). The
	// classification is conservative: MaxRepairLen == 0 is a sound witness of the CRDT fragment;
	// a positive value means compensation is defined and may fire.
	CompensationFree bool `json:"compensation_free"`
}

// CertifyConvergence translates a gsm build Report and the policy's digest into a portable
// certificate. Pass the Report returned by gsm.Registry.Build and the digest from
// Registry.PolicyDigest. A Report whose machine failed to build (WFC or CC false) still produces a
// certificate; it simply reports Converges == false, which a caller should refuse to deploy.
func CertifyConvergence(rep *gsm.Report, policyDigest string) ConfluenceCertificate {
	return ConfluenceCertificate{
		Machine:          rep.Name,
		PolicyDigest:     policyDigest,
		Converges:        rep.WFC && rep.CC,
		WFC:              rep.WFC,
		CC:               rep.CC,
		MaxRepairLen:     rep.MaxRepairLen,
		PairsTotal:       rep.PairsTotal,
		PairsDisjoint:    rep.PairsDisjoint,
		PairsBrute:       rep.PairsBrute,
		States:           rep.StateCount,
		CompensationFree: rep.WFC && rep.MaxRepairLen == 0,
	}
}

// Classification names the fragment the machine lives in, for a human-facing report.
func (c ConfluenceCertificate) Classification() string {
	if c.CompensationFree {
		return "CRDT (compensation-free fragment: no coordinator required)"
	}
	return "governed (compensation-bearing: strictly beyond CRDTs)"
}

// Marshal serializes the certificate for anchoring as an audit leaf (see audit.RecordConvergence).
func (c ConfluenceCertificate) Marshal() ([]byte, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("govern: marshal convergence certificate: %w", err)
	}
	return b, nil
}

// String renders the certificate for a build log or CLI.
func (c ConfluenceCertificate) String() string {
	verdict := "NOT GUARANTEED"
	if c.Converges {
		verdict = "GUARANTEED"
	}
	return fmt.Sprintf(
		"Convergence: %s  [%s]\n  machine: %s  policy: %s\n  WFC: %v (max repair depth %d)  CC: %v (%d pairs: %d disjoint, %d brute)\n  checked exhaustively over %d states",
		verdict, c.Classification(), c.Machine, short(c.PolicyDigest),
		c.WFC, c.MaxRepairLen, c.CC, c.PairsTotal, c.PairsDisjoint, c.PairsBrute, c.States)
}

func short(digest string) string {
	if len(digest) <= 16 {
		return digest
	}
	return digest[:16] + "..."
}
