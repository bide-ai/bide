package runview

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/bide-ai/bide/agent"
	"github.com/bide-ai/bide/audit"
)

// ApproverCard is one approver of an approval gate, as the page shows them: who, what they decided,
// whether their signature verified, the fingerprint of their key, and when they decided and when
// bide recorded it.
type ApproverCard struct {
	Name     string
	Initials string
	// State is approved, denied or pending.
	State string
	// Verify says what is known of the signature, in words: "signature verified", "signature NOT
	// verified", "in the signed log, signature not checked" or "signature not checked". VerifyOK
	// reports the first. Empty for a pending approver.
	Verify   string
	VerifyOK bool
	Why      string // why the decision did not count, when it did not
	// Fingerprint is the approver's key as "ed25519:3b6a27bc" (the first 8 hex digits of the
	// SHA-256 of its public key), when -approver-keys gave it.
	Fingerprint string
	// DecidedAt is when the approver says they decided, by their clock, inside the signature;
	// RecordedAt is when the decision was journaled, by the submitter's clock (not signed). Both are display text
	// in UTC ("2026-10-13 12:14:03.123"), empty when the record holds none. Gap is the difference
	// between them in words, shown without judgment, and Flags the sanity notes on them
	// (audit.FlagDecisionTimes, under audit.DefaultDecisionTimeTolerance), which never change
	// the gate's outcome. Empty for a pending approver.
	DecidedAt, RecordedAt string
	Gap                   string
	Flags                 []string
	// TimesNote says why no times are shown when the approver has several signed decisions with
	// the card's outcome and their signatures were not checked: which one counted, and so which
	// decided_at is signed, is not known.
	TimesNote string
	// Identity is the identity of a certificate-bound approver's counted decision (see
	// IdentityCard), nil for a bare-key approver and for a decision no identity record shows.
	// Binding says which kind of approver this is on a gate with a certificate-bound approver:
	// "certificate-bound" or "bare key (no identity)", empty on a gate of bare keys. IdentityNote is
	// what to read instead of a signature line for a certificate-bound approver whose decision has no
	// identity card, and IdentityBad that it is a problem.
	Identity     *IdentityCard
	Binding      string
	IdentityNote string
	IdentityBad  bool
}

