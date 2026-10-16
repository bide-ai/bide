package runview

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"
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
// Every value is the gate's claim at counting ("as recorded"), never a verification. The styles it
// derives: "audited" (the only green) needs a result from an audit the viewer was given (ApprovalCheck.Identity)
// that re-checked every approving decision and found none disputed; "final" is final as recorded, in a
// neutral style; provisional, waiting, held, incomplete, stripped, mismatched and disputed each have their own
// visibly non-final style. Nothing is called "verified" without an audit result.
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
	// Paused: the newest phase note says exec-hold but the journal holds no hold record, which is the
	// gate waiting for current status before it runs the call (nothing authoritative is written; the same
	// approvals run once status is fresh), not a revoked or no-longer-verifying decision.
	Paused                    bool   `json:"paused,omitempty"`
	tallyDigest, recordPolicy string // the current count's, for matching an audit result
	// Generation is the gate's newest count (0 is the first); Reasked says a call was held before
	// execution and the gate asked again, so earlier counts are history.
	Generation int  `json:"generation,omitempty"`
	Reasked    bool `json:"reasked,omitempty"`
	// Problems are why the identity data cannot be relied on, in words.
	Problems []string `json:"problems,omitempty"`
	// Class is audited, final, prov, bad or wait (see the type's comment); Line is one line saying it.
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
	// TierText names the tier in words; Class is audited, final, prov or bad.
	TierText  string `json:"tier_text"`
	Class     string `json:"class"`
	BasisKind string `json:"basis_kind,omitempty"`
	Basis     string `json:"basis,omitempty"`
	// Audited: an audit the viewer was given re-checked this decision and agrees (green only then);
	// StatusAge is S's age on the viewer's clock (MarkDeadlines).
	Audited   bool   `json:"audited,omitempty"`
	StatusAge string `json:"status_age,omitempty"`
	statusAt  string
	// StatusTime is S, the time of the revocation status the count rests on; AnchoredAt is when the
	// decision became provable and TimeSource what that time rests on; Window is W'. All are display
	// text in UTC.
	StatusTime string   `json:"status_time,omitempty"`
	AnchoredAt string   `json:"anchored_at,omitempty"`
	TimeSource string   `json:"time_source,omitempty"`
	Window     string   `json:"window,omitempty"`
	Leaf       string   `json:"leaf,omitempty"` // the first 16 hex digits of the leaf certificate's hash
	Warnings   []string `json:"warnings,omitempty"`
	// Disputed is set only from an audit result (ApprovalCheck.Identity), never from the journal: an audit found newer status showing a revocation (keyCompromise) effective at or
	// before the decision became provable, which voids the count back to the certificate's notBefore.
	Disputed bool `json:"disputed,omitempty"`
}

// IdentityHold is the pre-execution status re-check that held a call (agent.ExecHold).
type IdentityHold struct {
	Reason     string   `json:"reason"`
	Detail     string   `json:"detail,omitempty"`
	Approvers  []string `json:"approvers,omitempty"`
	CheckedAt  string   `json:"checked_at,omitempty"`
	StatusTime string   `json:"status_time,omitempty"`
	Serial     string   `json:"serial,omitempty"`
}

