package runview

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"unicode"

	"github.com/bide-ai/bide/agent"
)

// GateIdentity is what the page and bide approve -list show of a gate with a certificate-bound approver
// (docs/design/approver-identity.md): which approvers are bound to a certificate, the policy digest
// the gate paused under, what the gate paused for, each counted decision's identity (IdentityCard),
// and what does not check out. It is read from the journal alone (the pause note, the :ident tally,
// the approval-identity record and the decision records); this page does not recompute the
// policy digest, verify a certificate chain or fetch status. That is the gate's job when it counts
// and the auditor's when it re-checks.
//
// The rule for every style it derives: only a bound, counted gate whose identity record is present,
// matches the pause note's policy digest, and holds a final tier for every approving decision is
// "final". Provisional, still waiting, held, stripped, mismatched and disputed are each a different,
// visibly non-final style, and none is ever drawn in the verified (green) style.
type GateIdentity struct {
	Bound bool `json:"bound"`
	// Approvers are the approver ids that are certificate-bound, in policy order; the other approvers
	// of the gate are bare keys, whose decisions carry no identity.
	Approvers    []string `json:"approvers,omitempty"`
	PolicyDigest string   `json:"policy_digest,omitempty"`
	// Phase is the pause note's Pause (agent.Pause*), PhaseText the words for it ("paused: identity
	// status unknown"), empty when the gate is simply waiting for decisions, and Detail the note's
	// sentence.
	Phase     string `json:"phase,omitempty"`
	PhaseText string `json:"phase_text,omitempty"`
	Detail    string `json:"detail,omitempty"`
	// Unchecked are certificate-bound approvers who decided on a gate that has not counted yet: the
	// decision is recorded, and nothing here says it counts.
	Unchecked []string       `json:"unchecked,omitempty"`
	Cards     []IdentityCard `json:"cards,omitempty"`
	Hold      *IdentityHold  `json:"hold,omitempty"`
	// Problems are why the identity data cannot be relied on, in words.
	Problems []string `json:"problems,omitempty"`
	// Class is final, prov, bad or wait (see the type's comment); Line is one line saying it.
	Class string `json:"class"`
	Line  string `json:"line"`
}

// IdentityCard is one counted decision's identity as the identity record keeps it.
type IdentityCard struct {
	Approver   string `json:"approver"`
	Identifier string `json:"identifier"`
	// DisplayName is the certificate's subject common name, read from the decision record when its
	// leaf is the one the identity record names. It is the CA's label, not part of the identity.
	DisplayName string `json:"display_name,omitempty"`
	Tier        string `json:"tier"`
	// TierText names the tier in words; Class is final, prov or bad.
	TierText string `json:"tier_text"`
	Class    string `json:"class"`
	Basis    string `json:"basis,omitempty"`
	// StatusTime is S, the time of the revocation status the count rests on; AnchoredAt is when the
	// decision became provable and TimeSource what that time rests on; Window is W'. All are display
	// text in UTC.
	StatusTime string   `json:"status_time,omitempty"`
	AnchoredAt string   `json:"anchored_at,omitempty"`
	TimeSource string   `json:"time_source,omitempty"`
	Window     string   `json:"window,omitempty"`
	Leaf       string   `json:"leaf,omitempty"` // the first 16 hex digits of the leaf certificate's hash
	Warnings   []string `json:"warnings,omitempty"`
	// Disputed: an audit found newer status showing a revocation (keyCompromise) effective at or
	// before the decision became provable, which voids the count back to the certificate's notBefore.
	Disputed bool `json:"disputed,omitempty"`
}

// IdentityHold is the pre-execution status re-check that held a call (agent.ExecHold).
type IdentityHold struct {
	Reason     string `json:"reason"`
	Detail     string `json:"detail,omitempty"`
	Approver   string `json:"approver,omitempty"`
	CheckedAt  string `json:"checked_at,omitempty"`
	StatusTime string `json:"status_time,omitempty"`
}

// Identity classes.
const (
	identFinal = "final"
	identProv  = "prov"
	identBad   = "bad"
	identWait  = "wait"
)

// printable drops what is not safe to show: control characters and the invisible ones.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
}

// tierWords names a tier for a person. Only the two final tiers are final; every other value,
// including one this version does not know, is "not verified" and bad.
func tierWords(tier string) (text, class string) {
	switch tier {
	case agent.TierIndependentFinal:
		return "final (independent time)", identFinal
	case agent.TierLogclockFinal:
		return "final (log signer's clock)", identFinal
	case agent.TierProvisional:
		return "PROVISIONAL", identProv
	}
	return "NOT VERIFIED", identBad
}

// phaseWords is a pause phase in words for a person; "" for a gate that is waiting for decisions.
func phaseWords(phase, detail string) string {
	switch phase {
	case agent.PauseAwaitingAnchor:
		return "decision recorded, not yet provable"
	case agent.PauseAwaitingStatus:
		return "paused: identity status unknown"
	case agent.PauseAwaitingFinal:
		return "counted provisionally, not final"
	case agent.PauseExecHold:
		if detail != "" {
			return "held before execution: " + detail
		}
		return "held before execution"
	}
	return ""
}

// displayName reads the subject common name of the leaf of the decision record named step, when that
// leaf hashes to leaf (the identity record's hash): a certificate that is not the one named is not shown.
func displayName(byName map[string]agent.Record, step, leaf string) (name, problem string) {
	r, ok := byName[step]
	if !ok {
		return "", "the decision record " + printable(step) + " the identity record names is not in the journal"
	}
	chain := r.ApproverCert()
	if len(chain) == 0 {
		return "", "the decision record " + printable(step) + " carries no certificate"
	}
	h := sha256.Sum256(chain[0])
	if hex.EncodeToString(h[:]) != leaf {
		return "", "the certificate in decision record " + printable(step) + " is not the one the identity record names"
	}
	c, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return "", ""
	}
	return printable(c.Subject.CommonName), ""
}

