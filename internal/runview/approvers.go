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
	// RecordedAt is when bide journaled the decision, by bide's clock. Both are display text
	// in UTC ("2026-10-13 12:14:03.123"), empty when the record holds none. Gap is the difference
	// between them in words, shown without judgment, and Flags the sanity notes on them
	// (audit.FlagDecisionTimes, under audit.DefaultDecisionTimeTolerance), which never change
	// the gate's outcome. Empty for a pending approver.
	DecidedAt, RecordedAt string
	Gap                   string
	Flags                 []string
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
	names := slices.Clone(g.Approvers)
	for _, n := range slices.Concat(g.ApprovedBy, g.DeniedBy) {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	var out []ApproverCard
	for _, n := range names {
		c := ApproverCard{Name: n, Initials: initials(n), Fingerprint: chk.Keys[n]}
		switch {
		case slices.Contains(g.ApprovedBy, n):
			c.State = "approved"
		case slices.Contains(g.DeniedBy, n):
			c.State = "denied"
		default:
			c.State = "pending"
		}
		if c.State != "pending" {
			if t, ok := decisionTime(g, chk, n, c.State == "approved"); ok {
				c.DecidedAt, c.RecordedAt = displayTime(t.DecidedAt), displayTime(t.RecordedAt)
				c.Gap, c.Flags = gapWords(t), t.Flags
			}
			switch {
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

// HasTimes reports whether any of the gate's approver cards shows a decision time.
func (g Gate) HasTimes() bool {
	return slices.ContainsFunc(g.Cards, func(c ApproverCard) bool { return c.DecidedAt != "" || c.RecordedAt != "" })
}

// decisionTime is the two times of approver n's decision on g, with their sanity flags: from the
// signature check's verdict when it counted the decision (the exact decision the recount
// counted), else from the journal, the approver's first signed decision with that outcome, the
// one the counting rule would count if its signature verifies.
func decisionTime(g Gate, chk ApprovalCheck, n string, approved bool) (audit.DecisionTime, bool) {
	if t, ok := chk.Times[n]; ok {
		return t, true
	}
	for _, d := range g.Decisions {
		if d.Signed && d.Approver == n && d.Approved == approved {
			t := audit.DecisionTime{Approver: n, DecidedAt: d.DecidedAt, RecordedAt: d.RecordedAt}
			return audit.FlagDecisionTimes([]audit.DecisionTime{t}, g.AskedAt, audit.DefaultDecisionTimeTolerance)[0], true
		}
	}
	return audit.DecisionTime{}, false
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
	switch {
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