// initials is up to two letters standing for an approver id: the first letters of its words
// ("risk-lead" is RL), or its first two ("ops" is OP).
func initials(id string) string {
	words := strings.FieldsFunc(id, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var out []rune
	switch {
	case len(words) >= 2:
		for _, w := range words[:2] {
			out = append(out, []rune(w)[0])
		}
	case len(words) == 1:
		out = []rune(words[0])
		out = out[:min(2, len(out))]
	default:
		return "?"
	}
	return strings.ToUpper(string(out))
}

// fingerprints is each approver's key fingerprint under keys, the approvers that have a key only.
func fingerprints(approvers []string, keys agent.ApproverVerifierFor) map[string]string {
	if keys == nil {
		return nil
	}
	out := map[string]string{}
	for _, a := range approvers {
		v, ok := keys(a)
		if !ok {
			continue
		}
		var fps []string
		for _, id := range v.KeyIDs() {
			alg, hexKey, ok := strings.Cut(id, ":")
			if !ok {
				alg, hexKey = "", id
			}
			if len(hexKey) > 8 {
				hexKey = hexKey[:8]
			}
			if alg != "" {
				hexKey = alg + ":" + hexKey
			}
			fps = append(fps, hexKey)
		}
		if len(fps) > 0 {
			out[a] = strings.Join(fps, " + ")
		}
	}
	return out
}

// cards builds the gate's approver cards from its decisions and the proof's approval check, if
// there is one. A one-decision gate is not signed: it has none.
func cards(g Gate, p *Proof) []ApproverCard {
	if g.Single {
		return nil
	}
	chk, checked := p.approval(g.ToolUseID)
	var headAt string // when the signed head over the journal was signed, if one was given
	if p != nil && !p.SignedAt.IsZero() {
		headAt = agent.FormatTimestamp(p.SignedAt)
	}
	names := slices.Clone(g.Approvers)
	for _, n := range slices.Concat(g.ApprovedBy, g.DeniedBy) {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	var out []ApproverCard
	for _, n := range names {
		c := ApproverCard{Name: n, Initials: initials(n), Fingerprint: chk.Keys[n]}
		certBound := g.Identity != nil && g.Identity.Bound && slices.Contains(g.Identity.Approvers, n)
		if g.Identity != nil && g.Identity.Bound {
			c.Binding = "bare key (no identity)"
			if certBound {
				c.Binding = "certificate-bound"
			}
		}
		switch {
		case certBound && slices.Contains(g.Identity.Unchecked, n):
			c.State = "recorded"
			c.IdentityNote = "decision recorded, identity not checked: the gate counts it only after it has checked the chain, the identity and the revocation status"

		case slices.Contains(g.Excluded, n):
			// Fills no seat: the gate excludes the run's requester (or the tally excluded them).
			c.State = "excluded"
			if slices.ContainsFunc(g.Requesters, func(r string) bool { return agent.SameApprover(n, r) }) {
				c.Why = "excluded: the run's requester, who may not approve their own request (separation of duties)"
			} else {
				c.Why = "excluded: fills no seat in this gate"
			}
			out = append(out, c)
			continue
		case slices.Contains(g.ApprovedBy, n):
			c.State = "approved"
		case slices.Contains(g.DeniedBy, n):
			c.State = "denied"
		default:
			c.State = "pending"
			// A decision written after the gate's expiry: on record, but the expiry was the outcome.
			if i := slices.IndexFunc(g.Decisions, func(d Decision) bool { return d.Approver == n && d.Note != "" }); i >= 0 {
				c.Why = g.Decisions[i].Note
			}
		}
		if c.State != "pending" {
			t, ok, many := decisionTime(g, chk, headAt, n, c.State == "approved")
			switch {
			case ok:
				c.DecidedAt, c.RecordedAt = displayTime(t.DecidedAt), displayTime(t.RecordedAt)
				c.Gap, c.Flags = gapWords(t), t.Flags
			case many > 1:
				c.TimesNote = fmt.Sprintf("%d signed decisions with this outcome on record, and only the one that counted carries a time its signature covers: pass -approver-keys to show it", many)
			}
			switch {
			case certBound:
				// The identity block speaks for a certificate-bound approver; a bare signature line
				// would claim more than this page checked.
				if i := slices.IndexFunc(g.Identity.Cards, func(ic IdentityCard) bool { return ic.Approver == n }); i >= 0 {
					c.Identity = &g.Identity.Cards[i]
				} else if c.State == "recorded" {
					// said above
				} else if c.State == "approved" {
					c.IdentityNote, c.IdentityBad = "counted, but the journal holds no identity record for this decision", true
				} else {
					c.IdentityNote = "denial: no identity is shown for it"
				}
			case checked && (slices.Contains(chk.Counted, n) || slices.Contains(chk.Denied, n)):
				c.Verify, c.VerifyOK = "signature verified", true
			case checked:
				c.Verify, c.Why = "signature NOT verified", chk.Ignored[n]
			case p != nil && p.OK:
				c.Verify = "in the signed log, signature not checked"
			default:
				c.Verify = "signature not checked"
			}
		}
		out = append(out, c)
	}
	return out
}

// DecisionAt is when a one-decision gate's decision was journaled, as the page shows it
// ("2026-10-13 12:14:03.123"): the claim of the process that called agent.Approve, not signed. It
// is empty for an m-of-n gate (its approver cards show the times), for a gate with no decision,
// and for a decision that records none. A time that is not in the canonical form is not shown.
func (g Gate) DecisionAt() string {
	if !g.Single || len(g.Decisions) == 0 {
		return ""
	}
	s, _ := parseRecorded(g.Decisions[0].RecordedAt)
	return s
}

// HasTimes reports whether any of the gate's approver cards shows a decision time.
func (g Gate) HasTimes() bool {
	return slices.ContainsFunc(g.Cards, func(c ApproverCard) bool { return c.DecidedAt != "" || c.RecordedAt != "" })
}

// decisionTime is the two times of approver n's decision on g, with their sanity flags: from the
// signature check's verdict when it counted the decision (the exact decision the recount
// counted), else from the journal when the approver has exactly one signed decision with that
// outcome. With several and no check, the one that counted is not known (a copy of a valid
// signature under another decided_at never counts, but would read the same here), so none is
// shown: ok is false and many is how many there are.
func decisionTime(g Gate, chk ApprovalCheck, headAt string, n string, approved bool) (t audit.DecisionTime, ok bool, many int) {
	if t, ok := chk.Times[n]; ok {
		return t, true, 1
	}
	var only Decision
	for _, d := range g.Decisions {
		if d.Signed && d.Approver == n && d.Approved == approved {
			only = d
			many++
		}
	}
	if many != 1 || only.DecidedAt == "" && only.RecordedAt == "" {
		return audit.DecisionTime{}, false, many // none, several, or one an earlier version wrote
	}
	t = audit.DecisionTime{Approver: n, DecidedAt: only.DecidedAt, RecordedAt: only.RecordedAt}
	return audit.CheckDecisionTimes([]audit.DecisionTime{t}, audit.TimeCheck{AskedAt: g.AskedAt, HeadAt: headAt, Tolerance: audit.DefaultDecisionTimeTolerance})[0], true, 1
}

// displayTime renders a recorded time ("2026-10-13T12:14:03.123Z") as "2026-10-13 12:14:03.123"
// (the page says the times are UTC); a value not in canonical form is shown as it is, marked, and
// an empty one stays empty.
func displayTime(s string) string {
	if s == "" {
		return ""
	}
	t, err := agent.ParseTimestamp(s)
	if err != nil {
		return fmt.Sprintf("%q (not a canonical time)", s)
	}
	return t.Format("2006-01-02 15:04:05.000")
}

// gapWords phrases how long after the signed decision time bide recorded the decision, e.g.
// "recorded 2.5s after the signed time"; empty when either time is missing.
func gapWords(t audit.DecisionTime) string {
	gap, ok := t.Gap()
	_, errD := agent.ParseTimestamp(t.DecidedAt)
	_, errR := agent.ParseTimestamp(t.RecordedAt)
	switch {
	case !ok && errD == nil && errR == nil:
		return "more than 290 years apart"
	case !ok:
		return ""
	case gap == 0:
		return "recorded in the same millisecond"
	case gap > 0:
		return "recorded " + gap.Round(time.Millisecond).String() + " after the signed time"
	default:
		return "recorded " + (-gap).Round(time.Millisecond).String() + " before the signed time"
	}
}

// Plain reports whether the card may be drawn as an ordinary approval: a bare-key approver's, or a
// certificate-bound approver's whose identity an audit result re-checked. A certificate-bound approval
// that is only final as recorded, provisional, bad or missing is never drawn in the approved (green) style.
func (c ApproverCard) Plain() bool {
	return c.Binding != "certificate-bound" || c.Identity != nil && c.Identity.Audited
}

// Style is the card's state class: the state, except that a certificate-bound approval that is not
// plain has its own: idrecorded (final as recorded, neutral) or idcounted (anything else).
func (c ApproverCard) Style() string {
	switch {
	case c.State != "approved" || c.Plain():
		return c.State
	case c.Identity != nil && c.Identity.Class == identFinal:
		return "idrecorded"
	}
	return "idcounted"
}

// IdentityWord is a non-final approval's identity in a word, for the card's state line.
func (c ApproverCard) IdentityWord() string {
	if c.Identity != nil {
		return c.Identity.TierText
	}
	return "NOT SHOWN"
}
