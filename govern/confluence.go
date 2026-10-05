package govern

import (
	"encoding/json"
	"fmt"
	"strings"

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

	// Converges is the headline: gsm's Build returned a machine (WFC && CC, and the verified
	// table oracle gsm runs in-process certified its tables). True means every order of a set of
	// events, each delivered once, reaches the same normal form, checked exhaustively at build
	// time, under the obligations below: CausalOrderRequired and NotIdempotent name what the
	// delivery of events must guarantee. Every pair CC checked commutes; with pairs declared
	// (PairsUndeclared > 0), the undeclared pairs that do not commute are in CausalOrderRequired.
	Converges bool `json:"converges"`

	// WFC (well-founded compensation): every repair chain terminates; MaxRepairLen is the longest
	// one found. CC (compensation commutativity): independent event pairs commute.
	WFC          bool `json:"wfc"`
	CC           bool `json:"cc"`
	MaxRepairLen int  `json:"max_repair_len"`

	// The CC evidence: how many independent pairs were checked, split into those discharged by
	// footprint-disjointness and those checked by brute force over all valid states.
	// PairsDisjoint is nonzero only in a BuildCompositional report; it is always 0 for Build
	// since gsm v0.12.0, and kept so certificates keep one format.
	PairsTotal    int `json:"pairs_total"`
	PairsDisjoint int `json:"pairs_disjoint"`
	PairsBrute    int `json:"pairs_brute"`

	// PairsUndeclared is how many event pairs outside the pairs declared with Independent Build
	// also checked, without failing on them (gsm v0.13.0 and later); 0 when every pair is
	// certified, the default.
	PairsUndeclared int `json:"pairs_undeclared"`

	// CausalOrderRequired lists the undeclared pairs that do not commute. Convergence holds only
	// if the two events of each listed pair are causally ordered (one is issued after the other is
	// observed) and applied in that order, never concurrently. A log-backed governor
	// replays one shared EventLog in one total order, and a writer acts on a state folded from the
	// log before its append, so that order respects every causal dependency that runs through the
	// log. Declaring the pairs is the claim that the events of every other pair are never
	// concurrent; bide does not track causality, so that claim is the caller's.
	CausalOrderRequired []EventPair `json:"causal_order_required"`

	// NotIdempotent lists the events whose second application changes the state, so a duplicate
	// delivery changes the result. ApplyOnce applies an event at most once per id across every
	// process sharing the log, and EventTool and FederatedEventTool key it by the tool call. Apply
	// mints a fresh id per call, so a caller that retries Apply itself applies the event again.
	NotIdempotent []string `json:"not_idempotent"`

	// Saturations lists the rules whose write was clamped into a variable's range on some state
	// Build checked. Clamping is part of the verified semantics, so convergence is unaffected,
	// but an invariant meant to catch the overflow never sees it.
	Saturations []Saturation `json:"saturations"`

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
// Registry.PolicyDigest. A Report whose machine failed to build still produces a certificate; it
// simply reports Converges == false, which a caller should refuse to deploy. That includes a
// machine that passed gsm's WFC and CC checks but that gsm's in-process oracle gate refused
// (Report.OracleDisagreement): Build returned no machine, so its Assurance is none.
func CertifyConvergence(rep *gsm.Report, policyDigest string) ConfluenceCertificate {
	causal := make([]EventPair, 0, len(rep.CausalOrderRequired))
	for _, f := range rep.CausalOrderRequired {
		causal = append(causal, EventPair{First: f.Event1, Second: f.Event2})
	}
	sat := make([]Saturation, 0, len(rep.Saturations))
	for _, s := range rep.Saturations {
		sat = append(sat, Saturation{Rule: s.Rule, Var: s.Var, States: s.States})
	}
	return ConfluenceCertificate{
		Machine:             rep.Name,
		PolicyDigest:        policyDigest,
		Converges:           rep.WFC && rep.CC && rep.Assurance != gsm.AssuranceNone,
		WFC:                 rep.WFC,
		CC:                  rep.CC,
		MaxRepairLen:        rep.MaxRepairLen,
		PairsTotal:          rep.PairsTotal,
		PairsDisjoint:       rep.PairsDisjoint,
		PairsBrute:          rep.PairsBrute,
		PairsUndeclared:     rep.PairsUndeclared,
		CausalOrderRequired: causal,
		NotIdempotent:       append(make([]string, 0, len(rep.NotIdempotent)), rep.NotIdempotent...),
		Saturations:         sat,
		States:              rep.StateCount,
		CompensationFree:    rep.WFC && rep.MaxRepairLen == 0,
	}
}

// EventPair names two events, as gsm reports a pair that does not commute.
type EventPair struct {
	First  string `json:"first"`
	Second string `json:"second"`
}

// Saturation is a rule whose write was clamped into a variable's range (gsm.Saturation): the
// rule, the variable written, and on how many of the states Build checked.
type Saturation struct {
	Rule   string `json:"rule"`
	Var    string `json:"var"`
	States int    `json:"states"`
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
		if n := len(c.CausalOrderRequired); n > 0 {
			verdict = fmt.Sprintf("GUARANTEED under causal delivery of the %d undeclared pair(s) below", n)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b,
		"Convergence: %s  [%s]\n  machine: %s  policy: %s\n  WFC: %v (max repair depth %d)  CC: %v (%d pairs: %d disjoint, %d brute)\n  checked exhaustively over %d states",
		verdict, c.Classification(), c.Machine, short(c.PolicyDigest),
		c.WFC, c.MaxRepairLen, c.CC, c.PairsTotal, c.PairsDisjoint, c.PairsBrute, c.States)
	if len(c.CausalOrderRequired) > 0 {
		pairs := make([]string, len(c.CausalOrderRequired))
		for i, p := range c.CausalOrderRequired {
			pairs[i] = p.First + "/" + p.Second
		}
		fmt.Fprintf(&b, "\n  causal order required for %d undeclared pair(s): %s (a shared EventLog replays one causally consistent order; that these pairs are never concurrent is the caller's claim)",
			len(pairs), strings.Join(pairs, ", "))
	}
	if len(c.NotIdempotent) > 0 {
		fmt.Fprintf(&b, "\n  delivery: exactly once: %s (ApplyOnce deduplicates by id)", strings.Join(c.NotIdempotent, ", "))
	}
	for _, s := range c.Saturations {
		fmt.Fprintf(&b, "\n  saturation: %s clamps its write to %s on %d state(s)", s.Rule, s.Var, s.States)
	}
	return b.String()
}

func short(digest string) string {
	if len(digest) <= 16 {
		return digest
	}
	return digest[:16] + "..."
}