// Identity classes.
const (
	identAudited = "audited"
	identFinal   = "final"
	identProv    = "prov"
	identBad     = "bad"
	identWait    = "wait"
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

// tierWords names a tier for a person. The final tiers are "final as recorded": the gate's claim at
// counting. Every other value, including one this version does not know, is "not verified" and bad.
func tierWords(tier string) (text, class string) {
	switch tier {
	case agent.TierIndependentFinal:
		return "final as recorded (independent time)", identFinal
	case agent.TierLogclockFinal:
		return "final as recorded (log signer's clock)", identFinal
	case agent.TierProvisional:
		return "PROVISIONAL", identProv
	}
	return "NOT VERIFIED", identBad
}

// sourceWords names a time source (agent.TimeSource*).
func sourceWords(src, evidence string) string {
	switch src {
	case agent.TimeSourceLogSignerClock:
		return "log signer's clock"
	case agent.TimeSourceRFC3161:
		return "RFC 3161 time-stamp"
	case agent.TimeSourceAnchorReceipt:
		return "anchor receipt"
	}
	return "unknown time source " + printable(src)
}

// phaseWords is a pause phase in words for a person; "" for a gate that is waiting for decisions. The
// phase is a hint the authoritative records override.
func phaseWords(phase, detail string) string {
	switch phase {
	case agent.PauseAwaitingAnchor:
		return "decision recorded, not yet provable"
	case agent.PauseAwaitingStatus:
		return "paused: identity status unknown"
	case agent.PauseHeldUntilFinal:
		return "counted provisionally, held until final"
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
	return shorten(printable(c.Subject.CommonName), 48), ""
}

// latestIdentTally returns the :ident tally of the gate's newest count and its generation: the highest
// generation with an identity tally or an exec hold (agent.ApprovalIdentTallyStep, ApprovalExecHoldStep).
func latestIdentTally(byName map[string]agent.Record, id string) (r agent.Record, gen int, ok bool) {
	gen = -1
	for k := 0; ; k++ {
		_, t := byName[agent.ApprovalIdentTallyStep(id, k)]
		_, h := byName[agent.ApprovalExecHoldStep(id, k)]
		if !t && !h {
			break
		}
		gen = k
	}
	if gen < 0 {
		return agent.Record{}, 0, false
	}
	r, ok = byName[agent.ApprovalIdentTallyStep(id, gen)]
	return r, gen, ok
}

// identNotes reads the identity members of every pause note of the call (the first note, a re-ask's, the
// phase notes): identity_bound is the OR of them, so a later note from a binary that predates the
// feature never downgrades the display, and notes that state different policy digests are flagged.
func identNotes(recs []agent.Record, byName map[string]agent.Record, id string, latest agent.ApprovalWait) (bound bool, approvers []string, digest string, changed bool) {
	seen := map[string]bool{}
	add := func(w agent.ApprovalWait) {
		if w.IdentityBound {
			bound = true
			if len(approvers) == 0 {
				approvers = w.IdentityApprovers
			}
		}
		if w.IdentityPolicyDigest != "" {
			if digest == "" {
				digest = w.IdentityPolicyDigest
			}
			seen[w.IdentityPolicyDigest] = true
		}
	}
	add(latest)
	prefix := strings.TrimSuffix(agent.ApprovalPauseStep(id, 1), "1")
	for _, r := range recs {
		if r.Kind == agent.StepValue && (strings.HasPrefix(r.Name, prefix) || r.Name == agent.ApprovalPendingStep(id) ||
			r.Name == agent.ApprovalReaskStep(id, true) || r.Name == agent.ApprovalReaskStep(id, false)) {
			var w agent.ApprovalWait
			if json.Unmarshal(r.Result, &w) == nil {
				add(w)
			}
		}
	}
	return bound, approvers, digest, len(seen) > 1
}

// identityOf derives the gate's identity block from the journal, or nil for a gate with no
// certificate-bound approver. Precedence, per the newest generation: a result means executed; else a
// hold means held; else a tally means decided (its tier from the identity record); else the newest phase
// note, which is only a hint.
func identityOf(recs []agent.Record, byName map[string]agent.Record, id string, latest agent.ApprovalWait, g Gate, resulted bool) *GateIdentity {
	bound, approvers, digest, changed := identNotes(recs, byName, id, latest)
	tally, gen, haveTally := latestIdentTally(byName, id)
	certInDecision := slices.ContainsFunc(recs, func(r agent.Record) bool {
		return r.Kind == agent.StepApproval && r.ToolUseID == id && len(r.ApproverCert()) > 0
	})
	if !bound && !haveTally && !certInDecision {
		return nil
	}
	gi := &GateIdentity{Bound: true, Approvers: approvers, PolicyDigest: printable(digest), Generation: gen, Reasked: gen > 0}
	if w, ok := agent.LatestPausePhase(recs, id); ok {
		gi.Phase, gi.Detail = w.Pause, printable(w.PauseDetail)
	} else {
		gi.Phase, gi.Detail = latest.Pause, printable(latest.PauseDetail)
	}
	gi.PhaseText = printable(phaseWords(gi.Phase, gi.Detail))
	problem := func(s string) { gi.Problems = append(gi.Problems, s) }
	if !bound && !haveTally {
		problem("identity data stripped: a decision carries a certificate chain, but the gate records no identity (no note marks it identity-bound and there is no identity tally)")
	}
	if resulted && !haveTally {
		problem("the call ran, but the journal holds no identity tally for its gate")
	}
	if changed {
		problem("policy changed: the gate's notes state different policy digests")
	}
	if _, plain := byName[agent.ApprovalTallyStep(id)]; plain && !haveTally {
		problem("superseded (counted without identity): a tally under the plain name on an identity-bound gate is not the gate's outcome, and the gate asks again")
	}
	if haveTally {
		td := sha256.Sum256(tally.Result)
		want := hex.EncodeToString(td[:])
		var rec agent.ApprovalIdentity
		found := false
		for _, name := range []string{agent.ApprovalIdentityStep(id, gen), agent.ApprovalIdentityDigestStep(id, gen, want)} {
			if r, ok := byName[name]; ok && json.Unmarshal(r.Result, &rec) == nil && rec.TallyDigest == want {
				found = true
				break
			}
		}
		switch {
		case !found:
			problem("incomplete identity data: the identity tally has no approval-identity record that names it, so nothing here says what its count rested on")
		case digest != "" && rec.PolicyDigest != digest:
			problem("policy differs: the identity record's policy digest is not the one the gate paused under")
		case digest == "":
			problem("policy of record missing: the pause notes state no policy digest to check the identity record against")
		}
		gi.tallyDigest = want
		if found {
			gi.recordPolicy = rec.PolicyDigest
			gi.PolicyDigest = cmpStr(gi.PolicyDigest, printable(rec.PolicyDigest))
			for _, d := range rec.Decisions {
				text, class := tierWords(d.Tier)
				name, why := displayName(byName, d.Step, d.LeafHash)
				if why != "" {
					problem(why)
				}
				c := IdentityCard{Approver: printable(d.Approver), Identifier: printable(d.Identifier), DisplayName: name, Tier: printable(d.Tier), TierText: text, Class: class,
					BasisKind: printable(d.BasisKind), Basis: printable(d.Basis), Window: printable(d.Window), TimeSource: sourceWords(d.TimeSource, d.TimeEvidence), statusAt: d.StatusTime}
				if class == identFinal && d.BasisKind != agent.BasisAtOrAfterDecision {
					problem("a final tier whose basis is " + cond(d.BasisKind == "", "missing", "\""+printable(d.BasisKind)+"\"") + " rather than status at or after the decision: only that basis is final (a live count is provisional only)")
					c.Class = identBad
				}
				c.StatusTime, c.AnchoredAt = displayStamp(d.StatusTime), displayStamp(d.AnchoredAt)
				if len(d.LeafHash) >= 16 {
					c.Leaf = printable(d.LeafHash[:16])
				}
				for _, w := range d.Warnings {
					c.Warnings = append(c.Warnings, printable(w))
				}
				gi.Cards = append(gi.Cards, c)
			}
		}
	}
	if r, ok := byName[agent.ApprovalExecHoldStep(id, gen)]; ok && !resulted {
		var h agent.ExecHold
		if json.Unmarshal(r.Result, &h) == nil {
			gi.Hold = &IdentityHold{Reason: printable(h.Reason), Detail: printable(h.Detail), CheckedAt: displayStamp(h.CheckedAt), StatusTime: displayStamp(h.StatusTime), Serial: printable(h.Serial)}
			for _, a := range h.Approvers {
				gi.Hold.Approvers = append(gi.Hold.Approvers, printable(a))
			}
			gi.Phase = agent.PauseExecHold
			gi.PhaseText = "held before execution: " + cmpStr(gi.Hold.Detail, gi.Hold.Reason)
		}
	}
	// The phase note is a hint, but a counted gate whose newest note says exec-hold and whose call has no
	// result is shown paused even when the hold record is absent: never as final.
	if gi.Hold == nil && haveTally && !resulted && gi.Phase == agent.PauseExecHold {
		gi.Paused = true
		gi.PhaseText = "paused before execution: waiting for current status" + cond(gi.Detail != "", " ("+gi.Detail+")", "")
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
	// The identity records write RFC 3339 times with nanoseconds, trimmed of trailing zeros (so
	// "2026-10-16T18:35:22Z" is canonical there), unlike the decision times (agent.FormatTimestamp).
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC().Format("2006-01-02 15:04:05.000")
	}
	return printable(displayTime(s))
}

// identityClass is the style and the line of a gate's identity. "final" (as recorded) needs a counted
// gate, no problem, no hold, and a final card for every approving certificate-bound approver; it becomes
// audited only when an audit result re-checked every one of them (applyIdentityAudit).
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
	case gi.Paused:
		return identWait, gi.PhaseText
	case gi.Hold != nil:
		return identBad, gi.PhaseText + cond(gi.Reasked, " (re-asked after an earlier hold)", "")
	}
	worst := identFinal
	for _, c := range gi.Cards {
		switch c.Class {
		case identBad:
			return identBad, "identity " + c.TierText
		case identProv:
			worst = identProv
		}
	}
	for _, a := range g.ApprovedBy {
		if slices.Contains(gi.Approvers, a) && !slices.ContainsFunc(gi.Cards, func(c IdentityCard) bool { return c.Approver == a }) {
			return identBad, "no identity record for the counted decision of " + printable(a)
		}
	}
	if gi.Phase == agent.PauseHeldUntilFinal {
		worst = identProv // the gate's newest note says the count must be final before the call runs
	}
	if worst == identProv {
		return identProv, "counted provisionally: a later status could change it"
	}
	if len(gi.Cards) == 0 {
		return identWait, "no certificate-bound decision counted"
	}
	return identFinal, "identity " + gi.Cards[0].TierText + "; not re-verified here"
}