// identityOf derives the gate's identity block from the journal, or nil for a gate with no
// certificate-bound approver. wait is the gate's newest pause note (zero when there is none),
// tallied and resulted say whether the gate counted and whether the call has a result.
func identityOf(byName map[string]agent.Record, id string, wait agent.ApprovalWait, g Gate, resulted bool) *GateIdentity {
	idRec, haveID := byName[agent.ApprovalIdentityStep(id)]
	_, haveTally := byName[agent.ApprovalIdentTallyStep(id)]
	if !wait.IdentityBound && !haveID && !haveTally {
		return nil
	}
	gi := &GateIdentity{Bound: true, Approvers: wait.IdentityApprovers, PolicyDigest: printable(wait.IdentityPolicyDigest), Phase: wait.Pause, Detail: printable(wait.PauseDetail)}
	gi.PhaseText = printable(phaseWords(wait.Pause, gi.Detail))
	problem := func(s string) { gi.Problems = append(gi.Problems, s) }

	if _, plain := byName[agent.ApprovalTallyStep(id)]; plain && !haveTally {
		problem("superseded (counted without identity): a tally under the plain name on an identity-bound gate is not the gate's outcome, and the gate asks again")
	}
	var rec agent.ApprovalIdentity
	if haveID {
		if json.Unmarshal(idRec.Result, &rec) != nil {
			haveID = false
			problem("the approval-identity record cannot be read")
		}
	}
	if g.Tallied {
		switch {
		case !haveID:
			problem("identity data stripped: the gate is identity-bound and counted, but the journal holds no approval-identity record")
		case wait.IdentityPolicyDigest != "" && rec.PolicyDigest != wait.IdentityPolicyDigest:
			problem("policy differs: the identity record's policy digest is not the one the gate paused under")
		case wait.IdentityPolicyDigest == "":
			problem("policy of record missing: the pause note states no policy digest to check the identity record against")
		}
	}
	if haveID {
		gi.PolicyDigest = cmpStr(gi.PolicyDigest, printable(rec.PolicyDigest))
		for _, d := range rec.Decisions {
			text, class := tierWords(d.Tier)
			name, why := displayName(byName, d.Step, d.LeafHash)
			if why != "" {
				problem(why)
			}
			c := IdentityCard{Approver: printable(d.Approver), Identifier: printable(d.Identifier), DisplayName: name,
				Tier: printable(d.Tier), TierText: text, Class: class, Basis: printable(d.Basis), Window: printable(d.Window), TimeSource: printable(d.TimeSource), Disputed: d.Disputed}
			c.StatusTime, c.AnchoredAt = displayStamp(d.StatusTime), displayStamp(d.AnchoredAt)
			if len(d.LeafHash) >= 16 {
				c.Leaf = printable(d.LeafHash[:16])
			}
			for _, w := range d.Warnings {
				c.Warnings = append(c.Warnings, printable(w))
			}
			if d.Disputed {
				c.Class = identBad
			}
			gi.Cards = append(gi.Cards, c)
		}
	}
	if r, ok := byName[agent.ApprovalExecHoldStep(id)]; ok && !resulted {
		var h agent.ExecHold
		if json.Unmarshal(r.Result, &h) == nil {
			gi.Hold = &IdentityHold{Reason: printable(h.Reason), Detail: printable(h.Detail), Approver: printable(h.Approver), CheckedAt: displayStamp(h.CheckedAt), StatusTime: displayStamp(h.StatusTime)}
			gi.Phase = agent.PauseExecHold
			gi.PhaseText = "held before execution: " + cmpStr(gi.Hold.Detail, gi.Hold.Reason)
		}
	}
	gi.Class, gi.Line = identityClass(gi, g)
	return gi
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// displayStamp renders a canonical time as the page shows times, or a marked raw value.
func displayStamp(s string) string {
	if s == "" {
		return ""
	}
	return printable(displayTime(s))
}

// identityClass is the style and the line of a gate's identity. final needs a counted gate, no
// problem, no hold, and a final, undisputed card for every approving certificate-bound approver.
func identityClass(gi *GateIdentity, g Gate) (class, line string) {
	switch {
	case len(gi.Problems) > 0:
		return identBad, "identity data does not check out: " + gi.Problems[0]
	case !g.Tallied:
		switch {
		case gi.PhaseText != "":
			return identWait, gi.PhaseText
		case len(gi.Unchecked) > 0:
			return identWait, "decision recorded, identity not checked yet"
		}
		return identWait, "waiting for decisions"
	case gi.Hold != nil:
		return identBad, gi.PhaseText
	}
	worst := identFinal
	for _, c := range gi.Cards {
		switch c.Class {
		case identBad:
			return identBad, "identity " + c.TierText + cond(c.Disputed, ", DISPUTED", "")
		case identProv:
			worst = identProv
		}
	}
	for _, a := range g.ApprovedBy {
		if slices.Contains(gi.Approvers, a) && !slices.ContainsFunc(gi.Cards, func(c IdentityCard) bool { return c.Approver == a }) {
			return identBad, "no identity record for the counted decision of " + printable(a)
		}
	}
	if gi.Phase == agent.PauseAwaitingFinal {
		worst = identProv // the gate's newest note says the count is not final yet
	}
	if worst == identProv {
		return identProv, "counted provisionally: a later status could change it"
	}
	if len(gi.Cards) == 0 {
		return identWait, "no certificate-bound decision counted"
	}
	return identFinal, "identity " + gi.Cards[0].TierText
}

func cond(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