func cond(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

// applyIdentityAudit folds an audit's result for the gate (ApprovalCheck.Identity, by approver) into its
// identity block: a decision the audit disputes is bad and says why (a keyCompromise revocation effective
// at or before the decision became provable voids it back to notBefore), and the block is audited, the
// only green, when the audit re-checked every certificate-bound decision and agrees with the record.
func applyIdentityAudit(g *Gate, res map[string]IdentityAudit) {
	gi := g.Identity
	if gi == nil || res == nil {
		return
	}
	all := len(gi.Cards) > 0
	var audited []string
	for i := range gi.Cards {
		c := &gi.Cards[i]
		r, ok := res[c.Approver]
		switch {
		case ok && (r.TallyDigest == "" || gi.tallyDigest == "" || r.TallyDigest != gi.tallyDigest || r.PolicyDigest != gi.recordPolicy):
			// An audit of another count (an older tally, another generation or policy) says nothing of this one.
			gi.Problems = append(gi.Problems, "an audit result for "+printable(c.Approver)+" is for another count or policy than the journal's current one, and is not applied")
			all = false
		case !ok:
			all = false
		case r.Disputed:
			c.Class, c.Disputed, all = identBad, true, false
			gi.Problems = append(gi.Problems, "DISPUTED: "+shorten(printable(cmpStr(r.Detail, "newer status shows a revocation effective at or before the decision became provable (keyCompromise voids it back to the certificate's notBefore)")), 200))
		case r.OK && c.Class == identFinal:
			c.Audited = true
			if r.CheckedAt != "" {
				audited = append(audited, printable(c.Approver)+" at "+displayStamp(r.CheckedAt)+" UTC")
			}
		default:
			all = false
		}
	}
	gi.Class, gi.Line = identityClass(gi, *g)
	if gi.Class == identFinal && all {
		gi.Class, gi.Line = identAudited, "identity re-checked by an audit ("+strings.Join(audited, ", ")+"), status as of S; revocations first reported after S are not covered"
	}
}

// markStatusAges sets each card's StatusAge: how old S is on now, and the caveat that a revocation first
// reported after S is not covered.
func markStatusAges(gates []Gate, now time.Time) {
	for i := range gates {
		gi := gates[i].Identity
		if gi == nil {
			continue
		}
		for j := range gi.Cards {
			c := &gi.Cards[j]
			if t, err := agent.ParseTimestamp(c.statusAt); err == nil && !now.Before(t) {
				c.StatusAge = "S is " + durText(now.Sub(t)) + " old"
			}
		}
	}
}

// shorten cuts s to at most n runes, marking the cut.
func shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
